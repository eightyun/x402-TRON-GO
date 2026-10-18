package exact

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"net/http"
	"sync"
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
	ErrSettlementSimulation  = errors.New("结算只读调用模拟失败或返回无效数据")
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
	// BatchRPC 显式合并独立只读检查；节点必须支持 JSON-RPC batch，不自动降级。
	BatchRPC bool
}

// PreflightResult 仅记录本轮只读观察，不是最终性、扣款或结算证明。
type PreflightResult struct {
	Authorization  VerifiedAuthorization
	BlockNumber    uint64
	BlockHash      string
	BlockTimestamp time.Time
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
	return verifier.check(ctx, payload, requirements, nil, now)
}

// CheckWithSimulation 在同轮预检中额外模拟固定代理的完整settle调用，不签名或广播。
// executorAddress 必须是部署方固定的实际费用付款方，不能由付款请求指定；原Check仍只检查前置状态。
// 模拟不预留资金/nonce、不证明账户权限或费用资源充分，也不代表最终到账。
func (verifier *PreflightVerifier) CheckWithSimulation(ctx context.Context, payload types.PaymentPayload, requirements types.PaymentRequirements, executorAddress string, now time.Time) (PreflightResult, error) {
	executor, err := configuredAddress(executorAddress)
	if err != nil {
		return PreflightResult{}, ErrPreflightConfig
	}
	return verifier.check(ctx, payload, requirements, &executor, now)
}

func (verifier *PreflightVerifier) check(ctx context.Context, payload types.PaymentPayload, requirements types.PaymentRequirements, executor *[20]byte, now time.Time) (PreflightResult, error) {
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
	before, err := verifier.checkHead(ctx)
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
	if err := verifier.checkPaymentState(ctx, asset, payer, amount, verified.Nonce); err != nil {
		return PreflightResult{}, err
	}
	if executor != nil {
		// 与原生交易共用编码，避免模拟与实发使用不同的授权/收款人/签名规范。
		raw, err := parsePayload(payload.Payload)
		if err != nil {
			return PreflightResult{}, err
		}
		authorization, err := parseAuthorization(raw.Authorization)
		if err != nil {
			return PreflightResult{}, err
		}
		callData, err := settlementCallData(authorization, raw.Signature)
		if err != nil {
			return PreflightResult{}, err
		}
		if err := verifier.rpc.simulateSettlement(ctx, *executor, verifier.offline.proxy, callData); err != nil {
			return PreflightResult{}, err
		}
	}
	after, err := verifier.checkHead(ctx)
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
	return PreflightResult{Authorization: verified, BlockNumber: after.Number, BlockHash: "0x" + hex.EncodeToString(after.Hash[:]), BlockTimestamp: time.Unix(after.Timestamp, 0).UTC()}, nil
}

// 每组只有固定、独立的只读检查；等待全部退出，按原逻辑顺序选择错误，而非响应到达顺序。
func parallelChecks(ctx context.Context, checks ...func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	errorsByCheck := make([]error, len(checks))
	var workers sync.WaitGroup
	for index, check := range checks {
		workers.Go(func() { errorsByCheck[index] = check(ctx) })
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, err := range errorsByCheck {
		if err != nil {
			return err
		}
	}
	return nil
}

func (verifier *PreflightVerifier) checkHead(ctx context.Context) (observedHead, error) {
	var chain string
	var block rpcBlock
	var head observedHead
	if err := verifier.rpc.group(ctx,
		rpcQuery{"eth_chainId", []interface{}{}, &chain, func() error {
			chainID, err := rpcQuantity(chain)
			if err != nil {
				return err
			}
			if chainID != uint64(verifier.chainID) {
				return ErrChainMismatch
			}
			return nil
		}},
		rpcQuery{"eth_getBlockByNumber", []interface{}{"latest", false}, &block, func() error {
			var err error
			head, err = parseHead(block)
			return err
		}},
	); err != nil {
		return observedHead{}, err
	}
	return head, nil
}

func (verifier *PreflightVerifier) checkContracts(ctx context.Context, asset, payer [20]byte) error {
	contracts := []struct {
		address [20]byte
		hash    [32]byte
	}{
		{verifier.permit2, verifier.permit2CodeHash},
		{verifier.offline.proxy, verifier.proxyCodeHash},
		{asset, [32]byte{}},
	}
	getters := []struct {
		address [20]byte
		method  string
		want    [32]byte
	}{
		{verifier.offline.proxy, "PERMIT2()", addressWord(verifier.permit2)},
		{verifier.offline.proxy, "WITNESS_TYPEHASH()", keccak([]byte("Witness(address to,uint256 validAfter)"))},
		{verifier.permit2, "DOMAIN_SEPARATOR()", verifier.offline.domain},
	}
	var codes, words [3]string
	queries := make([]rpcQuery, 0, 6)
	for index, contract := range contracts {
		query := codeQuery(contract.address, &codes[index])
		query.validate = func() error {
			code, err := rpcCode(codes[index])
			if err != nil {
				return err
			}
			if len(code) == 0 || (contract.hash != [32]byte{} && keccak(code) != contract.hash) {
				return ErrContractMismatch
			}
			return nil
		}
		queries = append(queries, query)
	}
	for index, getter := range getters {
		query := wordQuery(&words[index], payer, getter.address, getter.method)
		query.validate = func() error {
			value, err := rpcWord(words[index])
			if err != nil {
				return err
			}
			if value != getter.want {
				return ErrContractMismatch
			}
			return nil
		}
		queries = append(queries, query)
	}
	return verifier.rpc.group(ctx, queries...)
}

func (verifier *PreflightVerifier) checkPaymentState(ctx context.Context, asset, payer [20]byte, amount [32]byte, nonce string) error {
	nonceWord, err := uint256Word(nonce, false)
	if err != nil {
		return ErrInvalidAuthorization
	}
	var wordPosition [32]byte
	new(big.Int).Rsh(new(big.Int).SetBytes(nonceWord[:]), 8).FillBytes(wordPosition[:])
	var values [3]string
	queries := []rpcQuery{
		wordQuery(&values[0], payer, asset, "balanceOf(address)", addressWord(payer)),
		wordQuery(&values[1], payer, asset, "allowance(address,address)", addressWord(payer), addressWord(verifier.permit2)),
		wordQuery(&values[2], payer, verifier.permit2, "nonceBitmap(address,uint256)", addressWord(payer), wordPosition),
	}
	for index := range queries {
		queries[index].validate = func() error {
			word, err := rpcWord(values[index])
			if err != nil {
				return err
			}
			if index < 2 {
				if new(big.Int).SetBytes(word[:]).Cmp(new(big.Int).SetBytes(amount[:])) < 0 {
					if index == 0 {
						return ErrInsufficientBalance
					}
					return ErrInsufficientAllowance
				}
			} else {
				bit := int(nonceWord[31])
				if word[31-bit/8]&(1<<uint(bit%8)) != 0 {
					return ErrNonceUsed
				}
			}
			return nil
		}
	}
	return verifier.rpc.group(ctx, queries...)
}
