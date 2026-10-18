package exact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eightyun/x402-tron-go/exact"
)

const (
	// 以下代码及哈希仅用于本地模拟节点；哈希与域由 TronWeb 独立计算，不是部署配置。
	permitHash  = "0xa9c7ddf1ae0410c3a502ed7c7152ebd190fad85ab7fe3497c68869d5079c5219"
	proxyHash   = "0x7efcce47028dabcb0d42f3a7eda8820bf6f7f4e618398c2547d52f703cafb073"
	permitWire  = "0xf62f506d1faa02e2354a9886b4a200496ee96f4b"
	domainWord  = "0xe7b78607f21dc61c0f1743639a2e5e0389476396bb887563161db36f84149bb6"
	witnessWord = "0xd97b3239a7f32295517bd14cb074edfdd188dfe5eb42f802bb26d4fd1eb12c37"
)

type nodeRequest struct {
	JSONRPC string            `json:"jsonrpc"`
	ID      int               `json:"id"`
	Method  string            `json:"method"`
	Params  []json.RawMessage `json:"params"`
}

type nodeReply struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result"`
	Error   interface{} `json:"error,omitempty"`
	status  int
	raw     string
}

func headFixture(input vector, number uint64) map[string]string {
	return map[string]string{
		"number":    fmt.Sprintf("0x%x", number),
		"hash":      fmt.Sprintf("0x%016x%s", number, strings.Repeat("11", 24)),
		"timestamp": fmt.Sprintf("0x%x", input.Now),
	}
}

func addressABI(address string) string { return strings.Repeat("0", 24) + address[2:] }

func testNode(t *testing.T, input vector, mutate func(nodeRequest, int, *nodeReply)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var lock sync.Mutex
	counts := make(map[string]int)
	calls := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		if request.Method != http.MethodPost || request.URL.Path != "/jsonrpc" || request.Header.Get("TRON-PRO-API-KEY") != "public-unit-test-placeholder" {
			t.Error("只读 RPC 请求方法、路径或 API key 头不符")
		}
		var query nodeRequest
		if json.NewDecoder(request.Body).Decode(&query) != nil || query.JSONRPC != "2.0" || query.ID != 1 {
			t.Error("请求不是预期的 JSON-RPC 2.0")
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		lock.Lock()
		counts[query.Method]++
		occurrence := counts[query.Method]
		lock.Unlock()
		reply := nodeReply{JSONRPC: "2.0", ID: 1, status: http.StatusOK}
		switch query.Method {
		case "eth_chainId":
			if len(query.Params) != 0 {
				t.Error("chainId 参数不为空")
			}
			reply.Result = "0xcd8690dc"
		case "eth_getBlockByNumber":
			if len(query.Params) != 2 || string(query.Params[0]) != `"latest"` || string(query.Params[1]) != "false" {
				t.Error("链头参数不符")
			}
			reply.Result = headFixture(input, 123)
		case "eth_getCode":
			if len(query.Params) != 2 || string(query.Params[1]) != `"latest"` {
				t.Error("代码查询必须使用 latest")
				return
			}
			var address string
			if json.Unmarshal(query.Params[0], &address) != nil {
				t.Error("代码查询地址无效")
				return
			}
			switch address {
			case permitWire:
				reply.Result = "0x6000600055"
			case input.Authorization.Spender:
				reply.Result = "0x6001600055"
			case input.Authorization.Permitted.Token:
				reply.Result = "0x6002600055"
			default:
				t.Errorf("查询了未预期的合约：%s", address)
			}
		case "eth_call":
			if len(query.Params) != 2 || string(query.Params[1]) != `"latest"` {
				t.Error("调用必须使用 latest")
				return
			}
			var call map[string]string
			if json.Unmarshal(query.Params[0], &call) != nil || call["from"] != input.Authorization.From {
				t.Error("eth_call 调用者不符")
				return
			}
			data := call["data"]
			if len(data) < 10 {
				t.Error("调用缺少 selector")
				return
			}
			payerWord := addressABI(input.Authorization.From)
			expected := ""
			to := ""
			switch data[:10] {
			case "0x6afdd850":
				expected, to, reply.Result = "0x6afdd850", input.Authorization.Spender, "0x"+addressABI(permitWire)
			case "0x2c03ae6a":
				expected, to, reply.Result = "0x2c03ae6a", input.Authorization.Spender, witnessWord
			case "0x3644e515":
				expected, to, reply.Result = "0x3644e515", permitWire, domainWord
			case "0x70a08231":
				expected, to, reply.Result = "0x70a08231"+payerWord, input.Authorization.Permitted.Token, fmt.Sprintf("0x%064x", 1000)
			case "0xdd62ed3e":
				expected, to, reply.Result = "0xdd62ed3e"+payerWord+addressABI(permitWire), input.Authorization.Permitted.Token, fmt.Sprintf("0x%064x", 1000)
			case "0x4fe02b44":
				nonce, ok := new(big.Int).SetString(input.Authorization.Nonce, 0)
				if !ok {
					t.Error("测试 nonce 无效")
					return
				}
				position := new(big.Int).Rsh(nonce, 8)
				expected, to, reply.Result = "0x4fe02b44"+payerWord+fmt.Sprintf("%064x", position), permitWire, "0x"+strings.Repeat("0", 64)
			default:
				t.Errorf("调用了未预期的方法：%s", data[:10])
			}
			if data != expected || call["to"] != to {
				t.Errorf("ABI calldata/目标不符：%s -> %s", data, call["to"])
			}
		default:
			t.Errorf("出现非预期或非只读方法：%s", query.Method)
		}
		if mutate != nil {
			mutate(query, occurrence, &reply)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(reply.status)
		if reply.raw != "" {
			_, _ = writer.Write([]byte(reply.raw))
			return
		}
		if err := json.NewEncoder(writer).Encode(reply); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	return server, calls
}

func preflightConfig(input vector, endpoint string) exact.PreflightConfig {
	return exact.PreflightConfig{
		Authorization: input.Config, NodeURL: endpoint, APIKey: "public-unit-test-placeholder",
		Permit2CodeHash: permitHash, ProxyCodeHash: proxyHash, AllowInsecureLocalHTTP: true,
	}
}

func preflightFor(t testing.TB, config exact.PreflightConfig) *exact.PreflightVerifier {
	t.Helper()
	verifier, err := exact.NewPreflightVerifier(config)
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func selector(query nodeRequest) string {
	if query.Method != "eth_call" || len(query.Params) == 0 {
		return ""
	}
	var call map[string]string
	if json.Unmarshal(query.Params[0], &call) != nil || len(call["data"]) < 10 {
		return ""
	}
	return call["data"][:10]
}

func codeAddress(query nodeRequest) string {
	var address string
	if query.Method == "eth_getCode" && len(query.Params) > 0 {
		_ = json.Unmarshal(query.Params[0], &address)
	}
	return address
}

func TestReadOnlyPreflight(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	server, calls := testNode(t, input, nil)
	verifier := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc"))
	for range 2 {
		result, err := verifier.Check(context.Background(), envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
		if err != nil || result.BlockNumber != 123 || result.BlockHash != headFixture(input, 123)["hash"] || result.Authorization.Network != input.Config.Network {
			t.Fatalf("只读预检 = %+v, %v", result, err)
		}
	}
	if calls.Load() != 26 {
		t.Fatalf("请求数 = %d，期望两轮各 13 个只读请求", calls.Load())
	}
}

func TestPreflightRejectsChainAndAccountFailures(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(nodeRequest, int, *nodeReply)
		want   error
	}{
		{"wrong_chain", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_chainId" {
				r.Result = "0x2b6653dc"
			}
		}, exact.ErrChainMismatch},
		{"changed_chain", func(q nodeRequest, n int, r *nodeReply) {
			if q.Method == "eth_chainId" && n == 2 {
				r.Result = "0x2b6653dc"
			}
		}, exact.ErrChainMismatch},
		{"changed_head", func(q nodeRequest, n int, r *nodeReply) {
			if q.Method == "eth_getBlockByNumber" && n == 2 {
				head := r.Result.(map[string]string)
				head["number"] = "0x7c"
				head["hash"] = fmt.Sprintf("0x%016x%s", 124, strings.Repeat("11", 24))
			}
		}, exact.ErrChainChanged},
		{"same_height_different_hash", func(q nodeRequest, n int, r *nodeReply) {
			if q.Method == "eth_getBlockByNumber" && n == 2 {
				r.Result.(map[string]string)["hash"] = fmt.Sprintf("0x%016x%s", 123, strings.Repeat("22", 24))
			}
		}, exact.ErrChainChanged},
		{"stale_head", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_getBlockByNumber" {
				r.Result.(map[string]string)["timestamp"] = fmt.Sprintf("0x%x", 1999999939)
			}
		}, exact.ErrStaleHead},
		{"future_head", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_getBlockByNumber" {
				r.Result.(map[string]string)["timestamp"] = fmt.Sprintf("0x%x", 2000000006)
			}
		}, exact.ErrStaleHead},
		{"huge_head_timestamp", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_getBlockByNumber" {
				r.Result.(map[string]string)["timestamp"] = "0x7fffffffffffffff"
			}
		}, exact.ErrStaleHead},
		{"wrong_permit_code", func(q nodeRequest, _ int, r *nodeReply) {
			if codeAddress(q) == permitWire {
				r.Result = "0x6003"
			}
		}, exact.ErrContractMismatch},
		{"wrong_proxy_code", func(q nodeRequest, _ int, r *nodeReply) {
			if codeAddress(q) == "0x3a2c916ce2b40f0ffc06cc80c0f72c0fc25be105" {
				r.Result = "0x6003"
			}
		}, exact.ErrContractMismatch},
		{"missing_token_code", func(q nodeRequest, _ int, r *nodeReply) {
			if codeAddress(q) == "0xeca9bc828a3005b9a3b909f2cc5c2a54794de05f" {
				r.Result = "0x"
			}
		}, exact.ErrContractMismatch},
		{"wrong_permit_binding", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0x6afdd850" {
				r.Result = "0x" + strings.Repeat("0", 64)
			}
		}, exact.ErrContractMismatch},
		{"wrong_witness", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0x2c03ae6a" {
				r.Result = "0x" + strings.Repeat("0", 64)
			}
		}, exact.ErrContractMismatch},
		{"wrong_domain", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0x3644e515" {
				r.Result = "0x" + strings.Repeat("0", 64)
			}
		}, exact.ErrContractMismatch},
		{"insufficient_balance", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0x70a08231" {
				r.Result = fmt.Sprintf("0x%064x", 999)
			}
		}, exact.ErrInsufficientBalance},
		{"insufficient_allowance", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0xdd62ed3e" {
				r.Result = fmt.Sprintf("0x%064x", 999)
			}
		}, exact.ErrInsufficientAllowance},
		{"nonce_used", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0x4fe02b44" {
				r.Result = fmt.Sprintf("0x%064x", new(big.Int).Lsh(big.NewInt(1), 17))
			}
		}, exact.ErrNonceUsed},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := loadVector(t)
			server, _ := testNode(t, input, test.mutate)
			result, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).Check(context.Background(), envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
			if !errors.Is(err, test.want) || result != (exact.PreflightResult{}) {
				t.Fatalf("未安全拒绝：%+v, %v，期望 %v", result, err, test.want)
			}
		})
	}
}

func TestPreflightRejectsMalformedNodeResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(nodeRequest, int, *nodeReply)
	}{
		{"http_error", func(_ nodeRequest, _ int, r *nodeReply) { r.status = 429; r.raw = "node-secret-must-not-be-exposed" }},
		{"rpc_error", func(_ nodeRequest, _ int, r *nodeReply) {
			r.Error = map[string]interface{}{"code": -32000, "message": "node-secret-must-not-be-exposed"}
		}},
		{"wrong_id", func(_ nodeRequest, _ int, r *nodeReply) { r.ID = 2 }},
		{"string_id", func(_ nodeRequest, _ int, r *nodeReply) { r.ID = "1" }},
		{"wrong_version", func(_ nodeRequest, _ int, r *nodeReply) { r.JSONRPC = "1.0" }},
		{"null", func(_ nodeRequest, _ int, r *nodeReply) { r.Result = nil }},
		{"invalid_json", func(_ nodeRequest, _ int, r *nodeReply) { r.raw = "{" }},
		{"multiple_objects", func(_ nodeRequest, _ int, r *nodeReply) { r.raw = `{"jsonrpc":"2.0","id":1,"result":"0xcd8690dc"}{}` }},
		{"oversized", func(_ nodeRequest, _ int, r *nodeReply) { r.raw = strings.Repeat(" ", 1<<20+1) }},
		{"quantity_leading_zero", func(_ nodeRequest, _ int, r *nodeReply) { r.Result = "0x0cd8690dc" }},
		{"quantity_plus", func(_ nodeRequest, _ int, r *nodeReply) { r.Result = "0x+1" }},
		{"quantity_overflow", func(_ nodeRequest, _ int, r *nodeReply) { r.Result = "0x10000000000000000" }},
		{"wrong_result_type", func(_ nodeRequest, _ int, r *nodeReply) { r.Result = 42 }},
		{"bad_head_height", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_getBlockByNumber" {
				r.Result.(map[string]string)["number"] = "0x7c"
			}
		}},
		{"zero_head_hash", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_getBlockByNumber" {
				r.Result.(map[string]string)["hash"] = "0x" + strings.Repeat("0", 64)
			}
		}},
		{"bad_head_timestamp", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_getBlockByNumber" {
				r.Result.(map[string]string)["timestamp"] = "0x8000000000000000"
			}
		}},
		{"bad_code", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_getCode" {
				r.Result = "0xzz"
			}
		}},
		{"odd_code", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_getCode" {
				r.Result = "0x1"
			}
		}},
		{"empty_abi", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_call" {
				r.Result = "0x"
			}
		}},
		{"invalid_abi", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_call" {
				r.Result = "0x" + strings.Repeat("g", 64)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := loadVector(t)
			server, _ := testNode(t, input, test.mutate)
			_, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).Check(context.Background(), envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
			if !errors.Is(err, exact.ErrNodeRead) || strings.Contains(err.Error(), "node-secret") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("RPC 失败未被安全处理：%v", err)
			}
		})
	}
}

func TestPreflightDoesNotReadForInvalidAuthorization(t *testing.T) {
	input := loadVector(t)
	server, calls := testNode(t, input, nil)
	payload := envelope(t, input)
	payload.X402Version = 1
	_, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).Check(context.Background(), payload, input.Requirements, time.Unix(input.Now, 0))
	if !errors.Is(err, exact.ErrInvalidRequirements) || calls.Load() != 0 {
		t.Fatalf("无效授权触发节点读取：%d, %v", calls.Load(), err)
	}
}

func TestPreflightOtherNonceBitDoesNotBlock(t *testing.T) {
	input := loadVector(t)
	server, _ := testNode(t, input, func(q nodeRequest, _ int, r *nodeReply) {
		if selector(q) == "0x4fe02b44" {
			r.Result = fmt.Sprintf("0x%064x", new(big.Int).Lsh(big.NewInt(1), 16))
		}
	})
	_, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).Check(context.Background(), envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreflightNonceBitmapBoundaries(t *testing.T) {
	for _, nonce := range []string{"0", "7", "8", "255", "256", new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1)).String()} {
		t.Run(nonce, func(t *testing.T) {
			t.Parallel()
			input := loadVector(t)
			input.Authorization.Nonce = nonce
			input = resignVector(t, input)
			server, _ := testNode(t, input, func(query nodeRequest, _ int, reply *nodeReply) {
				if selector(query) == "0x4fe02b44" {
					value, _ := new(big.Int).SetString(nonce, 10)
					reply.Result = fmt.Sprintf("0x%064x", new(big.Int).Lsh(big.NewInt(1), uint(value.Uint64()&255)))
				}
			})
			_, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).Check(context.Background(), envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
			if !errors.Is(err, exact.ErrNonceUsed) {
				t.Fatalf("nonce 位图边界未被识别：%v", err)
			}
		})
	}
}

func TestPreflightHTTPSAndClientIsolation(t *testing.T) {
	input := loadVector(t)
	plain, _ := testNode(t, input, nil)
	tlsServer := httptest.NewTLSServer(plain.Config.Handler)
	defer tlsServer.Close()
	client := tlsServer.Client()
	client.Timeout = time.Second
	config := preflightConfig(input, tlsServer.URL+"/jsonrpc")
	config.HTTPClient = client
	config.Timeout = 2 * time.Second
	config.AllowInsecureLocalHTTP = false
	if _, err := preflightFor(t, config).Check(context.Background(), envelope(t, input), input.Requirements, time.Unix(input.Now, 0)); err != nil {
		t.Fatal(err)
	}
	if client.Timeout != time.Second || client.CheckRedirect != nil {
		t.Fatal("预检修改了调用方的 HTTPClient")
	}
	config.HTTPClient = nil
	if _, err := preflightFor(t, config).Check(context.Background(), envelope(t, input), input.Requirements, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrNodeRead) {
		t.Fatalf("未拒绝不受信任的 TLS 证书：%v", err)
	}
}

func TestPreflightConfiguration(t *testing.T) {
	input := loadVector(t)
	for index, mutate := range []func(*exact.PreflightConfig){
		func(c *exact.PreflightConfig) { c.Authorization.Network = "tron:1" },
		func(c *exact.PreflightConfig) { c.NodeURL = "https://user:password@example.com/jsonrpc" },
		func(c *exact.PreflightConfig) { c.NodeURL = "https://example.com/jsonrpc?apiKey=value" },
		func(c *exact.PreflightConfig) { c.NodeURL = "https://example.com/jsonrpc#fragment" },
		func(c *exact.PreflightConfig) { c.NodeURL = "https://example.com/jsonrpc#" },
		func(c *exact.PreflightConfig) { c.NodeURL = "http://example.com/jsonrpc" },
		func(c *exact.PreflightConfig) {
			c.NodeURL = "http://127.0.0.1/jsonrpc"
			c.AllowInsecureLocalHTTP = false
		},
		func(c *exact.PreflightConfig) { c.NodeURL = "ftp://example.com/jsonrpc" },
		func(c *exact.PreflightConfig) { c.NodeURL = "https://example.com/jsonrpc?" },
		func(c *exact.PreflightConfig) { c.NodeURL = "://bad" },
		func(c *exact.PreflightConfig) { c.APIKey = "invalid\nheader" },
		func(c *exact.PreflightConfig) { c.APIKey = strings.Repeat("x", 4097) },
		func(c *exact.PreflightConfig) { c.Permit2CodeHash = "" },
		func(c *exact.PreflightConfig) { c.ProxyCodeHash = "0x" + strings.Repeat("0", 64) },
		func(c *exact.PreflightConfig) { c.Timeout = -time.Second },
		func(c *exact.PreflightConfig) { c.Timeout = 2 * time.Minute },
	} {
		t.Run(strconv.Itoa(index), func(t *testing.T) {
			config := preflightConfig(input, "https://example.com/jsonrpc")
			mutate(&config)
			if value, err := exact.NewPreflightVerifier(config); value != nil || !errors.Is(err, exact.ErrPreflightConfig) {
				t.Fatalf("无效配置未被拒绝：%v", err)
			}
		})
	}
	for _, verifier := range []*exact.PreflightVerifier{nil, {}} {
		if _, err := verifier.Check(context.Background(), envelope(t, input), input.Requirements, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrPreflightConfig) {
			t.Fatal(err)
		}
	}
}

func TestPreflightCancellationAndTimeout(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(strconv.FormatBool(timeout), func(t *testing.T) {
			entered := make(chan struct{})
			var once sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				once.Do(func() { close(entered) })
				<-r.Context().Done()
			}))
			defer server.Close()
			input := loadVector(t)
			config := preflightConfig(input, server.URL+"/jsonrpc")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := context.Canceled
			if timeout {
				config.Timeout = 250 * time.Millisecond
				want = context.DeadlineExceeded
			}
			verifier := preflightFor(t, config)
			finished := make(chan error, 1)
			payload := envelope(t, input)
			go func() {
				_, err := verifier.Check(ctx, payload, input.Requirements, time.Unix(input.Now, 0))
				finished <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("请求未到达本地节点")
			}
			if !timeout {
				cancel()
			}
			select {
			case err := <-finished:
				if !errors.Is(err, want) {
					t.Fatalf("取消/超时错误 = %v，期望 %v", err, want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("未及时取消预检")
			}
		})
	}
}

func TestPreflightRejectsRedirect(t *testing.T) {
	var targetCalls atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	input := loadVector(t)
	_, err := preflightFor(t, preflightConfig(input, redirect.URL+"/jsonrpc")).Check(context.Background(), envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
	if !errors.Is(err, exact.ErrNodeRead) || targetCalls.Load() != 0 {
		t.Fatalf("节点重定向未阻断：%d, %v", targetCalls.Load(), err)
	}
}

func TestPreflightConfigDoesNotSerializeAPIKey(t *testing.T) {
	config := preflightConfig(loadVector(t), "https://example.com/jsonrpc")
	encoded, err := json.Marshal(config)
	if err != nil || strings.Contains(string(encoded), config.APIKey) || strings.Contains(string(encoded), "APIKey") {
		t.Fatalf("配置序列化未排除 API key，错误 %v", err)
	}
}
