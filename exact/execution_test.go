package exact_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
)

func syntheticGenesis() string {
	return strings.Repeat("00", 8) + strings.Repeat("11", 20) + "cd8690dc"
}

type executionReply struct {
	status int
	raw    string
	value  interface{}
}

func executionNode(t *testing.T, fixture settlementVector, mutate func(string, int, *executionReply)) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var lock sync.Mutex
	counts := make(map[string]int)
	writes := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if r.Method != http.MethodPost || r.Header.Get("TRON-PRO-API-KEY") != "public-execution-placeholder" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("原生请求方法或认证头不符")
		}
		var params map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&params) != nil || len(params) != 1 {
			t.Error("原生请求参数无效")
		}
		lock.Lock()
		counts[r.URL.Path]++
		occurrence := counts[r.URL.Path]
		lock.Unlock()
		reply := executionReply{status: http.StatusOK}
		switch r.URL.Path {
		case "/wallet/getblockbynum":
			if string(params["num"]) != "0" {
				t.Error("不是创世区块查询")
			}
			reply.value = map[string]interface{}{"blockID": syntheticGenesis()}
		case "/wallet/broadcasthex":
			writes.Add(1)
			var payload string
			if json.Unmarshal(params["transaction"], &payload) != nil || payload != fixture.Payload {
				t.Error("广播字节不等于原始完整 Protobuf")
			}
			reply.value = map[string]interface{}{"result": true, "code": "SUCCESS", "txid": fixture.TransactionID, "message": "", "transaction": "ignored-node-echo"}
		case "/wallet/gettransactionbyid":
			var id string
			if json.Unmarshal(params["value"], &id) != nil || id != fixture.TransactionID {
				t.Error("查询的不是原 txid")
			}
			reply.value = map[string]interface{}{"txID": fixture.TransactionID, "raw_data_hex": fixture.RawDataHex, "raw_data": map[string]string{"untrusted_display": "ignored"}}
		default:
			t.Errorf("调用了非预期接口：%s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if mutate != nil {
			mutate(r.URL.Path, occurrence, &reply)
		}
		w.WriteHeader(reply.status)
		if reply.raw != "" {
			_, _ = io.WriteString(w, reply.raw)
		} else {
			_ = json.NewEncoder(w).Encode(reply.value)
		}
	}))
	t.Cleanup(server.Close)
	return server, writes
}

func executionConfig(url string) exact.ExecutionConfig {
	return exact.ExecutionConfig{Network: tron.NetworkNile, NodeURL: url, GenesisBlockID: syntheticGenesis(), APIKey: "public-execution-placeholder", AllowInsecureLocalHTTP: true}
}

func executionClient(t *testing.T, config exact.ExecutionConfig) *exact.ExecutionClient {
	t.Helper()
	client, err := exact.NewExecutionClient(config)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestExecutionBroadcastAndLookup(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	server, writes := executionNode(t, fixture, func(path string, occurrence int, reply *executionReply) {
		if path == "/wallet/gettransactionbyid" && occurrence == 2 {
			reply.value = map[string]interface{}{}
		}
	})
	client := executionClient(t, executionConfig(server.URL))
	prepared := preparedFor(t, fixture, input)
	signed := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
	if err := client.Broadcast(context.Background(), prepared, signed, time.Unix(input.Now, 0)); err != nil {
		t.Fatal(err)
	}
	for _, wantFound := range []bool{true, false} {
		observation, err := client.Lookup(context.Background(), signed.ID)
		if err != nil || observation.TransactionID != signed.ID || observation.Network != tron.NetworkNile || observation.Found != wantFound {
			t.Fatalf("原交易观察不符：%+v %v", observation, err)
		}
	}
	if writes.Load() != 1 {
		t.Fatal("查询执行了写请求")
	}
}

func TestExecutionBroadcastResponsesFailClosed(t *testing.T) {
	fixture, input := loadSettlementVector(t), loadVector(t)
	prepared := preparedFor(t, fixture, input)
	signed := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
	for _, test := range []struct {
		name string
		raw  string
		code int
		want error
	}{
		{"rejected", `{"result":false,"code":"SIGERROR","txid":"` + fixture.TransactionID + `","message":"private-node-message"}`, 200, exact.ErrBroadcastRejected},
		{"contradictory", `{"result":false,"code":"SUCCESS","txid":"` + fixture.TransactionID + `"}`, 200, exact.ErrBroadcastUnknown},
		{"wrong_id", `{"result":true,"txid":"` + strings.Repeat("a", 64) + `"}`, 200, exact.ErrBroadcastUnknown},
		{"null_result", `{"result":null,"txid":"` + fixture.TransactionID + `"}`, 200, exact.ErrBroadcastUnknown},
		{"string_result", `{"result":"true","txid":"` + fixture.TransactionID + `"}`, 200, exact.ErrBroadcastUnknown},
		{"duplicate_result", `{"result":false,"result":true,"txid":"` + fixture.TransactionID + `"}`, 200, exact.ErrBroadcastUnknown},
		{"wrong_code", `{"result":true,"code":"FAILED","txid":"` + fixture.TransactionID + `"}`, 200, exact.ErrBroadcastUnknown},
		{"null_code", `{"result":true,"code":null,"txid":"` + fixture.TransactionID + `"}`, 200, exact.ErrBroadcastUnknown},
		{"error_field", `{"Error":"private-node-message"}`, 200, exact.ErrBroadcastUnknown},
		{"trailing", `{} {}`, 200, exact.ErrBroadcastUnknown},
		{"array", `[]`, 200, exact.ErrBroadcastUnknown},
		{"null", `null`, 200, exact.ErrBroadcastUnknown},
		{"malformed", `{"result":`, 200, exact.ErrBroadcastUnknown},
		{"oversized", strings.Repeat("x", (1<<20)+1), 200, exact.ErrBroadcastUnknown},
		{"unavailable", "private-node-message", 503, exact.ErrBroadcastUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, writes := executionNode(t, fixture, func(path string, _ int, reply *executionReply) {
				if path == "/wallet/broadcasthex" {
					reply.status, reply.raw = test.code, test.raw
				}
			})
			client := executionClient(t, executionConfig(server.URL))
			err := client.Broadcast(context.Background(), prepared, signed, time.Unix(input.Now, 0))
			if !errors.Is(err, test.want) || strings.Contains(err.Error(), "private-node-message") || strings.Contains(err.Error(), server.URL) || writes.Load() != 1 {
				t.Fatalf("广播结果或错误脱敏不符：%v", err)
			}
		})
	}
}

func TestExecutionNetworkAndExpiryGuards(t *testing.T) {
	fixture, input := loadSettlementVector(t), loadVector(t)
	prepared := preparedFor(t, fixture, input)
	signed := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
	for _, changeAt := range []int{1, 2} {
		t.Run(string(rune('0'+changeAt)), func(t *testing.T) {
			server, writes := executionNode(t, fixture, func(path string, n int, reply *executionReply) {
				if path == "/wallet/getblockbynum" && n == changeAt {
					reply.value = map[string]string{"blockID": strings.Repeat("00", 32)}
				}
			})
			client := executionClient(t, executionConfig(server.URL))
			err := client.Broadcast(context.Background(), prepared, signed, time.Unix(input.Now, 0))
			if !errors.Is(err, exact.ErrChainMismatch) || writes.Load() != int64(changeAt-1) || (changeAt == 2 && !errors.Is(err, exact.ErrBroadcastUnknown)) {
				t.Fatalf("网络切换保护不符：%v", err)
			}
		})
	}
	server, writes := executionNode(t, fixture, nil)
	client := executionClient(t, executionConfig(server.URL))
	if err := client.Broadcast(context.Background(), prepared, signed, prepared.ExpiresAt().Add(-14*time.Second)); !errors.Is(err, exact.ErrSettlementValidity) {
		t.Fatal("过期交易被发送")
	}
	if err := client.Broadcast(context.Background(), prepared, signed, prepared.ExpiresAt().Add(-15*time.Second)); !errors.Is(err, exact.ErrSettlementValidity) {
		t.Fatal("网络读取耗时未计入有效期")
	}
	tampered := signed
	tampered.Payload = append([]byte(nil), signed.Payload...)
	tampered.Payload[4] ^= 1
	if err := client.Broadcast(context.Background(), prepared, tampered, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrSettlementBytes) || writes.Load() != 0 {
		t.Fatal("非法字节或时间仍触发了广播")
	}
	if err := client.Broadcast(context.Background(), nil, signed, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrSettlementConfig) {
		t.Fatal("nil 准备对象被接受")
	}
	config := executionConfig(server.URL)
	config.Network = tron.NetworkShasta
	config.GenesisBlockID = strings.Repeat("00", 8) + strings.Repeat("11", 20) + "94a9059e"
	if err := executionClient(t, config).Broadcast(context.Background(), prepared, signed, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrChainMismatch) || writes.Load() != 0 {
		t.Fatal("费用交易被发送到不同配置网络")
	}
}

func TestExecutionLookupRejectsInvalidBodies(t *testing.T) {
	fixture := loadSettlementVector(t)
	for _, raw := range []string{
		`{"txID":"wrong","raw_data_hex":"` + fixture.RawDataHex + `"}`,
		`{"txID":"` + fixture.TransactionID + `","raw_data_hex":"00"}`,
		`{"txID":"` + fixture.TransactionID + `","raw_data_hex":""}`,
		`{"txID":"` + fixture.TransactionID + `","raw_data_hex":"0xz0"}`,
		`{"txID":"` + fixture.TransactionID + `","raw_data_hex":"0"}`,
		`{"txID":"` + fixture.TransactionID + `","raw_data_hex":"00","raw_data_hex":"` + fixture.RawDataHex + `"}`,
		`{"error":"node-secret"}`, `[]`, `{} null`, `{"txID":`, `{"unrelated":true}`,
	} {
		server, writes := executionNode(t, fixture, func(path string, _ int, reply *executionReply) {
			if path == "/wallet/gettransactionbyid" {
				reply.raw = raw
			}
		})
		client := executionClient(t, executionConfig(server.URL))
		observation, err := client.Lookup(context.Background(), fixture.TransactionID)
		if !errors.Is(err, exact.ErrExecutionNode) || observation.Found || observation.TransactionID != fixture.TransactionID || writes.Load() != 0 {
			t.Fatalf("不可靠主体被接受：%v", err)
		}
	}
	server, _ := executionNode(t, fixture, func(path string, n int, reply *executionReply) {
		if path == "/wallet/getblockbynum" && n == 2 {
			reply.value = map[string]string{"blockID": strings.Repeat("00", 32)}
		}
	})
	observation, err := executionClient(t, executionConfig(server.URL)).Lookup(context.Background(), fixture.TransactionID)
	if !errors.Is(err, exact.ErrChainMismatch) || observation.Found {
		t.Fatal("查询期间网络切换仍返回已找到")
	}
}

func TestExecutionConfigAndZeroValues(t *testing.T) {
	base := executionConfig("https://trusted.invalid")
	base.AllowInsecureLocalHTTP = false
	for _, mutate := range []func(*exact.ExecutionConfig){
		func(c *exact.ExecutionConfig) { c.Network = "tron:1" },
		func(c *exact.ExecutionConfig) { c.GenesisBlockID = "0x" + syntheticGenesis() },
		func(c *exact.ExecutionConfig) { c.GenesisBlockID = strings.Repeat("f", 64) },
		func(c *exact.ExecutionConfig) { c.NodeURL = "http://trusted.invalid" },
		func(c *exact.ExecutionConfig) { c.NodeURL += "/jsonrpc" },
		func(c *exact.ExecutionConfig) { c.NodeURL += "/%2f" },
		func(c *exact.ExecutionConfig) { c.NodeURL += "?" },
		func(c *exact.ExecutionConfig) { c.NodeURL += "#" },
		func(c *exact.ExecutionConfig) { c.NodeURL = "https://user:secret@trusted.invalid" },
		func(c *exact.ExecutionConfig) { c.Timeout = -1 },
		func(c *exact.ExecutionConfig) { c.Timeout = time.Minute + 1 },
		func(c *exact.ExecutionConfig) { c.APIKey = "bad\nheader" },
	} {
		config := base
		mutate(&config)
		if _, err := exact.NewExecutionClient(config); !errors.Is(err, exact.ErrExecutionConfig) {
			t.Fatalf("非法配置被接受：%v", err)
		}
	}
	encoded, err := json.Marshal(base)
	if err != nil || strings.Contains(string(encoded), base.APIKey) || strings.Contains(string(encoded), "HTTPClient") {
		t.Fatal("配置 JSON 包含认证或 HTTP 客户端")
	}
	for _, client := range []*exact.ExecutionClient{nil, {}} {
		if client.Network() != "" {
			t.Fatal("零值节点返回网络")
		}
		if err := client.Broadcast(context.Background(), nil, exact.SettlementTransaction{}, time.Now()); !errors.Is(err, exact.ErrExecutionConfig) {
			t.Fatal("零值客户端执行了广播")
		}
		if _, err := client.Lookup(context.Background(), ""); !errors.Is(err, exact.ErrExecutionConfig) {
			t.Fatal("零值客户端执行了查询")
		}
	}
	client := executionClient(t, base)
	for _, id := range []string{"", strings.Repeat("0", 64), strings.Repeat("A", 64), "' OR 1=1 --"} {
		if _, err := client.Lookup(context.Background(), id); !errors.Is(err, exact.ErrExecutionInput) {
			t.Fatal("非法 txid 被接受")
		}
	}
}

func TestExecutionTLSRedirectAndCancellation(t *testing.T) {
	fixture, input := loadSettlementVector(t), loadVector(t)
	prepared := preparedFor(t, fixture, input)
	signed := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
	server, writes := executionNode(t, fixture, nil)
	tlsServer := httptest.NewTLSServer(server.Config.Handler)
	t.Cleanup(tlsServer.Close)
	config := executionConfig(tlsServer.URL)
	config.AllowInsecureLocalHTTP = false
	config.HTTPClient = tlsServer.Client()
	config.HTTPClient.Timeout = 2 * time.Minute
	client := executionClient(t, config)
	if err := client.Broadcast(context.Background(), prepared, signed, time.Unix(input.Now, 0)); err != nil || config.HTTPClient.Timeout != 2*time.Minute {
		t.Fatalf("TLS 或客户端隔离失败：%v", err)
	}
	config.HTTPClient = nil
	if _, err := executionClient(t, config).Lookup(context.Background(), signed.ID); !errors.Is(err, exact.ErrExecutionNode) {
		t.Fatal("未受信任的 TLS 证书被接受")
	}
	client = executionClient(t, executionConfig(server.URL))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := client.Broadcast(ctx, prepared, signed, time.Unix(input.Now, 0)); !errors.Is(err, context.Canceled) || writes.Load() != 1 {
		t.Fatal("取消后的请求仍执行了广播")
	}
	var redirects atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects.Add(1) }))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirector.Close)
	if _, err := executionClient(t, executionConfig(redirector.URL)).Lookup(context.Background(), signed.ID); !errors.Is(err, exact.ErrExecutionNode) || redirects.Load() != 0 {
		t.Fatal("原生节点请求跟随了重定向")
	}
	var writeRedirects atomic.Int64
	writeRedirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wallet/broadcasthex" {
			writeRedirects.Add(1)
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		server.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(writeRedirector.Close)
	if err := executionClient(t, executionConfig(writeRedirector.URL)).Broadcast(context.Background(), prepared, signed, time.Unix(input.Now, 0)); !errors.Is(err, exact.ErrBroadcastUnknown) || writeRedirects.Load() != 1 || redirects.Load() != 0 {
		t.Fatal("写请求重定向未保留未知结果或跟随了跳转")
	}
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(blocked.Close)
	config = executionConfig(blocked.URL)
	config.Timeout = 10 * time.Millisecond
	if _, err := executionClient(t, config).Lookup(context.Background(), signed.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("节点总预算未生效：%v", err)
	}
}
