package postgres_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// 仅包装合成节点的JSON-RPC路径，原生交易及固化回执仍由原夹具逐项核验。
func (fixture *facilitatorTest) batchNode(t *testing.T, calls *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if r.URL.Path != "/jsonrpc" || len(body) == 0 || body[0] != '[' {
			r.Body = io.NopCloser(bytes.NewReader(body))
			fixture.node(t, w, r)
			return
		}
		calls.Add(1)
		var requests []map[string]json.RawMessage
		if json.Unmarshal(body, &requests) != nil || len(requests) > 6 {
			t.Error("批量请求无效")
			w.WriteHeader(400)
			return
		}
		responses := make([]map[string]json.RawMessage, len(requests))
		for index, query := range requests {
			id := query["id"]
			query["id"] = json.RawMessage(`1`)
			b, _ := json.Marshal(query)
			request := httptest.NewRequest("POST", "/jsonrpc", bytes.NewReader(b))
			recorder := httptest.NewRecorder()
			fixture.node(t, recorder, request)
			if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &responses[len(requests)-1-index]) != nil {
				t.Error("批量夹具响应无效")
				w.WriteHeader(400)
				return
			}
			responses[len(requests)-1-index]["id"] = id
		}
		_ = json.NewEncoder(w).Encode(responses)
	})
}

func TestFacilitatorBatchSettleAndOriginalRecovery(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	var calls atomic.Int32
	server := httptest.NewServer(fixture.batchNode(t, &calls))
	defer server.Close()
	fixture.config.Preflight.NodeURL = server.URL + "/jsonrpc"
	fixture.config.Preflight.BatchRPC = true
	result, err := fixture.mechanism(t).Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	if err != nil || !result.Success || result.Transaction == "" || calls.Load() != 3 || fixture.jsonRPCs.Load() != 14 || fixture.simulations.Load() != 1 || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatalf("批量结算未保留完整校验: %v calls=%d reads=%d", err, calls.Load(), fixture.jsonRPCs.Load())
	}
	server.Close()
	fixture.server.Close()
	again, err := fixture.mechanism(t).Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	if err != nil || !again.Success || again.Transaction != result.Transaction || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 || calls.Load() != 3 {
		t.Fatal("恢复重复签名、广播或读取预检", err)
	}
}
