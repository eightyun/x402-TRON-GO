// payer是显式启用的测试网GET客户端；恢复始终复用原授权，不自动批准代币或另建付款。
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/internal/examplekeys"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/types"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 450*time.Second)
	defer cancel()
	if len(os.Args) != 2 || (os.Args[1] != "pay" && os.Args[1] != "recover") {
		fmt.Fprintln(os.Stderr, "用法：payer pay|recover；仅支持显式配置的测试网。")
		os.Exit(1)
	}
	c, err := loadConfig()
	if err == nil {
		err = run(ctx, c, os.Args[1], &http.Client{Timeout: 420 * time.Second}, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "付款操作未确认完成；保留状态目录，仅用recover恢复原请求。配置、节点或凭据错误原文不输出。")
		os.Exit(1)
	}
}

type savedPaymentTransport struct {
	base   http.RoundTripper
	config configuration
	mu     sync.Mutex
	sent   bool
}

func (t *savedPaymentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodGet || r.URL.String() != t.config.URL {
		return nil, errConfiguration
	}
	header := r.Header.Get("PAYMENT-SIGNATURE")
	if header != "" {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.sent || len(r.Header.Values("PAYMENT-SIGNATURE")) != 1 {
			return nil, errState
		}
		if _, err := checkedPayment(t.config, header, true); err != nil {
			return nil, errState
		}
		if writeState(t.config.stateDirectory, "payment-signature", []byte(header)) != nil {
			return nil, errState
		}
		t.sent = true
	}
	return t.base.RoundTrip(r)
}

func checkedPayment(c configuration, header string, current bool) (types.PaymentPayload, error) {
	var p types.PaymentPayload
	if len(header) > 16<<10 {
		return p, errState
	}
	b, err := base64.StdEncoding.Strict().DecodeString(header)
	if err != nil || json.Unmarshal(b, &p) != nil {
		return p, errState
	}
	asset, e1 := tron.NormalizeAddressBase58(p.Accepted.Asset)
	payTo, e2 := tron.NormalizeAddressBase58(p.Accepted.PayTo)
	amount, ok := new(big.Int).SetString(p.Accepted.Amount, 10)
	maximum, capOK := new(big.Int).SetString(c.MaxAmount, 10)
	if !ok || !capOK || amount.Sign() <= 0 || amount.Cmp(maximum) > 0 || e1 != nil || e2 != nil || asset != c.Asset || payTo != c.PayTo || p.Accepted.Network != c.Network {
		return p, errState
	}
	v, err := exact.NewOfflineVerifier(c.authorization())
	if err != nil {
		return p, errState
	}
	a, err := v.VerifyAuthorizationBinding(p, p.Accepted)
	if current {
		a, err = v.VerifyAuthorization(p, p.Accepted, time.Now())
	}
	if err != nil || a.Payer != c.Payer {
		return p, errState
	}
	return p, nil
}

type attempt struct {
	Mode          string    `json:"mode"`
	Time          time.Time `json:"time"`
	State         string    `json:"state"`
	HTTPStatus    int       `json:"httpStatus"`
	Success       bool      `json:"success"`
	TransactionID string    `json:"transactionID,omitempty"`
	ErrorReason   string    `json:"errorReason,omitempty"`
	ResponseFile  string    `json:"responseFile,omitempty"`
}

func run(ctx context.Context, c configuration, mode string, base *http.Client, output io.Writer) error {
	if !c.enabled || (mode != "pay" && mode != "recover") || base == nil {
		return errConfiguration
	}
	lock, err := lockState(c.stateDirectory)
	if err != nil {
		return err
	}
	defer lock.Close()
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if client.Timeout == 0 || client.Timeout > 420*time.Second {
		client.Timeout = 420 * time.Second
	}
	if client.Transport == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxResponseHeaderBytes = 32 << 10
		client.Transport = transport
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return errConfiguration
	}
	var signer *exact.EOASigner
	if mode == "pay" {
		intent, err := json.Marshal(c.policy)
		if err != nil || writeState(c.stateDirectory, "intent.json", intent) != nil {
			return errState
		}
		signer, err = examplekeys.Load(c.keyFile)
		if err != nil {
			return errConfiguration
		}
		defer signer.Close()
		if signer.Address() != c.Payer {
			return errConfiguration
		}
		mechanism, err := exact.NewClient(exact.ClientConfig{Authorization: c.authorization(), PayerAddress: c.Payer, Asset: c.Asset, PayTo: c.PayTo, MaxAmount: c.MaxAmount, Signer: func(ctx context.Context, digest [32]byte) ([]byte, error) {
			if writeState(c.stateDirectory, "authorization-reserved", digest[:]) != nil {
				return nil, errState
			}
			return signer.SignAuthorization(ctx, digest)
		}})
		if err != nil {
			return errConfiguration
		}
		client.Transport = &savedPaymentTransport{base: client.Transport, config: c}
		core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{AllowedAssets: []x402.SpendControlAsset{{Network: x402.Network(c.Network), Asset: c.Asset, MaxAmountPerPayment: c.MaxAmount}}})).Register(x402.Network(c.Network), mechanism)
		client = *x402http.WrapHTTPClientWithPayment(&client, x402http.Newx402HTTPClient(core))
	} else {
		intent, e := decodeIntent(c.stateDirectory)
		if e != nil || intent != c.policy {
			return errState
		}
		header, e := readState(c.stateDirectory, "payment-signature", 16<<10)
		if e != nil {
			return e
		}
		if _, e := checkedPayment(c, string(header), false); e != nil {
			return e
		}
		request.Header.Set("PAYMENT-SIGNATURE", string(header))
		clear(header)
	}
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return errState
	}
	prefix := "attempt-" + hex.EncodeToString(id[:])
	result := attempt{Mode: mode, Time: time.Now().UTC(), State: "request_started"}
	b, _ := json.Marshal(result)
	if writeState(c.stateDirectory, prefix+".request.json", b) != nil {
		return errState
	}
	response, requestErr := client.Do(request)
	result.Time = time.Now().UTC()
	result.State = "unconfirmed"
	if response != nil {
		result.HTTPStatus = response.StatusCode
		body, readErr := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
		_ = response.Body.Close()
		header, stateErr := readState(c.stateDirectory, "payment-signature", 16<<10)
		p, paymentErr := checkedPayment(c, string(header), false)
		clear(header)
		var receipt x402.SettleResponse
		encoded, e := base64.StdEncoding.Strict().DecodeString(response.Header.Get("PAYMENT-RESPONSE"))
		validReceipt := e == nil && len(encoded) <= 16<<10 && len(response.Header.Values("PAYMENT-RESPONSE")) == 1 && json.Unmarshal(encoded, &receipt) == nil
		if validReceipt {
			result.ErrorReason = safeReason(receipt.ErrorReason)
			if len(receipt.Transaction) == 64 {
				if tx, e := hex.DecodeString(receipt.Transaction); e == nil && len(tx) == 32 {
					result.TransactionID = receipt.Transaction
				}
			}
		}
		result.Success = requestErr == nil && readErr == nil && len(body) <= 1<<20 && stateErr == nil && paymentErr == nil && validReceipt && response.StatusCode == 200 && receipt.Success && receipt.ErrorReason == "" && result.TransactionID != "" && string(receipt.Network) == c.Network && receipt.Payer == c.Payer && receipt.Amount == p.Accepted.Amount
		if result.Success {
			result.ResponseFile = prefix + ".response.body"
			if writeState(c.stateDirectory, result.ResponseFile, body) != nil {
				return errState
			}
			result.State = "resource_received"
		}
	}
	b, _ = json.Marshal(result)
	if writeState(c.stateDirectory, prefix+".result.json", b) != nil {
		return errState
	}
	if json.NewEncoder(output).Encode(result) != nil {
		return errState
	}
	if !result.Success {
		return errPayment
	}
	return nil
}

func safeReason(reason string) string {
	switch reason {
	case "", "settlement_pending", "settlement_unavailable", "settlement_simulation_failed", "payment_expired", "invalid_signature", "invalid_payment", "insufficient_balance", "insufficient_allowance", "nonce_already_used", "settlement_conflict":
		return reason
	default:
		return "payment_unconfirmed"
	}
}
