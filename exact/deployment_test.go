package exact_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
)

func TestDeploymentCheckWithoutPaymentAuthorization(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	// 没有付款人/签名/nonce；固定getter的from使用配置代理，不伪造有效付款。
	input.Signature = ""
	input.Authorization.Nonce = ""
	input.Authorization.From = input.Authorization.Spender
	server, calls := testNode(t, input, nil)
	verifier := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc"))
	assetHex, err := tron.NormalizeAddressHex(input.Requirements.Asset)
	if err != nil {
		t.Fatal(err)
	}
	for _, asset := range []string{input.Requirements.Asset, assetHex} {
		result, err := verifier.CheckDeployment(context.Background(), asset, time.Unix(input.Now, 0))
		if err != nil || result.Network != input.Config.Network || result.Asset != input.Requirements.Asset || result.BlockNumber != 123 || result.BlockHash != headFixture(input, 123)["hash"] || !result.BlockTimestamp.Equal(time.Unix(input.Now, 0)) {
			t.Fatal("无钱包部署检查失败", err)
		}
		if result.Permit2Address != "41"+permitWire[2:] || result.ProxyAddress != "41"+input.Authorization.Spender[2:] || result.Permit2CodeHash != permitHash || result.ProxyCodeHash != proxyHash {
			t.Fatal("部署观测未绑定被检查的合约/代码哈希")
		}
	}
	if calls.Load() != 20 {
		t.Fatal("部署检查不应访问余额/allowance/nonce或模拟settle")
	}
}

func TestDeploymentRejectsNodeAndContractFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(nodeRequest, int, *nodeReply)
		want   error
	}{
		{"wrong_chain", func(query nodeRequest, _ int, reply *nodeReply) {
			if query.Method == "eth_chainId" {
				reply.Result = "0x2b6653dc"
			}
		}, exact.ErrChainMismatch},
		{"chain_changed", func(query nodeRequest, n int, reply *nodeReply) {
			if query.Method == "eth_chainId" && n == 2 {
				reply.Result = "0x2b6653dc"
			}
		}, exact.ErrChainMismatch},
		{"head_changed", func(query nodeRequest, n int, reply *nodeReply) {
			if query.Method == "eth_getBlockByNumber" && n == 2 {
				reply.Result = headFixture(loadVector(t), 124)
			}
		}, exact.ErrChainChanged},
		{"stale_head", func(query nodeRequest, _ int, reply *nodeReply) {
			if query.Method == "eth_getBlockByNumber" {
				head := headFixture(loadVector(t), 123)
				head["timestamp"] = "0x1"
				reply.Result = head
			}
		}, exact.ErrStaleHead},
		{"wrong_runtime", func(query nodeRequest, _ int, reply *nodeReply) {
			if query.Method == "eth_getCode" {
				reply.Result = "0x6003600055"
			}
		}, exact.ErrContractMismatch},
		{"asset_has_no_code", func(query nodeRequest, _ int, reply *nodeReply) {
			if codeAddress(query) == "0xeca9bc828a3005b9a3b909f2cc5c2a54794de05f" {
				reply.Result = "0x"
			}
		}, exact.ErrContractMismatch},
		{"wrong_permit_binding", func(query nodeRequest, _ int, reply *nodeReply) {
			if selector(query) == "0x6afdd850" {
				reply.Result = "0x" + strings.Repeat("0", 64)
			}
		}, exact.ErrContractMismatch},
		{"wrong_witness", func(query nodeRequest, _ int, reply *nodeReply) {
			if selector(query) == "0x2c03ae6a" {
				reply.Result = "0x" + strings.Repeat("0", 64)
			}
		}, exact.ErrContractMismatch},
		{"wrong_domain", func(query nodeRequest, _ int, reply *nodeReply) {
			if selector(query) == "0x3644e515" {
				reply.Result = "0x" + strings.Repeat("0", 64)
			}
		}, exact.ErrContractMismatch},
		{"ambiguous_response", func(_ nodeRequest, _ int, reply *nodeReply) {
			reply.raw = `{"jsonrpc":"2.0","id":1,"result":"0xcd8690dc","result":"0xcd8690dc"}`
		}, exact.ErrNodeRead},
		{"transport_failure", func(_ nodeRequest, _ int, reply *nodeReply) {
			reply.status, reply.raw = 503, "synthetic-sensitive-node-detail"
		}, exact.ErrNodeRead},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := loadVector(t)
			input.Authorization.From = input.Authorization.Spender
			server, _ := testNode(t, input, test.mutate)
			result, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).CheckDeployment(context.Background(), input.Requirements.Asset, time.Unix(input.Now, 0))
			if !errors.Is(err, test.want) || result != (exact.DeploymentResult{}) || strings.Contains(err.Error(), "synthetic-sensitive") {
				t.Fatal("部署检查错误被忽略或泄漏原文", err)
			}
		})
	}
}

func TestDeploymentBadInputsDoNotReadNode(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	server, calls := testNode(t, input, nil)
	verifier := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc"))
	for _, asset := range []string{"", "invalid", "0x" + strings.Repeat("1", 40), "41" + strings.Repeat("0", 40), input.Config.Permit2Address, input.Config.ProxyAddress} {
		if result, err := verifier.CheckDeployment(context.Background(), asset, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrPreflightConfig) || result != (exact.DeploymentResult{}) {
			t.Fatal("部署检查接受无效资产")
		}
	}
	for _, invalid := range []*exact.PreflightVerifier{nil, {}} {
		if _, err := invalid.CheckDeployment(context.Background(), input.Requirements.Asset, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrPreflightConfig) {
			t.Fatal("零值部署检查未拒绝")
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := verifier.CheckDeployment(ctx, input.Requirements.Asset, time.Unix(input.Now, 0)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("过期上下文未保留")
	}
	if calls.Load() != 0 {
		t.Fatal("无效参数仍访问节点")
	}
}

func TestDeploymentFinalFreshnessAndTotalBudget(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	input.Authorization.From = input.Authorization.Spender
	server, calls := testNode(t, input, nil)
	config := preflightConfig(input, server.URL+"/jsonrpc")
	// 初始恰好60秒可接受，但整轮RPC耗时必须计入最终新鲜度，不能只检查第一次。
	result, err := preflightFor(t, config).CheckDeployment(context.Background(), input.Requirements.Asset, time.Unix(input.Now+60, 0))
	if !errors.Is(err, exact.ErrStaleHead) || result != (exact.DeploymentResult{}) || calls.Load() != 10 {
		t.Fatal("部署检查忽略最终链头时效", err)
	}
	config.Timeout = time.Nanosecond
	result, err = preflightFor(t, config).CheckDeployment(context.Background(), input.Requirements.Asset, time.Unix(input.Now, 0))
	if !errors.Is(err, context.DeadlineExceeded) || result != (exact.DeploymentResult{}) {
		t.Fatal("部署检查未遵守整轮预算", err)
	}
}

func TestDeploymentCancellationAndConcurrentChecks(t *testing.T) {
	input := loadVector(t)
	input.Authorization.From = input.Authorization.Spender
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, _ := testNode(t, input, func(query nodeRequest, _ int, _ *nodeReply) {
		if selector(query) == "0x3644e515" {
			cancel()
		}
	})
	if _, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).CheckDeployment(ctx, input.Requirements.Asset, time.Unix(input.Now, 0)); !errors.Is(err, context.Canceled) {
		t.Fatal("部署检查取消未保留")
	}
	concurrent, calls := testNode(t, input, nil)
	verifier := preflightFor(t, preflightConfig(input, concurrent.URL+"/jsonrpc"))
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if _, err := verifier.CheckDeployment(context.Background(), input.Requirements.Asset, time.Unix(input.Now, 0)); err != nil {
				t.Error("并发部署检查失败", err)
			}
		})
	}
	workers.Wait()
	if calls.Load() != 80 {
		t.Fatal("并发检查请求数不符")
	}
}
