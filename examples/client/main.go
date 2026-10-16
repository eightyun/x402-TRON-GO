// 客户端示例仅在本机演示真实官方 SDK 的 HTTP 402 握手，不访问区块链或提供付费资源。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/types"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := run(ctx, os.Stdout); err != nil {
		// 不输出可能包含可广播签名、URL 或外部凭据的错误原文。
		_, _ = fmt.Fprintln(os.Stderr, "本地握手示例失败；未发起链上交易。")
		os.Exit(1)
	}
}

func run(ctx context.Context, output io.Writer) error {
	// 临时随机密钥只在本进程内生成和使用，绝不读取既有钱包或保存到文件。
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		return err
	}
	defer key.Zero()
	encoded := key.Serialize()
	signer, err := exact.NewEOASigner(encoded)
	clear(encoded)
	if err != nil {
		return err
	}
	defer signer.Close()
	// 下列地址仅作公开协议夹具，不能当作可信链上部署配置。
	contracts := exact.Config{
		Network:        tron.NetworkNile,
		Permit2Address: "TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h",
		ProxyAddress:   "TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F",
	}
	requirements := types.PaymentRequirements{
		Scheme: "exact", Network: tron.NetworkNile, Asset: "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf",
		Amount: "1000", PayTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL", MaxTimeoutSeconds: 60,
		Extra: map[string]interface{}{"assetTransferMethod": "permit2"},
	}
	mechanism, err := exact.NewClient(exact.ClientConfig{
		Authorization: contracts, PayerAddress: signer.Address(), Signer: signer.SignAuthorization,
		Asset: requirements.Asset, PayTo: requirements.PayTo, MaxAmount: "1000",
	})
	if err != nil {
		return err
	}
	verifier, err := exact.NewOfflineVerifier(contracts)
	if err != nil {
		return err
	}
	challenge, err := json.Marshal(types.PaymentRequired{X402Version: 2, Accepts: []types.PaymentRequirements{requirements}})
	if err != nil {
		return err
	}
	// 本地夹具不是资源服务端/Facilitator 实现；只验证握手，不发货、不返回成功结算回执。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/demo" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		header := r.Header.Get("PAYMENT-SIGNATURE")
		if header == "" {
			w.Header().Set("PAYMENT-REQUIRED", base64.StdEncoding.EncodeToString(challenge))
			w.WriteHeader(http.StatusPaymentRequired)
			return
		}
		if len(header) > 8192 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, err := base64.StdEncoding.DecodeString(header)
		var payload types.PaymentPayload
		if err != nil || json.Unmarshal(data, &payload) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, err := verifier.VerifyAuthorization(payload, requirements, time.Now()); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = io.WriteString(w, "authorization_verified_not_settled")
	}))
	defer server.Close()
	// 保留官方 spend controls，同时显式允许配置的资产和最小单位金额上限。
	core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{
		AllowedAssets: []x402.SpendControlAsset{{Network: x402.Network(mechanism.Network()), Asset: requirements.Asset, MaxAmountPerPayment: "1000"}},
	})).Register(x402.Network(mechanism.Network()), mechanism)
	client := x402http.WrapHTTPClientWithPayment(&http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, x402http.Newx402HTTPClient(core))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/demo", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil || resp.StatusCode != http.StatusAccepted || resp.Header.Get("PAYMENT-RESPONSE") != "" || string(body) != "authorization_verified_not_settled" {
		return errors.New("本地握手结果不一致")
	}
	_, err = fmt.Fprintln(output, "402 → TRON 授权签名 → 官方 HTTP 重试 → 202 已验签、未结算。无链上交易，无资金转移。")
	return err
}
