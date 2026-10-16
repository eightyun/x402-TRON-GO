// Package postgres 提供可选结算幂等日志及受控提交，不持有密钥或证明付款成功。
package postgres

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/x402-foundation/x402/go/v2/types"
)

// SchemaSQL 需由部署方显式执行，构造 Store 不修改数据库结构。
// 不得删除曾用于广播的日志；运行账号不应拥有 DELETE/TRUNCATE 权限。
//
//go:embed schema.sql
var SchemaSQL string

var (
	ErrInvalidStore     = errors.New("结算日志配置无效")
	ErrInvalidInput     = errors.New("结算日志参数无效")
	ErrStore            = errors.New("结算日志数据库操作失败")
	ErrNotFound         = errors.New("结算日志不存在")
	ErrConflict         = errors.New("授权 nonce 或交易内容已被其他结算占用")
	ErrNotPrepared      = errors.New("结算尚未保存签名交易")
	ErrBroadcastStarted = errors.New("原交易已进入广播或待确认阶段，只允许追踪原交易")
)

// State 仅表示本地执行进度，不代表链上付款结果。
type State string

const (
	StateReserved         State = "reserved"
	StatePrepared         State = "prepared"
	StateBroadcastUnknown State = "broadcast_unknown"
	StatePending          State = "pending"
)

// Transaction 保存可信交易构建器提供的交易 ID 与不透明签名字节。
// 日志只约束不可替换，不验证交易签名、内容与授权的绑定或链上有效性。
// Payload 是可广播的敏感数据，不能记录到日志或从商户 API 返回。
type Transaction struct {
	ID      string
	Payload []byte `json:"-"`
}

// Record 可用于进程重启后恢复原交易，不是资源交付凭据。
type Record struct {
	ID             string
	Permit2Address string
	Authorization  exact.VerifiedAuthorization
	State          State
	Transaction    *Transaction
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Store 可以并发使用；所有实例必须共享同一日志，禁止绕开日志另行广播。
// db 的生命周期及可信 search_path、TLS、权限、备份由部署方管理。
type Store struct {
	db       *pgxpool.Pool
	verifier *exact.OfflineVerifier
	network  string
	permit2  string
}

// New 固定一个可信网络及 Permit2 域，不连接节点、不自动执行 DDL。
func New(db *pgxpool.Pool, config exact.Config) (*Store, error) {
	verifier, err := exact.NewOfflineVerifier(config)
	if db == nil || err != nil {
		return nil, ErrInvalidStore
	}
	network, err := tron.NormalizeNetwork(config.Network)
	if err != nil {
		return nil, ErrInvalidStore
	}
	permit2, err := tron.NormalizeAddressHex(config.Permit2Address)
	if err != nil {
		return nil, ErrInvalidStore
	}
	return &Store{db: db, verifier: verifier, network: network, permit2: permit2}, nil
}

// Reserve 先离线校验，再持久化占用 Permit2 的付款人 nonce。
// 同一 nonce 的不同摘要会冲突；相同授权的重复请求返回原记录。
// 不预留链上余额，不替代预检；now 必须来自服务端可信时钟。
func (store *Store) Reserve(ctx context.Context, payload types.PaymentPayload, requirements types.PaymentRequirements, now time.Time) (Record, error) {
	if !store.valid() {
		return Record{}, ErrInvalidStore
	}
	verified, err := store.verifier.VerifyAuthorization(payload, requirements, now)
	if err != nil {
		return Record{}, err
	}
	identity := sha256.Sum256([]byte(store.network + "\x00" + store.permit2 + "\x00" + verified.Payer + "\x00" + verified.Nonce))
	id := hex.EncodeToString(identity[:])
	_, err = store.db.Exec(ctx, `
		INSERT INTO x402_tron_settlements (id, network, permit2_address, payer, nonce, digest)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING
	`, id, store.network, store.permit2, verified.Payer, verified.Nonce, verified.Digest[:])
	if err != nil {
		return Record{}, storageError(ctx, err)
	}
	record, err := store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Record{}, ErrConflict
	}
	if err != nil {
		return Record{}, err
	}
	if record.Authorization != verified {
		return Record{}, ErrConflict
	}
	return record, nil
}

const recordColumns = `id, network, permit2_address, payer, nonce::TEXT, digest, state,
	transaction_id, signed_payload, created_at, updated_at`

// Get 只读取当前 Store 网络及 Permit2 域的记录，即使原授权已过期也可追踪原交易。
func (store *Store) Get(ctx context.Context, id string) (Record, error) {
	if err := store.validateID(id); err != nil {
		return Record{}, err
	}
	return scanRecord(ctx, store.db.QueryRow(ctx, `SELECT `+recordColumns+`
		FROM x402_tron_settlements WHERE id = $1 AND network = $2 AND permit2_address = $3
	`, id, store.network, store.permit2))
}

// SaveTransaction 原子保存唯一的签名结果，广播前必须成功调用并使用返回的持久化字节。
// 相同结果可幂等重试；不同结果、不同摘要或其他授权已使用的 txid 均冲突。
// 调用方必须验证 ID 对应 Payload、授权绑定及有效期；本函数不解释交易编码。
func (store *Store) SaveTransaction(ctx context.Context, id string, digest [32]byte, transaction Transaction) (Record, error) {
	if err := store.validateID(id); err != nil {
		return Record{}, err
	}
	if digest == [32]byte{} || !validTransactionID(transaction.ID) || len(transaction.Payload) == 0 || len(transaction.Payload) > 1<<20 {
		return Record{}, ErrInvalidInput
	}
	record, err := scanRecord(ctx, store.db.QueryRow(ctx, `
		UPDATE x402_tron_settlements
		SET state = CASE WHEN state = 'reserved' THEN 'prepared' ELSE state END,
		    transaction_id = $5, signed_payload = $6,
		    updated_at = CASE WHEN state = 'reserved' THEN clock_timestamp() ELSE updated_at END
		WHERE id = $1 AND network = $2 AND permit2_address = $3 AND digest = $4
		  AND (state = 'reserved' OR (transaction_id = $5 AND signed_payload = $6))
		RETURNING `+recordColumns, id, store.network, store.permit2, digest[:], transaction.ID, transaction.Payload))
	if errors.Is(err, ErrNotFound) {
		return Record{}, store.missingOrConflict(ctx, id)
	}
	return record, err
}

// BeginBroadcast 是唯一的广播许可：先提交 unknown，再返回数据库中的原始签名结果。
// 只有一次调用能成功。调用方只能在该调用成功后广播返回值，不能广播本地未保存的交易。
// 数据库提交结果未知、进程退出或 RPC 超时都不得创建新交易；Get 后追踪原 txid。
// 崩溃发生在真正广播之前也可能留下 unknown，宁可人工恢复，不冒重复付款风险。
func (store *Store) BeginBroadcast(ctx context.Context, id string, digest [32]byte) (Record, error) {
	if err := store.validateID(id); err != nil {
		return Record{}, err
	}
	if digest == [32]byte{} {
		return Record{}, ErrInvalidInput
	}
	record, err := scanRecord(ctx, store.db.QueryRow(ctx, `
		UPDATE x402_tron_settlements SET state = 'broadcast_unknown', updated_at = clock_timestamp()
		WHERE id = $1 AND network = $2 AND permit2_address = $3 AND digest = $4 AND state = 'prepared'
		RETURNING `+recordColumns, id, store.network, store.permit2, digest[:]))
	if errors.Is(err, ErrNotFound) {
		existing, readErr := store.Get(ctx, id)
		if readErr != nil {
			return Record{}, readErr
		}
		if existing.Authorization.Digest != digest {
			return Record{}, ErrConflict
		}
		if existing.State == StateReserved {
			return Record{}, ErrNotPrepared
		}
		return Record{}, ErrBroadcastStarted
	}
	return record, err
}

// RecordAccepted 只记录节点已受理原 txid，不能据此交付资源或视为链上成功。
// 拒绝、超时、查无交易均不能释放 nonce 或替换原交易；未确认时保留 unknown/pending。
func (store *Store) RecordAccepted(ctx context.Context, id string, digest [32]byte, transactionID string) (Record, error) {
	if err := store.validateID(id); err != nil {
		return Record{}, err
	}
	if digest == [32]byte{} || !validTransactionID(transactionID) {
		return Record{}, ErrInvalidInput
	}
	record, err := scanRecord(ctx, store.db.QueryRow(ctx, `
		UPDATE x402_tron_settlements
		SET state = 'pending', updated_at = CASE WHEN state = 'broadcast_unknown' THEN clock_timestamp() ELSE updated_at END
		WHERE id = $1 AND network = $2 AND permit2_address = $3 AND digest = $4
		  AND transaction_id = $5 AND state IN ('broadcast_unknown', 'pending')
		RETURNING `+recordColumns, id, store.network, store.permit2, digest[:], transactionID))
	if errors.Is(err, ErrNotFound) {
		return Record{}, store.missingOrConflict(ctx, id)
	}
	return record, err
}

func (store *Store) valid() bool {
	return store != nil && store.db != nil && store.verifier != nil
}

func (store *Store) validateID(id string) error {
	if !store.valid() {
		return ErrInvalidStore
	}
	if !validHexID(id) {
		return ErrInvalidInput
	}
	return nil
}

func validHexID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validTransactionID(id string) bool {
	return validHexID(id) && id != strings.Repeat("0", 64)
}

func (store *Store) missingOrConflict(ctx context.Context, id string) error {
	_, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	return ErrConflict
}

func scanRecord(ctx context.Context, row pgx.Row) (Record, error) {
	var record Record
	var digest, payload []byte
	var transactionID *string
	err := row.Scan(&record.ID, &record.Authorization.Network, &record.Permit2Address,
		&record.Authorization.Payer, &record.Authorization.Nonce, &digest, &record.State,
		&transactionID, &payload, &record.CreatedAt, &record.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, storageError(ctx, err)
	}
	if len(digest) != 32 {
		return Record{}, ErrStore
	}
	copy(record.Authorization.Digest[:], digest)
	if transactionID != nil {
		record.Transaction = &Transaction{ID: *transactionID, Payload: payload}
	}
	return record, nil
}

func storageError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return context.DeadlineExceeded
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	// 数据库原始错误可能包含 SQL 参数、业务数据和连接信息，不向调用方透传。
	return ErrStore
}
