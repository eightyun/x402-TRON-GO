package postgres_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/exact/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/x402-foundation/x402/go/v2/types"
	"golang.org/x/crypto/sha3"
)

type vector struct {
	Now           int64                      `json:"now"`
	Config        exact.Config               `json:"config"`
	Requirements  types.PaymentRequirements  `json:"requirements"`
	Authorization exact.Permit2Authorization `json:"authorization"`
	Signature     string                     `json:"signature"`
}

func loadVector(t *testing.T) vector {
	t.Helper()
	data, err := os.ReadFile("../testdata/tronweb-permit2.json")
	if err != nil {
		t.Fatal(err)
	}
	var input vector
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	return input
}

func envelope(t *testing.T, input vector) types.PaymentPayload {
	t.Helper()
	data, err := json.Marshal(exact.Permit2Payload{Signature: input.Signature, Authorization: input.Authorization})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return types.PaymentPayload{X402Version: 2, Accepted: input.Requirements, Payload: fields}
}

func setup(t *testing.T) (context.Context, *pgxpool.Pool, *postgres.Store, vector) {
	t.Helper()
	dsn := os.Getenv("X402_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("未设置 X402_TEST_DATABASE_URL，需专用 PostgreSQL 测试库")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("解析测试数据库配置失败")
	}
	admin, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal("创建测试管理连接失败")
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{"x402_test_" + hex.EncodeToString(suffix[:])}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal("创建隔离测试 schema 失败")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		// 仅清理本用例刚创建的随机 schema，不操作调用方的其他表或数据库。
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error("清理隔离测试 schema 失败")
		}
	})
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.MaxConns = 8
	db, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("创建隔离测试连接失败")
	}
	t.Cleanup(db.Close)
	for range 2 {
		if _, err := db.Exec(ctx, postgres.SchemaSQL); err != nil {
			t.Fatal("应用结算日志 schema 失败")
		}
	}
	input := loadVector(t)
	store, err := postgres.New(db, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, db, store, input
}

func reserve(t *testing.T, ctx context.Context, store *postgres.Store, input vector) postgres.Record {
	t.Helper()
	record, err := store.Reserve(ctx, envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func transaction(label string) postgres.Transaction {
	// 只验证日志的不可替换性；这些不透明字节不是合法链上交易。
	return postgres.Transaction{ID: strings.Repeat(label, 64), Payload: []byte("journal-test-" + label)}
}

func TestReserveIdempotencyAndRecovery(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	first := reserve(t, ctx, store, input)
	if first.State != postgres.StateReserved || first.Transaction != nil {
		t.Fatal("新占用不应包含交易")
	}
	input.Config.Network = "tron:0xcd8690dc"
	input.Config.Permit2Address, _ = tron.NormalizeAddressHex(input.Config.Permit2Address)
	nonce, ok := new(big.Int).SetString(input.Authorization.Nonce[2:], 16)
	if !ok {
		t.Fatal("测试 nonce 无效")
	}
	input.Authorization.Nonce = nonce.String()
	restarted, err := postgres.New(db, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	again := reserve(t, ctx, restarted, input)
	if again.ID != first.ID || again.Authorization != first.Authorization || !again.CreatedAt.Equal(first.CreatedAt) {
		t.Fatal("重建存储器或规范化输入改变了占用")
	}
	if _, err := restarted.BeginBroadcast(ctx, first.ID, first.Authorization.Digest); !errors.Is(err, postgres.ErrNotPrepared) {
		t.Fatalf("未保存签名结果就允许广播：%v", err)
	}
	prepared, err := store.SaveTransaction(ctx, first.ID, first.Authorization.Digest, transaction("a"))
	if err != nil || prepared.State != postgres.StatePrepared {
		t.Fatalf("保存交易失败：%v", err)
	}
	started, err := restarted.BeginBroadcast(ctx, first.ID, first.Authorization.Digest)
	if err != nil || started.State != postgres.StateBroadcastUnknown || !bytes.Equal(started.Transaction.Payload, transaction("a").Payload) {
		t.Fatalf("广播前没有持久化 unknown：%v", err)
	}
	// 模拟广播超时、进程退出后，只重新构造 store，不记录 accepted。
	independentDB, err := pgxpool.NewWithConfig(ctx, db.Config())
	if err != nil {
		t.Fatal("创建独立恢复连接失败")
	}
	t.Cleanup(independentDB.Close)
	restarted, err = postgres.New(independentDB, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	unknown, err := restarted.Get(ctx, first.ID)
	if err != nil || unknown.State != postgres.StateBroadcastUnknown || unknown.Transaction.ID != transaction("a").ID {
		t.Fatalf("重启丢失了原交易：%v", err)
	}
	if _, err := restarted.Reserve(ctx, envelope(t, input), input.Requirements, time.Unix(input.Now+1000, 0)); !errors.Is(err, exact.ErrInvalidValidity) {
		t.Fatal("过期授权被允许重新申请结算")
	}
	if _, err := restarted.BeginBroadcast(ctx, first.ID, first.Authorization.Digest); !errors.Is(err, postgres.ErrBroadcastStarted) {
		t.Fatalf("unknown 被重新授予广播许可：%v", err)
	}
	if _, err := restarted.SaveTransaction(ctx, first.ID, first.Authorization.Digest, transaction("b")); !errors.Is(err, postgres.ErrConflict) {
		t.Fatalf("unknown 的原交易被替换：%v", err)
	}
	if _, err := restarted.RecordAccepted(ctx, first.ID, first.Authorization.Digest, transaction("b").ID); !errors.Is(err, postgres.ErrConflict) {
		t.Fatal("其他 txid 被记为已受理")
	}
	for range 2 {
		accepted, err := restarted.RecordAccepted(ctx, first.ID, first.Authorization.Digest, transaction("a").ID)
		if err != nil || accepted.State != postgres.StatePending {
			t.Fatalf("记录受理失败：%v", err)
		}
	}
	pending, err := restarted.SaveTransaction(ctx, first.ID, first.Authorization.Digest, transaction("a"))
	if err != nil || pending.State != postgres.StatePending {
		t.Fatalf("同笔保存重试回退了状态：%v", err)
	}
	if _, err := restarted.BeginBroadcast(ctx, first.ID, first.Authorization.Digest); !errors.Is(err, postgres.ErrBroadcastStarted) {
		t.Fatal("pending 被重新广播")
	}
	encoded, err := json.Marshal(pending)
	if err != nil || bytes.Contains(encoded, transaction("a").Payload) || bytes.Contains(encoded, []byte(`"Payload"`)) {
		t.Fatal("JSON 泄漏了签名交易")
	}
}

func TestConcurrentReservationSigningAndBroadcast(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	otherDB, err := pgxpool.NewWithConfig(ctx, db.Config())
	if err != nil {
		t.Fatal("创建第二个实例连接失败")
	}
	t.Cleanup(otherDB.Close)
	other, err := postgres.New(otherDB, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	stores := []*postgres.Store{store, other}
	payload := envelope(t, input)
	const workers = 24
	var wg sync.WaitGroup
	errorsFound := make(chan error, workers)
	ids := make(chan string, workers)
	for index := range workers {
		wg.Go(func() {
			record, err := stores[index%2].Reserve(ctx, payload, input.Requirements, time.Unix(input.Now, 0))
			if err != nil {
				errorsFound <- err
				return
			}
			ids <- record.ID
		})
	}
	wg.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Fatal(err)
	}
	close(ids)
	first := reserve(t, ctx, store, input)
	for id := range ids {
		if id != first.ID {
			t.Fatal("并发请求产生多个占用")
		}
	}
	var saved, broadcast atomic.Int32
	for index := range workers {
		wg.Go(func() {
			candidate := transaction("a")
			candidate.Payload = []byte{byte(index)}
			_, err := stores[index%2].SaveTransaction(ctx, first.ID, first.Authorization.Digest, candidate)
			if err == nil {
				saved.Add(1)
			} else if !errors.Is(err, postgres.ErrConflict) {
				t.Errorf("保存失败类别：%v", err)
			}
		})
	}
	wg.Wait()
	for index := range workers {
		wg.Go(func() {
			_, err := stores[index%2].BeginBroadcast(ctx, first.ID, first.Authorization.Digest)
			if err == nil {
				broadcast.Add(1)
			} else if !errors.Is(err, postgres.ErrBroadcastStarted) {
				t.Errorf("广播许可失败类别：%v", err)
			}
		})
	}
	wg.Wait()
	if saved.Load() != 1 || broadcast.Load() != 1 {
		t.Fatalf("保存/广播许可的胜者数：%d/%d", saved.Load(), broadcast.Load())
	}
}

func signedVariant(t *testing.T, input vector, key *secp256k1.PrivateKey) vector {
	t.Helper()
	publicKey := key.PubKey().SerializeUncompressed()
	hash := sha3.NewLegacyKeccak256()
	hash.Write(publicKey[1:])
	input.Authorization.From = "0x" + hex.EncodeToString(hash.Sum(nil)[12:])
	verifier, err := exact.NewOfflineVerifier(input.Config)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := verifier.Digest(input.Authorization)
	if err != nil {
		t.Fatal(err)
	}
	compact := ecdsa.SignCompact(key, digest[:], false)
	signature := append(append([]byte{}, compact[1:]...), compact[0])
	input.Signature = "0x" + hex.EncodeToString(signature)
	return input
}

func TestConflictingAuthorizationAndTransaction(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer key.Zero()
	input = signedVariant(t, input, key)
	first := reserve(t, ctx, store, input)
	changed := input
	changed.Authorization.Permitted.Amount = "1001"
	changed.Requirements.Amount = "1001"
	changed = signedVariant(t, changed, key)
	if _, err := store.Reserve(ctx, envelope(t, changed), changed.Requirements, time.Unix(input.Now, 0)); !errors.Is(err, postgres.ErrConflict) {
		t.Fatalf("同一 nonce 允许不同摘要：%v", err)
	}
	changed = input
	changed.Config.ProxyAddress = input.Requirements.PayTo
	changed.Authorization.Spender, err = tron.AddressForSigning(changed.Config.ProxyAddress)
	if err != nil {
		t.Fatal(err)
	}
	changed = signedVariant(t, changed, key)
	otherProxy, err := postgres.New(db, changed.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherProxy.Reserve(ctx, envelope(t, changed), changed.Requirements, time.Unix(input.Now, 0)); !errors.Is(err, postgres.ErrConflict) {
		t.Fatalf("不同代理绕过了同一个 Permit2 nonce 占用：%v", err)
	}
	changed = input
	changed.Authorization.Nonce = "42"
	changed = signedVariant(t, changed, key)
	second := reserve(t, ctx, store, changed)
	if _, err := store.SaveTransaction(ctx, first.ID, first.Authorization.Digest, transaction("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SaveTransaction(ctx, second.ID, second.Authorization.Digest, transaction("a")); !errors.Is(err, postgres.ErrConflict) {
		t.Fatalf("同一个 txid 绑定了另一笔授权：%v", err)
	}
	wrong := first.Authorization.Digest
	wrong[0] ^= 1
	if _, err := store.SaveTransaction(ctx, first.ID, wrong, transaction("a")); !errors.Is(err, postgres.ErrConflict) {
		t.Fatal("错误摘要保存成功")
	}
	if _, err := store.BeginBroadcast(ctx, first.ID, wrong); !errors.Is(err, postgres.ErrConflict) {
		t.Fatal("错误摘要获取了广播许可")
	}
	if _, err := store.RecordAccepted(ctx, first.ID, first.Authorization.Digest, transaction("b").ID); !errors.Is(err, postgres.ErrConflict) {
		t.Fatal("未广播或错误的交易被记为受理")
	}
	changed = input
	changed.Config.Network = tron.NetworkShasta
	changed.Requirements.Network = tron.NetworkShasta
	changed = signedVariant(t, changed, key)
	otherNetwork, err := postgres.New(db, changed.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := otherNetwork.Get(ctx, first.ID); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatal("读取了另一网络的日志")
	}
	separate := reserve(t, ctx, otherNetwork, changed)
	if separate.ID == first.ID {
		t.Fatal("不同网络的 nonce 共用了占用")
	}
	if _, err := otherNetwork.SaveTransaction(ctx, separate.ID, separate.Authorization.Digest, transaction("a")); err != nil {
		t.Fatalf("不同网络的 txid 被错误冲突：%v", err)
	}
	for _, nonce := range []string{"0", new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)).String()} {
		changed = input
		changed.Authorization.Nonce = nonce
		changed = signedVariant(t, changed, key)
		boundary := reserve(t, ctx, store, changed)
		if boundary.Authorization.Nonce != nonce {
			t.Fatal("数据库丢失了 uint256 精度")
		}
	}
}

func TestScopeCancellationAndDatabaseError(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	first := reserve(t, ctx, store, input)
	otherConfig := input.Config
	otherConfig.Permit2Address = input.Requirements.PayTo
	other, err := postgres.New(db, otherConfig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Get(ctx, first.ID); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatal("读取了其他 Permit2 域的交易")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Get(canceled, first.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消被吞掉：%v", err)
	}
	expired, expire := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer expire()
	if _, err := store.Get(expired, first.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("超时被吞掉：%v", err)
	}
	missing := strings.Repeat("f", 64)
	for _, call := range []func() error{
		func() error { _, err := store.Get(ctx, missing); return err },
		func() error {
			_, err := store.SaveTransaction(ctx, missing, first.Authorization.Digest, transaction("a"))
			return err
		},
		func() error { _, err := store.BeginBroadcast(ctx, missing, first.Authorization.Digest); return err },
		func() error {
			_, err := store.RecordAccepted(ctx, missing, first.Authorization.Digest, transaction("a").ID)
			return err
		},
	} {
		if err := call(); !errors.Is(err, postgres.ErrNotFound) {
			t.Fatalf("不存在记录错误类别：%v", err)
		}
	}
	// 测试账号模拟表不可访问，错误不得暴露数据库消息、SQL 或请求数据。
	if _, err := db.Exec(ctx, "ALTER TABLE x402_tron_settlements RENAME TO private_unavailable_journal"); err != nil {
		t.Fatal("模拟存储异常失败")
	}
	if _, err := store.Get(ctx, first.ID); err != postgres.ErrStore {
		t.Fatalf("原始数据库错误未脱敏：%v", err)
	}
	if _, err := store.Reserve(ctx, envelope(t, input), input.Requirements, time.Unix(input.Now, 0)); err != postgres.ErrStore {
		t.Fatalf("写入错误未脱敏：%v", err)
	}
}
