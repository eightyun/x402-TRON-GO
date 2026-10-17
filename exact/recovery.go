package exact

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"math/big"
	"time"

	tron "github.com/eightyun/x402-tron-go"
)

// RestoreSettlement 从原签名交易恢复历史核验所需的不可变绑定，不重新构建或签名交易。
// config 和 executorAddress 必须来自可信部署配置，而不是付款请求或节点响应。
// 仅接受本包构建器的固定规范格式；历史过期不影响恢复，但恢复不证明原报价、
// 预检或链上执行成功。调用方必须对照持久化授权标识，再调用 CheckSettlement。
// 调用期间不得修改 transaction.Payload；恢复对象不引用输入切片。
func RestoreSettlement(config Config, executorAddress string, transaction SettlementTransaction) (*PreparedSettlement, error) {
	verifier, err := NewOfflineVerifier(config)
	executor, executorErr := configuredAddress(executorAddress)
	if err != nil || executorErr != nil {
		return nil, ErrSettlementConfig
	}
	if len(transaction.Payload) > 1<<20 {
		return nil, ErrSettlementBytes
	}
	outer := settlementDecoder{data: transaction.Payload}
	raw, signature := outer.bytes(1), outer.bytes(2)
	if !outer.done() || len(signature) != 65 {
		return nil, ErrSettlementBytes
	}
	digest := sha256.Sum256(raw)
	if transaction.ID != hex.EncodeToString(digest[:]) {
		return nil, ErrSettlementBytes
	}
	decoder := settlementDecoder{data: raw}
	refBytes, refHash := decoder.bytes(1), decoder.bytes(4)
	expiration := decoder.uint(8)
	contract := decoder.bytes(11)
	timestamp, fee := decoder.uint(14), decoder.uint(18)
	if !decoder.done() || len(refBytes) != 2 || len(refHash) != 8 || timestamp > math.MaxInt64 || expiration > math.MaxInt64 || fee == 0 || fee > math.MaxInt64 {
		return nil, ErrSettlementBytes
	}
	createdAt, expiresAt := time.UnixMilli(int64(timestamp)).UTC(), time.UnixMilli(int64(expiration)).UTC()
	if !validSettlementTime(createdAt) || !validSettlementTime(expiresAt) || expiresAt.Before(createdAt.Add(minSettlementRemaining)) || expiresAt.After(createdAt.Add(10*time.Minute)) {
		return nil, ErrSettlementBytes
	}
	callData, err := restoreCall(contract, executor, verifier.proxy)
	if err != nil {
		return nil, err
	}
	payment, authorization, err := restoreAuthorization(verifier, callData)
	if err != nil {
		return nil, err
	}
	deadlineMillis := new(big.Int).Mul(new(big.Int).SetBytes(payment.deadline[:]), big.NewInt(1000))
	if new(big.Int).SetBytes(payment.validAfter[:]).Cmp(big.NewInt(createdAt.Unix())) > 0 || deadlineMillis.Cmp(big.NewInt(expiresAt.UnixMilli())) < 0 {
		return nil, ErrSettlementBytes
	}
	var blockID [32]byte
	copy(blockID[6:8], refBytes)
	copy(blockID[8:16], refHash)
	// TAPOS 只保存这些字节，不推断完整参考区块或其真实链上归属。
	canonical := settlementRaw(executor, verifier.proxy, callData, blockID, int64(timestamp), int64(expiration), int64(fee))
	if !bytes.Equal(raw, canonical) {
		return nil, ErrSettlementBytes
	}
	prefix := pbBytes(nil, 1, canonical)
	prefix = binary.AppendUvarint(prefix, 2<<3|2)
	prefix = binary.AppendUvarint(prefix, 65)
	prepared := &PreparedSettlement{
		raw: canonical, prefix: prefix, digest: digest, authorization: authorization,
		payment: payment, executor: executor, createdAt: createdAt, expiresAt: expiresAt,
	}
	if err := prepared.ValidateSigned(transaction); err != nil {
		return nil, err
	}
	return prepared, nil
}

func restoreCall(contract []byte, executor, proxy [20]byte) ([]byte, error) {
	decoder := settlementDecoder{data: contract}
	kind, parameter := decoder.uint(1), decoder.bytes(2)
	if !decoder.done() || kind != 31 {
		return nil, ErrSettlementBytes
	}
	decoder = settlementDecoder{data: parameter}
	typeURL, value := decoder.bytes(1), decoder.bytes(2)
	if !decoder.done() || string(typeURL) != "type.googleapis.com/protocol.TriggerSmartContract" {
		return nil, ErrSettlementBytes
	}
	decoder = settlementDecoder{data: value}
	owner, target, callData := decoder.bytes(1), decoder.bytes(2), decoder.bytes(4)
	if !decoder.done() || !bytes.Equal(owner, append([]byte{0x41}, executor[:]...)) || !bytes.Equal(target, append([]byte{0x41}, proxy[:]...)) {
		return nil, ErrSettlementBytes
	}
	return callData, nil
}

func restoreAuthorization(verifier *OfflineVerifier, callData []byte) (parsedAuthorization, VerifiedAuthorization, error) {
	var payment parsedAuthorization
	if len(callData) != 388 {
		return payment, VerifiedAuthorization{}, ErrSettlementBytes
	}
	for _, field := range []struct {
		word int
		out  *[20]byte
	}{{0, &payment.token}, {4, &payment.from}, {5, &payment.to}} {
		word := callData[4+field.word*32 : 4+(field.word+1)*32]
		if !bytes.Equal(word[:12], make([]byte, 12)) || bytes.Equal(word[12:], make([]byte, 20)) {
			return parsedAuthorization{}, VerifiedAuthorization{}, ErrSettlementBytes
		}
		copy(field.out[:], word[12:])
	}
	copy(payment.amount[:], callData[36:68])
	copy(payment.nonce[:], callData[68:100])
	copy(payment.deadline[:], callData[100:132])
	copy(payment.validAfter[:], callData[196:228])
	payment.spender = verifier.proxy
	if payment.amount == [32]byte{} || bytes.Compare(payment.validAfter[:], payment.deadline[:]) >= 0 {
		return parsedAuthorization{}, VerifiedAuthorization{}, ErrSettlementBytes
	}
	signature := "0x" + hex.EncodeToString(callData[292:357])
	canonical, err := settlementCallData(payment, signature)
	if err != nil || !bytes.Equal(callData, canonical) {
		return parsedAuthorization{}, VerifiedAuthorization{}, ErrSettlementBytes
	}
	digest := verifier.digest(payment)
	if !verifySignature(digest, signature, payment.from) {
		return parsedAuthorization{}, VerifiedAuthorization{}, ErrInvalidSignature
	}
	payer, err := tron.NormalizeAddressBase58("41" + hex.EncodeToString(payment.from[:]))
	if err != nil {
		return parsedAuthorization{}, VerifiedAuthorization{}, ErrSettlementBytes
	}
	return payment, VerifiedAuthorization{
		Network: verifier.network, Payer: payer, Nonce: new(big.Int).SetBytes(payment.nonce[:]).String(), Digest: digest,
	}, nil
}

// settlementDecoder 仅消费固定顺序字段，不跳过未知字段或接受非规范整数编码。
type settlementDecoder struct {
	data    []byte
	invalid bool
}

func (decoder *settlementDecoder) varint() uint64 {
	if decoder.invalid {
		return 0
	}
	value, length := binary.Uvarint(decoder.data)
	var canonical [10]byte
	if length <= 0 || binary.PutUvarint(canonical[:], value) != length {
		decoder.invalid = true
		return 0
	}
	decoder.data = decoder.data[length:]
	return value
}

func (decoder *settlementDecoder) uint(field uint64) uint64 {
	if decoder.varint() != field<<3 {
		decoder.invalid = true
	}
	return decoder.varint()
}

func (decoder *settlementDecoder) bytes(field uint64) []byte {
	if decoder.varint() != field<<3|2 {
		decoder.invalid = true
	}
	length := decoder.varint()
	if decoder.invalid || length > uint64(len(decoder.data)) {
		decoder.invalid = true
		return nil
	}
	value := decoder.data[:int(length)]
	decoder.data = decoder.data[int(length):]
	return value
}

func (decoder *settlementDecoder) done() bool {
	return !decoder.invalid && len(decoder.data) == 0
}
