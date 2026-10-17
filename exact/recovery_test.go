package exact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
)

func recoveryTransaction(raw, signature []byte) exact.SettlementTransaction {
	digest := sha256.Sum256(raw)
	payload := binary.AppendUvarint([]byte{0x0a}, uint64(len(raw)))
	payload = append(payload, raw...)
	payload = append(payload, 0x12, 65)
	payload = append(payload, signature...)
	return exact.SettlementTransaction{ID: hex.EncodeToString(digest[:]), Payload: payload}
}

func TestRestoreSettlementHistoricalOriginalPayment(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	tx := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
	restored, err := exact.RestoreSettlement(fixture.Config.Authorization, fixture.Config.ExecutorAddress, tx)
	if err != nil {
		t.Fatal(err)
	}
	original := preparedFor(t, fixture, input)
	if restored.Authorization() != original.Authorization() || restored.ID() != original.ID() || !bytes.Equal(restored.RawData(), original.RawData()) || !restored.ExpiresAt().Equal(original.ExpiresAt()) || restored.FeeLimitSUN() != fixture.Config.FeeLimitSUN {
		t.Fatal("恢复改变了原授权、交易字节或有效期")
	}
	if err := restored.ValidateSigned(tx); err != nil {
		t.Fatal(err)
	}
	if err := restored.ValidateBroadcast(tx, time.Unix(input.Now+900, 0)); !errors.Is(err, exact.ErrSettlementValidity) {
		t.Fatal("历史恢复延长了原交易有效期")
	}
	server, _ := receiptNode(t, nil)
	observed, err := executionClient(t, executionConfig(server.URL)).CheckSettlement(context.Background(), restored, tx, time.Unix(input.Now+900, 0))
	if err != nil || !observed.Confirmed || observed.TransactionID != fixture.TransactionID {
		t.Fatalf("过期原交易未能核验: %v", err)
	}
	tx.Payload[20] ^= 1
	copyOfRaw := restored.RawData()
	copyOfRaw[0] ^= 1
	if !bytes.Equal(restored.RawData(), original.RawData()) {
		t.Fatal("恢复对象引用了调用方切片")
	}
}

func TestRestoreSettlementTrustedConfiguration(t *testing.T) {
	t.Parallel()
	fixture := loadSettlementVector(t)
	tx := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
	for _, test := range []struct {
		name   string
		mutate func(*exact.Config, *string)
		want   error
	}{
		{"invalid_config", func(c *exact.Config, _ *string) { c.Network = "tron:1" }, exact.ErrSettlementConfig},
		{"invalid_executor", func(_ *exact.Config, e *string) { *e = "invalid" }, exact.ErrSettlementConfig},
		{"network", func(c *exact.Config, _ *string) { c.Network = tron.NetworkMainnet }, exact.ErrInvalidSignature},
		{"permit2", func(c *exact.Config, _ *string) { c.Permit2Address = fixture.Config.ExecutorAddress }, exact.ErrInvalidSignature},
		{"proxy", func(c *exact.Config, _ *string) { c.ProxyAddress = fixture.Config.ExecutorAddress }, exact.ErrSettlementBytes},
		{"executor", func(c *exact.Config, e *string) { *e = c.Permit2Address }, exact.ErrSettlementBytes},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, executor := fixture.Config.Authorization, fixture.Config.ExecutorAddress
			test.mutate(&config, &executor)
			if result, err := exact.RestoreSettlement(config, executor, tx); result != nil || !errors.Is(err, test.want) {
				t.Fatalf("接受了其他可信配置: %v", err)
			}
		})
	}
	config := fixture.Config.Authorization
	config.Network = "tron:0xcd8690dc"
	config.Permit2Address, _ = tron.NormalizeAddressHex(config.Permit2Address)
	config.ProxyAddress, _ = tron.NormalizeAddressHex(config.ProxyAddress)
	executor, _ := tron.NormalizeAddressHex(fixture.Config.ExecutorAddress)
	if restored, err := exact.RestoreSettlement(config, executor, tx); err != nil || restored.ID() != fixture.TransactionID {
		t.Fatal("等价可信配置未规范化")
	}
}

// 只用于测试构造重新封装的恶意 Protobuf，不参与生产解析。
func rewriteRecoveryField(t *testing.T, input []byte, field uint64, value []byte) []byte {
	t.Helper()
	var output []byte
	for len(input) > 0 {
		start := input
		key, n := binary.Uvarint(input)
		if n <= 0 {
			t.Fatal("测试输入 key 无效")
		}
		input = input[n:]
		number, n := binary.Uvarint(input)
		if n <= 0 {
			t.Fatal("测试输入 value 无效")
		}
		input = input[n:]
		if key&7 == 2 {
			input = input[int(number):]
		}
		if key>>3 == field {
			output = binary.AppendUvarint(output, key)
			if key&7 == 2 {
				output = binary.AppendUvarint(output, uint64(len(value)))
			}
			output = append(output, value...)
		} else {
			output = append(output, start[:len(start)-len(input)]...)
		}
	}
	return output
}

func TestRestoreSettlementRejectsMalformedOriginalBytes(t *testing.T) {
	t.Parallel()
	fixture := loadSettlementVector(t)
	for _, test := range []struct {
		name   string
		mutate func(*exact.SettlementTransaction)
	}{
		{"empty", func(tx *exact.SettlementTransaction) { tx.Payload = nil }},
		{"oversized", func(tx *exact.SettlementTransaction) { tx.Payload = make([]byte, (1<<20)+1) }},
		{"id", func(tx *exact.SettlementTransaction) { tx.ID = strings.Repeat("a", 64) }},
		{"uppercase_id", func(tx *exact.SettlementTransaction) { tx.ID = strings.ToUpper(tx.ID) }},
		{"trailing", func(tx *exact.SettlementTransaction) { tx.Payload = append(tx.Payload, 0x18, 1) }},
		{"duplicate_signature", func(tx *exact.SettlementTransaction) {
			tx.Payload = append(tx.Payload, tx.Payload[len(tx.Payload)-67:]...)
		}},
		{"noncanonical_key", func(tx *exact.SettlementTransaction) { tx.Payload = append([]byte{0x8a, 0}, tx.Payload[1:]...) }},
		{"overflow_length", func(tx *exact.SettlementTransaction) {
			tx.Payload = append([]byte{0x0a}, bytes.Repeat([]byte{0xff}, 10)...)
		}},
		{"truncated_signature", func(tx *exact.SettlementTransaction) { tx.Payload = tx.Payload[:len(tx.Payload)-1] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
			test.mutate(&tx)
			if restored, err := exact.RestoreSettlement(fixture.Config.Authorization, fixture.Config.ExecutorAddress, tx); err == nil || restored != nil {
				t.Fatal("接受了非法完整交易")
			}
		})
	}
	raw, signature := decodeHex(t, fixture.RawDataHex), decodeHex(t, fixture.Signature)
	for _, test := range []struct {
		name string
		raw  []byte
	}{
		{"zero_fee", rewriteRecoveryField(t, raw, 18, []byte{0})},
		{"overflow_fee", rewriteRecoveryField(t, raw, 18, binary.AppendUvarint(nil, math.MaxUint64))},
		{"overflow_timestamp", rewriteRecoveryField(t, raw, 14, binary.AppendUvarint(nil, math.MaxUint64))},
		{"overflow_expiration", rewriteRecoveryField(t, raw, 8, binary.AppendUvarint(nil, math.MaxUint64))},
		{"invalid_year", rewriteRecoveryField(t, raw, 14, []byte{1})},
		{"expiration_before_creation", rewriteRecoveryField(t, raw, 8, binary.AppendUvarint(nil, uint64(fixture.Now*1000)))},
		{"expiration_too_late", rewriteRecoveryField(t, raw, 8, binary.AppendUvarint(nil, uint64((fixture.Now+601)*1000)))},
		{"expiration_after_authorization", rewriteRecoveryField(t,
			rewriteRecoveryField(t, raw, 14, binary.AppendUvarint(nil, uint64((fixture.Now+1)*1000))),
			8, binary.AppendUvarint(nil, uint64((fixture.Now+600)*1000+1)))},
		{"noncanonical_integer", rewriteRecoveryField(t, raw, 18, []byte{0x81, 0})},
		{"short_reference", rewriteRecoveryField(t, raw, 1, []byte{1})},
		{"short_reference_hash", rewriteRecoveryField(t, raw, 4, []byte{1})},
		{"unknown_field", append(append([]byte(nil), raw...), 0x98, 1, 1)},
		{"contract_type", rewriteRecoveryField(t, raw, 11, []byte{8, 30})},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := recoveryTransaction(test.raw, signature)
			if restored, err := exact.RestoreSettlement(fixture.Config.Authorization, fixture.Config.ExecutorAddress, tx); restored != nil || !errors.Is(err, exact.ErrSettlementBytes) {
				t.Fatalf("接受了重新计算 ID 的坏交易: %v", err)
			}
		})
	}
	callData := decodeHex(t, fixture.CallData)
	for _, test := range []struct {
		name   string
		mutate func([]byte)
		want   error
	}{
		{"zero_amount", func(call []byte) { clear(call[36:68]) }, exact.ErrSettlementBytes},
		{"zero_address", func(call []byte) { clear(call[4:36]) }, exact.ErrSettlementBytes},
		{"address_padding", func(call []byte) { call[4] = 1 }, exact.ErrSettlementBytes},
		{"abi_padding", func(call []byte) { call[len(call)-1] = 1 }, exact.ErrSettlementBytes},
		{"abi_offset", func(call []byte) { call[259] ^= 1 }, exact.ErrSettlementBytes},
		{"authorization_signature", func(call []byte) { call[292] ^= 1 }, exact.ErrInvalidSignature},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := append([]byte(nil), callData...)
			test.mutate(changed)
			tx := recoveryTransaction(bytes.Replace(raw, callData, changed, 1), signature)
			if restored, err := exact.RestoreSettlement(fixture.Config.Authorization, fixture.Config.ExecutorAddress, tx); restored != nil || !errors.Is(err, test.want) {
				t.Fatalf("接受了坏 ABI 或授权签名: %v", err)
			}
		})
	}
	// 即便攻击者同步更新交易 ID，内外签名和完整固定格式仍不能被替换。
	for index := range raw {
		changed := append([]byte(nil), raw...)
		changed[index] ^= 1
		if restored, err := exact.RestoreSettlement(fixture.Config.Authorization, fixture.Config.ExecutorAddress, recoveryTransaction(changed, signature)); err == nil || restored != nil {
			t.Fatalf("接受了 raw 字节 %d 的篡改", index)
		}
	}
	for index := range signature {
		changed := append([]byte(nil), signature...)
		changed[index] ^= 1
		if restored, err := exact.RestoreSettlement(fixture.Config.Authorization, fixture.Config.ExecutorAddress, recoveryTransaction(raw, changed)); restored != nil || !errors.Is(err, exact.ErrSettlementSignature) {
			t.Fatalf("接受了交易签名字节 %d 的篡改: %v", index, err)
		}
	}
}

func TestRestoreSettlementConcurrent(t *testing.T) {
	t.Parallel()
	fixture := loadSettlementVector(t)
	tx := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			restored, err := exact.RestoreSettlement(fixture.Config.Authorization, fixture.Config.ExecutorAddress, tx)
			if err != nil || restored.ID() != fixture.TransactionID {
				t.Errorf("并发恢复失败: %v", err)
			}
		})
	}
	wg.Wait()
}

func FuzzRestoreSettlement(f *testing.F) {
	fixture := loadSettlementVector(f)
	f.Add(decodeHex(f, fixture.Payload), fixture.TransactionID)
	f.Add([]byte{}, "")
	f.Fuzz(func(t *testing.T, payload []byte, id string) {
		// 同步原 raw 的摘要，避免模糊输入全部停在 txid 比较，继续覆盖嵌套解析。
		if len(payload) > 1 && payload[0] == 0x0a {
			length, n := binary.Uvarint(payload[1:])
			if n > 0 && length <= uint64(len(payload)-1-n) {
				digest := sha256.Sum256(payload[1+n : 1+n+int(length)])
				id = hex.EncodeToString(digest[:])
			}
		}
		tx := exact.SettlementTransaction{ID: id, Payload: payload}
		restored, err := exact.RestoreSettlement(fixture.Config.Authorization, fixture.Config.ExecutorAddress, tx)
		if err == nil {
			if restored.ID() != id || sha256.Sum256(restored.RawData()) != restored.SigningDigest() || restored.ValidateSigned(tx) != nil {
				t.Fatal("恢复结果没有绑定原始签名交易")
			}
		} else if restored != nil {
			t.Fatal("失败时返回了准备对象")
		}
	})
}
