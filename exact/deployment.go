package exact

import (
	"context"
	"encoding/hex"
	"time"

	tron "github.com/eightyun/x402-tron-go"
)

// DeploymentResult only reflects the current round of node and deployment checks. It does not
// replace payment preflight, solidified payment confirmation or a source code security audit.
// Asset is only checked for its address and non-empty code; this does not prove token semantics,
// decimals or transfer behavior.
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

// CheckDeployment checks the chain, contract code, proxy binding and signing domain from the
// trusted configuration without requiring a wallet or payment authorization. The code hashes
// passed to the constructor must still come from independent trusted sources; it never learns
// code from the node or fills in missing configuration. It does not check balances, allowances,
// nonces, account permissions or resources, does not simulate settle, and does not access the
// native solidity node.
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
	before, err := verifier.checkHead(ctx)
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
	after, err := verifier.checkHead(ctx)
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
