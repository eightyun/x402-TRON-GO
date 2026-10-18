// 本程序仅供跨 SDK 离线互操作测试使用，不开网络监听、不读取钱包、不访问节点或数据库。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

var errBridge = errors.New("离线互操作测试请求无效")

type request struct {
	Operation    string                    `json:"operation"`
	Config       exact.Config              `json:"config"`
	Requirements types.PaymentRequirements `json:"requirements"`
	Payload      types.PaymentPayload      `json:"payload"`
	MaxAmount    string                    `json:"maxAmount"`
}

type response struct {
	Valid   bool                  `json:"valid"`
	Payer   string                `json:"payer,omitempty"`
	Payload *types.PaymentPayload `json:"payload,omitempty"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := run(ctx, os.Stdin, os.Stdout); err != nil {
		// stdout 只用于管道传递临时测试授权；错误不回显请求、签名或密钥。
		_, _ = fmt.Fprintln(os.Stderr, "interop_bridge_failed")
		os.Exit(1)
	}
}

func run(ctx context.Context, input io.Reader, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(input, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return errBridge
	}
	var request request
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(interface{})) != io.EOF {
		return errBridge
	}
	network, err := tron.NormalizeNetwork(request.Config.Network)
	if err != nil || (network != tron.NetworkNile && network != tron.NetworkShasta) {
		return errBridge
	}
	verifier, err := exact.NewOfflineVerifier(request.Config)
	if err != nil {
		return errBridge
	}
	var result response
	switch request.Operation {
	case "verify":
		verified, err := verifier.VerifyAuthorization(request.Payload, request.Requirements, time.Now())
		result.Valid = err == nil
		if err == nil {
			result.Payer = verified.Payer
		}
	case "create":
		// 每次生成临时零资金付款人，不提供输入私钥或访问既有钱包的通道。
		key, err := secp256k1.GeneratePrivateKey()
		if err != nil {
			return errBridge
		}
		defer key.Zero()
		encoded := key.Serialize()
		signer, err := exact.NewEOASigner(encoded)
		clear(encoded)
		if err != nil {
			return errBridge
		}
		defer signer.Close()
		client, err := exact.NewClient(exact.ClientConfig{
			Authorization: request.Config, PayerAddress: signer.Address(), Signer: signer.SignAuthorization,
			Asset: request.Requirements.Asset, PayTo: request.Requirements.PayTo, MaxAmount: request.MaxAmount,
		})
		if err != nil {
			return errBridge
		}
		// 注册收到的可信报价网络；旧上游使用hex别名，官方core不会自动规范化注册键。
		core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{AllowedAssets: []x402.SpendControlAsset{{
			Network: x402.Network(request.Requirements.Network), Asset: request.Requirements.Asset, MaxAmountPerPayment: request.MaxAmount,
		}}})).Register(x402.Network(request.Requirements.Network), client)
		payload, err := core.CreatePaymentPayload(ctx, request.Requirements, nil, nil)
		if err != nil {
			return errBridge
		}
		if _, err := verifier.VerifyAuthorization(payload, request.Requirements, time.Now()); err != nil {
			return errBridge
		}
		result = response{Valid: true, Payer: signer.Address(), Payload: &payload}
	default:
		return errBridge
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return json.NewEncoder(output).Encode(result)
}
