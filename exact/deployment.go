package exact

import (
	"context"
	"encoding/hex"
	"time"

	tron "github.com/eightyun/x402-tron-go"
)

// DeploymentResult 仅表示本轮节点/部署检查，不能替代付款预检、固化到账或源码安全审计。
// Asset 只检查地址和非空代码，不证明代币语义、精度或转账行为。
type DeploymentResult struct {
	Network         string    `json:"network"`
	Asset           string    `json:"asset"`
	Permit2Address  string    `json:"permit2Address"`
	ProxyAddress    string    `json:"proxyAddress"`
	Permit2CodeHash string    `json:"permit2CodeHash"`
	ProxyCodeHash   string    `json:"proxyCodeHash"`
	BlockNumber     uint64    `json:"blockNumber"`
	BlockHash       string    `json:"blockHash"`
	BlockTimestamp  time.Time `json:"blockTimestamp"`
}

// CheckDeployment 无需钱包或付款授权，检查可信配置中的链、合约代码、代理绑定和签名域。
// 仍要求构造器中的代码哈希来自独立可信资料，不自动学习节点代码或补齐缺失配置。
// 不检查余额/allowance/nonce/账户权限/资源，不模拟settle，不访问原生固化节点。
func (verifier *PreflightVerifier) CheckDeployment(ctx context.Context, assetAddress string, now time.Time) (DeploymentResult, error) {
	if verifier == nil || verifier.offline == nil || verifier.rpc == nil {
		return DeploymentResult{}, ErrPreflightConfig
	}
	asset, err := configuredAddress(assetAddress)
	if err != nil || asset == verifier.permit2 || asset == verifier.offline.proxy {
		return DeploymentResult{}, ErrPreflightConfig
	}
	canonical, err := tron.NormalizeAddressBase58(assetAddress)
	if err != nil {
		return DeploymentResult{}, ErrPreflightConfig
	}
	ctx, cancel := context.WithTimeout(ctx, verifier.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return DeploymentResult{}, err
	}
	started := time.Now()
	if err := verifier.checkChain(ctx); err != nil {
		return DeploymentResult{}, err
	}
	before, err := verifier.rpc.head(ctx)
	if err != nil {
		return DeploymentResult{}, err
	}
	if err := before.fresh(now); err != nil {
		return DeploymentResult{}, err
	}
	// 三个固定getter不依赖付款人，使用已配置代理作为from；不生成授权或调用settle。
	if err := verifier.checkContracts(ctx, asset, verifier.offline.proxy); err != nil {
		return DeploymentResult{}, err
	}
	if err := verifier.checkChain(ctx); err != nil {
		return DeploymentResult{}, err
	}
	after, err := verifier.rpc.head(ctx)
	if err != nil {
		return DeploymentResult{}, err
	}
	if before != after {
		return DeploymentResult{}, ErrChainChanged
	}
	if err := after.fresh(now.Add(time.Since(started))); err != nil {
		return DeploymentResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return DeploymentResult{}, err
	}
	return DeploymentResult{Network: verifier.offline.network, Asset: canonical,
		Permit2Address: "41" + hex.EncodeToString(verifier.permit2[:]), ProxyAddress: "41" + hex.EncodeToString(verifier.offline.proxy[:]),
		Permit2CodeHash: "0x" + hex.EncodeToString(verifier.permit2CodeHash[:]), ProxyCodeHash: "0x" + hex.EncodeToString(verifier.proxyCodeHash[:]),
		BlockNumber: after.Number, BlockHash: "0x" + hex.EncodeToString(after.Hash[:]), BlockTimestamp: time.Unix(after.Timestamp, 0).UTC()}, nil
}
