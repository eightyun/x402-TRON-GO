package exact

import (
	"context"
	"errors"

	tron "github.com/eightyun/x402-tron-go"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

var (
	ErrServerConfig       = errors.New("TRON 资源服务端配置无效")
	ErrServerPrice        = errors.New("TRON 报价必须是配置资产的规范最小单位正整数")
	ErrFacilitatorSupport = errors.New("facilitator 未声明匹配的 TRON exact Permit2 能力")
)

// ServerConfig 固定可信网络、单资产与收款人；不推断美元汇率或链上合约部署。
type ServerConfig struct {
	Network string
	Asset   string
	PayTo   string
}

// Server 实现官方资源服务端机制，使用 upfront 流程在结算成功后才运行 handler。
// 它不持有私钥、访问数据库或自行证明到账；必须使用可信且核验最终结果的 facilitator。
type Server struct {
	network string
	asset   string
	payTo   string
}

var (
	_ x402.SchemeNetworkServer         = (*Server)(nil)
	_ x402.FacilitatorSupportValidator = (*Server)(nil)
)

// NewServer 校验并规范化部署方的固定报价策略，不从付款请求获取配置。
func NewServer(config ServerConfig) (*Server, error) {
	network, networkErr := tron.NormalizeNetwork(config.Network)
	_, assetErr := configuredAddress(config.Asset)
	_, payToErr := configuredAddress(config.PayTo)
	if networkErr != nil || assetErr != nil || payToErr != nil {
		return nil, ErrServerConfig
	}
	asset, _ := tron.NormalizeAddressBase58(config.Asset)
	payTo, _ := tron.NormalizeAddressBase58(config.PayTo)
	return &Server{network: network, asset: asset, payTo: payTo}, nil
}

// Network 返回官方 SDK 注册使用的规范 CAIP-2 网络标识。
func (server *Server) Network() string {
	if server == nil {
		return ""
	}
	return server.network
}

// Scheme 返回机制名称。
func (*Server) Scheme() string { return "exact" }

// DefaultAssetTransferMethod 不默认为 TRON 代币并不普遍支持的 EIP-3009。
func (*Server) DefaultAssetTransferMethod() string { return "permit2" }

// PaymentFlows 只允许 handler 前结算；每次返回独立配置，调用方不能修改内部策略。
func (*Server) PaymentFlows() map[string]x402.PaymentFlowConfig {
	return map[string]x402.PaymentFlowConfig{"permit2": {
		Supported: []x402.PaymentFlowName{x402.PaymentFlowUpfront}, Default: x402.PaymentFlowUpfront,
	}}
}

// ParsePrice 接受最小单位字符串、官方 AssetAmount 或同结构的严格 map，不接受浮点/美元价格。
func (server *Server) ParsePrice(price x402.Price, network x402.Network) (x402.AssetAmount, error) {
	if server == nil || server.network == "" {
		return x402.AssetAmount{}, ErrServerConfig
	}
	if string(network) != server.network {
		return x402.AssetAmount{}, ErrServerPrice
	}
	var parsed x402.AssetAmount
	switch value := price.(type) {
	case string:
		parsed = x402.AssetAmount{Asset: server.asset, Amount: value}
	case x402.AssetAmount:
		parsed = value
	case *x402.AssetAmount:
		if value == nil {
			return x402.AssetAmount{}, ErrServerPrice
		}
		parsed = *value
	case map[string]interface{}:
		if len(value) != 2 && len(value) != 3 {
			return x402.AssetAmount{}, ErrServerPrice
		}
		var assetOK, amountOK bool
		parsed.Asset, assetOK = value["asset"].(string)
		parsed.Amount, amountOK = value["amount"].(string)
		if !assetOK || !amountOK {
			return x402.AssetAmount{}, ErrServerPrice
		}
		if extra, exists := value["extra"]; exists {
			var ok bool
			parsed.Extra, ok = extra.(map[string]interface{})
			if !ok {
				return x402.AssetAmount{}, ErrServerPrice
			}
		} else if len(value) != 2 {
			return x402.AssetAmount{}, ErrServerPrice
		}
	default:
		return x402.AssetAmount{}, ErrServerPrice
	}
	asset, err := tron.NormalizeAddressBase58(parsed.Asset)
	amount, amountErr := uint256Word(parsed.Amount, false)
	if err != nil || asset != server.asset || amountErr != nil || amount == [32]byte{} || !serverInputExtra(parsed.Extra) {
		return x402.AssetAmount{}, ErrServerPrice
	}
	return x402.AssetAmount{Asset: server.asset, Amount: parsed.Amount, Extra: upfrontExtra()}, nil
}

// ValidateFacilitatorSupport 要求 v2/exact/同网络，并明确声明支持 Permit2；不复制其其他元数据。
// supportedAssetTransferMethods 采用已有 TRON 生态能力字段，声明本身不是合约或到账证明。
func (server *Server) ValidateFacilitatorSupport(network x402.Network, kind types.SupportedKind, _ []string) error {
	if server == nil || server.network == "" {
		return ErrServerConfig
	}
	if string(network) != server.network || kind.Network != server.network || kind.X402Version != 2 || kind.Scheme != "exact" {
		return ErrFacilitatorSupport
	}
	supported := false
	switch methods := kind.Extra["supportedAssetTransferMethods"].(type) {
	case []string:
		for _, method := range methods {
			supported = supported || method == "permit2"
		}
	case []interface{}:
		for _, method := range methods {
			value, ok := method.(string)
			if !ok {
				return ErrFacilitatorSupport
			}
			supported = supported || value == "permit2"
		}
	}
	if !supported {
		return ErrFacilitatorSupport
	}
	return nil
}

// EnhancePaymentRequirements 固定报价身份及 upfront；扩展能力清单仅用于发现，不混入支付 extra。
func (server *Server) EnhancePaymentRequirements(ctx context.Context, requirements types.PaymentRequirements, kind types.SupportedKind, extensions []string) (types.PaymentRequirements, error) {
	if err := ctx.Err(); err != nil {
		return types.PaymentRequirements{}, err
	}
	if server == nil || server.network == "" {
		return types.PaymentRequirements{}, ErrServerConfig
	}
	if err := server.ValidateFacilitatorSupport(x402.Network(requirements.Network), kind, extensions); err != nil {
		return types.PaymentRequirements{}, err
	}
	asset, assetErr := tron.NormalizeAddressBase58(requirements.Asset)
	payTo, payToErr := tron.NormalizeAddressBase58(requirements.PayTo)
	amount, amountErr := uint256Word(requirements.Amount, false)
	if requirements.Scheme != "exact" || assetErr != nil || asset != server.asset || payToErr != nil || payTo != server.payTo || amountErr != nil || amount == [32]byte{} || requirements.MaxTimeoutSeconds < 6 || !serverInputExtra(requirements.Extra) {
		return types.PaymentRequirements{}, ErrInvalidRequirements
	}
	requirements.Asset, requirements.PayTo = server.asset, server.payTo
	requirements.Extra = upfrontExtra()
	return requirements, nil
}

func serverInputExtra(extra map[string]interface{}) bool {
	return len(extra) == 0 || validExtra(extra)
}

func upfrontExtra() map[string]interface{} {
	return map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"}
}
