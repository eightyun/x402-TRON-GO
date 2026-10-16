package tron_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"

	tron "github.com/eightyun/x402-tron-go"
)

func TestNormalizeAddressVectors(t *testing.T) {
	t.Parallel()
	// 前两组是 TRON 官方 Accounts and keys 文档中的公开向量；没有钱包私钥。
	for _, test := range []struct{ base58, hex string }{
		{"TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL", "418840e6c55b9ada326d211d818c34a994aeced808"},
		{"TDp4x8kCJ6aHz8wnqZgkaVQAm2ajH1og3L", "412a260a110bc7b03f19c40a0bd04ff2c5dcb57594"},
		{"T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb", "410000000000000000000000000000000000000000"},
	} {
		t.Run(test.base58, func(t *testing.T) {
			t.Parallel()
			for _, input := range []string{test.base58, test.hex, strings.ToUpper(test.hex)} {
				if value, err := tron.NormalizeAddressHex(input); err != nil || value != test.hex {
					t.Fatalf("十六进制地址 = %q, %v，期望 %q", value, err, test.hex)
				}
				if value, err := tron.NormalizeAddressBase58(input); err != nil || value != test.base58 {
					t.Fatalf("Base58 地址 = %q, %v，期望 %q", value, err, test.base58)
				}
				if value, err := tron.AddressForSigning(input); err != nil || value != "0x"+test.hex[2:] {
					t.Fatalf("签名地址 = %q, %v", value, err)
				}
			}
		})
	}
}

func TestRejectInvalidAddress(t *testing.T) {
	t.Parallel()
	const address = "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL"
	for _, test := range []struct{ name, value string }{
		{"empty", ""},
		{"short_base58", address[:33]},
		{"long_base58", address + "1"},
		{"checksum", address[:33] + "M"},
		{"zero_character", "T0" + address[2:]},
		{"ambiguous_character", "TO" + address[2:]},
		{"nul_character", "T\x00" + address[2:]},
		{"invalid_prefix", "1" + address[1:]},
		{"oversized_base58_value", strings.Repeat("z", 34)},
		{"wrong_hex_prefix", "42" + strings.Repeat("0", 40)},
		{"missing_hex_prefix", strings.Repeat("a", 40)},
		{"evm_address", "0x" + strings.Repeat("a", 40)},
		{"prefixed_tron_hex", "0x41" + strings.Repeat("a", 40)},
		{"bad_hex_character", "41" + strings.Repeat("g", 40)},
		{"short_hex", "41" + strings.Repeat("0", 39)},
		{"long_hex", "41" + strings.Repeat("0", 41)},
		{"leading_whitespace", " " + address},
		{"trailing_whitespace", address + "\n"},
		{"unicode", "T" + strings.Repeat("中", 11)},
		{"oversized_input", strings.Repeat("a", 1<<20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, normalize := range []func(string) (string, error){
				tron.NormalizeAddressHex, tron.NormalizeAddressBase58, tron.AddressForSigning,
			} {
				if value, err := normalize(test.value); value != "" || !errors.Is(err, tron.ErrInvalidAddress) {
					t.Fatalf("无效地址未被拒绝：值长度 %d，错误 %v", len(value), err)
				}
			}
		})
	}
}

func TestRejectOtherAddressVersions(t *testing.T) {
	t.Parallel()
	for _, version := range []byte{0, 0x40, 0x42, 0xff} {
		t.Run(hex.EncodeToString([]byte{version}), func(t *testing.T) {
			payload := make([]byte, 21)
			payload[0] = version
			for _, normalize := range []func(string) (string, error){
				tron.NormalizeAddressHex, tron.NormalizeAddressBase58, tron.AddressForSigning,
			} {
				if value, err := normalize(referenceBase58Check(payload)); value != "" || !errors.Is(err, tron.ErrInvalidAddress) {
					t.Fatalf("接受了其他地址前缀：%q, %v", value, err)
				}
			}
		})
	}
}

func FuzzAddressRoundTrip(f *testing.F) {
	f.Add(make([]byte, 20))
	f.Add([]byte(strings.Repeat("\xff", 20)))
	f.Add([]byte{1, 2, 3, 4})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) != 20 {
			return
		}
		payload := append([]byte{0x41}, raw...)
		hexAddress := hex.EncodeToString(payload)
		base58, err := tron.NormalizeAddressBase58(hexAddress)
		if err != nil || base58 != referenceBase58Check(payload) {
			t.Fatalf("与独立编码器不一致：%q, %v", base58, err)
		}
		normalized, err := tron.NormalizeAddressHex(base58)
		if err != nil || normalized != hexAddress {
			t.Fatalf("编码往返不一致：%q, %v", normalized, err)
		}
		signingAddress, err := tron.AddressForSigning(base58)
		if err != nil || signingAddress != "0x"+hex.EncodeToString(raw) {
			t.Fatalf("签名地址转换不一致：%q, %v", signingAddress, err)
		}
	})
}

func FuzzNormalizeAddress(f *testing.F) {
	for _, seed := range []string{
		"TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL",
		"418840E6C55B9ADA326D211D818C34A994AECED808",
		"T9yD14Nj9j7xAB4dbGeiX9h8unkKHxuWwb",
		"0x8840e6c55b9ada326d211d818c34a994aeced808", "", strings.Repeat("T", 34),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		normalized, err := tron.NormalizeAddressHex(input)
		if err != nil {
			if normalized != "" || !errors.Is(err, tron.ErrInvalidAddress) {
				t.Fatalf("失败结果未清空：%q, %v", normalized, err)
			}
			return
		}
		if len(normalized) != 42 || !strings.HasPrefix(normalized, "41") {
			t.Fatalf("输出格式无效：%q", normalized)
		}
		base58, err := tron.NormalizeAddressBase58(input)
		if err != nil || len(base58) != 34 || base58[0] != 'T' {
			t.Fatalf("Base58 格式无效：%q, %v", base58, err)
		}
		converted, err := tron.NormalizeAddressHex(base58)
		if err != nil || converted != normalized {
			t.Fatalf("Base58 与十六进制结果不一致：%q, %v", converted, err)
		}
	})
}

// referenceBase58Check 使用大整数作为独立测试对照，不复用生产编解码实现。
func referenceBase58Check(payload []byte) string {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	first := sha256.Sum256(payload)
	second := sha256.Sum256(first[:])
	checked := append(append([]byte(nil), payload...), second[:4]...)
	number := new(big.Int).SetBytes(checked)
	base, remainder := big.NewInt(58), new(big.Int)
	var result []byte
	for number.Sign() > 0 {
		number.QuoRem(number, base, remainder)
		result = append(result, alphabet[remainder.Int64()])
	}
	for _, value := range checked {
		if value != 0 {
			break
		}
		result = append(result, '1')
	}
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return string(result)
}
