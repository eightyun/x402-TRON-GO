package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/exact/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 本例只构造并注册机制，不连接数据库/节点，不持有私钥，也不签名或广播。
func ExampleNewFacilitator() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 这是未连接的示例连接池；实际应用注入自管的最小权限连接池并事先应用 SchemaSQL。
	pool, err := pgxpool.New(ctx, "postgres://example@127.0.0.1:1/unused?sslmode=disable")
	if err != nil {
		panic("示例连接池配置无效")
	}
	defer pool.Close()
	contracts := exact.Config{Network: tron.NetworkNile,
		Permit2Address: "TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h", ProxyAddress: "TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F"}
	store, err := postgres.New(pool, contracts)
	if err != nil {
		panic(err)
	}
	// 合约/节点/代码哈希仅为构造示例，不能据此发起付款；真实配置需独立核验。
	mechanism, err := postgres.NewFacilitator(store, postgres.FacilitatorConfig{
		Preflight: exact.PreflightConfig{Authorization: contracts, NodeURL: "https://node.example.invalid/jsonrpc",
			Permit2CodeHash: "0xfdd5ad3c59a61e0d232cc5ec5fe684bbe3083990c0873441a6c928bb452a4705",
			ProxyCodeHash:   "0x45b702594297f3ce7ea10f4ff622608653dd3ce289445269686607ac1d5f626d"},
		Execution: exact.ExecutionConfig{Network: tron.NetworkNile, NodeURL: "https://node.example.invalid",
			GenesisBlockID: "0000000000000000d698d4192c56cb6be724a558448e2684802de4d6cd8690dc"},
		Policy:          exact.ServerConfig{Network: tron.NetworkNile, Asset: "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf", PayTo: "TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL"},
		ExecutorAddress: "TPgkieREqm37Q2LXNDcv63Y7i5bo7aBFLG", MaxAmount: "1000", FeeLimitSUN: 20_000_000, Lifetime: time.Minute,
		Signer: func(context.Context, [32]byte) ([]byte, error) {
			return nil, errors.New("仅演示构造，不执行交易签名")
		},
	})
	if err != nil {
		panic(err)
	}
	// 直接交给官方 WithFacilitatorClient；无需HTTP服务、令牌或监听端口。
	client, err := postgres.NewLocalClient(mechanism, 8)
	if err != nil {
		panic(err)
	}
	capabilities, err := client.GetSupported(ctx)
	if err != nil {
		panic(err)
	}
	fmt.Println(capabilities.Kinds[0].Network)
	// Output: tron:3448148188
}
