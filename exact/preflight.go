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
	ErrPreflightConfig       = errors.New("exact: invalid preflight configuration")
	ErrNodeRead              = errors.New("exact: read-only RPC failed or returned invalid data")
	ErrChainMismatch         = errors.New("exact: node chain ID does not match the trusted network")
	ErrChainChanged          = errors.New("exact: chain head changed during preflight")
	ErrStaleHead             = errors.New("exact: node chain head timestamp is outside the allowed range")
	ErrContractMismatch      = errors.New("exact: contract code or Permit2 binding mismatch")
	ErrInsufficientBalance   = errors.New("exact: insufficient payer token balance")
	ErrInsufficientAllowance = errors.New("exact: insufficient payer Permit2 allowance")
	ErrNonceUsed             = errors.New("exact: payment authorization nonce already used on chain")
	ErrSettlementSimulation  = errors.New("exact: settlement simulation failed or returned invalid data")
)

// PreflightConfig is the deployer-provided read-only node and trusted contract configuration. It
// must not come from a payment request. Both CodeHash fields must be independently verified
// Keccak-256 hashes of the runtime bytecode, formatted as 0x followed by 32 bytes.
type PreflightConfig struct {
	Authorization          Config
	NodeURL                string
	APIKey                 string `json:"-"`
	Permit2CodeHash        string
	ProxyCodeHash          string
	Timeout                time.Duration
	HTTPClient             *http.Client `json:"-"`
	AllowInsecureLocalHTTP bool
	// BatchRPC explicitly combines independent read-only checks. The node must support JSON-RPC
	// batch requests; there is no automatic fallback.
	BatchRPC bool
}

// PreflightResult only records the read-only observations of this round. It is not proof of
// finality, debit or settlement.
type PreflightResult struct {
	Authorization  VerifiedAuthorization
	BlockNumber    uint64
	BlockHash      string
	BlockTimestamp time.Time
}

// PreflightVerifier combines offline signature verification with on-chain state preflight checks.
// It does not hold keys, consume nonces or broadcast transactions. It is safe for concurrent use;
// the payload and requirements passed in must not be modified during a call.
type PreflightVerifier struct {
	offline         *OfflineVerifier
	rpc             *readRPC
	chainID         uint32
	permit2         [20]byte
	permit2CodeHash [32]byte
	proxyCodeHash   [32]byte
	timeout         time.Duration
}

// NewPreflightVerifier pins the trusted configuration and never learns contract code hashes from
// node responses.
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

// Check runs the preflight at the given server time; time spent on RPC is included in the final
// authorization validity check. TRON eth_call and eth_getCode use latest; matching chain heads
// before and after are only a drift check, not an atomic snapshot. A successful result must still
// be rechecked before settlement, followed by persistent idempotency, on-chain execution and
// result verification.
func (verifier *PreflightVerifier) Check(ctx context.Context, payload types.PaymentPayload, requirements types.PaymentRequirements, now time.Time) (PreflightResult, error) {
	return verifier.check(ctx, payload, requirements, nil, now)
}

// CheckWithSimulation additionally simulates the full settle call on the pinned proxy within the
// same preflight round. It does not sign or broadcast. executorAddress must be the actual fee payer
// pinned by the deployer and must not be specified by the payment request; Check still only checks
// preconditions. The simulation does not reserve funds or nonces, does not prove account
// permissions or sufficient fee resources, and does not indicate final receipt of funds.
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
	asset, _, amount, err := verifier.offline.requirements(requirements)
	if err != nil {
		return PreflightResult{}, err
	}
	payer, err := configuredAddress(verified.Payer)
	if err != nil {
		return PreflightResult{}, ErrInvalidAuthorization
	}
	var before observedHead
	if verifier.rpc.batch {
		queries := append(verifier.headQueries(&before), verifier.contractQueries(asset, payer)...)
		payment, err := verifier.paymentQueries(asset, payer, amount, verified.Nonce)
		if err != nil {
			return PreflightResult{}, err
		}
		queries = append(queries, payment...)
		// 首组包含原始头，第二组只补独立只读状态；完整模拟仍需全部检查通过。
		if err := verifier.rpc.group(ctx, queries[:6]...); err != nil {
			return PreflightResult{}, err
		}
		if err := before.fresh(now); err != nil {
			return PreflightResult{}, err
		}
		if err := verifier.rpc.group(ctx, queries[6:]...); err != nil {
			return PreflightResult{}, err
		}
	} else {
		before, err = verifier.checkHead(ctx)
		if err != nil {
			return PreflightResult{}, err
		}
		if err := before.fresh(now); err != nil {
			return PreflightResult{}, err
		}
		if err := verifier.checkContracts(ctx, asset, payer); err != nil {
			return PreflightResult{}, err
		}
		queries, err := verifier.paymentQueries(asset, payer, amount, verified.Nonce)
		if err != nil {
			return PreflightResult{}, err
		}
		if err := verifier.rpc.group(ctx, queries...); err != nil {
			return PreflightResult{}, err
		}
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
	var head observedHead
	err := verifier.rpc.group(ctx, verifier.headQueries(&head)...)
	return head, err
}

func (verifier *PreflightVerifier) headQueries(head *observedHead) []rpcQuery {
	var chain string
	var block rpcBlock
	return []rpcQuery{
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
			*head, err = parseHead(block)
			return err
		}},
	}
}

func (verifier *PreflightVerifier) checkContracts(ctx context.Context, asset, payer [20]byte) error {
	return verifier.rpc.group(ctx, verifier.contractQueries(asset, payer)...)
}

func (verifier *PreflightVerifier) contractQueries(asset, payer [20]byte) []rpcQuery {
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
	return queries
}

func (verifier *PreflightVerifier) paymentQueries(asset, payer [20]byte, amount [32]byte, nonce string) ([]rpcQuery, error) {
	nonceWord, err := uint256Word(nonce, false)
	if err != nil {
		return nil, ErrInvalidAuthorization
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
	return queries, nil
}
