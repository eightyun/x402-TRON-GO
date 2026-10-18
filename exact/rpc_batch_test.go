package exact_test

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

	"github.com/eightyun/x402-tron-go/exact"
)

// 测试节点逐项复用原单请求夹具，再逆序返回；生产实现没有测试专用分支。
func batchNode(t *testing.T, base http.Handler, mutate func([]json.RawMessage) []byte) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	calls := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if len(body) > 0 && body[0] == '{' {
			r.Body = io.NopCloser(bytes.NewReader(body))
			base.ServeHTTP(w, r)
			return
		}
		var requests []nodeRequest
		if json.Unmarshal(body, &requests) != nil || len(requests) < 1 || len(requests) > 6 {
			t.Error("批量请求格式或数量不符")
			w.WriteHeader(400)
			return
		}
		responses := make([]json.RawMessage, len(requests))
		for index, query := range requests {
			if query.ID != index+1 {
				t.Error("请求id不唯一或不连续")
			}
			id := query.ID
			query.ID = 1
			encoded, _ := json.Marshal(query)
			request := httptest.NewRequest("POST", "/jsonrpc", bytes.NewReader(encoded))
			request.Header = r.Header.Clone()
			recorder := httptest.NewRecorder()
			base.ServeHTTP(recorder, request)
			if recorder.Code != 200 {
				w.WriteHeader(recorder.Code)
				return
			}
			var fields map[string]json.RawMessage
			if json.Unmarshal(recorder.Body.Bytes(), &fields) != nil {
				t.Error("夹具响应无效")
				return
			}
			fields["id"] = json.RawMessage(fmt.Sprint(id))
			responses[len(requests)-1-index], _ = json.Marshal(fields)
		}
		w.Header().Set("Content-Type", "application/json")
		if mutate != nil {
			_, _ = w.Write(mutate(responses))
		} else {
			_ = json.NewEncoder(w).Encode(responses)
		}
	}))
	t.Cleanup(server.Close)
	return server, calls
}

func TestBatchPreflightAndSimulation(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	base, reads, simulations := simulationNode(t, input, nil)
	server, calls := batchNode(t, base.Config.Handler, nil)
	config := preflightConfig(input, server.URL+"/jsonrpc")
	config.BatchRPC = true
	result, err := preflightFor(t, config).CheckWithSimulation(context.Background(), envelope(t, input), input.Requirements, loadSettlementVector(t).Config.ExecutorAddress, time.Unix(input.Now, 0))
	if err != nil || result.BlockNumber != 123 || reads.Load() != 13 || simulations.Load() != 1 || calls.Load() != 5 {
		t.Fatalf("batch改变检查或未减少往返: result=%+v err=%v reads=%d sim=%d requests=%d", result, err, reads.Load(), simulations.Load(), calls.Load())
	}
}

func TestBatchRejectsAmbiguousOrIncompleteResponses(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func([]json.RawMessage) []byte
	}{
		{"object", func(r []json.RawMessage) []byte { return r[0] }},
		{"empty", func([]json.RawMessage) []byte { return []byte(`[]`) }},
		{"missing", func(r []json.RawMessage) []byte { b, _ := json.Marshal(r[:1]); return b }},
		{"extra", func(r []json.RawMessage) []byte { b, _ := json.Marshal(append(r, r[0])); return b }},
		{"duplicate_id", func(r []json.RawMessage) []byte { b, _ := json.Marshal([]json.RawMessage{r[0], r[0]}); return b }},
		{"unknown_id", func(r []json.RawMessage) []byte {
			r[0] = bytes.Replace(r[0], []byte(`"id":2`), []byte(`"id":3`), 1)
			b, _ := json.Marshal(r)
			return b
		}},
		{"string_id", func(r []json.RawMessage) []byte {
			r[0] = bytes.Replace(r[0], []byte(`"id":2`), []byte(`"id":"2"`), 1)
			b, _ := json.Marshal(r)
			return b
		}},
		{"fraction_id", func(r []json.RawMessage) []byte {
			r[0] = bytes.Replace(r[0], []byte(`"id":2`), []byte(`"id":2.0`), 1)
			b, _ := json.Marshal(r)
			return b
		}},
		{"duplicate_field", func(r []json.RawMessage) []byte {
			r[0] = append([]byte(`{"id":2,`), r[0][1:]...)
			b, _ := json.Marshal(r)
			return b
		}},
		{"nested_duplicate", func(r []json.RawMessage) []byte {
			r[0] = bytes.Replace(r[0], []byte(`"number":`), []byte(`"number":"0x7b","number":`), 1)
			b, _ := json.Marshal(r)
			return b
		}},
		{"error_with_result", func(r []json.RawMessage) []byte {
			r[0] = append([]byte(`{"error":{"message":"private-node-detail"},`), r[0][1:]...)
			b, _ := json.Marshal(r)
			return b
		}},
		{"null_error_with_result", func(r []json.RawMessage) []byte {
			r[0] = append([]byte(`{"error":null,`), r[0][1:]...)
			b, _ := json.Marshal(r)
			return b
		}},
		{"null_result", func(r []json.RawMessage) []byte {
			r[0] = []byte(`{"jsonrpc":"2.0","id":2,"result":null}`)
			b, _ := json.Marshal(r)
			return b
		}},
		{"wrong_version", func(r []json.RawMessage) []byte {
			r[0] = bytes.Replace(r[0], []byte(`"2.0"`), []byte(`"1.0"`), 1)
			b, _ := json.Marshal(r)
			return b
		}},
		{"trailing", func(r []json.RawMessage) []byte { b, _ := json.Marshal(r); return append(b, []byte(` {}`)...) }},
		{"oversize", func([]json.RawMessage) []byte { return []byte(strings.Repeat(" ", 1<<20+1)) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := loadVector(t)
			base, _ := testNode(t, input, nil)
			server, calls := batchNode(t, base.Config.Handler, test.mutate)
			config := preflightConfig(input, server.URL+"/jsonrpc")
			config.BatchRPC = true
			result, err := preflightFor(t, config).Check(context.Background(), envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
			if !errors.Is(err, exact.ErrNodeRead) || result != (exact.PreflightResult{}) || calls.Load() != 1 || strings.Contains(err.Error(), "private-node-detail") {
				t.Fatalf("无效batch未拒绝或自动降级: result=%+v err=%v calls=%d", result, err, calls.Load())
			}
		})
	}
}

func TestBatchPreservesStateAndHeadChecks(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(nodeRequest, int, *nodeReply)
		want   error
	}{
		{"chain", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_chainId" {
				r.Result = "0x1"
			}
		}, exact.ErrChainMismatch},
		{"changed_head", func(q nodeRequest, n int, r *nodeReply) {
			if q.Method == "eth_getBlockByNumber" && n == 2 {
				r.Result.(map[string]string)["hash"] = fmt.Sprintf("0x%016x%s", 123, strings.Repeat("22", 24))
			}
		}, exact.ErrChainChanged},
		{"stale_head", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_getBlockByNumber" {
				r.Result.(map[string]string)["timestamp"] = "0x1"
			}
		}, exact.ErrStaleHead},
		{"code", func(q nodeRequest, _ int, r *nodeReply) {
			if q.Method == "eth_getCode" {
				r.Result = "0x"
			}
		}, exact.ErrContractMismatch},
		{"binding", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0x6afdd850" {
				r.Result = fmt.Sprintf("0x%064x", 0)
			}
		}, exact.ErrContractMismatch},
		{"balance", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0x70a08231" {
				r.Result = fmt.Sprintf("0x%064x", 0)
			}
		}, exact.ErrInsufficientBalance},
		{"allowance", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0xdd62ed3e" {
				r.Result = fmt.Sprintf("0x%064x", 0)
			}
		}, exact.ErrInsufficientAllowance},
		{"nonce", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0x4fe02b44" {
				r.Result = "0x" + strings.Repeat("f", 64)
			}
		}, exact.ErrNonceUsed},
		{"simulation", func(q nodeRequest, _ int, r *nodeReply) {
			if selector(q) == "0x13cd3b53" {
				r.Result = "0x01"
			}
		}, exact.ErrSettlementSimulation},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			input := loadVector(t)
			base, _, _ := simulationNode(t, input, test.mutate)
			server, _ := batchNode(t, base.Config.Handler, nil)
			config := preflightConfig(input, server.URL+"/jsonrpc")
			config.BatchRPC = true
			_, err := preflightFor(t, config).CheckWithSimulation(context.Background(), envelope(t, input), input.Requirements, loadSettlementVector(t).Config.ExecutorAddress, time.Unix(input.Now, 0))
			if !errors.Is(err, test.want) {
				t.Fatalf("检查被绕过: %v want %v", err, test.want)
			}
		})
	}
}

func TestBatchHonorsContextWithoutFallback(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	calls := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer server.Close()
	config := preflightConfig(input, server.URL)
	config.BatchRPC = true
	verifier := preflightFor(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := verifier.Check(ctx, envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("batch未按预算取消: %v calls=%d", err, calls.Load())
	}
}
