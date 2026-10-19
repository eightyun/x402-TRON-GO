package exact_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

// 示例只用临时无资金密钥和公开协议夹具，不加载钱包或连接节点。
func exampleEphemeralSigner() *exact.EOASigner {
	var key [32]byte
	defer clear(key[:])
	if _, err := rand.Read(key[:]); err != nil {
		panic(err)
	}
	signer, err := exact.NewEOASigner(key[:])
	if err != nil {
		panic(err)
	}
	return signer
}

func ExampleNewClient() {
	signer := exampleEphemeralSigner()
	defer signer.Close()
	// 仅为离线示例夹具；真实应用应独立核验并注入所选网络的合约配置。
	contracts := exact.Config{Network: tron.NetworkNile,
		Permit2Address: "TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h", ProxyAddress: "TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F"}
	requirements := types.PaymentRequirements{
		Scheme: "exact", Network: tron.NetworkNile, Asset: "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf",
		PayTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL", Amount: "1000", MaxTimeoutSeconds: 60,
		Extra: map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"},
	}
	mechanism, err := exact.NewClient(exact.ClientConfig{
		Authorization: contracts, PayerAddress: signer.Address(), Signer: signer.SignAuthorization,
		Asset: requirements.Asset, PayTo: requirements.PayTo, MaxAmount: "1000",
	})
	if err != nil {
		panic(err)
	}
	// 官方 core 负责机制选择、金额限制与协议信封，不另写 TRON 适配逻辑。
	core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{
		AllowedAssets: []x402.SpendControlAsset{{Network: x402.Network(mechanism.Network()), Asset: requirements.Asset, MaxAmountPerPayment: "1000"}},
	})).Register(x402.Network(mechanism.Network()), mechanism)
	payload, err := core.CreatePaymentPayload(context.Background(), requirements, nil, nil)
	if err != nil {
		panic(err)
	}
	verifier, err := exact.NewOfflineVerifier(contracts)
	if err != nil {
		panic(err)
	}
	_, err = verifier.VerifyAuthorization(payload, requirements, time.Now())
	if err != nil {
		panic(err)
	}
	// 离线签名通过仍不代表扣款或到账；实际 HTTP 应使用官方付款客户端封装。
	fmt.Println(payload.X402Version, payload.Accepted.Network)
	// Output: 2 tron:3448148188
}

func ExampleNewServer() {
	mechanism, err := exact.NewServer(exact.ServerConfig{
		Network: tron.NetworkNile, Asset: "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf", PayTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL",
	})
	if err != nil {
		panic(err)
	}
	core := x402.Newx402ResourceServer().Register(x402.Network(mechanism.Network()), mechanism)
	// 本例能力仅为夹具；实际由配置的可信 facilitator 发现并验证。
	kind := types.SupportedKind{X402Version: 2, Scheme: "exact", Network: mechanism.Network(),
		Extra: map[string]interface{}{"supportedAssetTransferMethods": []string{"permit2"}}}
	quote, err := core.BuildPaymentRequirements(context.Background(), x402.ResourceConfig{
		Scheme: "exact", Network: x402.Network(mechanism.Network()), PayTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL", Price: "1000", MaxTimeoutSeconds: 60,
	}, kind, nil)
	if err != nil {
		panic(err)
	}
	// 不启动服务或连接数据库；将该机制接入自己的官方 HTTP 中间件即可。
	fmt.Println(quote.Amount, quote.Extra["assetTransferMethod"], quote.Extra["paymentFlow"])
	// Output: 1000 permit2 upfront
}
