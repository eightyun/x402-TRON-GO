package tron_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	tron "github.com/eightyun/x402-tron-go"
)

func TestNormalizeNetwork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		canonical string
		alias     string
		chainID   uint32
	}{
		{"mainnet", tron.NetworkMainnet, "tron:0x2b6653dc", 728126428},
		{"nile", tron.NetworkNile, "tron:0xcd8690dc", 3448148188},
		{"shasta", tron.NetworkShasta, "tron:0x94a9059e", 2494104990},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			uppercaseAlias := "tron:" + strings.ToUpper(strings.TrimPrefix(test.alias, "tron:"))
			for _, input := range []string{test.canonical, test.alias, uppercaseAlias} {
				network, err := tron.NormalizeNetwork(input)
				if err != nil || network != test.canonical {
					t.Fatalf("规范网络 = %q, %v，期望 %q", network, err, test.canonical)
				}
				chainID, err := tron.ChainID(input)
				if err != nil || chainID != test.chainID {
					t.Fatalf("链 ID = %d, %v，期望 %d", chainID, err, test.chainID)
				}
			}
		})
	}
}

func TestRejectUnsupportedNetwork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ name, value string }{
		{"empty", ""},
		{"namespace_only", "tron:"},
		{"namespace_case", "TRON:3448148188"},
		{"missing_namespace", "3448148188"},
		{"unknown", "tron:1"},
		{"unknown_hex", "tron:0x1"},
		{"wildcard", "tron:*"},
		{"other_chain", "eip155:3448148188"},
		{"leading_zero", "tron:03448148188"},
		{"hex_leading_zero", "tron:0x0cd8690dc"},
		{"whitespace", " " + tron.NetworkNile},
		{"newline", tron.NetworkNile + "\n"},
		{"extra_separator", tron.NetworkNile + ":"},
		{"negative", "tron:-728126428"},
		{"plus", "tron:+728126428"},
		{"overflow", "tron:4294967296"},
		{"unicode", "tron:３４４８１４８１８８"},
		{"oversized", strings.Repeat("a", 1<<20)},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if value, err := tron.NormalizeNetwork(test.value); value != "" || !errors.Is(err, tron.ErrUnsupportedNetwork) {
				t.Fatalf("无效网络未被拒绝：值长度 %d，错误 %v", len(value), err)
			}
			if value, err := tron.ChainID(test.value); value != 0 || !errors.Is(err, tron.ErrUnsupportedNetwork) {
				t.Fatalf("无效网络返回链 ID：%d，错误 %v", value, err)
			}
		})
	}
}

func FuzzNormalizeNetwork(f *testing.F) {
	for _, seed := range []string{tron.NetworkMainnet, tron.NetworkNile, tron.NetworkShasta, "tron:0xcd8690dc", "tron:*", "", "tron:0X94A9059E"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input string) {
		network, err := tron.NormalizeNetwork(input)
		if err != nil {
			if network != "" || !errors.Is(err, tron.ErrUnsupportedNetwork) {
				t.Fatalf("失败结果未清空：%q, %v", network, err)
			}
			return
		}
		switch network {
		case tron.NetworkMainnet, tron.NetworkNile, tron.NetworkShasta:
		default:
			t.Fatalf("接受了未支持网络：%q", network)
		}
		chainID, err := tron.ChainID(input)
		if err != nil || "tron:"+strconv.FormatUint(uint64(chainID), 10) != network {
			t.Fatalf("链 ID 与规范网络不一致：%d, %q, %v", chainID, network, err)
		}
		canonical, err := tron.NormalizeNetwork(network)
		if err != nil || canonical != network {
			t.Fatalf("重复规范化不一致：%q, %v", canonical, err)
		}
	})
}
