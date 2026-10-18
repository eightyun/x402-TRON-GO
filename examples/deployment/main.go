// deployment 示例只读检查测试网部署，不加载钱包、连接数据库、签名或广播。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
)

var errConfiguration = errors.New("部署检查配置无效")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, asset, err := loadConfig()
	if err == nil {
		err = run(ctx, config, asset, os.Stdout)
	}
	if err != nil {
		// 只输出固定分类，绝不回显节点URL、API key、响应正文或环境变量。
		_, _ = fmt.Fprintln(os.Stderr, "部署检查未通过："+failureReason(err)+"；未加载钱包或发起链上交易。")
		os.Exit(1)
	}
}

func loadConfig() (exact.PreflightConfig, string, error) {
	batch := os.Getenv("X402_RPC_BATCH")
	if batch != "" && batch != "true" && batch != "false" {
		return exact.PreflightConfig{}, "", errConfiguration
	}
	network := os.Getenv("X402_NETWORK")
	if network == "" {
		network = tron.NetworkNile
	}
	config := exact.PreflightConfig{
		Authorization: exact.Config{Network: network, Permit2Address: os.Getenv("X402_PERMIT2_ADDRESS"), ProxyAddress: os.Getenv("X402_PROXY_ADDRESS")},
		NodeURL:       os.Getenv("X402_JSON_RPC_URL"), APIKey: os.Getenv("X402_NODE_API_KEY"),
		Permit2CodeHash: os.Getenv("X402_PERMIT2_CODE_HASH"), ProxyCodeHash: os.Getenv("X402_PROXY_CODE_HASH"), Timeout: 15 * time.Second, BatchRPC: batch == "true",
	}
	return config, os.Getenv("X402_ASSET"), nil
}

func run(ctx context.Context, config exact.PreflightConfig, asset string, output io.Writer) error {
	network, err := tron.NormalizeNetwork(config.Authorization.Network)
	if err != nil || network == tron.NetworkMainnet || config.AllowInsecureLocalHTTP {
		return errConfiguration
	}
	config.Timeout = 15 * time.Second
	verifier, err := exact.NewPreflightVerifier(config)
	if err != nil {
		return err
	}
	result, err := verifier.CheckDeployment(ctx, asset, time.Now())
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(struct {
		ReadOnly         bool `json:"readOnly"`
		PaymentValidated bool `json:"paymentValidated"`
		exact.DeploymentResult
	}{ReadOnly: true, PaymentValidated: false, DeploymentResult: result})
}

func failureReason(err error) string {
	switch {
	case errors.Is(err, errConfiguration), errors.Is(err, exact.ErrPreflightConfig), errors.Is(err, exact.ErrInvalidConfig):
		return "配置无效"
	case errors.Is(err, context.Canceled):
		return "已取消"
	case errors.Is(err, context.DeadlineExceeded):
		return "超时"
	case errors.Is(err, exact.ErrChainMismatch):
		return "链ID不符"
	case errors.Is(err, exact.ErrContractMismatch):
		return "代码、域或绑定不符"
	case errors.Is(err, exact.ErrChainChanged), errors.Is(err, exact.ErrStaleHead):
		return "链头变化或时间异常"
	case errors.Is(err, exact.ErrNodeRead):
		return "节点读取失败"
	default:
		return "输出或检查失败"
	}
}
