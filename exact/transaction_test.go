package exact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"golang.org/x/crypto/sha3"
)

type settlementVector struct {
	Now              int64                         `json:"now"`
	Config           exact.SettlementBuilderConfig `json:"config"`
	Reference        exact.ReferenceBlock          `json:"reference"`
	CallData         string                        `json:"callData"`
	RawDataHex       string                        `json:"rawDataHex"`
	TransactionID    string                        `json:"transactionID"`
	Signature        string                        `json:"signature"`
	TronWebSignature string                        `json:"tronWebSignature"`
	Payload          string                        `json:"payload"`
}

func loadSettlementVector(t testing.TB) settlementVector {
	t.Helper()
	data, err := os.ReadFile("testdata/tronweb-settlement.json")
	if err != nil {
		t.Fatal(err)
	}
	var input settlementVector
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	return input
}

func decodeHex(t testing.TB, value string) []byte {
	t.Helper()
	result, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal("公开交易向量编码无效")
	}
	return result
}

func preparedFor(t testing.TB, fixture settlementVector, input vector) *exact.PreparedSettlement {
	t.Helper()
	builder, err := exact.NewSettlementBuilder(fixture.Config)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(envelope(t, input), input.Requirements, fixture.Reference, time.Unix(input.Now, 0))
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func fixtureSigner(t testing.TB, fixture settlementVector, signature string) exact.DigestSigner {
	t.Helper()
	decoded := decodeHex(t, signature)
	return func(_ context.Context, digest [32]byte) ([]byte, error) {
		if hex.EncodeToString(digest[:]) != fixture.TransactionID {
			return nil, errors.New("签名器收到其他摘要")
		}
		return append([]byte(nil), decoded...), nil
	}
}

func TestTronWebSettlementVector(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	prepared := preparedFor(t, fixture, input)
	if prepared.ID() != fixture.TransactionID || hex.EncodeToString(prepared.RawData()) != fixture.RawDataHex {
		t.Fatal("本地 raw Protobuf/txID 与 TronWeb 不一致")
	}
	verified := prepared.Authorization()
	if prepared.SigningDigest() != sha256.Sum256(prepared.RawData()) || "0x"+hex.EncodeToString(verified.Digest[:]) != input.Digest {
		t.Fatal("交易签名摘要无效")
	}
	if !bytes.Contains(prepared.RawData(), decodeHex(t, fixture.CallData)) || !prepared.ExpiresAt().Equal(time.Unix(input.Now+60, 0)) {
		t.Fatal("代理 ABI 或过期时间不一致")
	}
	for _, signature := range []string{fixture.Signature, fixture.TronWebSignature} {
		signed, err := prepared.Sign(context.Background(), fixtureSigner(t, fixture, signature), time.Unix(input.Now, 0))
		if err != nil || signed.ID != fixture.TransactionID || hex.EncodeToString(signed.Payload) != fixture.Payload {
			t.Fatalf("与独立完整 Protobuf/签名不一致：%v", err)
		}
		if err := prepared.ValidateSigned(signed); err != nil {
			t.Fatal(err)
		}
		serialized, err := json.Marshal(signed)
		if err != nil || bytes.Contains(serialized, []byte("Payload")) || bytes.Contains(serialized, []byte(fixture.Payload)) {
			t.Fatal("交易 JSON 泄漏了签名字节")
		}
	}
	copyOfRaw := prepared.RawData()
	copyOfRaw[0] ^= 1
	if prepared.ID() != fixture.TransactionID || hex.EncodeToString(prepared.RawData()) != fixture.RawDataHex {
		t.Fatal("修改副本影响了准备对象")
	}
}

func TestSettlementCanonicalRepresentations(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	fixture.Config.Authorization.Network = "tron:0xcd8690dc"
	fixture.Config.ExecutorAddress, _ = tron.NormalizeAddressHex(fixture.Config.ExecutorAddress)
	fixture.Config.Authorization.ProxyAddress, _ = tron.NormalizeAddressHex(fixture.Config.Authorization.ProxyAddress)
	fixture.Reference.Network = "tron:0XCD8690DC"
	nonce, ok := new(big.Int).SetString(input.Authorization.Nonce[2:], 16)
	if !ok {
		t.Fatal("nonce 无效")
	}
	input.Authorization.Nonce = nonce.String()
	signature := decodeHex(t, input.Signature[2:])
	signature[64] -= 27
	input.Signature = "0x" + hex.EncodeToString(signature)
	prepared := preparedFor(t, fixture, input)
	if prepared.ID() != fixture.TransactionID {
		t.Fatal("地址/网络/nonce/签名 v 的等价表示改变了交易 ID")
	}
}

func TestSettlementRejectsChangedBytesAndSignatures(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	prepared := preparedFor(t, fixture, input)
	for _, test := range []struct {
		name   string
		mutate func(*exact.SettlementTransaction)
	}{
		{"id", func(tx *exact.SettlementTransaction) { tx.ID = strings.Repeat("a", 64) }},
		{"raw", func(tx *exact.SettlementTransaction) { tx.Payload[8] ^= 1 }},
		{"unknown_field", func(tx *exact.SettlementTransaction) { tx.Payload = append(tx.Payload, 0x18, 1) }},
		{"second_signature", func(tx *exact.SettlementTransaction) {
			tx.Payload = append(tx.Payload, append([]byte{0x12, 0x41}, tx.Payload[len(tx.Payload)-65:]...)...)
		}},
		{"truncated", func(tx *exact.SettlementTransaction) { tx.Payload = tx.Payload[:len(tx.Payload)-1] }},
		{"noncanonical_varint", func(tx *exact.SettlementTransaction) { tx.Payload = append([]byte{0x8a, 0}, tx.Payload[1:]...) }},
		{"json", func(tx *exact.SettlementTransaction) { tx.Payload = []byte(`{"raw_data_hex":"00"}`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			tx := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
			test.mutate(&tx)
			if err := prepared.ValidateSigned(tx); !errors.Is(err, exact.ErrSettlementBytes) {
				t.Fatalf("接受了变造字节：%v", err)
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"short", func(s []byte) []byte { return s[:64] }},
		{"long", func(s []byte) []byte { return append(s, 0) }},
		{"invalid_v", func(s []byte) []byte { s[64] = 2; return s }},
		{"zero_r", func(s []byte) []byte { clear(s[:32]); return s }},
		{"zero_s", func(s []byte) []byte { clear(s[32:64]); return s }},
		{"overflow_r", func(s []byte) []byte { copy(s[:32], bytes.Repeat([]byte{255}, 32)); return s }},
		{"high_s", func(s []byte) []byte {
			new(big.Int).Sub(secp256k1.Params().N, new(big.Int).SetBytes(s[32:64])).FillBytes(s[32:64])
			s[64] ^= 1
			return s
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			signature := test.mutate(decodeHex(t, fixture.Signature))
			_, err := prepared.Sign(context.Background(), func(context.Context, [32]byte) ([]byte, error) { return signature, nil }, time.Unix(input.Now, 0))
			if !errors.Is(err, exact.ErrSettlementSignature) {
				t.Fatalf("接受了非法交易签名：%v", err)
			}
		})
	}
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer key.Zero()
	_, err = prepared.Sign(context.Background(), func(_ context.Context, digest [32]byte) ([]byte, error) {
		compact := ecdsa.SignCompact(key, digest[:], false)
		return append(append([]byte{}, compact[1:]...), compact[0]-27), nil
	}, time.Unix(input.Now, 0))
	if !errors.Is(err, exact.ErrSettlementSignature) {
		t.Fatal("其他 EOA 的签名被接受")
	}
}

func TestSettlementReferenceAndPolicy(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	for _, mutate := range []func(*exact.SettlementBuilderConfig){
		func(c *exact.SettlementBuilderConfig) { c.ExecutorAddress = "0x" + strings.Repeat("11", 20) },
		func(c *exact.SettlementBuilderConfig) { c.ExecutorAddress = "T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb" },
		func(c *exact.SettlementBuilderConfig) { c.FeeLimitSUN = 0 },
		func(c *exact.SettlementBuilderConfig) { c.FeeLimitSUN = -1 },
		func(c *exact.SettlementBuilderConfig) { c.Lifetime = 15 * time.Second },
		func(c *exact.SettlementBuilderConfig) { c.Lifetime = 10*time.Minute + time.Millisecond },
		func(c *exact.SettlementBuilderConfig) { c.Lifetime += time.Microsecond },
		func(c *exact.SettlementBuilderConfig) { c.Authorization.Network = "tron:1" },
	} {
		config := fixture.Config
		mutate(&config)
		if _, err := exact.NewSettlementBuilder(config); !errors.Is(err, exact.ErrSettlementConfig) {
			t.Fatalf("非法构建策略被接受：%v", err)
		}
	}
	builder, err := exact.NewSettlementBuilder(fixture.Config)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*exact.ReferenceBlock){
		func(r *exact.ReferenceBlock) { r.Network = tron.NetworkMainnet },
		func(r *exact.ReferenceBlock) { r.Network = "tron:1" },
		func(r *exact.ReferenceBlock) { r.ID = "0x00" },
		func(r *exact.ReferenceBlock) { r.Number++ },
		func(r *exact.ReferenceBlock) { r.Number = 0 },
		func(r *exact.ReferenceBlock) { r.Number = math.MaxUint64 },
		func(r *exact.ReferenceBlock) { r.Timestamp = time.Time{} },
		func(r *exact.ReferenceBlock) {
			r.Timestamp = time.Unix(input.Now, 0).Add(-time.Minute - time.Nanosecond)
		},
		func(r *exact.ReferenceBlock) {
			r.Timestamp = time.Unix(input.Now, 0).Add(5*time.Second + time.Nanosecond)
		},
	} {
		reference := fixture.Reference
		mutate(&reference)
		if _, err := builder.Build(envelope(t, input), input.Requirements, reference, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrReferenceBlock) {
			t.Fatalf("非法参考区块被接受：%v", err)
		}
	}
	for _, shift := range []time.Duration{-time.Minute, 5 * time.Second} {
		reference := fixture.Reference
		reference.Timestamp = time.Unix(input.Now, 0).Add(shift)
		if _, err := builder.Build(envelope(t, input), input.Requirements, reference, time.Unix(input.Now, 0)); err != nil {
			t.Fatalf("合法参考时间边界被拒绝：%v", err)
		}
	}
	input.Signature = "0x00"
	if _, err := builder.Build(envelope(t, input), input.Requirements, fixture.Reference, time.Unix(input.Now, 0)); err == nil {
		t.Fatal("非法付款授权进入了构建器")
	}
}

func TestSettlementExpirationAndSignerFailures(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	prepared := preparedFor(t, fixture, input)
	now := time.Unix(input.Now, 0)
	for _, at := range []time.Time{now.Add(-time.Millisecond), now.Add(46 * time.Second), {}} {
		called := false
		_, err := prepared.Sign(context.Background(), func(context.Context, [32]byte) ([]byte, error) { called = true; return nil, nil }, at)
		if !errors.Is(err, exact.ErrSettlementValidity) || called {
			t.Fatal("无效有效期仍调用了签名器")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := prepared.Sign(ctx, fixtureSigner(t, fixture, fixture.Signature), now)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("取消未生效")
	}
	for _, test := range []struct{ cause, want error }{
		{errors.New("signer-url credential must not leak"), exact.ErrSettlementSigner},
		{fmt.Errorf("secret: %w", context.Canceled), context.Canceled},
		{fmt.Errorf("secret: %w", context.DeadlineExceeded), context.DeadlineExceeded},
	} {
		_, err := prepared.Sign(context.Background(), func(context.Context, [32]byte) ([]byte, error) { return nil, test.cause }, now)
		if err != test.want {
			t.Fatalf("签名器错误未脱敏/分类：%v", err)
		}
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	_, err = prepared.Sign(ctx, func(context.Context, [32]byte) ([]byte, error) { cancel(); return nil, nil }, now)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("签名后取消未生效")
	}
	_, err = prepared.Sign(context.Background(), func(context.Context, [32]byte) ([]byte, error) {
		time.Sleep(time.Millisecond)
		return decodeHex(t, fixture.Signature), nil
	}, prepared.ExpiresAt().Add(-15*time.Second))
	if !errors.Is(err, exact.ErrSettlementValidity) {
		t.Fatal("签名耗时没有计入有效期")
	}
	for _, seconds := range []int64{6, 20} {
		changed := input
		changed.Authorization.Deadline = fmt.Sprint(input.Now + seconds)
		changed = resignVector(t, changed)
		builder, err := exact.NewSettlementBuilder(fixture.Config)
		if err != nil {
			t.Fatal(err)
		}
		capped, err := builder.Build(envelope(t, changed), changed.Requirements, fixture.Reference, now)
		if seconds == 6 {
			if !errors.Is(err, exact.ErrSettlementValidity) {
				t.Fatal("过短授权仍生成交易")
			}
		} else if err != nil || !capped.ExpiresAt().Equal(now.Add(20*time.Second)) {
			t.Fatalf("交易超过付款授权截止时间：%v", err)
		}
	}
	if err := prepared.ValidateSigned(exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}); err != nil {
		t.Fatal("历史校验不应受真实墙钟时间影响")
	}
}

func TestPreparedSettlementZeroValueAndConcurrentUse(t *testing.T) {
	for _, prepared := range []*exact.PreparedSettlement{nil, {}} {
		if prepared.ID() != "" || prepared.SigningDigest() != [32]byte{} || len(prepared.RawData()) != 0 || prepared.Authorization() != (exact.VerifiedAuthorization{}) || !prepared.ExpiresAt().IsZero() {
			t.Fatal("零值访问器返回了有效交易信息")
		}
		if _, err := prepared.Sign(context.Background(), nil, time.Now()); !errors.Is(err, exact.ErrSettlementConfig) {
			t.Fatal("零值准备对象被接受")
		}
		if err := prepared.ValidateSigned(exact.SettlementTransaction{}); !errors.Is(err, exact.ErrSettlementConfig) {
			t.Fatal("零值准备对象校验通过")
		}
	}
	fixture, input := loadSettlementVector(t), loadVector(t)
	for _, builder := range []*exact.SettlementBuilder{nil, {}} {
		if _, err := builder.Build(envelope(t, input), input.Requirements, fixture.Reference, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrSettlementConfig) {
			t.Fatal("零值构建器被接受")
		}
	}
	prepared := preparedFor(t, fixture, input)
	if _, err := prepared.Sign(context.Background(), nil, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrSettlementConfig) {
		t.Fatal("nil 签名器被接受")
	}
	signer := fixtureSigner(t, fixture, fixture.Signature)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			signed, err := prepared.Sign(context.Background(), signer, time.Unix(input.Now, 0))
			if err != nil || hex.EncodeToString(signed.Payload) != fixture.Payload {
				t.Errorf("准备对象并发不安全：%v", err)
			}
		})
	}
	wg.Wait()
}

func FuzzValidateSettlementTransaction(f *testing.F) {
	fixture, input := loadSettlementVector(f), loadVector(f)
	prepared := preparedFor(f, fixture, input)
	canonical := decodeHex(f, fixture.Payload)
	f.Add(canonical, fixture.TransactionID)
	f.Add([]byte{}, "")
	f.Fuzz(func(t *testing.T, payload []byte, id string) {
		err := prepared.ValidateSigned(exact.SettlementTransaction{ID: id, Payload: payload})
		if err == nil && (id != fixture.TransactionID || len(payload) != len(canonical) || !bytes.Equal(payload[:len(payload)-65], canonical[:len(canonical)-65])) {
			t.Fatal("接受了其他 raw_data、字段或交易 ID")
		}
	})
}

func TestSettlementReferenceHeightEncoding(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	fixture.Reference.Number = 65536
	id := decodeHex(t, fixture.Reference.ID[2:])
	binary.BigEndian.PutUint64(id[:8], fixture.Reference.Number)
	fixture.Reference.ID = "0x" + hex.EncodeToString(id)
	prepared := preparedFor(t, fixture, input)
	if !bytes.HasPrefix(prepared.RawData(), []byte{0x0a, 0x02, 0, 0}) {
		t.Fatal("TAPOS 高度末两字节编码错误")
	}
}

func TestSettlementBindsExecutorAndFeePolicy(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	original := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
	for _, mutate := range []func(*exact.SettlementBuilderConfig){
		func(c *exact.SettlementBuilderConfig) { c.ExecutorAddress = input.Requirements.PayTo },
		func(c *exact.SettlementBuilderConfig) { c.FeeLimitSUN++ },
		func(c *exact.SettlementBuilderConfig) { c.Lifetime += time.Millisecond },
	} {
		changed := fixture
		mutate(&changed.Config)
		prepared := preparedFor(t, changed, input)
		if prepared.ID() == fixture.TransactionID || !errors.Is(prepared.ValidateSigned(original), exact.ErrSettlementBytes) {
			t.Fatal("费用付款方或策略改变后仍接受原交易")
		}
	}
	prepared := preparedFor(t, fixture, input)
	shared := decodeHex(t, fixture.TronWebSignature)
	before := append([]byte(nil), shared...)
	_, err := prepared.Sign(context.Background(), func(context.Context, [32]byte) ([]byte, error) { return shared, nil }, time.Unix(input.Now, 0))
	if err != nil || !bytes.Equal(shared, before) {
		t.Fatal("规范化签名修改了调用方的数据")
	}
}

func TestSettlementTimeAndLargeDeadline(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	input.Requirements.MaxTimeoutSeconds = math.MaxInt
	input.Authorization.Deadline = new(big.Int).Add(big.NewInt(input.Now), big.NewInt(int64(math.MaxInt))).String()
	input = resignVector(t, input)
	prepared := preparedFor(t, fixture, input)
	if !prepared.ExpiresAt().Equal(time.Unix(input.Now+60, 0)) {
		t.Fatal("大额 uint256 截止时间溢出了交易生命周期")
	}
	for _, at := range []time.Time{
		time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 50, 0, time.UTC),
	} {
		changed := loadVector(t)
		changed.Now = at.Unix()
		changed.Authorization.Deadline = fmt.Sprint(changed.Now + 600)
		changed = resignVector(t, changed)
		ref := fixture.Reference
		ref.Timestamp = at.Add(-time.Second)
		builder, err := exact.NewSettlementBuilder(fixture.Config)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := builder.Build(envelope(t, changed), changed.Requirements, ref, at); !errors.Is(err, exact.ErrSettlementValidity) {
			t.Fatalf("不支持的时间范围被接受：%v", err)
		}
	}
}

func TestSettlementWithEphemeralExecutorSigner(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer key.Zero()
	hash := sha3.NewLegacyKeccak256()
	hash.Write(key.PubKey().SerializeUncompressed()[1:])
	fixture.Config.ExecutorAddress, err = tron.NormalizeAddressBase58("41" + hex.EncodeToString(hash.Sum(nil)[12:]))
	if err != nil {
		t.Fatal(err)
	}
	prepared := preparedFor(t, fixture, input)
	signed, err := prepared.Sign(context.Background(), func(_ context.Context, digest [32]byte) ([]byte, error) {
		compact := ecdsa.SignCompact(key, digest[:], false)
		return append(append([]byte{}, compact[1:]...), compact[0]-27), nil
	}, time.Unix(input.Now, 0))
	if err != nil || prepared.ValidateSigned(signed) != nil || signed.ID != prepared.ID() {
		t.Fatalf("随机外部费用付款签名未通过：%v", err)
	}
}
