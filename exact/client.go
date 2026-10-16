package exact

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

var (
	ErrClientConfig = errors.New("TRON 客户端配置无效")
	ErrClientPolicy = errors.New("支付报价超出客户端授权策略")
	ErrClientSign   = errors.New("付款授权签名失败")
)

// AuthorizationSigner 只签署已计算的 TIP-712 摘要，不得再次哈希或添加 personal_sign 前缀。
// 返回低 s 的 65 字节 r||s||v；须遵守 context、支持并发且不得记录密钥或授权签名。
// 它与结算交易的 SHA-256 DigestSigner 属于不同签名边界。
type AuthorizationSigner func(context.Context, [32]byte) ([]byte, error)

// ClientConfig 固定可信合约及付款策略；金额均为代币最小单位，不是美元价格。
// Lifetime 为整秒，默认 60 秒，范围 6 秒至 10 分钟，实际期限不超过报价 timeout。
type ClientConfig struct {
	Authorization Config
	PayerAddress  string
	Asset         string
	PayTo         string
	MaxAmount     string
	Lifetime      time.Duration
	Signer        AuthorizationSigner `json:"-"`
}

// Client 实现官方 x402 v2 客户端机制，不访问节点、数据库或自动批准代币授权。
// 每次调用创建独立可扣款授权，不提供累计预算、业务重试幂等或到账证明。
type Client struct {
	verifier *OfflineVerifier
	payer    [20]byte
	asset    [20]byte
	payTo    [20]byte
	max      [32]byte
	lifetime time.Duration
	signer   AuthorizationSigner
}

var _ x402.SchemeNetworkClient = (*Client)(nil)

// NewClient 验证并复制付款策略，外部报价不能替换签名域、合约或付款上限。
func NewClient(config ClientConfig) (*Client, error) {
	verifier, verifyErr := NewOfflineVerifier(config.Authorization)
	payer, payerErr := configuredAddress(config.PayerAddress)
	asset, assetErr := configuredAddress(config.Asset)
	payTo, payToErr := configuredAddress(config.PayTo)
	maxAmount, maxErr := uint256Word(config.MaxAmount, false)
	if config.Lifetime == 0 {
		config.Lifetime = time.Minute
	}
	if verifyErr != nil || payerErr != nil || assetErr != nil || payToErr != nil || maxErr != nil || maxAmount == [32]byte{} || config.Signer == nil || config.Lifetime < 6*time.Second || config.Lifetime > 10*time.Minute || config.Lifetime%time.Second != 0 {
		return nil, ErrClientConfig
	}
	return &Client{
		verifier: verifier, payer: payer, asset: asset, payTo: payTo, max: maxAmount,
		lifetime: config.Lifetime, signer: config.Signer,
	}, nil
}

// Scheme 返回官方 SDK 用于机制注册的名称。
func (*Client) Scheme() string { return "exact" }

// Network 返回注册用的规范网络标识。
func (client *Client) Network() string {
	if client == nil || client.verifier == nil {
		return ""
	}
	return client.verifier.network
}

// CreatePaymentPayload 封装 nonce、授权、TIP-712 摘要和签名核验，由官方 SDK 负责 HTTP 重试。
// 调用者应提供有界 context；签名完成后重新检查时效，错误不回显签名器原文。
func (client *Client) CreatePaymentPayload(ctx context.Context, requirements types.PaymentRequirements, payloadCtx x402.PaymentPayloadContext) (types.PaymentPayload, error) {
	if client == nil || client.verifier == nil || client.signer == nil {
		return types.PaymentPayload{}, ErrClientConfig
	}
	if err := ctx.Err(); err != nil {
		return types.PaymentPayload{}, err
	}
	asset, payTo, amount, err := client.verifier.requirements(requirements)
	if err != nil || !validExtra(requirements.Extra) || len(payloadCtx.Extensions) != 0 {
		return types.PaymentPayload{}, ErrInvalidRequirements
	}
	if asset != client.asset || payTo != client.payTo || bytes.Compare(amount[:], client.max[:]) > 0 {
		return types.PaymentPayload{}, ErrClientPolicy
	}
	if payloadCtx.MaxAmountPerPayment != "" {
		capAmount, capErr := uint256Word(payloadCtx.MaxAmountPerPayment, false)
		if capErr != nil || bytes.Compare(amount[:], capAmount[:]) > 0 {
			return types.PaymentPayload{}, ErrClientPolicy
		}
	}
	seconds := min(int64(client.lifetime/time.Second), int64(requirements.MaxTimeoutSeconds))
	if seconds < 6 {
		return types.PaymentPayload{}, ErrInvalidValidity
	}
	// 已验证的 extra 只含允许的字符串字段，复制后不再共享调用方的 map。
	extra := make(map[string]interface{}, len(requirements.Extra))
	for key, value := range requirements.Extra {
		extra[key] = value
	}
	requirements.Extra = extra
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return types.PaymentPayload{}, ErrClientSign
	}
	now := time.Now()
	if now.Unix() < 0 {
		return types.PaymentPayload{}, ErrInvalidValidity
	}
	authorization := Permit2Authorization{
		From:      "0x" + hex.EncodeToString(client.payer[:]),
		Permitted: TokenPermissions{Token: "0x" + hex.EncodeToString(asset[:]), Amount: requirements.Amount},
		Spender:   "0x" + hex.EncodeToString(client.verifier.proxy[:]),
		Nonce:     "0x" + hex.EncodeToString(nonce[:]), Deadline: strconv.FormatInt(now.Unix()+seconds, 10),
		Witness: Witness{To: "0x" + hex.EncodeToString(payTo[:]), ValidAfter: "0"},
	}
	digest, err := client.verifier.Digest(authorization)
	if err != nil {
		return types.PaymentPayload{}, err
	}
	signature, err := client.signer(ctx, digest)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return types.PaymentPayload{}, ctxErr
	}
	if err != nil {
		// 仅保留标准取消/超时标识，不传出外部签名器的原始消息。
		if errors.Is(err, context.Canceled) {
			return types.PaymentPayload{}, errors.Join(ErrClientSign, context.Canceled)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return types.PaymentPayload{}, errors.Join(ErrClientSign, context.DeadlineExceeded)
		}
		return types.PaymentPayload{}, ErrClientSign
	}
	if len(signature) != 65 {
		return types.PaymentPayload{}, ErrInvalidSignature
	}
	payload := types.PaymentPayload{
		X402Version: 2, Accepted: requirements,
		Payload: map[string]interface{}{
			"signature": "0x" + hex.EncodeToString(signature),
			"permit2Authorization": map[string]interface{}{
				"from": authorization.From, "spender": authorization.Spender,
				"nonce": authorization.Nonce, "deadline": authorization.Deadline,
				"permitted": map[string]interface{}{"token": authorization.Permitted.Token, "amount": authorization.Permitted.Amount},
				"witness":   map[string]interface{}{"to": authorization.Witness.To, "validAfter": authorization.Witness.ValidAfter},
			},
		},
	}
	if _, err := client.verifier.VerifyAuthorization(payload, requirements, time.Now()); err != nil {
		return types.PaymentPayload{}, err
	}
	if err := ctx.Err(); err != nil {
		return types.PaymentPayload{}, err
	}
	return payload, nil
}
