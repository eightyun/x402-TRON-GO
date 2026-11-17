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
	ErrSettlementConfig    = errors.New("exact: invalid settlement builder configuration")
	ErrReferenceBlock      = errors.New("exact: settlement reference block invalid or stale")
	ErrSettlementValidity  = errors.New("exact: insufficient settlement validity remaining")
	ErrSettlementSigner    = errors.New("exact: external settlement signer failed")
	ErrSettlementSignature = errors.New("exact: settlement signature invalid or not from the fee payer")
	ErrSettlementBytes     = errors.New("exact: settlement bytes or ID do not match the prepared transaction")
)

const minSettlementRemaining = 15 * time.Second

// SettlementBuilderConfig explicitly constrains the fee payer, the per-transaction fee limit and
// the transaction lifetime. Lifetime must be greater than 15 seconds, at most 10 minutes and a
// whole number of milliseconds. FeeLimitSUN must be positive.
type SettlementBuilderConfig struct {
	Authorization   Config
	ExecutorAddress string
	FeeLimitSUN     int64
	Lifetime        time.Duration
}

// ReferenceBlock must come from the deployer's trusted node and must not be taken from a payment
// request. Offline checks do not prove the block's actual chain membership, solidification or the
// node's honesty.
type ReferenceBlock struct {
	Network   string
	Number    uint64
	ID        string
	Timestamp time.Time
}

// DigestSigner is a caller-managed external key boundary that only signs the fixed SHA-256
// digest. It returns a low-s r||s||v signature where v may be 0/1 or 27/28; the output is
// normalized to 0/1. It must honor the context and must never log or return the private key.
type DigestSigner func(context.Context, [32]byte) ([]byte, error)

// SettlementTransaction is a complete TRON Transaction protobuf, not JSON. Payload can be used for
// a later broadcasthex call; it is sensitive broadcastable data and must never be printed or
// returned publicly.
type SettlementTransaction struct {
	ID      string
	Payload []byte `json:"-"`
}

// SettlementBuilder holds no keys or RPC clients and is safe for concurrent use.
type SettlementBuilder struct {
	offline  *OfflineVerifier
	executor [20]byte
	fee      int64
	lifetime time.Duration
}

// PreparedSettlement is an immutable single exact call. Its raw_data cannot be replaced before
// signing.
type PreparedSettlement struct {
	raw           []byte
	prefix        []byte
	digest        [32]byte
	authorization VerifiedAuthorization
	payment       parsedAuthorization
	executor      [20]byte
	fee           int64
	createdAt     time.Time
	expiresAt     time.Time
}

// NewSettlementBuilder only creates a builder for the fixed transaction subset. It does not verify
// on-chain deployments or account permissions.
func NewSettlementBuilder(config SettlementBuilderConfig) (*SettlementBuilder, error) {
	offline, err := NewOfflineVerifier(config.Authorization)
	executor, addressErr := configuredAddress(config.ExecutorAddress)
	if err != nil || addressErr != nil || config.FeeLimitSUN <= 0 || config.Lifetime <= minSettlementRemaining || config.Lifetime > 10*time.Minute || config.Lifetime%time.Millisecond != 0 {
		return nil, ErrSettlementConfig
	}
	return &SettlementBuilder{offline: offline, executor: executor, fee: config.FeeLimitSUN, lifetime: config.Lifetime}, nil
}

// Build verifies the authorization offline and builds the transaction locally; it never trusts
// arbitrary transaction JSON or raw_data_hex provided by a node. now must come from the server
// clock. On-chain preflight, persistence and final result verification are still required
// separately. payload and requirements must not be modified during the call.
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
		executor: builder.executor, fee: builder.fee, createdAt: createdAt, expiresAt: expiresAt,
	}, nil
}

// ID returns the canonical hex transaction ID, SHA-256(raw_data).
func (prepared *PreparedSettlement) ID() string {
	if prepared == nil || len(prepared.raw) == 0 {
		return ""
	}
	return hex.EncodeToString(prepared.digest[:])
}

// SigningDigest returns the SHA-256 digest of the raw protobuf. It is not the TIP-712
// authorization digest.
func (prepared *PreparedSettlement) SigningDigest() [32]byte {
	if prepared == nil {
		return [32]byte{}
	}
	return prepared.digest
}

// RawData returns a copy for review by external signing approval; modifying the copy does not
// affect the prepared settlement. The content includes the payment authorization signature, is
// still sensitive data and must never be printed or returned publicly.
func (prepared *PreparedSettlement) RawData() []byte {
	if prepared == nil {
		return nil
	}
	return append([]byte(nil), prepared.raw...)
}

// Authorization returns the offline authorization identifiers bound to this transaction. It is
// still not proof of payment.
func (prepared *PreparedSettlement) Authorization() VerifiedAuthorization {
	if prepared == nil {
		return VerifiedAuthorization{}
	}
	return prepared.authorization
}

// ExpiresAt returns the transaction expiration time pinned for this settlement. It must not be
// extended, nor the payment rebuilt, because of retries.
func (prepared *PreparedSettlement) ExpiresAt() time.Time {
	if prepared == nil {
		return time.Time{}
	}
	return prepared.expiresAt
}

// FeeLimitSUN returns the fee limit fixed in the originally signed content, so that a restored
// settlement can be checked against the current broadcast policy.
func (prepared *PreparedSettlement) FeeLimitSUN() int64 {
	if prepared == nil {
		return 0
	}
	return prepared.fee
}

// Sign calls the external signer and recovers the public key to verify the fee payer, checking the
// remaining validity both before and after signing. now must come from the server; the time spent
// signing is included in the recheck. Sign does not persist or broadcast, and success does not mean
// the payment succeeded.
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

// ValidateSigned checks the complete canonical bytes of this prepared settlement and the fee payer
// signature; it does not check the current validity window. It can be used before persisting or
// for historical audits, and rejects a different raw, unknown or duplicate fields, extra signatures
// and non-canonical encodings. It does not verify on-chain permissions, balances, actual execution
// or solidification; recovery must not treat it as proof for delivery.
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

// ValidateBroadcast validates the prepared transaction and its current remaining validity. It does
// not grant broadcast permission or perform on-chain preflight. It is shared by the persistence
// orchestration and the underlying client; now must come from a trusted server clock.
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
