package consumer_test

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/exact/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

var (
	_ x402.SchemeNetworkClient      = (*exact.Client)(nil)
	_ x402.SchemeNetworkServer      = (*exact.Server)(nil)
	_ x402.SchemeNetworkFacilitator = (*postgres.Facilitator)(nil)
)

// 只验证导包和离线协议；地址均为夹具，不证明所选网络的部署或真实付款。
func TestImportedClientAndServer(t *testing.T) {
	for _, network := range []string{tron.NetworkNile, tron.NetworkShasta} {
		t.Run(network, func(t *testing.T) {
			var key [32]byte
			if _, err := rand.Read(key[:]); err != nil {
				t.Fatal("临时随机源失败")
			}
			signer, err := exact.NewEOASigner(key[:])
			clear(key[:])
			if err != nil {
				t.Fatal(err)
			}
			defer signer.Close()
			contracts := exact.Config{Network: network, Permit2Address: "TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h", ProxyAddress: "TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F"}
			server, err := exact.NewServer(exact.ServerConfig{Network: network, Asset: "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf", PayTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL"})
			if err != nil {
				t.Fatal(err)
			}
			kind := types.SupportedKind{X402Version: 2, Scheme: "exact", Network: network, Extra: map[string]interface{}{"supportedAssetTransferMethods": []string{"permit2"}}}
			resource := x402.Newx402ResourceServer().Register(x402.Network(network), server)
			quote, err := resource.BuildPaymentRequirements(context.Background(), x402.ResourceConfig{Scheme: "exact", Network: x402.Network(network), PayTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL", Price: "1000", MaxTimeoutSeconds: 60}, kind, nil)
			if err != nil {
				t.Fatal(err)
			}
			client, err := exact.NewClient(exact.ClientConfig{Authorization: contracts, PayerAddress: signer.Address(), Signer: signer.SignAuthorization, Asset: quote.Asset, PayTo: quote.PayTo, MaxAmount: "1000"})
			if err != nil {
				t.Fatal(err)
			}
			core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{AllowedAssets: []x402.SpendControlAsset{{Network: x402.Network(network), Asset: quote.Asset, MaxAmountPerPayment: "1000"}}})).Register(x402.Network(network), client)
			payload, err := core.CreatePaymentPayload(context.Background(), quote, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			verifier, err := exact.NewOfflineVerifier(contracts)
			if err != nil {
				t.Fatal(err)
			}
			verified, err := verifier.VerifyAuthorization(payload, quote, time.Now())
			if err != nil || verified.Payer != signer.Address() || verified.Network != network {
				t.Fatal("外部导包签名绑定失败")
			}
			quote.Amount = "1001"
			if _, err = client.CreatePaymentPayload(context.Background(), quote, x402.PaymentPayloadContext{}); !errors.Is(err, exact.ErrClientPolicy) {
				t.Fatal("外部报价限额失效")
			}
		})
	}
}

func TestImportedFacilitatorConstructionIsLazy(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig("postgres://consumer@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		t.Fatal("夹具配置错误")
	}
	var calls atomic.Int32
	config.ConnConfig.DialFunc = func(context.Context, string, string) (net.Conn, error) {
		calls.Add(1)
		return nil, errors.New("不允许网络连接")
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("创建惰性连接池失败")
	}
	defer pool.Close()
	authorization := exact.Config{Network: tron.NetworkNile, Permit2Address: "TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h", ProxyAddress: "TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F"}
	store, err := postgres.New(pool, authorization)
	if err != nil {
		t.Fatal(err)
	}
	mechanism, err := postgres.NewFacilitator(store, postgres.FacilitatorConfig{
		Preflight:       exact.PreflightConfig{Authorization: authorization, NodeURL: "https://node.example.invalid/jsonrpc", Permit2CodeHash: "0x" + strings.Repeat("1", 64), ProxyCodeHash: "0x" + strings.Repeat("2", 64)},
		Execution:       exact.ExecutionConfig{Network: tron.NetworkNile, NodeURL: "https://node.example.invalid", GenesisBlockID: "0000000000000000d698d4192c56cb6be724a558448e2684802de4d6cd8690dc"},
		Policy:          exact.ServerConfig{Network: tron.NetworkNile, Asset: "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf", PayTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL"},
		ExecutorAddress: "TPgkieREqm37Q2LXNDcv63Y7i5bo7aBFLG", MaxAmount: "1000", FeeLimitSUN: 20_000_000, Lifetime: time.Minute,
		Signer: func(context.Context, [32]byte) ([]byte, error) {
			calls.Add(1)
			return nil, errors.New("不允许交易签名")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	core := x402.Newx402Facilitator().Register([]x402.Network{tron.NetworkNile}, mechanism)
	if len(core.GetSupported().Kinds) != 1 || calls.Load() != 0 {
		t.Fatal("导包/构造/注册触发网络或签名")
	}
	local, err := postgres.NewLocalClient(mechanism, 8)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := exact.NewServer(exact.ServerConfig{Network: tron.NetworkNile, Asset: "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf", PayTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL"})
	if err != nil {
		t.Fatal(err)
	}
	// 直接作为官方资源服务端依赖，不启动facilitator HTTP服务或连接数据库。
	_ = x402.Newx402ResourceServer(x402.WithFacilitatorClient(local)).Register(x402.Network(tron.NetworkNile), resource)
	supported, err := local.GetSupported(ctx)
	if err != nil || len(supported.Kinds) != 1 || supported.Kinds[0].Network != tron.NetworkNile || calls.Load() != 0 {
		t.Fatal("进程内导包/能力发现触发网络或签名，或能力不匹配")
	}
}
