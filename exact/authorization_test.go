package exact_test

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/x402-foundation/x402/go/v2/types"
	"golang.org/x/crypto/sha3"
)

type vector struct {
	Now           int64                      `json:"now"`
	Config        exact.Config               `json:"config"`
	Requirements  types.PaymentRequirements  `json:"requirements"`
	Authorization exact.Permit2Authorization `json:"authorization"`
	Signature     string                     `json:"signature"`
	Digest        string                     `json:"digest"`
}

func loadVector(t testing.TB) vector {
	t.Helper()
	data, err := os.ReadFile("testdata/tronweb-permit2.json")
	if err != nil {
		t.Fatal(err)
	}
	var result vector
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func verifierFor(t testing.TB, config exact.Config) *exact.OfflineVerifier {
	t.Helper()
	verifier, err := exact.NewOfflineVerifier(config)
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func envelope(t testing.TB, input vector) types.PaymentPayload {
	t.Helper()
	raw, err := json.Marshal(exact.Permit2Payload{Signature: input.Signature, Authorization: input.Authorization})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	return types.PaymentPayload{X402Version: 2, Accepted: input.Requirements, Payload: fields}
}

func TestTronWebAuthorizationVector(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	verifier := verifierFor(t, input.Config)
	digest, err := verifier.Digest(input.Authorization)
	if err != nil || "0x"+hex.EncodeToString(digest[:]) != input.Digest {
		t.Fatalf("Go 与 TronWeb 摘要不一致：%x, %v", digest, err)
	}
	result, err := verifier.VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
	if err != nil || result.Digest != digest || result.Network != tron.NetworkNile {
		t.Fatalf("TronWeb 授权未通过离线验证：%+v, %v", result, err)
	}
	payer, err := tron.AddressForSigning(result.Payer)
	if err != nil || payer != input.Authorization.From {
		t.Fatalf("恢复的付款人不一致：%q, %v", payer, err)
	}
	// 同一已签名 nonce 的十六进制与十进制形式必须编码为同一 uint256。
	nonce, ok := new(big.Int).SetString(input.Authorization.Nonce[2:], 16)
	if !ok {
		t.Fatal("公开测试 nonce 无效")
	}
	input.Authorization.Nonce = nonce.String()
	decimalResult, err := verifier.VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
	if err != nil || decimalResult != result || result.Nonce != nonce.String() {
		t.Fatalf("nonce 表示规范化不一致：%+v, %v", decimalResult, err)
	}
}

func TestRejectTamperedAuthorization(t *testing.T) {
	t.Parallel()
	otherAddress := "0x1111111111111111111111111111111111111111"
	for _, test := range []struct {
		name   string
		mutate func(*vector)
		want   error
	}{
		{"amount", func(v *vector) { v.Authorization.Permitted.Amount = "1001" }, exact.ErrInvalidAuthorization},
		{"token", func(v *vector) { v.Authorization.Permitted.Token = otherAddress }, exact.ErrInvalidAuthorization},
		{"recipient", func(v *vector) { v.Authorization.Witness.To = otherAddress }, exact.ErrInvalidAuthorization},
		{"spender", func(v *vector) { v.Authorization.Spender = otherAddress }, exact.ErrInvalidAuthorization},
		{"payer", func(v *vector) { v.Authorization.From = otherAddress }, exact.ErrInvalidSignature},
		{"nonce", func(v *vector) { v.Authorization.Nonce = "42" }, exact.ErrInvalidSignature},
		{"deadline", func(v *vector) { v.Authorization.Deadline = "2000000599" }, exact.ErrInvalidSignature},
		{"valid_after", func(v *vector) { v.Authorization.Witness.ValidAfter = "1" }, exact.ErrInvalidSignature},
		{"future_valid_after", func(v *vector) { v.Authorization.Witness.ValidAfter = "2000000001" }, exact.ErrInvalidValidity},
		{"equal_time_bounds", func(v *vector) { v.Authorization.Witness.ValidAfter = v.Authorization.Deadline }, exact.ErrInvalidAuthorization},
		{"short_validity", func(v *vector) { v.Authorization.Deadline = "2000000005" }, exact.ErrInvalidValidity},
		{"expired", func(v *vector) { v.Authorization.Deadline = "1999999999" }, exact.ErrInvalidValidity},
		{"too_long_validity", func(v *vector) { v.Authorization.Deadline = "2000000601" }, exact.ErrInvalidValidity},
		{"negative_clock", func(v *vector) { v.Now = -1 }, exact.ErrInvalidValidity},
		{"different_domain_network", func(v *vector) { v.Config.Network = tron.NetworkShasta; v.Requirements.Network = tron.NetworkShasta }, exact.ErrInvalidSignature},
		{"different_permit2", func(v *vector) { v.Config.Permit2Address = v.Requirements.PayTo }, exact.ErrInvalidSignature},
		{"different_proxy", func(v *vector) { v.Config.ProxyAddress = v.Requirements.PayTo }, exact.ErrInvalidAuthorization},
		{"wrong_scheme", func(v *vector) { v.Requirements.Scheme = "upto" }, exact.ErrInvalidRequirements},
		{"wrong_network", func(v *vector) { v.Requirements.Network = tron.NetworkMainnet }, exact.ErrInvalidRequirements},
		{"unknown_network", func(v *vector) { v.Requirements.Network = "tron:1" }, exact.ErrInvalidRequirements},
		{"wrong_transfer_method", func(v *vector) { v.Requirements.Extra["assetTransferMethod"] = "eip3009" }, exact.ErrInvalidRequirements},
		{"missing_transfer_method", func(v *vector) { v.Requirements.Extra = nil }, exact.ErrInvalidRequirements},
		{"future_extra", func(v *vector) { v.Requirements.Extra["futureFee"] = "1" }, exact.ErrInvalidRequirements},
		{"numeric_amount", func(v *vector) { v.Requirements.Amount = "1e3" }, exact.ErrInvalidRequirements},
		{"zero_amount", func(v *vector) { v.Requirements.Amount = "0" }, exact.ErrInvalidRequirements},
		{"zero_timeout", func(v *vector) { v.Requirements.MaxTimeoutSeconds = 0 }, exact.ErrInvalidRequirements},
		{"negative_timeout", func(v *vector) { v.Requirements.MaxTimeoutSeconds = -1 }, exact.ErrInvalidRequirements},
		{"evm_requirements_asset", func(v *vector) { v.Requirements.Asset = v.Authorization.Permitted.Token }, exact.ErrInvalidRequirements},
		{"evm_requirements_recipient", func(v *vector) { v.Requirements.PayTo = v.Authorization.Witness.To }, exact.ErrInvalidRequirements},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := loadVector(t)
			test.mutate(&input)
			verifier := verifierFor(t, input.Config)
			result, err := verifier.VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
			if !errors.Is(err, test.want) || result != (exact.VerifiedAuthorization{}) {
				t.Fatalf("篡改检查 = %+v, %v，期望 %v", result, err, test.want)
			}
		})
	}
}

func TestRejectInvalidUint256Fields(t *testing.T) {
	t.Parallel()
	for index, bad := range []string{"", "-1", "+1", "01", " 1", "1 ", "1.0", "1e3", "0x1", "0x" + strings.Repeat("g", 64), new(big.Int).Lsh(big.NewInt(1), 256).String(), strings.Repeat("9", 1024)} {
		for _, field := range []string{"amount", "nonce", "deadline", "validAfter"} {
			t.Run(field+"_"+strconv.Itoa(index), func(t *testing.T) {
				input := loadVector(t)
				switch field {
				case "amount":
					input.Authorization.Permitted.Amount = bad
				case "nonce":
					input.Authorization.Nonce = bad
				case "deadline":
					input.Authorization.Deadline = bad
				case "validAfter":
					input.Authorization.Witness.ValidAfter = bad
				}
				if _, err := verifierFor(t, input.Config).Digest(input.Authorization); !errors.Is(err, exact.ErrInvalidAuthorization) {
					t.Fatalf("未拒绝无效 uint256：%v", err)
				}
			})
		}
	}
}

func TestRejectInvalidWireAddresses(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", "0x" + strings.Repeat("0", 40), "0x1", "0X" + strings.Repeat("1", 40), "0x" + strings.Repeat("g", 40), "41" + strings.Repeat("1", 40), "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL"} {
		input := loadVector(t)
		input.Authorization.From = bad
		if _, err := verifierFor(t, input.Config).Digest(input.Authorization); !errors.Is(err, exact.ErrInvalidAuthorization) {
			t.Fatalf("未拒绝无效 wire 地址，长度 %d，错误 %v", len(bad), err)
		}
	}
}

func TestRejectMismatchedEnvelope(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*types.PaymentPayload)
	}{
		{"version", func(p *types.PaymentPayload) { p.X402Version = 1 }},
		{"extensions", func(p *types.PaymentPayload) { p.Extensions = map[string]interface{}{"unknown": true} }},
		{"scheme", func(p *types.PaymentPayload) { p.Accepted.Scheme = "upto" }},
		{"network", func(p *types.PaymentPayload) { p.Accepted.Network = tron.NetworkMainnet }},
		{"amount", func(p *types.PaymentPayload) { p.Accepted.Amount = "1001" }},
		{"asset", func(p *types.PaymentPayload) { p.Accepted.Asset = p.Accepted.PayTo }},
		{"recipient", func(p *types.PaymentPayload) { p.Accepted.PayTo = p.Accepted.Asset }},
		{"timeout", func(p *types.PaymentPayload) { p.Accepted.MaxTimeoutSeconds++ }},
		{"extra", func(p *types.PaymentPayload) { p.Accepted.Extra = nil }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := loadVector(t)
			payload := envelope(t, input)
			test.mutate(&payload)
			if _, err := verifierFor(t, input.Config).VerifyAuthorization(payload, input.Requirements, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrInvalidRequirements) {
				t.Fatalf("未拒绝不一致的 x402 信封：%v", err)
			}
		})
	}
}

func TestRejectMalformedPayload(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(map[string]interface{})
	}{
		{"missing_signature", func(p map[string]interface{}) { delete(p, "signature") }},
		{"non_string_signature", func(p map[string]interface{}) { p["signature"] = 42 }},
		{"unexpected_payload_field", func(p map[string]interface{}) { p["authorization"] = nil }},
		{"null_authorization", func(p map[string]interface{}) { p["permit2Authorization"] = nil }},
		{"missing_auth_field", func(p map[string]interface{}) { delete(p["permit2Authorization"].(map[string]interface{}), "from") }},
		{"unexpected_auth_field", func(p map[string]interface{}) {
			p["permit2Authorization"].(map[string]interface{})["facilitator"] = "x"
		}},
		{"null_permitted", func(p map[string]interface{}) { p["permit2Authorization"].(map[string]interface{})["permitted"] = nil }},
		{"unexpected_witness", func(p map[string]interface{}) {
			p["permit2Authorization"].(map[string]interface{})["witness"].(map[string]interface{})["facilitator"] = "x"
		}},
		{"non_string_amount", func(p map[string]interface{}) {
			p["permit2Authorization"].(map[string]interface{})["permitted"].(map[string]interface{})["amount"] = 1000
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := loadVector(t)
			payload := envelope(t, input)
			test.mutate(payload.Payload)
			if _, err := verifierFor(t, input.Config).VerifyAuthorization(payload, input.Requirements, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrInvalidAuthorization) {
				t.Fatalf("未拒绝歧义或缺失的 payload：%v", err)
			}
		})
	}
}

func TestSignatureCanonicality(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
		valid  bool
	}{
		{"v_0_or_1", func(s []byte) []byte { s[64] -= 27; return s }, true},
		{"zero_r", func(s []byte) []byte { clear(s[:32]); return s }, false},
		{"zero_s", func(s []byte) []byte { clear(s[32:64]); return s }, false},
		{"overflow_r", func(s []byte) []byte {
			for i := range 32 {
				s[i] = 0xff
			}
			return s
		}, false},
		{"overflow_s", func(s []byte) []byte {
			for i := 32; i < 64; i++ {
				s[i] = 0xff
			}
			return s
		}, false},
		{"invalid_v", func(s []byte) []byte { s[64] = 29; return s }, false},
		{"compact_signature", func(s []byte) []byte { return s[:64] }, false},
		{"high_s", func(s []byte) []byte {
			highS := new(big.Int).Sub(secp256k1.Params().N, new(big.Int).SetBytes(s[32:64]))
			highS.FillBytes(s[32:64])
			s[64] = 27 + ((s[64] - 27) ^ 1)
			return s
		}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := loadVector(t)
			decoded, err := hex.DecodeString(input.Signature[2:])
			if err != nil {
				t.Fatal(err)
			}
			input.Signature = "0x" + hex.EncodeToString(test.mutate(decoded))
			_, err = verifierFor(t, input.Config).VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
			if test.valid && err != nil {
				t.Fatal(err)
			}
			if !test.valid && !errors.Is(err, exact.ErrInvalidSignature) {
				t.Fatalf("非规范签名未被拒绝：%v", err)
			}
		})
	}
	input := loadVector(t)
	input.Signature = "0x" + strings.Repeat("g", 130)
	if _, err := verifierFor(t, input.Config).VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrInvalidSignature) {
		t.Fatalf("未拒绝非十六进制签名：%v", err)
	}
}

func TestVerifierConfigurationAndZeroValue(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*exact.Config){
		func(c *exact.Config) { c.Network = "tron:*" },
		func(c *exact.Config) { c.Permit2Address = "" },
		func(c *exact.Config) { c.ProxyAddress = "" },
		func(c *exact.Config) { c.ProxyAddress = c.Permit2Address },
		func(c *exact.Config) { c.Permit2Address = "410000000000000000000000000000000000000000" },
	} {
		input := loadVector(t)
		mutate(&input.Config)
		if value, err := exact.NewOfflineVerifier(input.Config); value != nil || !errors.Is(err, exact.ErrInvalidConfig) {
			t.Fatalf("未拒绝无效配置：%v", err)
		}
	}
	for _, verifier := range []*exact.OfflineVerifier{nil, {}} {
		input := loadVector(t)
		if _, err := verifier.Digest(input.Authorization); !errors.Is(err, exact.ErrInvalidConfig) {
			t.Fatal(err)
		}
		if _, err := verifier.VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrInvalidConfig) {
			t.Fatal(err)
		}
	}
}

func TestOfflineVerificationDoesNotConsumeNonce(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	verifier := verifierFor(t, input.Config)
	for range 2 {
		if _, err := verifier.VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now, 0)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVerifierConcurrentUse(t *testing.T) {
	input := loadVector(t)
	verifier := verifierFor(t, input.Config)
	payload := envelope(t, input)
	for index := range 16 {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			t.Parallel()
			if _, err := verifier.VerifyAuthorization(payload, input.Requirements, time.Unix(input.Now, 0)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGeneratedEOASignatureAndValidityBoundary(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer key.Zero()
	hasher := sha3.NewLegacyKeccak256()
	_, _ = hasher.Write(key.PubKey().SerializeUncompressed()[1:])
	input.Authorization.From = "0x" + hex.EncodeToString(hasher.Sum(nil)[12:])
	input.Authorization.Witness.ValidAfter = "2000000000"
	input.Authorization.Deadline = "2000000006"
	input.Authorization.Nonce = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)).String()
	verifier := verifierFor(t, input.Config)
	digest, err := verifier.Digest(input.Authorization)
	if err != nil {
		t.Fatal(err)
	}
	compact := ecdsa.SignCompact(key, digest[:], false)
	signature := append(append([]byte(nil), compact[1:]...), compact[0])
	input.Signature = "0x" + hex.EncodeToString(signature)
	if _, err := verifier.VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now, 0)); err != nil {
		t.Fatalf("随机 EOA 签名/有效期边界/uint256 上限未通过：%v", err)
	}
	input.Config.Network = "tron:0xcd8690dc"
	input.Requirements.Network = "tron:0XCD8690DC"
	if _, err := verifierFor(t, input.Config).VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now, 0)); err != nil {
		t.Fatalf("历史网络别名影响签名域：%v", err)
	}
}

func FuzzVerifyAuthorization(f *testing.F) {
	input := loadVector(f)
	seed, err := json.Marshal(envelope(f, input))
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{"x402Version":2,"payload":null}`))
	verifier := verifierFor(f, input.Config)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			return
		}
		var payload types.PaymentPayload
		if json.Unmarshal(data, &payload) != nil {
			return
		}
		result, err := verifier.VerifyAuthorization(payload, input.Requirements, time.Unix(input.Now, 0))
		if err != nil && result != (exact.VerifiedAuthorization{}) {
			t.Fatalf("失败返回了有效授权结果：%v", err)
		}
		if err == nil && (result.Network != tron.NetworkNile || result.Payer == "") {
			t.Fatal("成功结果缺少网络或付款人")
		}
	})
}
