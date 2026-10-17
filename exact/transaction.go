package exact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"math/big"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/x402-foundation/x402/go/v2/types"
)

var (
	ErrSettlementConfig    = errors.New("结算交易构建配置无效")
	ErrReferenceBlock      = errors.New("结算参考区块无效或已过期")
	ErrSettlementValidity  = errors.New("结算交易剩余有效期不足")
	ErrSettlementSigner    = errors.New("外部结算签名器失败")
	ErrSettlementSignature = errors.New("结算交易签名无效或与费用付款方不符")
	ErrSettlementBytes     = errors.New("结算交易字节或 ID 与已准备交易不符")
)

const minSettlementRemaining = 15 * time.Second

// SettlementBuilderConfig 显式约束费用付款方、单笔费用上限和交易生命周期。
// Lifetime 必须大于 15 秒、不超过 10 分钟且为整毫秒；FeeLimitSUN 必须为正。
type SettlementBuilderConfig struct {
	Authorization   Config
	ExecutorAddress string
	FeeLimitSUN     int64
	Lifetime        time.Duration
}

// ReferenceBlock 必须来自部署方可信节点，不可从付款请求获取。
// 离线检查不证明该区块的真实归属、固化性或节点诚实性。
type ReferenceBlock struct {
	Network   string
	Number    uint64
	ID        string
	Timestamp time.Time
}

// DigestSigner 是调用方管理的外部密钥边界，仅签署固定的 SHA-256 摘要。
// 返回低 s 的 r||s||v，v 可为 0/1 或 27/28，输出统一规范化为 0/1。
// 必须遵守 context，不能记录或返回私钥。
type DigestSigner func(context.Context, [32]byte) ([]byte, error)

// SettlementTransaction 是完整的 TRON Transaction Protobuf，不是 JSON。
// Payload 可用于后续 broadcasthex，属于可广播敏感数据，不得打印或公开返回。
type SettlementTransaction struct {
	ID      string
	Payload []byte `json:"-"`
}

// SettlementBuilder 不持有密钥或 RPC，可以并发使用。
type SettlementBuilder struct {
	offline  *OfflineVerifier
	executor [20]byte
	fee      int64
	lifetime time.Duration
}

// PreparedSettlement 是不可变的单笔 exact 调用，不能替换其 raw_data 后再签名。
type PreparedSettlement struct {
	raw           []byte
	prefix        []byte
	digest        [32]byte
	authorization VerifiedAuthorization
	payment       parsedAuthorization
	executor      [20]byte
	createdAt     time.Time
	expiresAt     time.Time
}

// NewSettlementBuilder 只创建固定子集构建器，不核验链上部署或账户权限。
func NewSettlementBuilder(config SettlementBuilderConfig) (*SettlementBuilder, error) {
	offline, err := NewOfflineVerifier(config.Authorization)
	executor, addressErr := configuredAddress(config.ExecutorAddress)
	if err != nil || addressErr != nil || config.FeeLimitSUN <= 0 || config.Lifetime <= minSettlementRemaining || config.Lifetime > 10*time.Minute || config.Lifetime%time.Millisecond != 0 {
		return nil, ErrSettlementConfig
	}
	return &SettlementBuilder{offline: offline, executor: executor, fee: config.FeeLimitSUN, lifetime: config.Lifetime}, nil
}

// Build 离线校验授权并本地构建交易，不信任节点提供的任意交易 JSON/raw_data_hex。
// now 必须来自服务端时钟；仍需单独进行链上预检、持久化和最终结果核验。
// 调用期间不能修改 payload/requirements。
func (builder *SettlementBuilder) Build(payload types.PaymentPayload, requirements types.PaymentRequirements, reference ReferenceBlock, now time.Time) (*PreparedSettlement, error) {
	if builder == nil || builder.offline == nil {
		return nil, ErrSettlementConfig
	}
	verified, err := builder.offline.VerifyAuthorization(payload, requirements, now)
	if err != nil {
		return nil, err
	}
	if !validSettlementTime(now) {
		return nil, ErrSettlementValidity
	}
	network, networkErr := tron.NormalizeNetwork(reference.Network)
	blockID, hashErr := rpcWord(reference.ID)
	if networkErr != nil || network != builder.offline.network || hashErr != nil || reference.Number == 0 || reference.Number > math.MaxInt64 || binary.BigEndian.Uint64(blockID[:8]) != reference.Number || !validSettlementTime(reference.Timestamp) || reference.Timestamp.Before(now.Add(-time.Minute)) || reference.Timestamp.After(now.Add(5*time.Second)) {
		return nil, ErrReferenceBlock
	}
	rawPayload, err := parsePayload(payload.Payload)
	if err != nil {
		return nil, err
	}
	authorization, err := parseAuthorization(rawPayload.Authorization)
	if err != nil {
		return nil, err
	}
	expiresAt := time.UnixMilli(now.Add(builder.lifetime).UnixMilli()).UTC()
	deadlineMillis := new(big.Int).Mul(new(big.Int).SetBytes(authorization.deadline[:]), big.NewInt(1000))
	if deadlineMillis.Cmp(big.NewInt(expiresAt.UnixMilli())) < 0 {
		expiresAt = time.UnixMilli(deadlineMillis.Int64()).UTC()
	}
	if !validSettlementTime(expiresAt) || expiresAt.Before(now.Add(minSettlementRemaining)) {
		return nil, ErrSettlementValidity
	}
	callData, err := settlementCallData(authorization, rawPayload.Signature)
	if err != nil {
		return nil, err
	}
	createdAt := time.UnixMilli(now.UnixMilli()).UTC()
	raw := settlementRaw(builder.executor, builder.offline.proxy, callData, blockID, createdAt.UnixMilli(), expiresAt.UnixMilli(), builder.fee)
	// 仅允许 raw_data(1) 和单个 signature(2)，不提供通用 Protobuf 导入器。
	prefix := pbBytes(nil, 1, raw)
	prefix = binary.AppendUvarint(prefix, 2<<3|2)
	prefix = binary.AppendUvarint(prefix, 65)
	return &PreparedSettlement{
		raw: raw, prefix: prefix, digest: sha256.Sum256(raw), authorization: verified, payment: authorization,
		executor: builder.executor, createdAt: createdAt, expiresAt: expiresAt,
	}, nil
}

// ID 返回 SHA-256(raw_data) 的规范十六进制交易 ID。
func (prepared *PreparedSettlement) ID() string {
	if prepared == nil || len(prepared.raw) == 0 {
		return ""
	}
	return hex.EncodeToString(prepared.digest[:])
}

// SigningDigest 返回原始 Protobuf 的 SHA-256 摘要，不是 TIP-712 授权摘要。
func (prepared *PreparedSettlement) SigningDigest() [32]byte {
	if prepared == nil {
		return [32]byte{}
	}
	return prepared.digest
}

// RawData 返回副本，供外部签名审批核对，修改副本不会影响准备对象。
// 内容包含付款授权签名，仍属敏感数据，不得打印或公开返回。
func (prepared *PreparedSettlement) RawData() []byte {
	if prepared == nil {
		return nil
	}
	return append([]byte(nil), prepared.raw...)
}

// Authorization 返回与该交易绑定的离线授权标识，仍不是付款证明。
func (prepared *PreparedSettlement) Authorization() VerifiedAuthorization {
	if prepared == nil {
		return VerifiedAuthorization{}
	}
	return prepared.authorization
}

// ExpiresAt 返回本次已固定的交易过期时间；不得因重试延长或重新构建付款。
func (prepared *PreparedSettlement) ExpiresAt() time.Time {
	if prepared == nil {
		return time.Time{}
	}
	return prepared.expiresAt
}

// Sign 调用外部签名器并恢复公钥核对费用付款方，签名前后均检查剩余有效期。
// now 必须来自服务端，签名耗时计入复查。该函数不保存/广播，成功不代表付款成功。
func (prepared *PreparedSettlement) Sign(ctx context.Context, signer DigestSigner, now time.Time) (SettlementTransaction, error) {
	if prepared == nil || len(prepared.raw) == 0 || signer == nil {
		return SettlementTransaction{}, ErrSettlementConfig
	}
	if err := ctx.Err(); err != nil {
		return SettlementTransaction{}, err
	}
	if !prepared.signingTimeValid(now) {
		return SettlementTransaction{}, ErrSettlementValidity
	}
	started := time.Now()
	signature, err := signer(ctx, prepared.digest)
	if ctx.Err() != nil {
		return SettlementTransaction{}, ctx.Err()
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return SettlementTransaction{}, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return SettlementTransaction{}, context.DeadlineExceeded
		}
		return SettlementTransaction{}, ErrSettlementSigner
	}
	if len(signature) != 65 {
		return SettlementTransaction{}, ErrSettlementSignature
	}
	signature = append([]byte(nil), signature...)
	if signature[64] == 27 || signature[64] == 28 {
		signature[64] -= 27
	}
	transaction := SettlementTransaction{ID: prepared.ID(), Payload: append(append([]byte(nil), prepared.prefix...), signature...)}
	if err := prepared.ValidateSigned(transaction); err != nil {
		return SettlementTransaction{}, err
	}
	if !prepared.signingTimeValid(now.Add(time.Since(started))) {
		return SettlementTransaction{}, ErrSettlementValidity
	}
	if err := ctx.Err(); err != nil {
		return SettlementTransaction{}, err
	}
	return transaction, nil
}

// ValidateSigned 核对本准备对象的完整规范字节和费用付款方签名，不检查当前有效期。
// 可用于保存前或历史审计；拒绝不同 raw、未知/重复字段、额外签名与非规范编码。
// 不验证链上权限、余额、实际执行或固化；恢复过程不能将其当作交付凭据。
func (prepared *PreparedSettlement) ValidateSigned(transaction SettlementTransaction) error {
	if prepared == nil || len(prepared.raw) == 0 {
		return ErrSettlementConfig
	}
	if transaction.ID != prepared.ID() || len(transaction.Payload) != len(prepared.prefix)+65 || !bytes.HasPrefix(transaction.Payload, prepared.prefix) {
		return ErrSettlementBytes
	}
	signature := transaction.Payload[len(prepared.prefix):]
	if signature[64] > 1 || !verifySignature(prepared.digest, "0x"+hex.EncodeToString(signature), prepared.executor) {
		return ErrSettlementSignature
	}
	return nil
}

// ValidateBroadcast 校验已准备交易及当前剩余有效期，不提供广播许可或链上预检。
// 持久化串联与底层客户端共同使用；now 必须来自服务端可信时钟。
func (prepared *PreparedSettlement) ValidateBroadcast(transaction SettlementTransaction, now time.Time) error {
	if err := prepared.ValidateSigned(transaction); err != nil {
		return err
	}
	if !prepared.signingTimeValid(now) {
		return ErrSettlementValidity
	}
	return nil
}

func validSettlementTime(value time.Time) bool {
	year := value.UTC().Year()
	return year >= 2018 && year <= 9999
}

func (prepared *PreparedSettlement) signingTimeValid(now time.Time) bool {
	return validSettlementTime(now) && !now.Before(prepared.createdAt) && !prepared.expiresAt.Before(now.Add(minSettlementRemaining))
}

func settlementCallData(authorization parsedAuthorization, encodedSignature string) ([]byte, error) {
	if len(encodedSignature) != 132 {
		return nil, ErrInvalidSignature
	}
	signature, err := hex.DecodeString(encodedSignature[2:])
	if err != nil || len(signature) != 65 {
		return nil, ErrInvalidSignature
	}
	if signature[64] < 2 {
		signature[64] += 27
	}
	selector := keccak([]byte("settle(((address,uint256),uint256,uint256),address,(address,uint256),bytes)"))
	var offset, length [32]byte
	binary.BigEndian.PutUint64(offset[24:], 8*32)
	length[31] = 65
	data := append([]byte(nil), selector[:4]...)
	for _, word := range [][32]byte{
		addressWord(authorization.token), authorization.amount, authorization.nonce, authorization.deadline,
		addressWord(authorization.from), addressWord(authorization.to), authorization.validAfter, offset, length,
	} {
		data = append(data, word[:]...)
	}
	data = append(data, signature...)
	return append(data, make([]byte, 31)...), nil
}

// settlementRaw 按官方 Tron.proto / smart_contract.proto 仅编码受限调用。
// 不包含 TRX/TRC10 附带转账、其他权限、备忘录或其他合约。
func settlementRaw(executor, proxy [20]byte, callData []byte, blockID [32]byte, timestamp, expiration, fee int64) []byte {
	call := pbBytes(nil, 1, append([]byte{0x41}, executor[:]...))
	call = pbBytes(call, 2, append([]byte{0x41}, proxy[:]...))
	call = pbBytes(call, 4, callData)
	parameter := pbBytes(nil, 1, []byte("type.googleapis.com/protocol.TriggerSmartContract"))
	parameter = pbBytes(parameter, 2, call)
	contract := pbUint(nil, 1, 31)
	contract = pbBytes(contract, 2, parameter)
	raw := pbBytes(nil, 1, blockID[6:8])
	raw = pbBytes(raw, 4, blockID[8:16])
	raw = pbUint(raw, 8, uint64(expiration))
	raw = pbBytes(raw, 11, contract)
	raw = pbUint(raw, 14, uint64(timestamp))
	return pbUint(raw, 18, uint64(fee))
}

func pbBytes(output []byte, field uint64, value []byte) []byte {
	output = binary.AppendUvarint(output, field<<3|2)
	output = binary.AppendUvarint(output, uint64(len(value)))
	return append(output, value...)
}

func pbUint(output []byte, field, value uint64) []byte {
	output = binary.AppendUvarint(output, field<<3)
	return binary.AppendUvarint(output, value)
}
