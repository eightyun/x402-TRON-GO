package postgres_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/exact/postgres"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"
)

const httpTestToken = "test-only-not-a-deployment-token-123456789"

type testHTTPAuth struct{}

func (testHTTPAuth) GetAuthHeaders(context.Context) (x402http.AuthHeaders, error) {
	header := map[string]string{"Authorization": "Bearer " + httpTestToken}
	return x402http.AuthHeaders{Supported: header, Verify: header, Settle: header}, nil
}

func httpFixtureClient(t *testing.T, fixture *facilitatorTest) *x402http.HTTPFacilitatorClient {
	t.Helper()
	handler, err := postgres.NewHTTPHandler(fixture.mechanism(t), postgres.HTTPConfig{BearerToken: httpTestToken})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	client := server.Client()
	client.Timeout = 10 * time.Second
	return x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{URL: server.URL, HTTPClient: client, AuthProvider: testHTTPAuth{}})
}

func TestHTTPOfficialClientSettlementAndCachedRecovery(t *testing.T) {
	fixture := prepareFacilitator(t, 0)
	client := httpFixtureClient(t, fixture)
	kinds, err := client.GetSupported(fixture.ctx)
	if err != nil || len(kinds.Kinds) != 1 || kinds.Kinds[0].Network != fixture.input.Config.Network {
		t.Fatal("官方 HTTP 能力/鉴权失败", err)
	}
	payload, _ := json.Marshal(envelope(t, fixture.input))
	requirements, _ := json.Marshal(fixture.input.Requirements)
	verified, err := client.Verify(fixture.ctx, payload, requirements)
	if err != nil || !verified.IsValid || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatal("HTTP Verify 失败或产生付款副作用", err)
	}
	fixture.mode.Store(1)
	settled, err := client.Settle(fixture.ctx, payload, requirements)
	if err != nil || !settled.Success || settled.Transaction == "" {
		t.Fatal("HTTP 未能取得真实机制核验成功", err)
	}
	fixture.server.Close()
	recovered, err := client.Settle(fixture.ctx, payload, requirements)
	if err != nil || !recovered.Success || recovered.Transaction != settled.Transaction || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatal("HTTP 成功重试没有恢复原付款", err)
	}
}

func TestHTTPPendingErrorPreservesTransactionForOfficialClient(t *testing.T) {
	fixture := prepareFacilitator(t, 0)
	client := httpFixtureClient(t, fixture)
	fixture.mode.Store(3)
	payload, _ := json.Marshal(envelope(t, fixture.input))
	requirements, _ := json.Marshal(fixture.input.Requirements)
	result, err := client.Settle(fixture.ctx, payload, requirements)
	var pending *x402.SettleError
	if result != nil || !errors.As(err, &pending) || pending.ErrorReason != x402.ErrSettlementPending || pending.Transaction == "" || string(pending.Network) != fixture.input.Config.Network {
		t.Fatal("原 txid/pending 未经 HTTP 保留", err)
	}
	fixture.mode.Store(1)
	result, err = client.Settle(fixture.ctx, payload, requirements)
	if err != nil || !result.Success || result.Transaction != pending.Transaction || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatal("pending 重试发生替换/重签/重播", err)
	}
}

func TestHTTPProtocolErrorsDoNotExposeUnderlyingFailures(t *testing.T) {
	fixture := prepareFacilitator(t, 0)
	fixture.config.Signer = func(context.Context, [32]byte) ([]byte, error) {
		return nil, errors.New("synthetic-sensitive-signer-detail")
	}
	client := httpFixtureClient(t, fixture)
	payload, _ := json.Marshal(envelope(t, fixture.input))
	bad := fixture.input.Requirements
	bad.PayTo = fixture.input.Config.ProxyAddress
	requirements, _ := json.Marshal(bad)
	var invalid *x402.VerifyError
	if result, err := client.Verify(fixture.ctx, payload, requirements); result != nil || !errors.As(err, &invalid) || invalid.InvalidReason == "" {
		t.Fatal("Verify 协议错误未保留类型")
	}
	requirements, _ = json.Marshal(fixture.input.Requirements)
	var failed *x402.SettleError
	if result, err := client.Settle(fixture.ctx, payload, requirements); result != nil || !errors.As(err, &failed) || failed.ErrorReason == "" || strings.Contains(err.Error(), "synthetic-sensitive") {
		t.Fatal("Settle 错误丢失类型或泄漏原文")
	}
	if fixture.writes.Load() != 0 {
		t.Fatal("签名错误仍广播")
	}
}

func TestHTTPResourceServerGateWithRealMechanism(t *testing.T) {
	fixture := prepareFacilitator(t, 0)
	client := httpFixtureClient(t, fixture)
	mechanism, err := exact.NewServer(fixture.config.Policy)
	if err != nil {
		t.Fatal(err)
	}
	core := x402.Newx402ResourceServer(x402.WithFacilitatorClient(client)).Register(x402.Network(mechanism.Network()), mechanism)
	server := x402http.Wrappedx402HTTPResourceServer(x402http.RoutesConfig{"GET /resource": {
		Accepts: []x402http.PaymentOption{{Scheme: "exact", Network: x402.Network(mechanism.Network()),
			PayTo: fixture.input.Requirements.PayTo, Price: fixture.input.Requirements.Amount, MaxTimeoutSeconds: fixture.input.Requirements.MaxTimeoutSeconds}},
	}}, core)
	if err := server.Initialize(fixture.ctx); err != nil {
		t.Fatal("资源服务无法发现真实 HTTP facilitator", err)
	}
	calls := 0
	handler := nethttp.PaymentMiddlewareFromHTTPServer(server, nethttp.WithSyncFacilitatorOnStart(false))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		var state string
		if err := fixture.db.QueryRow(fixture.ctx, "SELECT state FROM x402_tron_settlements").Scan(&state); err != nil || state != string(postgres.StateSucceeded) {
			t.Error("成功落库前交付资源")
		}
		_, _ = io.WriteString(w, "paid-resource")
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/resource", nil))
	if response.Code != http.StatusPaymentRequired || calls != 0 || fixture.signs.Load() != 0 {
		t.Fatal("未支付请求越过真实付款闸门")
	}
	payload, _ := json.Marshal(envelope(t, fixture.input))
	request := func() *http.Request {
		req := httptest.NewRequest("GET", "/resource", nil)
		req.Header.Set("PAYMENT-SIGNATURE", base64.StdEncoding.EncodeToString(payload))
		return req
	}
	fixture.mode.Store(3)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request())
	if response.Code < 400 || calls != 0 || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatal("真实 HTTP pending 被当成成功或官方重试再次付款")
	}
	fixture.mode.Store(1)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request())
	if response.Code != http.StatusOK || calls != 1 || response.Body.String() != "paid-resource" || response.Header().Get("PAYMENT-RESPONSE") == "" || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatal("恢复原交易后未交付或重复付款")
	}
}

func TestHTTPRejectsRequestsBeforeSideEffects(t *testing.T) {
	fixture := prepareFacilitator(t, 0)
	mechanism := fixture.mechanism(t)
	for _, config := range []postgres.HTTPConfig{{}, {BearerToken: "short"}, {BearerToken: strings.Repeat("x", 257)}, {BearerToken: strings.Repeat("x", 32) + "\n"}, {BearerToken: httpTestToken, MaxConcurrent: -1}, {BearerToken: httpTestToken, MaxConcurrent: 65}} {
		if handler, err := postgres.NewHTTPHandler(mechanism, config); !errors.Is(err, postgres.ErrHTTPConfig) || handler != nil {
			t.Fatal("错误 HTTP 配置放行")
		}
	}
	for _, mechanism := range []*postgres.Facilitator{nil, {}} {
		if _, err := postgres.NewHTTPHandler(mechanism, postgres.HTTPConfig{BearerToken: httpTestToken}); !errors.Is(err, postgres.ErrHTTPConfig) {
			t.Fatal("无效机制放行")
		}
	}
	handler, err := postgres.NewHTTPHandler(mechanism, postgres.HTTPConfig{BearerToken: httpTestToken})
	if err != nil {
		t.Fatal(err)
	}
	valid, _ := json.Marshal(map[string]interface{}{"x402Version": 2, "paymentPayload": envelope(t, fixture.input), "paymentRequirements": fixture.input.Requirements})
	for _, test := range []struct {
		name, method, path, body, auth, media string
		status                                int
	}{
		{"no_auth", "POST", "/settle", string(valid), "", "application/json", 401},
		{"wrong_auth", "GET", "/supported", "", "Bearer wrong", "", 401},
		{"head", "HEAD", "/supported", "", "Bearer " + httpTestToken, "", 405},
		{"get_settle", "GET", "/settle", "", "Bearer " + httpTestToken, "", 405},
		{"path", "POST", "/other", "", "Bearer " + httpTestToken, "", 404},
		{"query", "GET", "/supported?token=ignored", "", "Bearer " + httpTestToken, "", 400},
		{"media", "POST", "/settle", string(valid), "Bearer " + httpTestToken, "text/plain", 415},
		{"large", "POST", "/settle", strings.Repeat(" ", 65537), "Bearer " + httpTestToken, "application/json", 413},
		{"trailing", "POST", "/settle", string(valid) + `{}`, "Bearer " + httpTestToken, "application/json", 400},
		{"duplicate", "POST", "/settle", strings.Replace(string(valid), `"x402Version":2`, `"x402Version":1,"x402Version":2`, 1), "Bearer " + httpTestToken, "application/json", 400},
		{"case_duplicate", "POST", "/settle", strings.Replace(string(valid), `"amount":"1000"`, `"amount":"1","Amount":"1000"`, 1), "Bearer " + httpTestToken, "application/json", 400},
		{"nested_duplicate", "POST", "/settle", strings.Replace(string(valid), `"signature":`, `"signature":"bad","signature":`, 1), "Bearer " + httpTestToken, "application/json", 400},
		{"unknown", "POST", "/settle", `{"other":1,` + string(valid)[1:], "Bearer " + httpTestToken, "application/json", 400},
		{"version", "POST", "/settle", strings.ReplaceAll(string(valid), `"x402Version":2`, `"x402Version":1`), "Bearer " + httpTestToken, "application/json", 400},
		{"depth", "POST", "/settle", strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34), "Bearer " + httpTestToken, "application/json", 400},
		{"syntax", "POST", "/settle", `{"x402Version":`, "Bearer " + httpTestToken, "application/json", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.auth != "" {
				request.Header.Set("Authorization", test.auth)
			}
			request.Header.Set("Content-Type", test.media)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.status || strings.Contains(response.Body.String(), fixture.input.Signature) || strings.Contains(response.Body.String(), httpTestToken) {
				t.Fatalf("拒绝状态错误或响应泄密: %d", response.Code)
			}
		})
	}
	for _, header := range []string{"Authorization", "Content-Encoding"} {
		request := httptest.NewRequest("POST", "/settle", bytes.NewReader(valid))
		request.Header.Set("Authorization", "Bearer "+httpTestToken)
		request.Header.Set("Content-Type", "application/json")
		status := http.StatusUnsupportedMediaType
		if header == "Authorization" {
			request.Header.Add(header, "Bearer "+httpTestToken)
			status = http.StatusUnauthorized
		} else {
			request.Header.Set(header, "gzip")
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != status {
			t.Fatal("歧义认证或编码头未拒绝")
		}
	}
	if fixture.jsonRPCs.Load() != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatal("无效请求触发节点或资金副作用")
	}
}

func TestHTTPLimitsConcurrentExecution(t *testing.T) {
	fixture := prepareFacilitator(t, 0)
	fixture.mode.Store(0)
	handler, err := postgres.NewHTTPHandler(fixture.mechanism(t), postgres.HTTPConfig{BearerToken: httpTestToken, MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	fixture.onMissing = func() { once.Do(func() { close(entered) }); <-release }
	server := httptest.NewServer(handler)
	defer server.Close()
	// 清理时先释放节点，再关闭服务器，避免失败路径遗留阻塞请求。
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	body, _ := json.Marshal(map[string]interface{}{"x402Version": 2, "paymentPayload": envelope(t, fixture.input), "paymentRequirements": fixture.input.Requirements})
	request := func() (*http.Response, error) {
		req, _ := http.NewRequestWithContext(fixture.ctx, "POST", server.URL+"/settle", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+httpTestToken)
		req.Header.Set("Content-Type", "application/json")
		return server.Client().Do(req)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		response, err := request()
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-fixture.ctx.Done():
		t.Fatal("首次结算未到达等待点")
	}
	response, err := request()
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatal("超容量请求未被无排队拒绝")
	}
	fixture.mode.Store(1)
	unblock()
	select {
	case <-done:
	case <-fixture.ctx.Done():
		t.Fatal("首请求未退出")
	}
}
