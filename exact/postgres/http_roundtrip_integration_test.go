package postgres_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/exact/postgres"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/http/nethttp"
)

// 三端均使用公开实现，只有区块链状态和回执是合成夹具，不代表实链付款。
func TestHTTPAutomaticPaymentRoundTripAndOriginalRecovery(t *testing.T) {
	for _, mode := range []string{"success", "simulation_rejected", "pending", "price_policy"} {
		t.Run(mode, func(t *testing.T) {
			fixture := prepareFacilitator(t, 0)
			if mode == "simulation_rejected" {
				fixture.simulationMode.Store(1)
			}
			if mode == "pending" {
				fixture.mode.Store(3)
			}
			facilitator := httpFixtureClient(t, fixture)
			resource, err := exact.NewServer(fixture.config.Policy)
			if err != nil {
				t.Fatal(err)
			}
			price := fixture.input.Requirements.Amount
			if mode == "price_policy" {
				price = "2000"
			}
			core := x402.Newx402ResourceServer(x402.WithFacilitatorClient(facilitator)).Register(x402.Network(resource.Network()), resource)
			server := x402http.Wrappedx402HTTPResourceServer(x402http.RoutesConfig{"GET /resource": {
				Accepts: []x402http.PaymentOption{{Scheme: "exact", Network: x402.Network(resource.Network()),
					PayTo: fixture.config.Policy.PayTo, Price: price, MaxTimeoutSeconds: 60}},
			}}, core)
			if err := server.Initialize(fixture.ctx); err != nil {
				t.Fatal("资源服务能力发现失败", err)
			}
			var deliveries, requests, paidRequests atomic.Int32
			handler := nethttp.PaymentMiddlewareFromHTTPServer(server, nethttp.WithSyncFacilitatorOnStart(false))(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				var state string
				if err := fixture.db.QueryRow(fixture.ctx, "SELECT state FROM x402_tron_settlements").Scan(&state); err != nil || state != string(postgres.StateSucceeded) {
					t.Error("成功落库前执行资源处理器")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				deliveries.Add(1)
				_, _ = io.WriteString(w, "paid-resource")
			}))
			resourceHTTP := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("PAYMENT-SIGNATURE") != "" {
					paidRequests.Add(1)
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(resourceHTTP.Close)

			key, err := secp256k1.GeneratePrivateKey()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(key.Zero)
			encoded := key.Serialize()
			signer, err := exact.NewEOASigner(encoded)
			clear(encoded)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = signer.Close() })
			var authorizationSigns, created atomic.Int32
			mechanism, err := exact.NewClient(exact.ClientConfig{
				Authorization: fixture.input.Config, PayerAddress: signer.Address(), Asset: fixture.config.Policy.Asset,
				PayTo: fixture.config.Policy.PayTo, MaxAmount: "1000",
				Signer: func(ctx context.Context, digest [32]byte) ([]byte, error) {
					authorizationSigns.Add(1)
					return signer.SignAuthorization(ctx, digest)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			clientCore := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{AllowedAssets: []x402.SpendControlAsset{{
				Network: x402.Network(mechanism.Network()), Asset: fixture.config.Policy.Asset, MaxAmountPerPayment: "1000",
			}}})).Register(x402.Network(mechanism.Network()), mechanism)
			var original []byte
			clientCore.OnAfterPaymentCreation(func(payment x402.PaymentCreatedContext) error {
				created.Add(1)
				// 此时还未发出付费重试，也未查询节点；仅设置合成节点对应的付款人/nonce/事件预期。
				// 不更改客户端载荷或签名，后续由真实机制独立验签并绑定固定报价。
				data, err := json.Marshal(payment.Payload.GetPayload())
				var permit exact.Permit2Payload
				if err != nil || json.Unmarshal(data, &permit) != nil {
					t.Error("无法读取真实客户端授权")
					return nil
				}
				fixture.input.Authorization, fixture.input.Signature = permit.Authorization, permit.Signature
				original, err = json.Marshal(payment.Payload)
				if err != nil {
					t.Error("无法保存本轮内存恢复请求")
				}
				return nil
			})
			base := resourceHTTP.Client()
			base.Timeout = 10 * time.Second
			base.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			client := x402http.WrapHTTPClientWithPayment(base, x402http.Newx402HTTPClient(clientCore))
			request, err := http.NewRequestWithContext(fixture.ctx, http.MethodGet, resourceHTTP.URL+"/resource", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if mode == "price_policy" {
				if response != nil {
					_ = response.Body.Close()
				}
				if err == nil || authorizationSigns.Load() != 0 || created.Load() != 0 || requests.Load() != 1 || paidRequests.Load() != 0 || fixture.jsonRPCs.Load() != 0 {
					t.Fatal("超额报价未在签名和结算前拒绝")
				}
				assertRoundTripNoPayment(t, fixture, deliveries.Load())
				return
			}
			if err != nil {
				t.Fatal("官方客户端自动付款握手失败")
			}
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10))
			_ = response.Body.Close()
			if readErr != nil || requests.Load() != 2 || paidRequests.Load() != 1 || authorizationSigns.Load() != 1 || created.Load() != 1 || len(original) == 0 {
				t.Fatal("未通过真实402报价生成唯一授权并自动重试一次")
			}
			if mode == "simulation_rejected" {
				failed := roundTripReceipt(t, response)
				if response.StatusCode < 400 || failed.Success || failed.ErrorReason != "settlement_simulation_failed" || failed.Transaction != "" || strings.Contains(string(body), "paid-resource") || strings.Contains(string(body), "synthetic-sensitive") || fixture.simulations.Load() != 1 {
					t.Fatal("模拟拒绝被当成成功或泄漏节点错误")
				}
				assertRoundTripNoPayment(t, fixture, deliveries.Load())
				return
			}
			if mode == "pending" {
				failed := roundTripReceipt(t, response)
				if response.StatusCode < 400 || failed.Success || failed.ErrorReason != x402.ErrSettlementPending || failed.Transaction == "" || deliveries.Load() != 0 || strings.Contains(string(body), "paid-resource") {
					t.Fatal("待确认请求执行了资源处理器")
				}
				var state, id string
				if err := fixture.db.QueryRow(fixture.ctx, "SELECT state, transaction_id FROM x402_tron_settlements").Scan(&state, &id); err != nil || state != string(postgres.StatePending) || id != failed.Transaction || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
					t.Fatal("待确认未保留唯一原交易", err)
				}
				fixture.mode.Store(1)
			} else {
				assertRoundTripSettled(t, fixture, response, body, deliveries.Load(), 1)
			}
			// 未知结果只能重发同一授权；不能再次调用自动付款客户端生成新 nonce。
			replay, err := http.NewRequestWithContext(fixture.ctx, http.MethodGet, resourceHTTP.URL+"/resource", nil)
			if err != nil {
				t.Fatal(err)
			}
			replay.Header.Set("PAYMENT-SIGNATURE", base64.StdEncoding.EncodeToString(original))
			response, err = base.Do(replay)
			if err != nil {
				t.Fatal("原授权恢复请求失败")
			}
			body, readErr = io.ReadAll(io.LimitReader(response.Body, 16<<10))
			_ = response.Body.Close()
			wantDeliveries := int32(2)
			if mode == "pending" {
				wantDeliveries = 1
			}
			if readErr != nil || authorizationSigns.Load() != 1 || created.Load() != 1 || requests.Load() != 3 || paidRequests.Load() != 2 {
				t.Fatal("恢复原付款时意外生成新授权或增加HTTP重试")
			}
			assertRoundTripSettled(t, fixture, response, body, deliveries.Load(), wantDeliveries)
			// 已成功付款由日志恢复；节点完全不可用也不能重新付款。
			fixture.server.Close()
			response, err = base.Do(replay.Clone(fixture.ctx))
			if err != nil {
				t.Fatal("已成功付款无法恢复")
			}
			body, readErr = io.ReadAll(io.LimitReader(response.Body, 16<<10))
			_ = response.Body.Close()
			if readErr != nil {
				t.Fatal("读取成功恢复结果失败")
			}
			assertRoundTripSettled(t, fixture, response, body, deliveries.Load(), wantDeliveries+1)
		})
	}
}

func assertRoundTripNoPayment(t *testing.T, fixture *facilitatorTest, deliveries int32) {
	t.Helper()
	var count int
	if err := fixture.db.QueryRow(fixture.ctx, "SELECT count(*) FROM x402_tron_settlements").Scan(&count); err != nil || count != 0 || deliveries != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatal("拒绝请求仍占用日志、签名、广播或交付资源", err)
	}
}

func assertRoundTripSettled(t *testing.T, fixture *facilitatorTest, response *http.Response, body []byte, deliveries, wantDeliveries int32) {
	t.Helper()
	settled := roundTripReceipt(t, response)
	if !settled.Success || settled.ErrorReason != "" || response.StatusCode != http.StatusOK || string(body) != "paid-resource" || deliveries != wantDeliveries {
		t.Fatal("资源交付或结算响应不符")
	}
	var state, id, payer string
	var signed []byte
	var block uint64
	var count int
	if err := fixture.db.QueryRow(fixture.ctx, "SELECT state, transaction_id, payer, signed_payload, confirmed_block_number, count(*) OVER () FROM x402_tron_settlements").Scan(&state, &id, &payer, &signed, &block, &count); err != nil {
		t.Fatal("读取付款审计失败")
	}
	fixture.mu.Lock()
	originalID, originalSigned := fixture.signed.ID, append([]byte(nil), fixture.signed.Payload...)
	fixture.mu.Unlock()
	if count != 1 || state != string(postgres.StateSucceeded) || id != originalID || id != settled.Transaction || !bytes.Equal(signed, originalSigned) || block != facilitatorHeight+1 || settled.Payer != payer || settled.Amount != fixture.input.Requirements.Amount || string(settled.Network) != fixture.config.Policy.Network || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 || fixture.simulations.Load() != 1 {
		t.Fatal("未保存同一交易/精确结算结果，或恢复时发生重复付款")
	}
}

func roundTripReceipt(t *testing.T, response *http.Response) x402.SettleResponse {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(response.Header.Get("PAYMENT-RESPONSE"))
	var receipt x402.SettleResponse
	if err != nil || json.Unmarshal(data, &receipt) != nil {
		t.Fatal("缺少可解析的结算响应头")
	}
	return receipt
}
