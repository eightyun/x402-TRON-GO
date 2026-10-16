package exact_test

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/types"
)

func clientConfigFor(t testing.TB) (exact.ClientConfig, types.PaymentRequirements) {
	t.Helper()
	input := loadVector(t)
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer key.Zero()
	encoded := key.Serialize()
	defer clear(encoded)
	signer, err := exact.NewEOASigner(encoded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = signer.Close() })
	return exact.ClientConfig{
		Authorization: input.Config, PayerAddress: signer.Address(), Asset: input.Requirements.Asset,
		PayTo: input.Requirements.PayTo, MaxAmount: "1000", Signer: signer.SignAuthorization,
	}, input.Requirements
}

func clientFor(t testing.TB, config exact.ClientConfig) *exact.Client {
	t.Helper()
	client, err := exact.NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestClientCreatesVerifiedPayload(t *testing.T) {
	t.Parallel()
	config, requirements := clientConfigFor(t)
	client := clientFor(t, config)
	if client.Scheme() != "exact" || client.Network() != tron.NetworkNile {
		t.Fatal("注册信息不一致")
	}
	for _, timeout := range []int{30, 600} {
		requirements.MaxTimeoutSeconds = timeout
		before := time.Now().Unix()
		payload, err := client.CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{MaxAmountPerPayment: "1000"})
		if err != nil {
			t.Fatal(err)
		}
		verified, err := verifierFor(t, config.Authorization).VerifyAuthorization(payload, requirements, time.Now())
		if err != nil || verified.Payer != config.PayerAddress {
			t.Fatalf("本地客户端授权未通过校验: %v", err)
		}
		auth := payload.Payload["permit2Authorization"].(map[string]interface{})
		nonce, err := hex.DecodeString(strings.TrimPrefix(auth["nonce"].(string), "0x"))
		if err != nil || len(nonce) != 32 {
			t.Fatal("nonce 不是 32 字节随机值")
		}
		deadline, err := strconv.ParseInt(auth["deadline"].(string), 10, 64)
		if err != nil || deadline < before+int64(min(timeout, 60)) || deadline > time.Now().Unix()+int64(min(timeout, 60)) {
			t.Fatal("未应用本地和服务端有效期上限")
		}
		requirements.Extra["assetTransferMethod"] = "changed"
		if payload.Accepted.Extra["assetTransferMethod"] != "permit2" {
			t.Fatal("返回载荷与输入共享 extra")
		}
		requirements.Extra["assetTransferMethod"] = "permit2"
		data, err := json.Marshal(payload)
		var decoded types.PaymentPayload
		if err != nil || json.Unmarshal(data, &decoded) != nil {
			t.Fatal("协议 JSON 往返失败")
		}
		if _, err := verifierFor(t, config.Authorization).VerifyAuthorization(decoded, requirements, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClientRejectsUnsafeQuotesBeforeSigning(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		edit func(*types.PaymentRequirements, *x402.PaymentPayloadContext)
		want error
	}{
		{"network", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.Network = tron.NetworkMainnet }, exact.ErrInvalidRequirements},
		{"scheme", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.Scheme = "upto" }, exact.ErrInvalidRequirements},
		{"asset", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.Asset = r.PayTo }, exact.ErrClientPolicy},
		{"recipient", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.PayTo = r.Asset }, exact.ErrClientPolicy},
		{"amount", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.Amount = "1001" }, exact.ErrClientPolicy},
		{"zero", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.Amount = "0" }, exact.ErrInvalidRequirements},
		{"noncanonical", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.Amount = "0100" }, exact.ErrInvalidRequirements},
		{"short", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.MaxTimeoutSeconds = 5 }, exact.ErrInvalidValidity},
		{"zero_timeout", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.MaxTimeoutSeconds = 0 }, exact.ErrInvalidRequirements},
		{"missing_extra", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.Extra = nil }, exact.ErrInvalidRequirements},
		{"other_extra", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) { r.Extra["spender"] = r.PayTo }, exact.ErrInvalidRequirements},
		{"transfer_method", func(r *types.PaymentRequirements, _ *x402.PaymentPayloadContext) {
			r.Extra["assetTransferMethod"] = "eip3009"
		}, exact.ErrInvalidRequirements},
		{"extensions", func(_ *types.PaymentRequirements, p *x402.PaymentPayloadContext) {
			p.Extensions = map[string]interface{}{"unknown": true}
		}, exact.ErrInvalidRequirements},
		{"sdk_cap", func(_ *types.PaymentRequirements, p *x402.PaymentPayloadContext) { p.MaxAmountPerPayment = "999" }, exact.ErrClientPolicy},
		{"sdk_zero", func(_ *types.PaymentRequirements, p *x402.PaymentPayloadContext) { p.MaxAmountPerPayment = "0" }, exact.ErrClientPolicy},
		{"sdk_invalid", func(_ *types.PaymentRequirements, p *x402.PaymentPayloadContext) { p.MaxAmountPerPayment = "$1" }, exact.ErrClientPolicy},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			config, requirements := clientConfigFor(t)
			var called atomic.Int32
			config.Signer = func(context.Context, [32]byte) ([]byte, error) { called.Add(1); return nil, nil }
			var payloadCtx x402.PaymentPayloadContext
			test.edit(&requirements, &payloadCtx)
			payload, err := clientFor(t, config).CreatePaymentPayload(context.Background(), requirements, payloadCtx)
			if !errors.Is(err, test.want) || !reflect.DeepEqual(payload, types.PaymentPayload{}) || called.Load() != 0 {
				t.Fatalf("未在签名前拒绝报价: %v", err)
			}
		})
	}
}

func TestClientConfigValidation(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*exact.ClientConfig){
		"domain":          func(c *exact.ClientConfig) { c.Authorization.Permit2Address = "invalid" },
		"payer":           func(c *exact.ClientConfig) { c.PayerAddress = "invalid" },
		"asset":           func(c *exact.ClientConfig) { c.Asset = "invalid" },
		"recipient":       func(c *exact.ClientConfig) { c.PayTo = "invalid" },
		"amount":          func(c *exact.ClientConfig) { c.MaxAmount = "0" },
		"amount_overflow": func(c *exact.ClientConfig) { c.MaxAmount = strings.Repeat("9", 79) },
		"signer":          func(c *exact.ClientConfig) { c.Signer = nil },
		"negative":        func(c *exact.ClientConfig) { c.Lifetime = -time.Second },
		"short":           func(c *exact.ClientConfig) { c.Lifetime = 5 * time.Second },
		"long":            func(c *exact.ClientConfig) { c.Lifetime = 601 * time.Second },
		"fractional":      func(c *exact.ClientConfig) { c.Lifetime = 60*time.Second + time.Millisecond },
	} {
		t.Run(name, func(t *testing.T) {
			config, _ := clientConfigFor(t)
			edit(&config)
			if client, err := exact.NewClient(config); !errors.Is(err, exact.ErrClientConfig) || client != nil {
				t.Fatalf("配置未拒绝: %v", err)
			}
		})
	}
	for _, client := range []*exact.Client{nil, {}} {
		payload, err := client.CreatePaymentPayload(context.Background(), types.PaymentRequirements{}, x402.PaymentPayloadContext{})
		if !errors.Is(err, exact.ErrClientConfig) || !reflect.DeepEqual(payload, types.PaymentPayload{}) || client.Network() != "" {
			t.Fatal("零值客户端未安全拒绝")
		}
	}
}

func TestClientSignerFailuresAndElapsedTime(t *testing.T) {
	t.Parallel()
	for name, signer := range map[string]exact.AuthorizationSigner{
		"secret": func(context.Context, [32]byte) ([]byte, error) { return nil, errors.New("private sensitive detail") },
		"timeout": func(context.Context, [32]byte) ([]byte, error) {
			return nil, fmt.Errorf("private sensitive detail: %w", context.DeadlineExceeded)
		},
		"canceled": func(context.Context, [32]byte) ([]byte, error) {
			return nil, fmt.Errorf("private sensitive detail: %w", context.Canceled)
		},
		"empty":         func(context.Context, [32]byte) ([]byte, error) { return nil, nil },
		"bad_signature": func(context.Context, [32]byte) ([]byte, error) { return make([]byte, 65), nil },
	} {
		t.Run(name, func(t *testing.T) {
			config, requirements := clientConfigFor(t)
			config.Signer = signer
			payload, err := clientFor(t, config).CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{})
			if err == nil || strings.Contains(err.Error(), "private") || !reflect.DeepEqual(payload, types.PaymentPayload{}) {
				t.Fatal("签名器错误未安全处理")
			}
			if name == "timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
	t.Run("wrong_payer", func(t *testing.T) {
		config, requirements := clientConfigFor(t)
		config.PayerAddress = requirements.PayTo
		if _, err := clientFor(t, config).CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{}); !errors.Is(err, exact.ErrInvalidSignature) {
			t.Fatal(err)
		}
	})
	t.Run("expired_while_signing", func(t *testing.T) {
		config, requirements := clientConfigFor(t)
		config.Lifetime = 6 * time.Second
		sign := config.Signer
		config.Signer = func(ctx context.Context, digest [32]byte) ([]byte, error) {
			time.Sleep(1100 * time.Millisecond)
			return sign(ctx, digest)
		}
		if _, err := clientFor(t, config).CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{}); !errors.Is(err, exact.ErrInvalidValidity) {
			t.Fatal(err)
		}
	})
	t.Run("input_extra_changed_by_callback", func(t *testing.T) {
		config, requirements := clientConfigFor(t)
		sign := config.Signer
		config.Signer = func(ctx context.Context, digest [32]byte) ([]byte, error) {
			requirements.Extra["assetTransferMethod"] = "changed"
			return sign(ctx, digest)
		}
		if _, err := clientFor(t, config).CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("context", func(t *testing.T) {
		config, requirements := clientConfigFor(t)
		client := clientFor(t, config)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := client.CreatePaymentPayload(ctx, requirements, x402.PaymentPayloadContext{}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		config.Signer = func(ctx context.Context, _ [32]byte) ([]byte, error) {
			<-ctx.Done()
			return nil, errors.New("sensitive detail")
		}
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Millisecond)
		defer stop()
		if _, err := clientFor(t, config).CreatePaymentPayload(ctx, requirements, x402.PaymentPayloadContext{}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	})
}

func TestClientConcurrentNonces(t *testing.T) {
	t.Parallel()
	config, requirements := clientConfigFor(t)
	client := clientFor(t, config)
	var wg sync.WaitGroup
	nonces := make(chan string, 64)
	for range 64 {
		wg.Go(func() {
			payload, err := client.CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{})
			if err != nil {
				t.Error(err)
				return
			}
			nonces <- payload.Payload["permit2Authorization"].(map[string]interface{})["nonce"].(string)
		})
	}
	wg.Wait()
	close(nonces)
	seen := make(map[string]bool)
	for nonce := range nonces {
		if seen[nonce] {
			t.Fatal("并发生成重复 nonce")
		}
		seen[nonce] = true
	}
	if len(seen) != 64 {
		t.Fatal("并发签名未全部完成")
	}
}

func TestClientCustomLifetimeAndSignatureEncoding(t *testing.T) {
	t.Parallel()
	for _, lifetime := range []time.Duration{30 * time.Second, 10 * time.Minute} {
		t.Run(lifetime.String(), func(t *testing.T) {
			config, requirements := clientConfigFor(t)
			config.Lifetime = lifetime
			sign := config.Signer
			config.Signer = func(ctx context.Context, digest [32]byte) ([]byte, error) {
				signature, err := sign(ctx, digest)
				if err == nil {
					signature[64] -= 27
				}
				return signature, err
			}
			before := time.Now().Unix()
			payload, err := clientFor(t, config).CreatePaymentPayload(context.Background(), requirements, x402.PaymentPayloadContext{})
			if err != nil {
				t.Fatal(err)
			}
			auth := payload.Payload["permit2Authorization"].(map[string]interface{})
			deadline, err := strconv.ParseInt(auth["deadline"].(string), 10, 64)
			if err != nil || deadline < before+int64(lifetime/time.Second) || deadline > time.Now().Unix()+int64(lifetime/time.Second) {
				t.Fatal("自定义期限不符")
			}
		})
	}
}

func TestClientOfficialHTTPRejectsUntrustedQuote(t *testing.T) {
	t.Parallel()
	config, requirements := clientConfigFor(t)
	var signs, requests atomic.Int32
	sign := config.Signer
	config.Signer = func(ctx context.Context, digest [32]byte) ([]byte, error) { signs.Add(1); return sign(ctx, digest) }
	client := clientFor(t, config)
	requirements.PayTo = requirements.Asset
	challenge, err := json.Marshal(types.PaymentRequired{X402Version: 2, Accepts: []types.PaymentRequirements{requirements}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("PAYMENT-SIGNATURE") != "" {
			t.Error("向不可信收款人发送付款授权")
		}
		w.Header().Set("PAYMENT-REQUIRED", base64.StdEncoding.EncodeToString(challenge))
		w.WriteHeader(http.StatusPaymentRequired)
	}))
	defer server.Close()
	core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{AllowedAssets: []x402.SpendControlAsset{{
		Network: x402.Network(client.Network()), Asset: config.Asset, MaxAmountPerPayment: config.MaxAmount,
	}}})).Register(x402.Network(client.Network()), client)
	httpClient := x402http.WrapHTTPClientWithPayment(&http.Client{Timeout: 5 * time.Second}, x402http.Newx402HTTPClient(core))
	if resp, err := httpClient.Get(server.URL); err == nil {
		resp.Body.Close()
		t.Fatal("未拒绝收款人被替换的报价")
	}
	if signs.Load() != 0 || requests.Load() != 1 {
		t.Fatal("拒绝前签名或重复请求")
	}
}

func TestClientOfficialHTTPHandshake(t *testing.T) {
	t.Parallel()
	for _, finalStatus := range []int{http.StatusAccepted, http.StatusPaymentRequired} {
		t.Run(strconv.Itoa(finalStatus), func(t *testing.T) {
			config, requirements := clientConfigFor(t)
			client := clientFor(t, config)
			challenge, err := json.Marshal(types.PaymentRequired{X402Version: 2, Accepts: []types.PaymentRequirements{requirements}})
			if err != nil {
				t.Fatal(err)
			}
			var requests, signed atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				header := r.Header.Get("PAYMENT-SIGNATURE")
				if header == "" {
					w.Header().Set("PAYMENT-REQUIRED", base64.StdEncoding.EncodeToString(challenge))
					w.WriteHeader(http.StatusPaymentRequired)
					return
				}
				data, err := base64.StdEncoding.DecodeString(header)
				var payload types.PaymentPayload
				if err != nil || json.Unmarshal(data, &payload) != nil {
					t.Error("非法支付凭证")
					w.WriteHeader(400)
					return
				}
				if _, err := verifierFor(t, config.Authorization).VerifyAuthorization(payload, requirements, time.Now()); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				signed.Add(1)
				// 验签不等于结算：只返回未结算 202 或仍拒绝的 402，不伪造成功回执。
				w.WriteHeader(finalStatus)
			}))
			defer server.Close()
			core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{AllowedAssets: []x402.SpendControlAsset{{
				Network: x402.Network(client.Network()), Asset: config.Asset, MaxAmountPerPayment: config.MaxAmount,
			}}})).Register(x402.Network(client.Network()), client)
			httpClient := x402http.WrapHTTPClientWithPayment(&http.Client{Timeout: 5 * time.Second}, x402http.Newx402HTTPClient(core))
			resp, err := httpClient.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.StatusCode != finalStatus || requests.Load() != 2 || signed.Load() != 1 || resp.Header.Get("PAYMENT-RESPONSE") != "" {
				t.Fatal("官方 HTTP 握手未按预期执行或错误地重试付款")
			}
		})
	}
}
