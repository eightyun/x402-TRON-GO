package exact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

var (
	ErrSettlementEvidence  = errors.New("原交易固化证据缺失、矛盾或与准备对象不符")
	ErrSettlementExecution = errors.New("节点报告原交易执行未成功，不能确认付款")
	ErrSettlementTransfer  = errors.New("原交易未包含唯一且精确的预期代币付款")
)

// SettlementObservation 是可信原生节点的只读交叉核验结果，不是恶意节点的状态证明。
// Confirmed 仅在原调用、固化归属、执行及标准代币事件全部匹配后为 true。
type SettlementObservation struct {
	Network       string
	TransactionID string
	Confirmed     bool
	BlockNumber   uint64
	BlockID       string
}

// CheckSettlement 仅从 walletsolidity 核验原交易，不广播、不改日志、不释放占用。
// now 来自可信服务端；历史交易可过期，准备对象须保留或从可信绑定的原签名交易恢复，不能用当前报价重建。
// 缺少固化主体或回执返回 Confirmed=false；错误和未确认结果都不允许重新付款或交付资源。
func (client *ExecutionClient) CheckSettlement(ctx context.Context, prepared *PreparedSettlement, transaction SettlementTransaction, now time.Time) (SettlementObservation, error) {
	if !client.valid() {
		return SettlementObservation{}, ErrExecutionConfig
	}
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	started := time.Now()
	if err := ctx.Err(); err != nil {
		return SettlementObservation{}, err
	}
	if err := prepared.ValidateSigned(transaction); err != nil {
		return SettlementObservation{}, err
	}
	if prepared.Authorization().Network != client.network {
		return SettlementObservation{}, ErrChainMismatch
	}
	if !validSettlementTime(now) {
		return SettlementObservation{}, ErrExecutionInput
	}
	observation := SettlementObservation{Network: client.network, TransactionID: prepared.ID()}
	if err := client.checkSolidNetwork(ctx); err != nil {
		return observation, err
	}
	head, err := client.solidHead(ctx, now.Add(time.Since(started)))
	if err != nil {
		return observation, err
	}
	body, err := client.post(ctx, "/walletsolidity/gettransactionbyid", map[string]string{"value": observation.TransactionID})
	if err != nil {
		return observation, err
	}
	info, err := client.post(ctx, "/walletsolidity/gettransactioninfobyid", map[string]string{"value": observation.TransactionID})
	if err != nil {
		return observation, err
	}
	if len(body) == 0 || len(info) == 0 {
		if err := client.finishSolidRead(ctx, head, now.Add(time.Since(started))); err != nil {
			return observation, err
		}
		return observation, nil
	}
	if err := settlementBody(body, prepared); err != nil {
		return observation, err
	}
	var id string
	if json.Unmarshal(info["id"], &id) != nil || id != observation.TransactionID {
		return observation, ErrSettlementEvidence
	}
	height, heightErr := nativePositiveInt(info["blockNumber"])
	millis, timeErr := nativePositiveInt(info["blockTimeStamp"])
	if heightErr != nil || timeErr != nil {
		return observation, ErrSettlementEvidence
	}
	stamp := time.UnixMilli(millis).UTC()
	if !validSettlementTime(stamp) || stamp.Before(prepared.createdAt.Add(-5*time.Second)) || stamp.After(prepared.expiresAt) {
		return observation, ErrSettlementEvidence
	}
	if uint64(height) > head.number {
		// 两次读取间可能刚固化新块；本轮不以超出首个固化头的证据放行。
		if err := client.finishSolidRead(ctx, head, now.Add(time.Since(started))); err != nil {
			return observation, err
		}
		return observation, nil
	}
	if stamp.After(head.timestamp) {
		return observation, ErrSettlementEvidence
	}
	if err := nativeSuccessCode(info, "result"); err != nil {
		return observation, err
	}
	receipt, err := nativeObject(info["receipt"])
	if err != nil {
		return observation, err
	}
	if err := contractSuccess(receipt, "result"); err != nil {
		return observation, err
	}
	block, err := client.settlementBlock(ctx, uint64(height), stamp, prepared)
	if err != nil {
		return observation, err
	}
	if block.number == head.number && (block.id != head.id || !block.timestamp.Equal(head.timestamp)) {
		return observation, ErrSettlementEvidence
	}
	if err := settlementTransfer(info["log"], prepared.payment); err != nil {
		return observation, err
	}
	if err := client.finishSolidRead(ctx, head, now.Add(time.Since(started))); err != nil {
		return observation, err
	}
	again, err := client.settlementBlock(ctx, uint64(height), stamp, prepared)
	if err != nil {
		return observation, err
	}
	if again.id != block.id {
		return observation, ErrSettlementEvidence
	}
	if err := client.checkSolidNetwork(ctx); err != nil {
		return observation, err
	}
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	if head.timestamp.Before(now.Add(time.Since(started)).Add(-2 * time.Minute)) {
		return observation, ErrSettlementEvidence
	}
	observation.Confirmed, observation.BlockNumber, observation.BlockID = true, uint64(height), block.id
	return observation, nil
}

type solidBlock struct {
	id        string
	number    uint64
	timestamp time.Time
}

func (client *ExecutionClient) checkSolidNetwork(ctx context.Context) error {
	result, err := client.post(ctx, "/walletsolidity/getblockbynum", map[string]int{"num": 0})
	if err != nil {
		return err
	}
	id, err := nativeWord(result["blockID"])
	if err != nil {
		return err
	}
	if id != client.genesis {
		return ErrChainMismatch
	}
	return nil
}

func (client *ExecutionClient) solidHead(ctx context.Context, now time.Time) (solidBlock, error) {
	started := time.Now()
	result, err := client.post(ctx, "/walletsolidity/getnowblock", struct{}{})
	if err != nil {
		return solidBlock{}, err
	}
	head, err := parseSolidBlock(result)
	if err != nil {
		return solidBlock{}, err
	}
	now = now.Add(time.Since(started))
	if head.timestamp.Before(now.Add(-2*time.Minute)) || head.timestamp.After(now.Add(5*time.Second)) {
		return solidBlock{}, ErrSettlementEvidence
	}
	return head, nil
}

func (client *ExecutionClient) finishSolidRead(ctx context.Context, previous solidBlock, now time.Time) error {
	head, err := client.solidHead(ctx, now)
	if err != nil {
		return err
	}
	if head.number < previous.number || head.timestamp.Before(previous.timestamp) || head.number == previous.number && (head.id != previous.id || !head.timestamp.Equal(previous.timestamp)) {
		return ErrSettlementEvidence
	}
	return client.checkSolidNetwork(ctx)
}

func parseSolidBlock(result map[string]json.RawMessage) (solidBlock, error) {
	id, err := nativeWord(result["blockID"])
	if err != nil {
		return solidBlock{}, err
	}
	header, err := nativeObject(result["block_header"])
	if err != nil {
		return solidBlock{}, err
	}
	raw, err := nativeObject(header["raw_data"])
	if err != nil {
		return solidBlock{}, err
	}
	height, heightErr := nativePositiveInt(raw["number"])
	millis, timeErr := nativePositiveInt(raw["timestamp"])
	if heightErr != nil || timeErr != nil || binary.BigEndian.Uint64(id[:8]) != uint64(height) {
		return solidBlock{}, ErrSettlementEvidence
	}
	stamp := time.UnixMilli(millis).UTC()
	if !validSettlementTime(stamp) {
		return solidBlock{}, ErrSettlementEvidence
	}
	return solidBlock{id: hex.EncodeToString(id[:]), number: uint64(height), timestamp: stamp}, nil
}

func (client *ExecutionClient) settlementBlock(ctx context.Context, height uint64, stamp time.Time, prepared *PreparedSettlement) (solidBlock, error) {
	result, err := client.post(ctx, "/walletsolidity/getblockbynum", map[string]uint64{"num": height})
	if err != nil {
		return solidBlock{}, err
	}
	block, err := parseSolidBlock(result)
	if err != nil {
		return solidBlock{}, err
	}
	if block.number != height || !block.timestamp.Equal(stamp) {
		return solidBlock{}, ErrSettlementEvidence
	}
	var transactions []json.RawMessage
	if json.Unmarshal(result["transactions"], &transactions) != nil || len(transactions) == 0 {
		return solidBlock{}, ErrSettlementEvidence
	}
	matches := 0
	for _, raw := range transactions {
		body, err := nativeObject(raw)
		if err != nil {
			return solidBlock{}, err
		}
		var id string
		if json.Unmarshal(body["txID"], &id) != nil || !validExecutionID(id) {
			return solidBlock{}, ErrSettlementEvidence
		}
		if id == prepared.ID() {
			matches++
			if err := settlementBody(body, prepared); err != nil {
				return solidBlock{}, err
			}
		}
	}
	if matches != 1 {
		return solidBlock{}, ErrSettlementEvidence
	}
	return block, nil
}

func settlementBody(body map[string]json.RawMessage, prepared *PreparedSettlement) error {
	var id, rawHex string
	if json.Unmarshal(body["txID"], &id) != nil || id != prepared.ID() || json.Unmarshal(body["raw_data_hex"], &rawHex) != nil || len(rawHex) != 2*len(prepared.raw) {
		return ErrSettlementEvidence
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil || !bytes.Equal(raw, prepared.raw) || sha256.Sum256(raw) != prepared.digest {
		return ErrSettlementEvidence
	}
	var results []json.RawMessage
	if json.Unmarshal(body["ret"], &results) != nil || len(results) != 1 {
		return ErrSettlementEvidence
	}
	result, err := nativeObject(results[0])
	if err != nil {
		return err
	}
	if err := nativeSuccessCode(result, "ret"); err != nil {
		return err
	}
	return contractSuccess(result, "contractRet")
}

func nativeSuccessCode(fields map[string]json.RawMessage, key string) error {
	raw, exists := fields[key]
	if !exists {
		return nil
	} // proto3 零值 SUCESS 默认省略，不能要求虚构的 SUCCESS 拼写。
	var value string
	if json.Unmarshal(raw, &value) != nil || value == "" {
		return ErrSettlementEvidence
	}
	if value != "SUCESS" {
		return ErrSettlementExecution
	}
	return nil
}

func contractSuccess(fields map[string]json.RawMessage, key string) error {
	var value string
	if json.Unmarshal(fields[key], &value) != nil || value == "" {
		return ErrSettlementEvidence
	}
	if value != "SUCCESS" {
		return ErrSettlementExecution
	}
	return nil
}

func nativePositiveInt(raw json.RawMessage) (int64, error) {
	var number int64
	if json.Unmarshal(raw, &number) != nil || number <= 0 || strconv.FormatInt(number, 10) != string(bytes.TrimSpace(raw)) {
		return 0, ErrSettlementEvidence
	}
	return number, nil
}

func nativeWord(raw json.RawMessage) ([32]byte, error) {
	var word [32]byte
	var value string
	if json.Unmarshal(raw, &value) != nil || len(value) != 64 {
		return word, ErrSettlementEvidence
	}
	if _, err := hex.Decode(word[:], []byte(value)); err != nil {
		return [32]byte{}, ErrSettlementEvidence
	}
	return word, nil
}

func settlementTransfer(raw json.RawMessage, payment parsedAuthorization) error {
	if payment.from == payment.to {
		return ErrSettlementTransfer
	}
	var logs []json.RawMessage
	if json.Unmarshal(raw, &logs) != nil || len(logs) == 0 {
		return ErrSettlementTransfer
	}
	transfer := keccak([]byte("Transfer(address,address,uint256)"))
	matches := 0
	for _, rawLog := range logs {
		entry, err := nativeObject(rawLog)
		if err != nil {
			return err
		}
		var address string
		var contract [20]byte
		if json.Unmarshal(entry["address"], &address) != nil || len(address) != 40 {
			return ErrSettlementEvidence
		}
		if _, err := hex.Decode(contract[:], []byte(address)); err != nil {
			return ErrSettlementEvidence
		}
		if contract != payment.token {
			continue
		}
		var topics []json.RawMessage
		if json.Unmarshal(entry["topics"], &topics) != nil {
			return ErrSettlementEvidence
		}
		if len(topics) == 0 {
			return ErrSettlementEvidence
		}
		signature, err := nativeWord(topics[0])
		if err != nil {
			return err
		}
		if signature != transfer {
			continue
		}
		if len(topics) != 3 {
			return ErrSettlementTransfer
		}
		from, fromErr := nativeWord(topics[1])
		to, toErr := nativeWord(topics[2])
		amount, amountErr := nativeWord(entry["data"])
		if fromErr != nil || toErr != nil || amountErr != nil || !bytes.Equal(from[:12], make([]byte, 12)) || !bytes.Equal(to[:12], make([]byte, 12)) {
			return ErrSettlementTransfer
		}
		payer, recipient := addressWord(payment.from), addressWord(payment.to)
		if from == payer && to == recipient && amount == payment.amount {
			matches++
			continue
		}
		// 其他涉及任一方的同资产 Transfer 可能是少付、费用或回转，不把毛额当净到账。
		if from == payer || from == recipient || to == payer || to == recipient {
			return ErrSettlementTransfer
		}
	}
	if matches != 1 {
		return ErrSettlementTransfer
	}
	return nil
}
