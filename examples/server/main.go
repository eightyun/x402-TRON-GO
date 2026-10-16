// 服务端示例通过官方 net/http 中间件接入可信 facilitator；启动前必须成功完成配置和能力校验。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		// 不输出可能含签名、URL 凭据或上游响应的错误原文。
		_, _ = fmt.Fprintln(os.Stderr, "资源服务启动或运行失败；请核对可信配置与 facilitator 状态。")
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	network := os.Getenv("X402_NETWORK")
	if network == "" {
		network = tron.NetworkNile
	}
	payTo := os.Getenv("X402_PAY_TO")
	mechanism, err := exact.NewServer(exact.ServerConfig{
		Network: network, Asset: os.Getenv("X402_ASSET"), PayTo: payTo,
	})
	if err != nil {
		return err
	}
	facilitatorURL := os.Getenv("X402_FACILITATOR_URL")
	endpoint, err := url.Parse(facilitatorURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
		return errors.New("必须显式配置可信 HTTPS facilitator URL，且不含凭据、query 或 fragment")
	}
	facilitator := x402http.NewHTTPFacilitatorClient(&x402http.FacilitatorConfig{
		URL:        facilitatorURL,
		HTTPClient: &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	handler, err := buildHandler(startup, mechanism, facilitator, payTo, os.Getenv("X402_PRICE_ATOMIC"), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// upfront 已通过可信 facilitator 结算；真实应用仍须防止同一付款重复履约。
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resource":"paid resource"}`))
	}))
	cancel()
	if err != nil {
		return err
	}
	address := os.Getenv("X402_LISTEN_ADDR")
	if address == "" {
		address = "127.0.0.1:8080"
	}
	server := &http.Server{
		Addr: address, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 35 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
	}
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer done()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

func buildHandler(ctx context.Context, mechanism *exact.Server, facilitator x402.FacilitatorClient, payTo, price string, resource http.Handler) (http.Handler, error) {
	if mechanism == nil || facilitator == nil || resource == nil {
		return nil, errors.New("资源服务依赖缺失")
	}
	if _, err := mechanism.ParsePrice(price, x402.Network(mechanism.Network())); err != nil {
		return nil, err
	}
	routes := x402http.RoutesConfig{"GET /resource": {Accepts: []x402http.PaymentOption{{
		Scheme: "exact", Network: x402.Network(mechanism.Network()), PayTo: payTo, Price: price, MaxTimeoutSeconds: 60,
	}}}}
	core := x402.Newx402ResourceServer(x402.WithFacilitatorClient(facilitator)).Register(x402.Network(mechanism.Network()), mechanism)
	httpServer := x402http.Wrappedx402HTTPResourceServer(routes, core)
	// 显式检查错误；不使用初始化失败后仅记录日志并继续的便捷启动方式。
	if err := httpServer.Initialize(ctx); err != nil {
		return nil, err
	}
	if _, err := httpServer.BuildPaymentRequirementsFromOptions(ctx, routes["GET /resource"].Accepts, x402http.HTTPRequestContext{}); err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /resource", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// net/http 的 GET 路由也匹配 HEAD；禁止 HEAD 绕过按 GET 配置的付款闸门。
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		resource.ServeHTTP(w, r)
	}))
	return nethttp.PaymentMiddlewareFromHTTPServer(httpServer, nethttp.WithSyncFacilitatorOnStart(false), nethttp.WithTimeout(25*time.Second))(mux), nil
}
