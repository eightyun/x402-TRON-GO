package exact_test

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"sync"
	"testing"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

func serverFor(t testing.TB) (*exact.Server, types.PaymentRequirements, types.SupportedKind) {
	t.Helper()
	input := loadVector(t)
	server, err := exact.NewServer(exact.ServerConfig{Network: input.Config.Network, Asset: input.Requirements.Asset, PayTo: input.Requirements.PayTo})
	if err != nil {
		t.Fatal(err)
	}
	return server, input.Requirements, types.SupportedKind{
		X402Version: 2, Scheme: "exact", Network: tron.NetworkNile,
		Extra: map[string]interface{}{"supportedAssetTransferMethods": []string{"permit2"}},
	}
}

func TestServerPriceParsing(t *testing.T) {
	t.Parallel()
	server, requirements, _ := serverFor(t)
	max := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)).String()
	for _, price := range []interface{}{
		"1000", max,
		x402.AssetAmount{Asset: requirements.Asset, Amount: "1000"},
		&x402.AssetAmount{Asset: requirements.Asset, Amount: "1000", Extra: requirements.Extra},
		map[string]interface{}{"asset": requirements.Asset, "amount": "1000"},
		map[string]interface{}{"asset": requirements.Asset, "amount": "1000", "extra": map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"}},
	} {
		parsed, err := server.ParsePrice(price, x402.Network(server.Network()))
		if err != nil || parsed.Asset != requirements.Asset || parsed.Extra["paymentFlow"] != "upfront" {
			t.Fatalf("报价解析失败: %v", err)
		}
		parsed.Extra["paymentFlow"] = "changed"
		if again, err := server.ParsePrice(price, x402.Network(server.Network())); err != nil || again.Extra["paymentFlow"] != "upfront" {
			t.Fatal("报价共享内部 extra")
		}
	}
	for _, bad := range []interface{}{
		nil, 1000, 1.5, "$1", "1.5", "01", "0", "-1", "1e3", " 1", new(big.Int).Lsh(big.NewInt(1), 256).String(),
		(*x402.AssetAmount)(nil), x402.AssetAmount{Asset: requirements.PayTo, Amount: "1000"},
		x402.AssetAmount{Asset: requirements.Asset, Amount: "1000", Extra: map[string]interface{}{"assetTransferMethod": "eip3009"}},
		x402.AssetAmount{Asset: requirements.Asset, Amount: "1000", Extra: map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "authorization"}},
		map[string]interface{}{"asset": requirements.Asset, "amount": 1000},
		map[string]interface{}{"asset": requirements.Asset, "amount": "1000", "other": true},
		map[string]interface{}{"asset": requirements.Asset, "amount": "1000", "extra": nil},
	} {
		if parsed, err := server.ParsePrice(bad, x402.Network(server.Network())); !errors.Is(err, exact.ErrServerPrice) || !reflect.DeepEqual(parsed, x402.AssetAmount{}) {
			t.Fatal("接受不明确或超界报价")
		}
	}
	if _, err := server.ParsePrice("1000", x402.Network(tron.NetworkMainnet)); !errors.Is(err, exact.ErrServerPrice) {
		t.Fatal(err)
	}
}

func TestServerEnhanceAndOfficialBuild(t *testing.T) {
	t.Parallel()
	server, requirements, kind := serverFor(t)
	if server.Scheme() != "exact" || server.DefaultAssetTransferMethod() != "permit2" {
		t.Fatal("机制信息无效")
	}
	flows := server.PaymentFlows()
	flows["permit2"].Supported[0] = x402.PaymentFlowAuthorization
	if server.PaymentFlows()["permit2"].Supported[0] != x402.PaymentFlowUpfront {
		t.Fatal("流程共享可变数组")
	}
	core := x402.Newx402ResourceServer().Register(x402.Network(server.Network()), server)
	built, err := core.BuildPaymentRequirements(context.Background(), x402.ResourceConfig{
		Scheme: "exact", Network: x402.Network(server.Network()), PayTo: requirements.PayTo, Price: "1000",
	}, kind, []string{"not_copied"})
	if err != nil || built.MaxTimeoutSeconds != 300 || built.Extra["paymentFlow"] != "upfront" || len(built.Extra) != 2 {
		t.Fatalf("官方报价链路失败: %v", err)
	}
	_, flow, err := x402.ResolvePaymentFlow(server, built)
	if err != nil || flow != x402.PaymentFlowUpfront {
		t.Fatal("未采用先结算流程")
	}
	built.Extra["paymentFlow"] = "authorization"
	if _, _, err := x402.ResolvePaymentFlow(server, built); err == nil {
		t.Fatal("允许降级为先执行业务")
	}
	for _, extra := range []map[string]interface{}{nil, {}, requirements.Extra, {"assetTransferMethod": "permit2", "paymentFlow": "upfront"}} {
		requirements.Extra = extra
		enhanced, err := server.EnhancePaymentRequirements(context.Background(), requirements, kind, nil)
		if err != nil || enhanced.Extra["paymentFlow"] != "upfront" {
			t.Fatal(err)
		}
		if len(extra) > 0 {
			extra["assetTransferMethod"] = "changed"
		}
		if enhanced.Extra["assetTransferMethod"] != "permit2" {
			t.Fatal("报价复用了输入 map")
		}
	}
}

func TestServerRejectsMismatchedRequirementsAndCapabilities(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*types.PaymentRequirements){
		"scheme":    func(r *types.PaymentRequirements) { r.Scheme = "upto" },
		"asset":     func(r *types.PaymentRequirements) { r.Asset = r.PayTo },
		"recipient": func(r *types.PaymentRequirements) { r.PayTo = r.Asset },
		"amount":    func(r *types.PaymentRequirements) { r.Amount = "0" },
		"timeout":   func(r *types.PaymentRequirements) { r.MaxTimeoutSeconds = 5 },
		"extra":     func(r *types.PaymentRequirements) { r.Extra["future"] = true },
		"flow":      func(r *types.PaymentRequirements) { r.Extra["paymentFlow"] = "authorization" },
	} {
		t.Run(name, func(t *testing.T) {
			server, requirements, kind := serverFor(t)
			edit(&requirements)
			if _, err := server.EnhancePaymentRequirements(context.Background(), requirements, kind, nil); !errors.Is(err, exact.ErrInvalidRequirements) {
				t.Fatal(err)
			}
		})
	}
	for _, methods := range []interface{}{[]string{"permit2"}, []interface{}{"eip3009", "permit2"}} {
		server, _, kind := serverFor(t)
		kind.Extra["supportedAssetTransferMethods"] = methods
		kind.Extra["permit2FacilitatorAddress"] = "ignored metadata"
		if err := server.ValidateFacilitatorSupport(x402.Network(server.Network()), kind, []string{"ignored capability"}); err != nil {
			t.Fatal(err)
		}
	}
	for name, edit := range map[string]func(*types.SupportedKind){
		"version":      func(k *types.SupportedKind) { k.X402Version = 1 },
		"scheme":       func(k *types.SupportedKind) { k.Scheme = "upto" },
		"network":      func(k *types.SupportedKind) { k.Network = tron.NetworkMainnet },
		"empty":        func(k *types.SupportedKind) { k.Extra = nil },
		"wrong_method": func(k *types.SupportedKind) { k.Extra["supportedAssetTransferMethods"] = []string{"eip3009"} },
		"wrong_type":   func(k *types.SupportedKind) { k.Extra["supportedAssetTransferMethods"] = "permit2" },
		"bad_list":     func(k *types.SupportedKind) { k.Extra["supportedAssetTransferMethods"] = []interface{}{"permit2", 1} },
	} {
		t.Run(name, func(t *testing.T) {
			server, requirements, kind := serverFor(t)
			edit(&kind)
			if err := server.ValidateFacilitatorSupport(x402.Network(server.Network()), kind, nil); !errors.Is(err, exact.ErrFacilitatorSupport) {
				t.Fatal(err)
			}
			if _, err := server.EnhancePaymentRequirements(context.Background(), requirements, kind, nil); !errors.Is(err, exact.ErrFacilitatorSupport) {
				t.Fatal(err)
			}
		})
	}
}

func TestServerConfigCancellationAndConcurrency(t *testing.T) {
	t.Parallel()
	_, requirements, _ := serverFor(t)
	for _, config := range []exact.ServerConfig{
		{}, {Network: "tron:1", Asset: requirements.Asset, PayTo: requirements.PayTo},
		{Network: tron.NetworkNile, Asset: "invalid", PayTo: requirements.PayTo},
		{Network: tron.NetworkNile, Asset: requirements.Asset, PayTo: "invalid"},
	} {
		if server, err := exact.NewServer(config); server != nil || !errors.Is(err, exact.ErrServerConfig) {
			t.Fatal("无效配置未拒绝")
		}
	}
	for _, server := range []*exact.Server{nil, {}} {
		if server.Network() != "" {
			t.Fatal("零值网络不为空")
		}
		if _, err := server.ParsePrice("1", ""); !errors.Is(err, exact.ErrServerConfig) {
			t.Fatal(err)
		}
		if err := server.ValidateFacilitatorSupport("", types.SupportedKind{}, nil); !errors.Is(err, exact.ErrServerConfig) {
			t.Fatal(err)
		}
		if _, err := server.EnhancePaymentRequirements(context.Background(), requirements, types.SupportedKind{}, nil); !errors.Is(err, exact.ErrServerConfig) {
			t.Fatal(err)
		}
	}
	server, requirements, kind := serverFor(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := server.EnhancePaymentRequirements(ctx, requirements, kind, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if _, err := server.ParsePrice("1000", x402.Network(server.Network())); err != nil {
				t.Error(err)
			}
			if _, err := server.EnhancePaymentRequirements(context.Background(), requirements, kind, nil); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}

func TestUpfrontAuthorizationBindingAndClient(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	input.Requirements.Extra["paymentFlow"] = "upfront"
	verifier := verifierFor(t, input.Config)
	payload := envelope(t, input)
	if _, err := verifier.VerifyAuthorization(payload, input.Requirements, time.Unix(input.Now, 0)); err != nil {
		t.Fatal(err)
	}
	payload.Accepted.Extra = map[string]interface{}{"assetTransferMethod": "permit2"}
	if _, err := verifier.VerifyAuthorization(payload, input.Requirements, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrInvalidRequirements) {
		t.Fatal("允许剥离 upfront")
	}
	input.Requirements.Extra = payload.Accepted.Extra
	payload.Accepted.Extra = map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"}
	if _, err := verifier.VerifyAuthorization(payload, input.Requirements, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrInvalidRequirements) {
		t.Fatal("允许篡改流程")
	}
	config, requirements := clientConfigFor(t)
	requirements.Extra["paymentFlow"] = "upfront"
	created, err := clientFor(t, config).CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{})
	if err != nil || created.Accepted.Extra["paymentFlow"] != "upfront" {
		t.Fatal("客户端未保留 upfront")
	}
	if _, err := verifierFor(t, config.Authorization).VerifyAuthorization(created, requirements, time.Now()); err != nil {
		t.Fatal(err)
	}
}
