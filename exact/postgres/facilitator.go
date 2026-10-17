package postgres

import (
	"context"
	"errors"
	"math/big"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

var ErrFacilitatorConfig = errors.New("TRON facilitator 配置无效")

// FacilitatorConfig 是单网络、单资产/收款人的可信部署策略，不接受请求覆盖。
// Signer 只签名，禁止自行广播，必须遵守 context；不提供密钥加载或默认合约部署。
type FacilitatorConfig struct {
	Preflight       exact.PreflightConfig
	Execution       exact.ExecutionConfig
	Policy          exact.ServerConfig
	ExecutorAddress string
	MaxAmount       string
	FeeLimitSUN     int64
	Lifetime        time.Duration
	Signer          exact.DigestSigner `json:"-"`
	Timeout         time.Duration
	PollInterval    time.Duration
}

// Facilitator 实现官方 v2 exact/Permit2/upfront 机制，可并发/跨实例共享同一日志。
// 本包不启动 HTTP、后台任务或自动授权；公开端点仍需鉴权/限流及累计费用预算。
type Facilitator struct {
	store     *Store
	policy    *exact.Server
	payTo     string
	maximum   *big.Int
	feeLimit  int64
	executor  string
	preflight *exact.PreflightVerifier
	execution *exact.ExecutionClient
	builder   *exact.SettlementBuilder
	signer    exact.DigestSigner
	timeout   time.Duration
	interval  time.Duration
}

var _ x402.SchemeNetworkFacilitator = (*Facilitator)(nil)

// NewFacilitator 仅校验/固定配置，不连接节点、迁移数据库、调用签名器或广播。
// Timeout 默认90秒，最多3分钟；PollInterval默认3秒，范围1~10秒。
func NewFacilitator(store *Store, config FacilitatorConfig) (*Facilitator, error) {
	if !store.valid() || config.Signer == nil {
		return nil, ErrFacilitatorConfig
	}
	policy, policyErr := exact.NewServer(config.Policy)
	preflight, preflightErr := exact.NewPreflightVerifier(config.Preflight)
	execution, executionErr := exact.NewExecutionClient(config.Execution)
	builder, builderErr := exact.NewSettlementBuilder(exact.SettlementBuilderConfig{
		Authorization: config.Preflight.Authorization, ExecutorAddress: config.ExecutorAddress,
		FeeLimitSUN: config.FeeLimitSUN, Lifetime: config.Lifetime,
	})
	permit2, permitErr := tron.NormalizeAddressHex(config.Preflight.Authorization.Permit2Address)
	proxy, proxyErr := tron.NormalizeAddressHex(config.Preflight.Authorization.ProxyAddress)
	storeProxy, storeProxyErr := tron.NormalizeAddressHex(store.config.ProxyAddress)
	network, networkErr := tron.NormalizeNetwork(config.Preflight.Authorization.Network)
	if policyErr != nil || preflightErr != nil || executionErr != nil || builderErr != nil || permitErr != nil || proxyErr != nil || storeProxyErr != nil || networkErr != nil || policy.Network() != store.network || network != store.network || execution.Network() != store.network || permit2 != store.permit2 || proxy != storeProxy {
		return nil, ErrFacilitatorConfig
	}
	price, err := policy.ParsePrice(config.MaxAmount, x402.Network(store.network))
	if err != nil {
		return nil, ErrFacilitatorConfig
	}
	maximum, _ := new(big.Int).SetString(price.Amount, 10)
	payTo, _ := tron.NormalizeAddressBase58(config.Policy.PayTo)
	executor, _ := tron.NormalizeAddressBase58(config.ExecutorAddress)
	if config.Timeout == 0 {
		config.Timeout = 90 * time.Second
	}
	if config.PollInterval == 0 {
		config.PollInterval = 3 * time.Second
	}
	if config.Timeout < 0 || config.Timeout > 3*time.Minute || config.PollInterval < time.Second || config.PollInterval > 10*time.Second {
		return nil, ErrFacilitatorConfig
	}
	return &Facilitator{store: store, policy: policy, payTo: payTo, maximum: maximum, feeLimit: config.FeeLimitSUN, executor: executor,
		preflight: preflight, execution: execution, builder: builder, signer: config.Signer,
		timeout: config.Timeout, interval: config.PollInterval}, nil
}

func (*Facilitator) Scheme() string     { return "exact" }
func (*Facilitator) CaipFamily() string { return "tron:*" }

// Network 是官方 SDK 注册使用的规范网络标识。
func (facilitator *Facilitator) Network() string {
	if facilitator == nil || facilitator.store == nil {
		return ""
	}
	return facilitator.store.network
}

func (facilitator *Facilitator) GetExtra(network x402.Network) map[string]interface{} {
	if facilitator.Network() == "" || string(network) != facilitator.Network() {
		return nil
	}
	return map[string]interface{}{"supportedAssetTransferMethods": []string{"permit2"}}
}

func (facilitator *Facilitator) GetSigners(network x402.Network) []string {
	if facilitator.Network() == "" || string(network) != facilitator.Network() {
		return nil
	}
	return []string{facilitator.executor}
}

// Verify 是只读的新授权预检，不预留 nonce，不承诺之后结算必然成功。
func (facilitator *Facilitator) Verify(ctx context.Context, payload types.PaymentPayload, requirements types.PaymentRequirements, _ *x402.FacilitatorContext) (*x402.VerifyResponse, error) {
	if !facilitator.valid() {
		return nil, ErrFacilitatorConfig
	}
	ctx, cancel := context.WithTimeout(ctx, facilitator.timeout)
	defer cancel()
	if err := facilitator.checkPolicy(requirements); err != nil {
		return nil, x402.NewVerifyError(facilitatorReason(err), "", "")
	}
	result, err := facilitator.preflight.Check(ctx, payload, requirements, time.Now())
	if err != nil {
		return nil, protocolContextError(x402.NewVerifyError(facilitatorReason(err), "", ""), err)
	}
	return &x402.VerifyResponse{IsValid: true, Payer: result.Authorization.Payer}, nil
}

// Settle 独立完成验签/准入，不要求先 Verify；只在核验成功并已持久化后返回 success。
// 调用期间不得修改请求。历史请求仅恢复同一交易，不重新检查已消费 nonce 或重签/重播。
func (facilitator *Facilitator) Settle(ctx context.Context, payload types.PaymentPayload, requirements types.PaymentRequirements, _ *x402.FacilitatorContext) (*x402.SettleResponse, error) {
	if !facilitator.valid() {
		return nil, ErrFacilitatorConfig
	}
	ctx, cancel := context.WithTimeout(ctx, facilitator.timeout)
	defer cancel()
	if err := facilitator.checkPolicy(requirements); err != nil {
		return nil, facilitator.settleError(err, Record{}, "")
	}
	verified, err := facilitator.store.verifier.VerifyAuthorizationBinding(payload, requirements)
	if err != nil {
		return nil, facilitator.settleError(err, Record{}, "")
	}
	record, err := facilitator.store.Get(ctx, facilitator.store.authorizationID(verified))
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, facilitator.settleError(err, Record{}, "")
	}
	if err == nil && record.Authorization != verified {
		return nil, facilitator.settleError(ErrConflict, Record{}, "")
	}
	if errors.Is(err, ErrNotFound) || record.State == StateReserved || record.State == StatePrepared {
		preflight, err := facilitator.preflight.Check(ctx, payload, requirements, time.Now())
		if err != nil {
			return facilitator.recoverConcurrent(ctx, verified, record, requirements, err)
		}
		if preflight.Authorization != verified {
			return nil, facilitator.settleError(ErrConflict, record, "")
		}
		if record.ID == "" {
			record, err = facilitator.store.Reserve(ctx, payload, requirements, time.Now())
			if err != nil {
				return facilitator.recoverConcurrent(ctx, verified, Record{}, requirements, err)
			}
		}
		if record.State == StateReserved {
			record, err = facilitator.prepare(ctx, record, payload, requirements, preflight)
			if err != nil {
				return nil, facilitator.settleError(err, record, "settlement_recovery_required")
			}
		}
	}
	if record.State == StatePrepared {
		prepared, err := facilitator.store.restoreRecord(record, facilitator.executor)
		if err != nil {
			return nil, facilitator.settleError(err, record, "")
		}
		if prepared.FeeLimitSUN() > facilitator.feeLimit {
			return nil, facilitator.settleError(ErrFacilitatorConfig, record, "settlement_policy_rejected")
		}
		submitted, err := facilitator.store.Submit(ctx, record.ID, prepared, facilitator.execution, time.Now())
		if errors.Is(err, ErrBroadcastStarted) {
			return facilitator.recoverConcurrent(ctx, verified, record, requirements, err)
		}
		if err != nil && submitted.ID == "" {
			return nil, facilitator.settleError(err, record, "settlement_recovery_required")
		}
		record = submitted
		if err != nil && record.State == StatePrepared {
			return nil, facilitator.settleError(err, record, "")
		}
	}
	return facilitator.waitForSettlement(ctx, record, requirements)
}

// 预检/占用期间另一实例可能已提交并消费 nonce，重新读取后只能追踪原交易。
func (facilitator *Facilitator) recoverConcurrent(ctx context.Context, verified exact.VerifiedAuthorization, previous Record, requirements types.PaymentRequirements, cause error) (*x402.SettleResponse, error) {
	record, err := facilitator.store.Get(ctx, facilitator.store.authorizationID(verified))
	if err == nil && record.Authorization != verified {
		return nil, facilitator.settleError(ErrConflict, Record{}, "")
	}
	if err == nil && record.Authorization == verified && (record.State == StateBroadcastUnknown || record.State == StatePending || record.State == StateSucceeded) {
		return facilitator.waitForSettlement(ctx, record, requirements)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, facilitator.settleError(err, previous, "settlement_recovery_required")
	}
	return nil, facilitator.settleError(cause, previous, "")
}

func (facilitator *Facilitator) prepare(ctx context.Context, expected Record, payload types.PaymentPayload, requirements types.PaymentRequirements, preflight exact.PreflightResult) (Record, error) {
	tx, err := facilitator.store.db.Begin(ctx)
	if err != nil {
		return Record{}, storageError(ctx, err)
	}
	defer tx.Rollback(ctx)
	record, err := scanRecord(ctx, tx.QueryRow(ctx, `SELECT `+recordColumns+` FROM x402_tron_settlements
		WHERE id=$1 AND network=$2 AND permit2_address=$3 FOR UPDATE`, expected.ID, facilitator.Network(), facilitator.store.permit2))
	if err != nil {
		return Record{}, err
	}
	if record.Authorization != expected.Authorization || preflight.Authorization != expected.Authorization {
		return Record{}, ErrConflict
	}
	if record.State != StateReserved {
		return record, nil
	}
	prepared, err := facilitator.builder.Build(payload, requirements, exact.ReferenceBlock{
		Network: facilitator.Network(), Number: preflight.BlockNumber, ID: preflight.BlockHash, Timestamp: preflight.BlockTimestamp,
	}, time.Now())
	if err != nil {
		return record, err
	}
	signed, err := prepared.Sign(ctx, facilitator.signer, time.Now())
	if err != nil {
		return record, err
	}
	saved, err := facilitator.store.saveTransaction(ctx, tx, record.ID, record.Authorization.Digest, Transaction{ID: signed.ID, Payload: signed.Payload})
	if err != nil {
		return record, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Record{}, storageError(ctx, err)
	}
	return saved, nil
}

func (facilitator *Facilitator) waitForSettlement(ctx context.Context, record Record, requirements types.PaymentRequirements) (*x402.SettleResponse, error) {
	for {
		confirmed, err := facilitator.store.ConfirmSettlement(ctx, record.ID, facilitator.executor, facilitator.execution, time.Now())
		if err == nil {
			return &x402.SettleResponse{Success: true, Payer: confirmed.Authorization.Payer,
				Network: x402.Network(facilitator.Network()), Transaction: confirmed.Transaction.ID, Amount: requirements.Amount}, nil
		}
		if !errors.Is(err, ErrSettlementUnconfirmed) {
			return nil, facilitator.settleError(err, record, x402.ErrSettlementPending)
		}
		timer := time.NewTimer(facilitator.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, facilitator.settleError(ctx.Err(), record, x402.ErrSettlementPending)
		case <-timer.C:
		}
	}
}

func (facilitator *Facilitator) checkPolicy(requirements types.PaymentRequirements) error {
	network, err := tron.NormalizeNetwork(requirements.Network)
	payTo, payErr := tron.NormalizeAddressBase58(requirements.PayTo)
	if err != nil || network != facilitator.Network() || payErr != nil || payTo != facilitator.payTo || requirements.Scheme != "exact" || requirements.MaxTimeoutSeconds < 6 || requirements.MaxTimeoutSeconds > 600 || requirements.Extra["paymentFlow"] != "upfront" {
		return exact.ErrInvalidRequirements
	}
	price, err := facilitator.policy.ParsePrice(x402.AssetAmount{Asset: requirements.Asset, Amount: requirements.Amount}, x402.Network(network))
	if err != nil {
		return exact.ErrInvalidRequirements
	}
	amount, _ := new(big.Int).SetString(price.Amount, 10)
	if amount.Cmp(facilitator.maximum) > 0 {
		return exact.ErrInvalidRequirements
	}
	return nil
}

func (facilitator *Facilitator) valid() bool {
	return facilitator != nil && facilitator.store.valid() && facilitator.policy != nil && facilitator.preflight != nil && facilitator.execution != nil && facilitator.builder != nil && facilitator.signer != nil
}

func (facilitator *Facilitator) settleError(err error, record Record, reason string) error {
	if reason == "" {
		reason = facilitatorReason(err)
	}
	transaction := ""
	if record.Transaction != nil {
		transaction = record.Transaction.ID
	}
	return protocolContextError(x402.NewSettleError(reason, record.Authorization.Payer, x402.Network(facilitator.Network()), transaction, ""), err)
}

func protocolContextError(protocol, cause error) error {
	if errors.Is(cause, context.Canceled) {
		return errors.Join(protocol, context.Canceled)
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return errors.Join(protocol, context.DeadlineExceeded)
	}
	return protocol
}

func facilitatorReason(err error) string {
	switch {
	case errors.Is(err, exact.ErrInvalidValidity), errors.Is(err, exact.ErrSettlementValidity):
		return "payment_expired"
	case errors.Is(err, exact.ErrInvalidSignature), errors.Is(err, exact.ErrSettlementSignature):
		return "invalid_signature"
	case errors.Is(err, exact.ErrInvalidRequirements), errors.Is(err, exact.ErrInvalidAuthorization):
		return "invalid_payment"
	case errors.Is(err, exact.ErrInsufficientBalance):
		return "insufficient_balance"
	case errors.Is(err, exact.ErrInsufficientAllowance):
		return "insufficient_allowance"
	case errors.Is(err, exact.ErrNonceUsed):
		return "nonce_already_used"
	case errors.Is(err, ErrConflict):
		return "settlement_conflict"
	default:
		return "settlement_unavailable"
	}
}
