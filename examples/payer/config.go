package main

import (
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	x402 "github.com/x402-foundation/x402/go/v2"
)

var errConfiguration = errors.New("付款示例配置无效")
var errState = errors.New("付款状态不可用；保留原目录，不创建替代授权")
var errPayment = errors.New("未确认资源交付成功；仅可恢复原请求，不据此判定未扣款")

type policy struct {
	URL       string `json:"url"`
	Network   string `json:"network"`
	Permit2   string `json:"permit2"`
	Proxy     string `json:"proxy"`
	Payer     string `json:"payer"`
	Asset     string `json:"asset"`
	PayTo     string `json:"payTo"`
	MaxAmount string `json:"maxAmount"`
}
type configuration struct {
	policy
	stateDirectory, keyFile string
	enabled                 bool
}

func loadConfig() (configuration, error) {
	c := configuration{policy: policy{URL: os.Getenv("X402_RESOURCE_URL"), Network: os.Getenv("X402_NETWORK"), Permit2: os.Getenv("X402_PERMIT2_ADDRESS"), Proxy: os.Getenv("X402_PROXY_ADDRESS"), Payer: os.Getenv("X402_PAYER_ADDRESS"), Asset: os.Getenv("X402_ASSET"), PayTo: os.Getenv("X402_PAY_TO"), MaxAmount: os.Getenv("X402_MAX_AMOUNT_ATOMIC")}, stateDirectory: os.Getenv("X402_PAYMENT_STATE_DIR"), keyFile: os.Getenv("X402_PAYER_KEY_FILE")}
	if c.Network == "" {
		c.Network = tron.NetworkNile
	}
	var err error
	c.Network, err = tron.NormalizeNetwork(c.Network)
	if err != nil || c.Network == tron.NetworkMainnet {
		return configuration{}, errConfiguration
	}
	for _, p := range []*string{&c.Permit2, &c.Proxy, &c.Payer, &c.Asset, &c.PayTo} {
		*p, err = tron.NormalizeAddressBase58(*p)
		if err != nil {
			return configuration{}, errConfiguration
		}
	}
	if _, err := exact.NewOfflineVerifier(c.authorization()); err != nil {
		return configuration{}, errConfiguration
	}
	s, err := exact.NewServer(exact.ServerConfig{Network: c.Network, Asset: c.Asset, PayTo: c.PayTo})
	if err != nil {
		return configuration{}, errConfiguration
	}
	if _, err = s.ParsePrice(c.MaxAmount, x402.Network(c.Network)); err != nil {
		return configuration{}, errConfiguration
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(c.URL, "#") || u.Opaque != "" {
		return configuration{}, errConfiguration
	}
	local := os.Getenv("X402_ALLOW_INSECURE_LOCAL_RESOURCE_HTTP")
	if local != "" && local != "false" && local != "true" {
		return configuration{}, errConfiguration
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && local == "true" && ip != nil && ip.IsLoopback()) {
		return configuration{}, errConfiguration
	}
	if !filepath.IsAbs(c.stateDirectory) || filepath.Clean(c.stateDirectory) != c.stateDirectory || c.stateDirectory == string(filepath.Separator) {
		return configuration{}, errConfiguration
	}
	switch os.Getenv("X402_ENABLE_TESTNET_PAYMENTS") {
	case "true":
		c.enabled = true
	case "", "false":
	default:
		return configuration{}, errConfiguration
	}
	return c, nil
}

func (c configuration) authorization() exact.Config {
	return exact.Config{Network: c.Network, Permit2Address: c.Permit2, ProxyAddress: c.Proxy}
}
