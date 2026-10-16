package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/eightyun/x402-tron-go/exact"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/types"
)

// 模拟成功仅验证官方中间件控制流；这个 facilitator 只在测试内存在，不证明任何链上付款。
type fixtureFacilitator struct {
	config  exact.Config
	kind    types.SupportedKind
	mode    string
	settles atomic.Int32
	settled atomic.Bool
}

func (f *fixtureFacilitator) GetSupported(context.Context) (x402.SupportedResponse, error) {
	if f.mode == "init_error" {
		return x402.SupportedResponse{}, errors.New("unavailable")
	}
	return x402.SupportedResponse{Kinds: []types.SupportedKind{f.kind}}, nil
}

func (f *fixtureFacilitator) verify(payloadBytes, requirementBytes []byte) (exact.VerifiedAuthorization, error) {
	var payload types.PaymentPayload
	var requirements types.PaymentRequirements
	if json.Unmarshal(payloadBytes, &payload) != nil || json.Unmarshal(requirementBytes, &requirements) != nil {
		return exact.VerifiedAuthorization{}, errors.New("invalid fixture input")
	}
	verifier, err := exact.NewOfflineVerifier(f.config)
	if err != nil {
		return exact.VerifiedAuthorization{}, err
	}
	return verifier.VerifyAuthorization(payload, requirements, time.Now())
}

func (f *fixtureFacilitator) Verify(_ context.Context, payloadBytes, requirementBytes []byte) (*x402.VerifyResponse, error) {
	verified, err := f.verify(payloadBytes, requirementBytes)
	if err != nil {
		return nil, err
	}
	return &x402.VerifyResponse{IsValid: true, Payer: verified.Payer}, nil
}

func (f *fixtureFacilitator) Settle(ctx context.Context, payloadBytes, requirementBytes []byte) (*x402.SettleResponse, error) {
	f.settles.Add(1)
	verified, err := f.verify(payloadBytes, requirementBytes)
	if err != nil {
		return nil, err
	}
	if f.mode == "timeout" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.mode == "error" {
		return nil, errors.New("fixture unavailable")
	}
	if f.mode != "success" {
		return &x402.SettleResponse{Success: false, ErrorReason: "settlement_pending", Network: x402.Network(f.config.Network)}, nil
	}
	f.settled.Store(true)
	return &x402.SettleResponse{
		Success: true, Transaction: strings.Repeat("1", 64), Network: x402.Network(f.config.Network), Payer: verified.Payer,
	}, nil
}

func setup(t testing.TB, mode string) (*exact.Server, *exact.Client, *fixtureFacilitator, string) {
	t.Helper()
	data, err := os.ReadFile("../../exact/testdata/tronweb-permit2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Config       exact.Config              `json:"config"`
		Requirements types.PaymentRequirements `json:"requirements"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	server, err := exact.NewServer(exact.ServerConfig{Network: fixture.Config.Network, Asset: fixture.Requirements.Asset, PayTo: fixture.Requirements.PayTo})
	if err != nil {
		t.Fatal(err)
	}
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer key.Zero()
	encoded := key.Serialize()
	signer, err := exact.NewEOASigner(encoded)
	clear(encoded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = signer.Close() })
	client, err := exact.NewClient(exact.ClientConfig{
		Authorization: fixture.Config, PayerAddress: signer.Address(), Signer: signer.SignAuthorization,
		Asset: fixture.Requirements.Asset, PayTo: fixture.Requirements.PayTo, MaxAmount: "1000",
	})
	if err != nil {
		t.Fatal(err)
	}
	facilitator := &fixtureFacilitator{config: fixture.Config, mode: mode, kind: types.SupportedKind{
		X402Version: 2, Scheme: "exact", Network: fixture.Config.Network,
		Extra: map[string]interface{}{"supportedAssetTransferMethods": []string{"permit2"}},
	}}
	return server, client, facilitator, fixture.Requirements.PayTo
}

func getChallenge(t testing.TB, handler http.Handler) types.PaymentRequired {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/resource", nil))
	if recorder.Code != http.StatusPaymentRequired {
		t.Fatalf("未付款应返回402，实际%d", recorder.Code)
	}
	data, err := base64.StdEncoding.DecodeString(recorder.Header().Get("PAYMENT-REQUIRED"))
	if err != nil {
		t.Fatal(err)
	}
	var required types.PaymentRequired
	if json.Unmarshal(data, &required) != nil || len(required.Accepts) != 1 || required.Accepts[0].Extra["paymentFlow"] != "upfront" {
		t.Fatal("未生成 upfront TRON 报价")
	}
	return required
}

func TestServerMiddlewareSettlementGate(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"success", "pending", "error", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			mechanism, client, facilitator, payTo := setup(t, mode)
			var calls atomic.Int32
			handler, err := buildHandler(context.Background(), mechanism, facilitator, payTo, "1000", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if !facilitator.settled.Load() {
					t.Error("结算成功前运行业务")
				}
				calls.Add(1)
				_, _ = io.WriteString(w, "protected fixture")
			}))
			if err != nil {
				t.Fatal(err)
			}
			required := getChallenge(t, handler)
			payload, err := client.CreatePaymentPayload(context.Background(), required.Accepts[0], x402.PaymentPayloadContext{})
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/resource", nil)
			req.Header.Set("PAYMENT-SIGNATURE", base64.StdEncoding.EncodeToString(data))
			budget := 5 * time.Second
			if mode == "timeout" {
				budget = 250 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req.WithContext(ctx))
			if facilitator.settles.Load() != 1 {
				t.Fatal("结算次数不为1")
			}
			if mode == "success" {
				if recorder.Code != http.StatusOK || calls.Load() != 1 || recorder.Body.String() != "protected fixture" {
					t.Fatal("模拟成功后未交付一次")
				}
				decoded, err := base64.StdEncoding.DecodeString(recorder.Header().Get("PAYMENT-RESPONSE"))
				var settled x402.SettleResponse
				if err != nil || json.Unmarshal(decoded, &settled) != nil || !settled.Success {
					t.Fatal("未返回模拟结算回执")
				}
			} else if calls.Load() != 0 || recorder.Code < 400 || strings.Contains(recorder.Body.String(), "protected fixture") {
				t.Fatal("结算未成功仍开放资源")
			}
		})
	}
}

func TestServerMiddlewareRejectsBadPayloadAndHEAD(t *testing.T) {
	t.Parallel()
	mechanism, client, facilitator, payTo := setup(t, "success")
	var calls atomic.Int32
	handler, err := buildHandler(context.Background(), mechanism, facilitator, payTo, "1000", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	if err != nil {
		t.Fatal(err)
	}
	required := getChallenge(t, handler)
	payload, err := client.CreatePaymentPayload(context.Background(), required.Accepts[0], x402.PaymentPayloadContext{})
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"invalid-base64", base64.StdEncoding.EncodeToString([]byte(`{}`))} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/resource", nil)
		req.Header.Set("PAYMENT-SIGNATURE", header)
		handler.ServeHTTP(recorder, req)
		if recorder.Code < 400 {
			t.Fatal("接受非法协议载荷")
		}
	}
	if calls.Load() != 0 || facilitator.settles.Load() != 0 {
		t.Fatal("非法协议载荷触发结算或业务")
	}
	payload.Payload["signature"] = "0x" + strings.Repeat("0", 130)
	data, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodGet, "/resource", nil)
	req.Header.Set("PAYMENT-SIGNATURE", base64.StdEncoding.EncodeToString(data))
	handler.ServeHTTP(httptest.NewRecorder(), req)
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodOptions} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(method, "/resource", nil))
		if recorder.Code < 400 {
			t.Fatalf("%s 意外放行", method)
		}
	}
	// upfront 由 Settle 完整校验授权，不先调用 Verify；无效签名须在 facilitator 内拒绝。
	if calls.Load() != 0 || facilitator.settles.Load() != 1 {
		t.Fatal("非法签名或未付费方法请求执行了业务")
	}
}

func TestServerOfficialClientRoundTrip(t *testing.T) {
	t.Parallel()
	mechanism, client, facilitator, payTo := setup(t, "success")
	handler, err := buildHandler(context.Background(), mechanism, facilitator, payTo, "1000", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "fixture") }))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	data, _ := os.ReadFile("../../exact/testdata/tronweb-permit2.json")
	var fixture struct {
		Requirements types.PaymentRequirements `json:"requirements"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{AllowedAssets: []x402.SpendControlAsset{{
		Network: x402.Network(client.Network()), Asset: fixture.Requirements.Asset, MaxAmountPerPayment: "1000",
	}}})).Register(x402.Network(client.Network()), client)
	httpClient := x402http.WrapHTTPClientWithPayment(&http.Client{Timeout: 5 * time.Second}, x402http.Newx402HTTPClient(core))
	resp, err := httpClient.Get(server.URL + "/resource")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK || string(body) != "fixture" || facilitator.settles.Load() != 1 {
		t.Fatal("官方客户端/服务端串联失败")
	}
}

func TestServerFailsInitialization(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"init_error", "unsupported", "v1", "bad_price", "wrong_payto"} {
		t.Run(mode, func(t *testing.T) {
			mechanism, _, facilitator, payTo := setup(t, mode)
			price := "1000"
			if mode == "unsupported" {
				facilitator.kind.Extra = nil
			}
			if mode == "v1" {
				facilitator.kind.X402Version = 1
			}
			if mode == "bad_price" {
				price = "$1"
			}
			if mode == "wrong_payto" {
				payTo = facilitator.config.ProxyAddress
			}
			handler, err := buildHandler(context.Background(), mechanism, facilitator, payTo, price, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("启动失败仍运行业务") }))
			if err == nil || handler != nil {
				t.Fatal("配置/能力错误未阻断初始化")
			}
		})
	}
}

func TestServerUpfrontHandlerFailureDoesNotSettleAgain(t *testing.T) {
	t.Parallel()
	mechanism, client, facilitator, payTo := setup(t, "success")
	handler, err := buildHandler(context.Background(), mechanism, facilitator, payTo, "1000", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !facilitator.settled.Load() {
			t.Error("付款前执行业务")
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if err != nil {
		t.Fatal(err)
	}
	required := getChallenge(t, handler)
	payload, err := client.CreatePaymentPayload(context.Background(), required.Accepts[0], x402.PaymentPayloadContext{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/resource", nil)
	req.Header.Set("PAYMENT-SIGNATURE", base64.StdEncoding.EncodeToString(data))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusInternalServerError || facilitator.settles.Load() != 1 || recorder.Header().Get("PAYMENT-RESPONSE") == "" {
		t.Fatal("业务失败未保留原结算或发生二次扣款")
	}
}

func TestServerRunRejectsConfigurationBeforeListening(t *testing.T) {
	for _, test := range []struct{ key, value string }{
		{"X402_NETWORK", "tron:1"},
		{"X402_ASSET", ""},
		{"X402_PAY_TO", ""},
		{"X402_FACILITATOR_URL", ""},
		{"X402_FACILITATOR_URL", "http://example.invalid"},
		{"X402_FACILITATOR_URL", "https://user:credential@example.invalid"},
		{"X402_FACILITATOR_URL", "https://example.invalid/?token=invalid"},
		{"X402_FACILITATOR_URL", "https://example.invalid/#fragment"},
		{"X402_PRICE_ATOMIC", "$1"},
	} {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			t.Setenv("X402_NETWORK", "tron:3448148188")
			t.Setenv("X402_ASSET", "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf")
			t.Setenv("X402_PAY_TO", "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL")
			t.Setenv("X402_FACILITATOR_URL", "https://example.invalid")
			t.Setenv("X402_PRICE_ATOMIC", "1000")
			t.Setenv(test.key, test.value)
			// 所有坏配置都应在发请求/创建监听前失败；取消上下文同时防止错误实现访问网络。
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := run(ctx); err == nil || errors.Is(err, context.Canceled) {
				t.Fatal("错误配置未在网络初始化之前拒绝")
			}
		})
	}
}
