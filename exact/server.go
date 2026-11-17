package exact

import (
	"context"
	"errors"

	tron "github.com/eightyun/x402-tron-go"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

var (
	ErrServerConfig       = errors.New("exact: invalid resource server configuration")
	ErrServerPrice        = errors.New("exact: price must be a canonical positive atomic amount of the configured asset")
	ErrFacilitatorSupport = errors.New("exact: facilitator does not advertise matching TRON exact Permit2 support")
)

// ServerConfig fixes the trusted network, a single asset and the recipient. It does not infer USD
// exchange rates or on-chain contract deployments.
type ServerConfig struct {
	Network string
	Asset   string
	PayTo   string
}

// Server implements the official resource server scheme and uses the upfront flow, so the handler
// runs only after settlement succeeds. It holds no private keys, does not access databases and
// does not prove payment by itself; a trusted facilitator that verifies the final result must be
// used.
type Server struct {
	network string
	asset   string
	payTo   string
}

var (
	_ x402.SchemeNetworkServer         = (*Server)(nil)
	_ x402.FacilitatorSupportValidator = (*Server)(nil)
)

// NewServer validates and normalizes the deployer's fixed pricing policy. Configuration is never
// taken from payment requests.
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

// Network returns the canonical CAIP-2 network identifier used for registration with the official
// SDK.
func (server *Server) Network() string {
	if server == nil {
		return ""
	}
	return server.network
}

// Scheme returns the scheme name.
func (*Server) Scheme() string { return "exact" }

// DefaultAssetTransferMethod returns permit2 rather than defaulting to EIP-3009, which TRON tokens
// do not commonly support.
func (*Server) DefaultAssetTransferMethod() string { return "permit2" }

// PaymentFlows allows only settlement before the handler runs. It returns a fresh configuration
// on each call, so callers cannot modify the internal policy.
func (*Server) PaymentFlows() map[string]x402.PaymentFlowConfig {
	return map[string]x402.PaymentFlowConfig{"permit2": {
		Supported: []x402.PaymentFlowName{x402.PaymentFlowUpfront}, Default: x402.PaymentFlowUpfront,
	}}
}

// ParsePrice accepts an atomic-unit string, an official AssetAmount, or a strict map with the same
// structure. Floating-point and USD prices are rejected.
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

// ValidateFacilitatorSupport requires x402 v2, the exact scheme, the same network and an explicit
// declaration of Permit2 support; other metadata is not copied. It uses the existing TRON
// ecosystem capability field supportedAssetTransferMethods; the declaration itself is not proof
// of contracts or of payment.
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

// EnhancePaymentRequirements pins the quote identity and the upfront flow. The extensions list is
// used only for discovery and is not merged into the payment extra.
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
