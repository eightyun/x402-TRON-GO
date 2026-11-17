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
	ErrClientConfig = errors.New("exact: invalid client configuration")
	ErrClientPolicy = errors.New("exact: payment quote violates the client payment policy")
	ErrClientSign   = errors.New("exact: failed to sign payment authorization")
)

// AuthorizationSigner signs only an already computed TIP-712 digest; it must not hash it again
// or add a personal_sign prefix. It returns a 65-byte low-s r||s||v signature, must honor the
// context, must be safe for concurrent use, and must not log keys or authorization signatures.
// It belongs to a different signing boundary from the SHA-256 DigestSigner used for settlement
// transactions.
type AuthorizationSigner func(context.Context, [32]byte) ([]byte, error)

// ClientConfig fixes the trusted contracts and the payment policy. All amounts are in atomic
// token units, not USD prices. Lifetime must be a whole number of seconds, defaults to 60 seconds
// and ranges from 6 seconds to 10 minutes; the effective deadline never exceeds the quote timeout.
type ClientConfig struct {
	Authorization Config
	PayerAddress  string
	Asset         string
	PayTo         string
	MaxAmount     string
	Lifetime      time.Duration
	Signer        AuthorizationSigner `json:"-"`
}

// Client implements the official x402 v2 client scheme. It does not access nodes or databases
// and does not automatically approve token allowances. Each call creates an independent,
// chargeable authorization; it provides no aggregate budget, idempotency for business retries,
// or proof of payment.
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

// NewClient validates and copies the payment policy. External quotes cannot replace the signing
// domain, the contracts or the payment cap.
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

// Scheme returns the name the official SDK uses to register the scheme.
func (*Client) Scheme() string { return "exact" }

// Network returns the canonical network identifier used for registration.
func (client *Client) Network() string {
	if client == nil || client.verifier == nil {
		return ""
	}
	return client.verifier.network
}

// CreatePaymentPayload builds the nonce, authorization and TIP-712 digest and verifies the
// signature; HTTP retries are handled by the official SDK. Callers should pass a bounded context.
// The validity window is rechecked after signing, and errors never echo the signer's original
// message.
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
