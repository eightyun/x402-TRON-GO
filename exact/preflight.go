package exact

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"net/http"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/x402-foundation/x402/go/v2/types"
)

var (
	ErrPreflightConfig       = errors.New("链上预检配置无效")
	ErrNodeRead              = errors.New("链上只读 RPC 失败或返回无效数据")
	ErrChainMismatch         = errors.New("节点链 ID 与可信网络配置不符")
	ErrChainChanged          = errors.New("预检期间链头发生变化")
	ErrStaleHead             = errors.New("节点链头时间超出允许范围")
	ErrContractMismatch      = errors.New("合约代码或 Permit2 绑定不符")
	ErrInsufficientBalance   = errors.New("付款人代币余额不足")
	ErrInsufficientAllowance = errors.New("付款人 Permit2 授权额度不足")
	ErrNonceUsed             = errors.New("付款授权 nonce 已被链上使用")
)

// PreflightConfig 是部署方提供的只读节点与可信合约配置，不可来自付款请求。
// 两项 CodeHash 必须是独立核验的 runtime bytecode Keccak-256，采用 0x + 32 字节格式。
type PreflightConfig struct {
	Authorization          Config
	NodeURL                string
	APIKey                 string `json:"-"`
	Permit2CodeHash        string
	ProxyCodeHash          string
	Timeout                time.Duration
	HTTPClient             *http.Client `json:"-"`
	AllowInsecureLocalHTTP bool
}

// PreflightResult 仅记录本轮只读观察，不是最终性、扣款或结算证明。
type PreflightResult struct {
	Authorization VerifiedAuthorization
	BlockNumber   uint64
	BlockHash     string
}

// PreflightVerifier 组合离线验签和链上状态预检，不持有密钥、消费 nonce 或广播交易。
// 可并发使用；调用期间不可修改传入的 payload/requirements。
type PreflightVerifier struct {
	offline         *OfflineVerifier
	rpc             *readRPC
	chainID         uint32
	permit2         [20]byte
	permit2CodeHash [32]byte
	proxyCodeHash   [32]byte
	timeout         time.Duration
}

// NewPreflightVerifier 固定可信配置，不自动学习节点返回的合约代码哈希。
func NewPreflightVerifier(config PreflightConfig) (*PreflightVerifier, error) {
	offline, offlineErr := NewOfflineVerifier(config.Authorization)
	chainID, chainErr := tron.ChainID(config.Authorization.Network)
	permit2, permitErr := configuredAddress(config.Authorization.Permit2Address)
	permitHash, permitHashErr := rpcWord(config.Permit2CodeHash)
	proxyHash, proxyHashErr := rpcWord(config.ProxyCodeHash)
	if offlineErr != nil || chainErr != nil || permitErr != nil || permitHashErr != nil || proxyHashErr != nil || permitHash == [32]byte{} || proxyHash == [32]byte{} {
		return nil, ErrPreflightConfig
	}
	if config.Timeout == 0 {
		config.Timeout = 15 * time.Second
	}
	if config.Timeout < 0 || config.Timeout > time.Minute {
		return nil, ErrPreflightConfig
	}
	rpc, err := newReadRPC(config)
	if err != nil {
		return nil, err
	}
	return &PreflightVerifier{
		offline: offline, rpc: rpc, chainID: chainID, permit2: permit2,
		permit2CodeHash: permitHash, proxyCodeHash: proxyHash, timeout: config.Timeout,
	}, nil
}

// Check 在指定服务端时间下执行预检，RPC 用时会计入最后一次授权有效期检查。
// TRON eth_call/getCode 使用 latest；前后链头一致只是漂移检查，不是原子快照。
// 返回成功仍须在结算前重新检查，并进行持久化幂等、链上执行和结果核验。
func (verifier *PreflightVerifier) Check(ctx context.Context, payload types.PaymentPayload, requirements types.PaymentRequirements, now time.Time) (PreflightResult, error) {
	if verifier == nil || verifier.offline == nil || verifier.rpc == nil {
		return PreflightResult{}, ErrPreflightConfig
	}
	ctx, cancel := context.WithTimeout(ctx, verifier.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return PreflightResult{}, err
	}
	started := time.Now()
	verified, err := verifier.offline.VerifyAuthorization(payload, requirements, now)
	if err != nil {
		return PreflightResult{}, err
	}
	if err := verifier.checkChain(ctx); err != nil {
		return PreflightResult{}, err
	}
	before, err := verifier.rpc.head(ctx)
	if err != nil {
		return PreflightResult{}, err
	}
	if err := before.fresh(now); err != nil {
		return PreflightResult{}, err
	}
	asset, _, amount, err := verifier.offline.requirements(requirements)
	if err != nil {
		return PreflightResult{}, err
	}
	payer, err := configuredAddress(verified.Payer)
	if err != nil {
		return PreflightResult{}, ErrInvalidAuthorization
	}
	if err := verifier.checkContracts(ctx, asset, payer); err != nil {
		return PreflightResult{}, err
	}
	if err := verifier.checkFunds(ctx, asset, payer, amount); err != nil {
		return PreflightResult{}, err
	}
	if err := verifier.checkNonce(ctx, payer, verified.Nonce); err != nil {
		return PreflightResult{}, err
	}
	if err := verifier.checkChain(ctx); err != nil {
		return PreflightResult{}, err
	}
	after, err := verifier.rpc.head(ctx)
	if err != nil {
		return PreflightResult{}, err
	}
	if before != after {
		return PreflightResult{}, ErrChainChanged
	}
	finishedAt := now.Add(time.Since(started))
	if err := after.fresh(finishedAt); err != nil {
		return PreflightResult{}, err
	}
	final, err := verifier.offline.VerifyAuthorization(payload, requirements, finishedAt)
	if err != nil {
		return PreflightResult{}, err
	}
	if final != verified {
		return PreflightResult{}, ErrInvalidAuthorization
	}
	if err := ctx.Err(); err != nil {
		return PreflightResult{}, err
	}
	return PreflightResult{Authorization: verified, BlockNumber: after.Number, BlockHash: "0x" + hex.EncodeToString(after.Hash[:])}, nil
}

func (verifier *PreflightVerifier) checkChain(ctx context.Context) error {
	var value string
	if err := verifier.rpc.call(ctx, "eth_chainId", []interface{}{}, &value); err != nil {
		return err
	}
	chainID, err := rpcQuantity(value)
	if err != nil {
		return err
	}
	if chainID != uint64(verifier.chainID) {
		return ErrChainMismatch
	}
	return nil
}

func (verifier *PreflightVerifier) checkContracts(ctx context.Context, asset, payer [20]byte) error {
	for _, contract := range []struct {
		address [20]byte
		hash    [32]byte
	}{
		{verifier.permit2, verifier.permit2CodeHash},
		{verifier.offline.proxy, verifier.proxyCodeHash},
		{asset, [32]byte{}},
	} {
		code, err := verifier.rpc.code(ctx, contract.address)
		if err != nil {
			return err
		}
		if len(code) == 0 || (contract.hash != [32]byte{} && keccak(code) != contract.hash) {
			return ErrContractMismatch
		}
	}
	permit2, err := verifier.rpc.contractWord(ctx, payer, verifier.offline.proxy, "PERMIT2()")
	if err != nil {
		return err
	}
	if permit2 != addressWord(verifier.permit2) {
		return ErrContractMismatch
	}
	witness, err := verifier.rpc.contractWord(ctx, payer, verifier.offline.proxy, "WITNESS_TYPEHASH()")
	if err != nil {
		return err
	}
	if witness != keccak([]byte("Witness(address to,uint256 validAfter)")) {
		return ErrContractMismatch
	}
	domain, err := verifier.rpc.contractWord(ctx, payer, verifier.permit2, "DOMAIN_SEPARATOR()")
	if err != nil {
		return err
	}
	if domain != verifier.offline.domain {
		return ErrContractMismatch
	}
	return nil
}

func (verifier *PreflightVerifier) checkFunds(ctx context.Context, asset, payer [20]byte, amount [32]byte) error {
	balance, err := verifier.rpc.contractWord(ctx, payer, asset, "balanceOf(address)", addressWord(payer))
	if err != nil {
		return err
	}
	if new(big.Int).SetBytes(balance[:]).Cmp(new(big.Int).SetBytes(amount[:])) < 0 {
		return ErrInsufficientBalance
	}
	allowance, err := verifier.rpc.contractWord(ctx, payer, asset, "allowance(address,address)", addressWord(payer), addressWord(verifier.permit2))
	if err != nil {
		return err
	}
	if new(big.Int).SetBytes(allowance[:]).Cmp(new(big.Int).SetBytes(amount[:])) < 0 {
		return ErrInsufficientAllowance
	}
	return nil
}

func (verifier *PreflightVerifier) checkNonce(ctx context.Context, payer [20]byte, nonceText string) error {
	nonce, err := uint256Word(nonceText, false)
	if err != nil {
		return ErrInvalidAuthorization
	}
	var wordPosition [32]byte
	new(big.Int).Rsh(new(big.Int).SetBytes(nonce[:]), 8).FillBytes(wordPosition[:])
	bitmap, err := verifier.rpc.contractWord(ctx, payer, verifier.permit2, "nonceBitmap(address,uint256)", addressWord(payer), wordPosition)
	if err != nil {
		return err
	}
	bit := int(nonce[31])
	if bitmap[31-bit/8]&(1<<uint(bit%8)) != 0 {
		return ErrNonceUsed
	}
	return nil
}
