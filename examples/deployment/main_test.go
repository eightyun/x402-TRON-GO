package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
)

const (
	permit = "TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h"
	proxy  = "TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F"
	asset  = "TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf"
)

func testConfig(t *testing.T) (exact.PreflightConfig, *atomic.Int32, *atomic.Bool) {
	t.Helper()
	calls, fail := new(atomic.Int32), new(atomic.Bool)
	stamp := time.Now().Unix()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/jsonrpc" || r.Header.Get("TRON-PRO-API-KEY") != "test-only-api-key" {
			t.Error("只读节点传输参数错误")
		}
		if fail.Load() {
			w.WriteHeader(503)
			_, _ = io.WriteString(w, "synthetic-sensitive-node-error")
			return
		}
		var request struct {
			JSONRPC string            `json:"jsonrpc"`
			ID      int               `json:"id"`
			Method  string            `json:"method"`
			Params  []json.RawMessage `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil || request.JSONRPC != "2.0" || request.ID != 1 {
			t.Error("无效节点请求")
			return
		}
		var result interface{}
		switch request.Method {
		case "eth_chainId":
			result = "0xcd8690dc"
		case "eth_getBlockByNumber":
			result = map[string]string{"number": "0x7b", "hash": "0x000000000000007b" + strings.Repeat("11", 24), "timestamp": fmt.Sprintf("0x%x", stamp)}
		case "eth_getCode":
			var address string
			_ = json.Unmarshal(request.Params[0], &address)
			switch address {
			case "0xf62f506d1faa02e2354a9886b4a200496ee96f4b":
				result = "0x6000600055"
			case "0x3a2c916ce2b40f0ffc06cc80c0f72c0fc25be105":
				result = "0x6001600055"
			case "0xeca9bc828a3005b9a3b909f2cc5c2a54794de05f":
				result = "0x6002600055"
			default:
				t.Error("查询了非配置合约")
			}
		case "eth_call":
			var call map[string]string
			_ = json.Unmarshal(request.Params[0], &call)
			if call["from"] != "0x3a2c916ce2b40f0ffc06cc80c0f72c0fc25be105" || len(call) != 3 || len(request.Params) != 2 || string(request.Params[1]) != `"latest"` {
				t.Error("固定getter调用者/参数不符")
			}
			switch call["data"] {
			case "0x6afdd850":
				result = "0x" + strings.Repeat("0", 24) + "f62f506d1faa02e2354a9886b4a200496ee96f4b"
			case "0x2c03ae6a":
				result = "0xd97b3239a7f32295517bd14cb074edfdd188dfe5eb42f802bb26d4fd1eb12c37"
			case "0x3644e515":
				result = "0xe7b78607f21dc61c0f1743639a2e5e0389476396bb887563161db36f84149bb6"
			default:
				t.Error("部署检查不应调用付款/余额/allowance/nonce方法")
			}
		default:
			t.Error("出现非只读节点方法")
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": result})
	}))
	t.Cleanup(server.Close)
	return exact.PreflightConfig{
		Authorization: exact.Config{Network: tron.NetworkNile, Permit2Address: permit, ProxyAddress: proxy},
		NodeURL:       server.URL + "/jsonrpc", APIKey: "test-only-api-key", HTTPClient: server.Client(),
		Permit2CodeHash: "0xa9c7ddf1ae0410c3a502ed7c7152ebd190fad85ab7fe3497c68869d5079c5219",
		ProxyCodeHash:   "0x7efcce47028dabcb0d42f3a7eda8820bf6f7f4e618398c2547d52f703cafb073",
	}, calls, fail
}

func TestRunReadOnlyDeploymentCheck(t *testing.T) {
	config, calls, _ := testConfig(t)
	// 即使设置不可读的既有钱包/DB环境变量，也不能成为本命令的依赖。
	t.Setenv("X402_EXECUTOR_KEY_FILE", "/nonexistent/must-not-read-wallet.key")
	t.Setenv("X402_DATABASE_URL", "invalid-must-not-connect")
	var output bytes.Buffer
	if err := run(context.Background(), config, asset, &output); err != nil {
		t.Fatal("部署检查失败", err)
	}
	var result struct {
		ReadOnly         bool `json:"readOnly"`
		PaymentValidated bool `json:"paymentValidated"`
		exact.DeploymentResult
	}
	if json.Unmarshal(output.Bytes(), &result) != nil || !result.ReadOnly || result.PaymentValidated || result.Network != tron.NetworkNile || result.Asset != asset || result.BlockNumber != 123 || calls.Load() != 10 {
		t.Fatal("输出未明确限定只读部署结果")
	}
	if strings.Contains(output.String(), "test-only-api-key") || strings.Contains(output.String(), config.NodeURL) {
		t.Fatal("输出泄漏节点配置")
	}
}

func TestRunRejectsUnsafeConfigurationWithoutRPC(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*exact.PreflightConfig)
	}{
		{"mainnet", func(config *exact.PreflightConfig) { config.Authorization.Network = tron.NetworkMainnet }},
		{"mainnet_alias", func(config *exact.PreflightConfig) { config.Authorization.Network = "tron:0x2b6653dc" }},
		{"unknown_network", func(config *exact.PreflightConfig) { config.Authorization.Network = "tron:1" }},
		{"missing_hash", func(config *exact.PreflightConfig) { config.ProxyCodeHash = "" }},
		{"zero_hash", func(config *exact.PreflightConfig) { config.Permit2CodeHash = "0x" + strings.Repeat("0", 64) }},
		{"missing_contract", func(config *exact.PreflightConfig) { config.Authorization.Permit2Address = "" }},
		{"url_credentials", func(config *exact.PreflightConfig) { config.NodeURL = "https://user:secret@example.com/jsonrpc" }},
		{"query_credentials", func(config *exact.PreflightConfig) { config.NodeURL += "?key=secret" }},
		{"http", func(config *exact.PreflightConfig) { config.NodeURL = "http://127.0.0.1:1" }},
		{"http_override", func(config *exact.PreflightConfig) { config.AllowInsecureLocalHTTP = true }},
		{"bad_key", func(config *exact.PreflightConfig) { config.APIKey = "secret\r\nHeader:value" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, calls, _ := testConfig(t)
			test.change(&config)
			var output bytes.Buffer
			if err := run(context.Background(), config, asset, &output); err == nil || calls.Load() != 0 || output.Len() != 0 {
				t.Fatal("不安全配置仍查询或输出结果")
			}
		})
	}
}

func TestRunFailureAndOutputErrors(t *testing.T) {
	config, calls, fail := testConfig(t)
	fail.Store(true)
	var output bytes.Buffer
	err := run(context.Background(), config, asset, &output)
	failedReads := calls.Load()
	if !errors.Is(err, exact.ErrNodeRead) || strings.Contains(err.Error(), "synthetic-sensitive") || output.Len() != 0 || failedReads != 2 {
		t.Fatal("节点错误被忽略或泄漏")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, config, asset, &output); !errors.Is(err, context.Canceled) || calls.Load() != failedReads {
		t.Fatal("取消后仍访问节点")
	}
	fail.Store(false)
	if err := run(context.Background(), config, asset, failingWriter{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("未报告输出失败", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDeploymentEnvironmentAndFailureClassification(t *testing.T) {
	for _, key := range []string{"X402_NETWORK", "X402_ASSET", "X402_JSON_RPC_URL", "X402_NODE_API_KEY", "X402_PERMIT2_ADDRESS", "X402_PROXY_ADDRESS", "X402_PERMIT2_CODE_HASH", "X402_PROXY_CODE_HASH", "X402_RPC_BATCH"} {
		t.Setenv(key, "")
	}
	config, selectedAsset, err := loadConfig()
	if err != nil || config.BatchRPC {
		t.Fatal("默认批量配置无效")
	}
	if config.Authorization.Network != tron.NetworkNile || config.Timeout != 15*time.Second || selectedAsset != "" || config.Permit2CodeHash != "" || config.ProxyCodeHash != "" {
		t.Fatal("默认配置自动填充了资产/代码哈希或改变预算")
	}
	t.Setenv("X402_NETWORK", tron.NetworkShasta)
	t.Setenv("X402_ASSET", asset)
	t.Setenv("X402_NODE_API_KEY", "test-only-api-key")
	t.Setenv("X402_RPC_BATCH", "true")
	config, selectedAsset, err = loadConfig()
	if err != nil || !config.BatchRPC {
		t.Fatal("显式批量配置未生效")
	}
	if config.Authorization.Network != tron.NetworkShasta || config.APIKey != "test-only-api-key" || selectedAsset != asset {
		t.Fatal("环境配置未正确读取")
	}
	t.Setenv("X402_RPC_BATCH", "TRUE")
	if _, _, err := loadConfig(); !errors.Is(err, errConfiguration) {
		t.Fatal("无效批量开关未拒绝")
	}
	for err, want := range map[error]string{
		errConfiguration: "配置无效", exact.ErrPreflightConfig: "配置无效", exact.ErrInvalidConfig: "配置无效",
		context.Canceled: "已取消", context.DeadlineExceeded: "超时", exact.ErrChainMismatch: "链ID不符",
		exact.ErrContractMismatch: "代码、域或绑定不符", exact.ErrChainChanged: "链头变化或时间异常", exact.ErrStaleHead: "链头变化或时间异常",
		exact.ErrNodeRead: "节点读取失败", errors.New("synthetic-sensitive"): "输出或检查失败",
	} {
		if failureReason(err) != want {
			t.Fatal("错误分类未脱敏或不明确")
		}
	}
}
