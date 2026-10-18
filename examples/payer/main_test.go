package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

func fixtureConfig(t *testing.T) (configuration, *exact.EOASigner) {
	t.Helper()
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer key.Zero()
	b := key.Serialize()
	defer clear(b)
	signer, err := exact.NewEOASigner(b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = signer.Close() })
	keyFile := filepath.Join(t.TempDir(), "payer.key")
	encoded := []byte(hex.EncodeToString(b))
	defer clear(encoded)
	if os.WriteFile(keyFile, encoded, 0600) != nil {
		t.Fatal("测试密钥保存失败")
	}
	stateDirectory := t.TempDir()
	if os.Chmod(stateDirectory, 0700) != nil {
		t.Fatal("临时状态目录权限设置失败")
	}
	return configuration{policy: policy{Network: tron.NetworkNile, Permit2: "TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h", Proxy: "TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F", Payer: signer.Address(), Asset: "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf", PayTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL", MaxAmount: "1000"}, stateDirectory: stateDirectory, keyFile: keyFile, enabled: true}, signer
}

func TestPayAndRecoverPreserveOriginalAuthorization(t *testing.T) {
	for _, mode := range []string{"success", "pending", "unknown", "overprice", "wrong_receipt", "oversized_body"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := fixtureConfig(t)
			var recovered atomic.Bool
			var requests, paid atomic.Int32
			var mu sync.Mutex
			original := ""
			v, _ := exact.NewOfflineVerifier(c.authorization())
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				h := r.Header.Get("PAYMENT-SIGNATURE")
				if h == "" {
					amount := "1000"
					if mode == "overprice" {
						amount = "1001"
					}
					b, _ := json.Marshal(types.PaymentRequired{X402Version: 2, Accepts: []types.PaymentRequirements{{Scheme: "exact", Network: c.Network, Asset: c.Asset, PayTo: c.PayTo, Amount: amount, MaxTimeoutSeconds: 60, Extra: map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"}}}})
					w.Header().Set("PAYMENT-REQUIRED", base64.StdEncoding.EncodeToString(b))
					w.WriteHeader(402)
					return
				}
				paid.Add(1)
				saved, e := readState(c.stateDirectory, "payment-signature", 16<<10)
				if e != nil || string(saved) != h {
					t.Error("付费请求先于授权持久化")
					w.WriteHeader(500)
					return
				}
				mu.Lock()
				if original == "" {
					original = h
				} else if original != h {
					t.Error("恢复生成了不同授权")
				}
				mu.Unlock()
				b, e := base64.StdEncoding.DecodeString(h)
				var p types.PaymentPayload
				if e != nil || json.Unmarshal(b, &p) != nil {
					t.Error("授权无法解析")
					w.WriteHeader(500)
					return
				}
				if _, e := v.VerifyAuthorizationBinding(p, p.Accepted); e != nil {
					t.Error("签名/报价绑定无效")
					w.WriteHeader(500)
					return
				}
				if mode == "unknown" && !recovered.Load() {
					connection, _, e := w.(http.Hijacker).Hijack()
					if e == nil {
						_ = connection.Close()
					}
					return
				}
				receipt := x402.SettleResponse{Success: true, Network: x402.Network(c.Network), Transaction: strings.Repeat("a", 64), Payer: c.Payer, Amount: "1000"}
				status := 200
				if mode == "pending" && !recovered.Load() {
					receipt.Success = false
					receipt.ErrorReason = "settlement_pending"
					status = 500
				}
				if mode == "wrong_receipt" && !recovered.Load() {
					receipt.Amount = "2000"
				}
				b, _ = json.Marshal(receipt)
				w.Header().Set("PAYMENT-RESPONSE", base64.StdEncoding.EncodeToString(b))
				w.WriteHeader(status)
				if mode == "oversized_body" && !recovered.Load() {
					_, _ = io.WriteString(w, strings.Repeat("b", (1<<20)+1))
				} else {
					_, _ = io.WriteString(w, "private paid content")
				}
			}))
			defer server.Close()
			c.URL = server.URL
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var output bytes.Buffer
			err := run(ctx, c, "pay", server.Client(), &output)
			if mode == "success" {
				if err != nil {
					t.Fatal("实客户端握手失败", err)
				}
			} else if !errors.Is(err, errPayment) {
				t.Fatal("失败/未知结果不能视为成功", err)
			}
			if strings.Contains(output.String(), "private paid content") || strings.Contains(output.String(), "permit2Authorization") || strings.Contains(output.String(), "signature") {
				t.Fatal("输出泄漏私有资源或授权")
			}
			before := requests.Load()
			if err := run(ctx, c, "pay", server.Client(), io.Discard); !errors.Is(err, errState) || requests.Load() != before {
				t.Fatal("重复pay生成了新请求/授权")
			}
			if mode == "overprice" {
				if paid.Load() != 0 {
					t.Fatal("超额报价仍发送付款")
				}
				if _, e := os.Stat(filepath.Join(c.stateDirectory, "authorization-reserved")); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("超额报价仍签名")
				}
				return
			}
			header, e := readState(c.stateDirectory, "payment-signature", 16<<10)
			if e != nil {
				t.Fatal(e)
			}
			changed := c
			changed.PayTo = c.Asset
			if err := run(ctx, changed, "recover", server.Client(), io.Discard); !errors.Is(err, errState) || requests.Load() != before {
				t.Fatal("不同策略可恢复原授权")
			}
			recovered.Store(true)
			c.keyFile = "/does-not-exist-and-must-not-be-loaded"
			if err := run(ctx, c, "recover", server.Client(), io.Discard); err != nil {
				t.Fatal("原授权无法在不加载密钥时恢复", err)
			}
			again, e := readState(c.stateDirectory, "payment-signature", 16<<10)
			if e != nil || !bytes.Equal(header, again) {
				t.Fatal("恢复替换了原授权")
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSavedPaymentTransportFailureNeverForwards(t *testing.T) {
	c, s := fixtureConfig(t)
	c.URL = "https://resource.invalid/resource"
	m, e := exact.NewClient(exact.ClientConfig{Authorization: c.authorization(), PayerAddress: c.Payer, Asset: c.Asset, PayTo: c.PayTo, MaxAmount: c.MaxAmount, Signer: s.SignAuthorization})
	if e != nil {
		t.Fatal(e)
	}
	p, e := m.CreatePaymentPayload(context.Background(), types.PaymentRequirements{Scheme: "exact", Network: c.Network, Asset: c.Asset, PayTo: c.PayTo, Amount: "1000", MaxTimeoutSeconds: 60, Extra: map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"}}, x402.PaymentPayloadContext{})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(p)
	header := base64.StdEncoding.EncodeToString(b)
	if writeState(c.stateDirectory, "payment-signature", []byte("existing")) != nil {
		t.Fatal("构造原文件失败")
	}
	calls := 0
	tr := &savedPaymentTransport{config: c, base: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return nil, nil })}
	r, _ := http.NewRequest(http.MethodGet, c.URL, nil)
	r.Header.Set("PAYMENT-SIGNATURE", header)
	if _, e := tr.RoundTrip(r); !errors.Is(e, errState) || calls != 0 {
		t.Fatal("落盘失败仍发送")
	}
}

func TestRecoverExpiredAuthorizationWithoutKeyOrNewNonce(t *testing.T) {
	c, s := fixtureConfig(t)
	requirements := types.PaymentRequirements{Scheme: "exact", Network: c.Network, Asset: c.Asset, PayTo: c.PayTo, Amount: "1000", MaxTimeoutSeconds: 60, Extra: map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"}}
	m, e := exact.NewClient(exact.ClientConfig{Authorization: c.authorization(), PayerAddress: c.Payer, Asset: c.Asset, PayTo: c.PayTo, MaxAmount: c.MaxAmount, Signer: s.SignAuthorization})
	if e != nil {
		t.Fatal(e)
	}
	p, e := m.CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(p.Payload)
	var permit exact.Permit2Payload
	if json.Unmarshal(b, &permit) != nil {
		t.Fatal("测试授权解析失败")
	}
	// 仅用临时测试密钥生成历史授权，不伪造生产校验时钟。
	permit.Authorization.Deadline = strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	v, _ := exact.NewOfflineVerifier(c.authorization())
	digest, e := v.Digest(permit.Authorization)
	if e != nil {
		t.Fatal(e)
	}
	signature, e := s.SignAuthorization(context.Background(), digest)
	if e != nil {
		t.Fatal(e)
	}
	permit.Signature = "0x" + hex.EncodeToString(signature)
	b, _ = json.Marshal(permit)
	if json.Unmarshal(b, &p.Payload) != nil {
		t.Fatal("测试payload失败")
	}
	b, _ = json.Marshal(p)
	header := base64.StdEncoding.EncodeToString(b)
	if _, e := checkedPayment(c, header, true); e == nil {
		t.Fatal("历史授权被新付款验证放行")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PAYMENT-SIGNATURE") != header {
			t.Error("没有原样恢复历史授权")
			w.WriteHeader(500)
			return
		}
		receipt, _ := json.Marshal(x402.SettleResponse{Success: true, Network: x402.Network(c.Network), Transaction: strings.Repeat("b", 64), Payer: c.Payer, Amount: "1000"})
		w.Header().Set("PAYMENT-RESPONSE", base64.StdEncoding.EncodeToString(receipt))
		_, _ = io.WriteString(w, "cached resource")
	}))
	defer server.Close()
	c.URL = server.URL
	c.keyFile = "/key-must-not-load"
	b, _ = json.Marshal(c.policy)
	if writeState(c.stateDirectory, "intent.json", b) != nil || writeState(c.stateDirectory, "payment-signature", []byte(header)) != nil {
		t.Fatal("原状态构造失败")
	}
	if e := run(context.Background(), c, "recover", server.Client(), io.Discard); e != nil {
		t.Fatal("历史已结算授权不能恢复", e)
	}
	if _, e := os.Stat(filepath.Join(c.stateDirectory, "authorization-reserved")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("恢复生成了新授权")
	}
}
