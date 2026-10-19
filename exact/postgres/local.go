package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

var (
	ErrLocalClientConfig   = errors.New("进程内 facilitator 客户端配置无效")
	ErrLocalClientCapacity = errors.New("进程内 facilitator 并发容量已满")
)

type localClient struct {
	verify    func(context.Context, []byte, []byte) (*x402.VerifyResponse, error)
	settle    func(context.Context, []byte, []byte) (*x402.SettleResponse, error)
	supported func() x402.SupportedResponse
	active    chan struct{}
}

var _ x402.FacilitatorClient = (*localClient)(nil)

// NewLocalClient 将现有机制接到官方资源服务端，无需HTTP服务、令牌或监听端口。
// 构造只注册官方core，不访问节点/数据库或签名。maxConcurrent为0时取8，允许1~64。
// 容量为每个client独立上限，不替代数据库幂等、应用鉴权或累计费用预算。
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
	decode := func(data []byte, output interface{}) bool {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		return decoder.Decode(output) == nil
	}
	return decode(payload, &payment) && decode(requirements, &quote) && payment != nil && quote != nil && payment.X402Version == 2
}
