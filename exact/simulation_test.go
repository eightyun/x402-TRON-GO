package exact_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
)

// 模拟请求与公开TronWeb向量逐字比较，不使用被测编码器反推期望数据。
func simulationNode(t *testing.T, input vector, mutate func(nodeRequest, int, *nodeReply), executors ...string) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	fixture := loadSettlementVector(t)
	if len(executors) == 0 {
		executors = []string{fixture.Config.ExecutorAddress}
	}
	allowed := make(map[string]bool)
	for _, executor := range executors {
		address, err := tron.AddressForSigning(executor)
		if err != nil {
			t.Fatal(err)
		}
		allowed[address] = true
	}
	base, reads := testNode(t, input, mutate)
	simulations := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		var query nodeRequest
		if err != nil || json.Unmarshal(data, &query) != nil {
			t.Error("模拟节点收到无效JSON")
			w.WriteHeader(400)
			return
		}
		if selector(query) != "0x13cd3b53" {
			r.Body = io.NopCloser(bytes.NewReader(data))
			base.Config.Handler.ServeHTTP(w, r)
			return
		}
		simulations.Add(1)
		var call map[string]string
		if len(query.Params) != 2 || json.Unmarshal(query.Params[0], &call) != nil {
			t.Error("模拟参数无效")
			w.WriteHeader(400)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/jsonrpc" || r.Header.Get("TRON-PRO-API-KEY") != "public-unit-test-placeholder" || query.JSONRPC != "2.0" || query.ID != 1 {
			t.Error("模拟HTTP/JSON-RPC元数据不符")
		}
		if len(call) != 4 || !allowed[call["from"]] || call["to"] != input.Authorization.Spender || call["value"] != "0x0" || call["data"] != "0x"+fixture.CallData || string(query.Params[1]) != `"latest"` {
			t.Error("模拟调用者/目标/value/latest/完整TronWeb ABI不符")
		}
		reply := nodeReply{JSONRPC: "2.0", ID: 1, Result: "0x", status: 200}
		if mutate != nil {
			mutate(query, 1, &reply)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		if reply.raw != "" {
			_, _ = io.WriteString(w, reply.raw)
		} else {
			_ = json.NewEncoder(w).Encode(reply)
		}
	}))
	t.Cleanup(server.Close)
	return server, reads, simulations
}

func TestPreflightSimulationMatchesTronWebCalldata(t *testing.T) {
	t.Parallel()
	fixture := loadSettlementVector(t)
	for _, recoveryID := range []string{"1b", "00"} {
		input := loadVector(t)
		input.Signature = input.Signature[:len(input.Signature)-2] + recoveryID
		server, reads, simulations := simulationNode(t, input, nil)
		verifier := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc"))
		result, err := verifier.CheckWithSimulation(context.Background(), envelope(t, input), input.Requirements, fixture.Config.ExecutorAddress, time.Unix(input.Now, 0))
		if err != nil || result.Authorization.Payer == "" || result.BlockNumber != 123 || reads.Load() != 13 || simulations.Load() != 1 {
			t.Fatalf("完整模拟预检失败或改变了原检查: %v", err)
		}
	}
}

func TestPreflightSimulationRejectsErrorsAndAmbiguousResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		raw  string
	}{
		{"revert", `{"jsonrpc":"2.0","id":1,"error":{"code":3,"message":"private-node-detail","data":"sensitive-payload"}}`},
		{"method_unavailable", `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"private-node-detail"}}`},
		{"both_result_and_error", `{"jsonrpc":"2.0","id":1,"result":"0x","error":{"code":3}}`},
		{"null", `{"jsonrpc":"2.0","id":1,"result":null}`},
		{"object", `{"jsonrpc":"2.0","id":1,"result":{}}`},
		{"boolean", `{"jsonrpc":"2.0","id":1,"result":true}`},
		{"empty_string", `{"jsonrpc":"2.0","id":1,"result":""}`},
		{"boolean_word", `{"jsonrpc":"2.0","id":1,"result":"0x01"}`},
		{"revert_bytes", `{"jsonrpc":"2.0","id":1,"result":"0x08c379a0"}`},
		{"wrong_id", `{"jsonrpc":"2.0","id":2,"result":"0x"}`},
		{"duplicate_result", `{"jsonrpc":"2.0","id":1,"result":"0xdead","result":"0x"}`},
		{"duplicate_error", `{"jsonrpc":"2.0","id":1,"error":{"code":3},"error":null,"result":"0x"}`},
		{"duplicate_version", `{"jsonrpc":"1.0","jsonrpc":"2.0","id":1,"result":"0x"}`},
		{"case_override", `{"jsonrpc":"2.0","id":1,"result":"0xdead","Result":"0x"}`},
		{"trailing", `{"jsonrpc":"2.0","id":1,"result":"0x"}{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := loadVector(t)
			server, _, simulations := simulationNode(t, input, func(query nodeRequest, _ int, reply *nodeReply) {
				if selector(query) == "0x13cd3b53" {
					reply.raw = test.raw
				}
			})
			result, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).CheckWithSimulation(context.Background(), envelope(t, input), input.Requirements, loadSettlementVector(t).Config.ExecutorAddress, time.Unix(input.Now, 0))
			if !errors.Is(err, exact.ErrSettlementSimulation) || result != (exact.PreflightResult{}) || simulations.Load() != 1 || strings.Contains(err.Error(), "private-node-detail") || strings.Contains(err.Error(), "sensitive-payload") {
				t.Fatal("模拟错误被接受、未执行或泄露节点原文")
			}
		})
	}
}

func TestPreflightSimulationRespectsChainAndFinalValidity(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		expire bool
		want   error
	}{
		{"chain_drift", false, exact.ErrChainChanged},
		{"expired_during_call", true, exact.ErrInvalidValidity},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := loadVector(t)
			now := time.Unix(input.Now, 0)
			if test.expire {
				// 固定测试时钟置于秒边界前1ns，任何真实RPC耗时都必须计入最终时效；不依赖短TTL睡眠。
				now = time.Unix(input.Now+594, 999999999)
			}
			server, _, simulations := simulationNode(t, input, func(query nodeRequest, n int, reply *nodeReply) {
				if query.Method == "eth_getBlockByNumber" {
					head := headFixture(input, 123)
					if test.expire {
						head["timestamp"] = "0x" + strconv.FormatInt(now.Unix(), 16)
					} else if n == 2 {
						head = headFixture(input, 124)
					}
					reply.Result = head
				}
			})
			result, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).CheckWithSimulation(context.Background(), envelope(t, input), input.Requirements, loadSettlementVector(t).Config.ExecutorAddress, now)
			if !errors.Is(err, test.want) || result != (exact.PreflightResult{}) || simulations.Load() != 1 {
				t.Fatal("模拟绕过了后置链头或时效检查")
			}
		})
	}
}

func TestPreflightSimulationTransportFailureAndShortCircuit(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		want   error
		calls  int64
		mutate func(nodeRequest, int, *nodeReply)
	}{
		{"http_unavailable", exact.ErrSettlementSimulation, 1, func(query nodeRequest, _ int, reply *nodeReply) {
			if selector(query) == "0x13cd3b53" {
				reply.status, reply.raw = 503, "private-node-detail"
			}
		}},
		{"insufficient_funds", exact.ErrInsufficientBalance, 0, func(query nodeRequest, _ int, reply *nodeReply) {
			if selector(query) == "0x70a08231" {
				reply.Result = "0x" + strings.Repeat("0", 64)
			}
		}},
		{"used_nonce", exact.ErrNonceUsed, 0, func(query nodeRequest, _ int, reply *nodeReply) {
			if selector(query) == "0x4fe02b44" {
				reply.Result = "0x" + strings.Repeat("f", 64)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := loadVector(t)
			server, _, simulations := simulationNode(t, input, test.mutate)
			result, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).CheckWithSimulation(context.Background(), envelope(t, input), input.Requirements, loadSettlementVector(t).Config.ExecutorAddress, time.Unix(input.Now, 0))
			if !errors.Is(err, test.want) || result != (exact.PreflightResult{}) || simulations.Load() != test.calls || strings.Contains(err.Error(), "private-node-detail") {
				t.Fatal("模拟传输错误/前置拒绝处理不正确")
			}
		})
	}
}

func TestPreflightSimulationRejectsBadCallerAndCancelledContext(t *testing.T) {
	input := loadVector(t)
	server, reads, simulations := simulationNode(t, input, nil)
	verifier := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc"))
	for _, address := range []string{"", "invalid", "0x" + strings.Repeat("1", 40)} {
		if _, err := verifier.CheckWithSimulation(context.Background(), envelope(t, input), input.Requirements, address, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrPreflightConfig) {
			t.Fatal("模拟接受了无效费用付款人")
		}
	}
	executor := loadSettlementVector(t).Config.ExecutorAddress
	for _, zero := range []*exact.PreflightVerifier{nil, {}} {
		if _, err := zero.CheckWithSimulation(context.Background(), envelope(t, input), input.Requirements, executor, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrPreflightConfig) {
			t.Fatal("零值预检被接受")
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := verifier.CheckWithSimulation(ctx, envelope(t, input), input.Requirements, executor, time.Unix(input.Now, 0)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("未遵守整轮超时")
	}
	if reads.Load() != 0 || simulations.Load() != 0 {
		t.Fatal("坏参数/上下文仍调用节点")
	}
}

func TestPreflightSimulationCancellationAndConcurrentCallers(t *testing.T) {
	input := loadVector(t)
	executor := loadSettlementVector(t).Config.ExecutorAddress
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server, _, simulations := simulationNode(t, input, func(query nodeRequest, _ int, _ *nodeReply) {
		if selector(query) == "0x13cd3b53" {
			cancel()
		}
	})
	if _, err := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc")).CheckWithSimulation(ctx, envelope(t, input), input.Requirements, executor, time.Unix(input.Now, 0)); !errors.Is(err, context.Canceled) || simulations.Load() != 1 {
		t.Fatal("模拟中取消未传递")
	}
	callers := []string{executor, input.Config.Permit2Address}
	concurrent, reads, calls := simulationNode(t, input, nil, callers...)
	verifier := preflightFor(t, preflightConfig(input, concurrent.URL+"/jsonrpc"))
	var workers sync.WaitGroup
	for index := range 8 {
		workers.Go(func() {
			if _, err := verifier.CheckWithSimulation(context.Background(), envelope(t, input), input.Requirements, callers[index%len(callers)], time.Unix(input.Now, 0)); err != nil {
				t.Error("并发模拟调用失败")
			}
		})
	}
	workers.Wait()
	if calls.Load() != 8 || reads.Load() != 8*13 {
		t.Fatal("并发模拟未保持各自独立的整轮检查")
	}
}
