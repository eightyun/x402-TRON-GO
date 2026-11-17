package postgres

import (
	"context"
	"errors"

	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

var (
	ErrLocalClientConfig   = errors.New("postgres: invalid local facilitator client configuration")
	ErrLocalClientCapacity = errors.New("postgres: local facilitator client at capacity")
)

type localClient struct {
	verify    func(context.Context, []byte, []byte) (*x402.VerifyResponse, error)
	settle    func(context.Context, []byte, []byte) (*x402.SettleResponse, error)
	supported func() x402.SupportedResponse
	active    chan struct{}
}

var _ x402.FacilitatorClient = (*localClient)(nil)

// NewLocalClient connects an existing mechanism to the official resource server without an HTTP
// service, token or listening port. Construction only registers the official core and does not
// contact nodes or the database or sign anything. A maxConcurrent of 0 means 8; allowed values
// are 1 to 64. The capacity is a separate limit per client and does not replace database
// idempotency, application authentication or an aggregate fee budget.
func NewLocalClient(mechanism *Facilitator, maxConcurrent int) (x402.FacilitatorClient, error) {
	if maxConcurrent == 0 {
		maxConcurrent = 8
	}
	if !mechanism.valid() || maxConcurrent < 1 || maxConcurrent > 64 {
		return nil, ErrLocalClientConfig
	}
	// 绑定方法保留同一官方core，不复制协议路由或定义额外机制接口。
	core := x402.Newx402Facilitator().Register([]x402.Network{x402.Network(mechanism.Network())}, mechanism)
	return &localClient{verify: core.Verify, settle: core.Settle, supported: core.GetSupported, active: make(chan struct{}, maxConcurrent)}, nil
}

func (client *localClient) Verify(ctx context.Context, payload, requirements []byte) (*x402.VerifyResponse, error) {
	if err := client.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-client.active }()
	if !validLocalRequest(payload, requirements) {
		return nil, x402.NewVerifyError("invalid_request", "", "")
	}
	return client.verify(ctx, payload, requirements)
}

func (client *localClient) Settle(ctx context.Context, payload, requirements []byte) (*x402.SettleResponse, error) {
	if err := client.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { <-client.active }()
	if !validLocalRequest(payload, requirements) {
		return nil, x402.NewSettleError("invalid_request", "", "", "", "")
	}
	return client.settle(ctx, payload, requirements)
}

func (client *localClient) GetSupported(ctx context.Context) (x402.SupportedResponse, error) {
	if err := ctx.Err(); err != nil {
		return x402.SupportedResponse{}, err
	}
	return client.supported(), nil
}

func (client *localClient) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case client.active <- struct{}{}:
		return nil
	default:
		return ErrLocalClientCapacity
	}
}

func validLocalRequest(payload, requirements []byte) bool {
	const limit = 64 << 10
	if len(payload) == 0 || len(requirements) == 0 || len(requirements) > limit || len(payload) > limit-len(requirements) || !uniqueHTTPJSON(payload) || !uniqueHTTPJSON(requirements) {
		return false
	}
	var payment *types.PaymentPayload
	var quote *types.PaymentRequirements
	return strictPaymentPayload(payload, &payment) && strictPaymentRequirements(requirements, &quote) && payment != nil && quote != nil && payment.X402Version == 2
}
