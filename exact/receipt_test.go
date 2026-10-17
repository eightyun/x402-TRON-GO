package exact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

const receiptHeight = uint64(71846435)

func receiptBlockID(height uint64, suffix string) string {
	return fmt.Sprintf("%016x", height) + strings.Repeat(suffix, 24)
}

func receiptBody(fixture settlementVector) map[string]interface{} {
	return map[string]interface{}{
		"txID": fixture.TransactionID, "raw_data_hex": fixture.RawDataHex,
		"ret":      []interface{}{map[string]interface{}{"contractRet": "SUCCESS"}},
		"raw_data": map[string]interface{}{"untrusted_display": "ignored"},
	}
}

func receiptLog(input vector) map[string]interface{} {
	return map[string]interface{}{
		"address": input.Authorization.Permitted.Token[2:],
		"topics": []interface{}{
			"ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
			strings.Repeat("0", 24) + input.Authorization.From[2:],
			strings.Repeat("0", 24) + input.Authorization.Witness.To[2:],
		},
		"data": fmt.Sprintf("%064x", 1000),
	}
}

func receiptBlock(height uint64, stamp int64, suffix string) map[string]interface{} {
	return map[string]interface{}{
		"blockID":      receiptBlockID(height, suffix),
		"block_header": map[string]interface{}{"raw_data": map[string]interface{}{"number": height, "timestamp": stamp}},
	}
}

// 所有区块/回执/日志均为合成夹具，使用公开离线签名向量，不读取真实节点。
func receiptNode(t *testing.T, mutate func(string, int, *executionReply)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	fixture, input := loadSettlementVector(t), loadVector(t)
	reads := new(atomic.Int32)
	var mu sync.Mutex
	counts := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		reads.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("TRON-PRO-API-KEY") != "public-execution-placeholder" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("请求方法/头无效")
		}
		var params map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&params) != nil {
			t.Error("请求参数无效")
		}
		key := r.URL.Path
		reply := executionReply{status: http.StatusOK}
		switch key {
		case "/walletsolidity/getblockbynum":
			key += ":" + string(params["num"])
			if len(params) != 1 {
				t.Error("区块查询参数无效")
			}
			if string(params["num"]) == "0" {
				reply.value = map[string]interface{}{"blockID": syntheticGenesis()}
			} else {
				if string(params["num"]) != fmt.Sprint(receiptHeight) {
					t.Error("查询了非预期区块")
				}
				block := receiptBlock(receiptHeight, (input.Now+3)*1000, "44")
				block["transactions"] = []interface{}{receiptBody(fixture)}
				reply.value = block
			}
		case "/walletsolidity/getnowblock":
			if len(params) != 0 {
				t.Error("固化头参数无效")
			}
			reply.value = receiptBlock(receiptHeight+300, (input.Now+900)*1000, "55")
		case "/walletsolidity/gettransactionbyid", "/walletsolidity/gettransactioninfobyid":
			if len(params) != 1 || string(params["value"]) != `"`+fixture.TransactionID+`"` {
				t.Error("未查询原交易")
			}
			if key == "/walletsolidity/gettransactionbyid" {
				reply.value = receiptBody(fixture)
			} else {
				reply.value = map[string]interface{}{
					"id": fixture.TransactionID, "blockNumber": receiptHeight, "blockTimeStamp": (input.Now + 3) * 1000,
					"receipt": map[string]interface{}{"result": "SUCCESS"}, "contractResult": []string{""},
					"log": []interface{}{receiptLog(input)},
				}
			}
		default:
			t.Errorf("核验调用了非只读 solidity 接口: %s", key)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		counts[key]++
		count := counts[key]
		mu.Unlock()
		if mutate != nil {
			mutate(key, count, &reply)
		}
		w.WriteHeader(reply.status)
		if reply.raw != "" {
			_, _ = io.WriteString(w, reply.raw)
		} else {
			_ = json.NewEncoder(w).Encode(reply.value)
		}
	}))
	t.Cleanup(server.Close)
	return server, reads
}

func checkReceipt(t *testing.T, config exact.ExecutionConfig) (exact.SettlementObservation, error) {
	t.Helper()
	fixture, input := loadSettlementVector(t), loadVector(t)
	return executionClient(t, config).CheckSettlement(context.Background(), preparedFor(t, fixture, input),
		exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}, time.Unix(input.Now+900, 0))
}

func TestCheckSettlementConfirmsOnlyOriginalPayment(t *testing.T) {
	t.Parallel()
	server, reads := receiptNode(t, nil)
	observation, err := checkReceipt(t, executionConfig(server.URL))
	if err != nil || !observation.Confirmed || observation.BlockID != receiptBlockID(receiptHeight, "44") || observation.BlockNumber != receiptHeight || observation.Network != tron.NetworkNile || observation.TransactionID != loadSettlementVector(t).TransactionID {
		t.Fatalf("未核验原付款: %+v %v", observation, err)
	}
	if reads.Load() != 9 {
		t.Fatalf("核验读取次数不符: %d", reads.Load())
	}
	// 当前时间已超过交易和付款授权 deadline，历史核验不能重新调用有时效的授权验签。
	input := loadVector(t)
	if time.Unix(input.Now+900, 0).Unix() <= input.Now+600 {
		t.Fatal("历史夹具无效")
	}
}

func TestCheckSettlementNativeEnumAndHeadBoundary(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	for _, matchingHead := range []bool{true, false} {
		server, _ := receiptNode(t, func(key string, _ int, reply *executionReply) {
			if key == "/walletsolidity/getnowblock" {
				suffix := "44"
				if !matchingHead {
					suffix = "55"
				}
				reply.value = receiptBlock(receiptHeight, (input.Now+3)*1000, suffix)
			}
			if key == "/walletsolidity/gettransactioninfobyid" {
				reply.value.(map[string]interface{})["result"] = "SUCESS"
			}
			if key == "/walletsolidity/gettransactionbyid" {
				reply.value.(map[string]interface{})["ret"].([]interface{})[0].(map[string]interface{})["ret"] = "SUCESS"
			}
		})
		observation, err := executionClient(t, executionConfig(server.URL)).CheckSettlement(context.Background(), preparedFor(t, fixture, input), exact.SettlementTransaction{
			ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload),
		}, time.Unix(input.Now+3, 0))
		if matchingHead {
			if err != nil || !observation.Confirmed {
				t.Fatalf("原生零值枚举或头边界未兼容: %v", err)
			}
		} else if err == nil || observation.Confirmed {
			t.Fatal("同高度不同固化块被接受")
		}
	}
}

func TestCheckSettlementMissingAndAdvancingFinality(t *testing.T) {
	t.Parallel()
	for _, missing := range []string{"/walletsolidity/gettransactionbyid", "/walletsolidity/gettransactioninfobyid", "both", "above_head"} {
		t.Run(missing, func(t *testing.T) {
			server, reads := receiptNode(t, func(key string, _ int, reply *executionReply) {
				if key == missing || missing == "both" && (key == "/walletsolidity/gettransactionbyid" || key == "/walletsolidity/gettransactioninfobyid") {
					reply.value = map[string]interface{}{}
				}
				if missing == "above_head" && key == "/walletsolidity/getnowblock" {
					reply.value = receiptBlock(receiptHeight-1, (loadVector(t).Now+900)*1000, "55")
				}
			})
			observation, err := checkReceipt(t, executionConfig(server.URL))
			if err != nil || observation.Confirmed || observation.BlockID != "" || observation.TransactionID == "" || reads.Load() != 6 {
				t.Fatalf("缺少固化证据被当作成功或失败: %+v %v", observation, err)
			}
		})
	}
	server, _ := receiptNode(t, func(key string, count int, reply *executionReply) {
		if key == "/walletsolidity/getnowblock" && count == 2 {
			reply.value = receiptBlock(receiptHeight+301, (loadVector(t).Now+901)*1000, "66")
		}
	})
	if observation, err := checkReceipt(t, executionConfig(server.URL)); err != nil || !observation.Confirmed {
		t.Fatal("正常固化头推进被拒绝")
	}
}

func TestCheckSettlementEvidenceFailsClosed(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	bodyPath, infoPath := "/walletsolidity/gettransactionbyid", "/walletsolidity/gettransactioninfobyid"
	blockPath := fmt.Sprintf("/walletsolidity/getblockbynum:%d", receiptHeight)
	for _, test := range []struct {
		name, key string
		count     int
		edit      func(map[string]interface{})
	}{
		{"body_id", bodyPath, 1, func(m map[string]interface{}) { m["txID"] = strings.Repeat("a", 64) }},
		{"body_raw", bodyPath, 1, func(m map[string]interface{}) { m["raw_data_hex"] = "00" + fixture.RawDataHex[2:] }},
		{"body_ret_missing", bodyPath, 1, func(m map[string]interface{}) { delete(m, "ret") }},
		{"body_revert", bodyPath, 1, func(m map[string]interface{}) {
			m["ret"] = []interface{}{map[string]interface{}{"contractRet": "REVERT"}}
		}},
		{"body_multiple", bodyPath, 1, func(m map[string]interface{}) {
			m["ret"] = []interface{}{map[string]interface{}{"contractRet": "SUCCESS"}, map[string]interface{}{"contractRet": "SUCCESS"}}
		}},
		{"body_failed", bodyPath, 1, func(m map[string]interface{}) { m["ret"].([]interface{})[0].(map[string]interface{})["ret"] = "FAILED" }},
		{"receipt_id", infoPath, 1, func(m map[string]interface{}) { m["id"] = strings.Repeat("a", 64) }},
		{"receipt_failed", infoPath, 1, func(m map[string]interface{}) { m["result"] = "FAILED" }},
		{"receipt_null", infoPath, 1, func(m map[string]interface{}) { m["result"] = nil }},
		{"execution_revert", infoPath, 1, func(m map[string]interface{}) { m["receipt"].(map[string]interface{})["result"] = "REVERT" }},
		{"execution_missing", infoPath, 1, func(m map[string]interface{}) { m["receipt"] = map[string]interface{}{} }},
		{"height_string", infoPath, 1, func(m map[string]interface{}) { m["blockNumber"] = "71846435" }},
		{"height_float", infoPath, 1, func(m map[string]interface{}) { m["blockNumber"] = 71846435.5 }},
		{"height_zero", infoPath, 1, func(m map[string]interface{}) { m["blockNumber"] = 0 }},
		{"time_mismatch", infoPath, 1, func(m map[string]interface{}) { m["blockTimeStamp"] = (input.Now + 4) * 1000 }},
		{"time_expired", infoPath, 1, func(m map[string]interface{}) { m["blockTimeStamp"] = (input.Now + 61) * 1000 }},
		{"time_before_creation", infoPath, 1, func(m map[string]interface{}) { m["blockTimeStamp"] = (input.Now - 6) * 1000 }},
		{"block_wrong_height", blockPath, 1, func(m map[string]interface{}) { m["blockID"] = receiptBlockID(receiptHeight+1, "44") }},
		{"block_missing_body", blockPath, 1, func(m map[string]interface{}) { m["transactions"] = []interface{}{} }},
		{"block_wrong_body", blockPath, 1, func(m map[string]interface{}) {
			m["transactions"].([]interface{})[0].(map[string]interface{})["raw_data_hex"] = "00"
		}},
		{"block_duplicate_body", blockPath, 1, func(m map[string]interface{}) {
			m["transactions"] = []interface{}{receiptBody(fixture), receiptBody(fixture)}
		}},
		{"block_other_tx", blockPath, 1, func(m map[string]interface{}) {
			m["transactions"].([]interface{})[0].(map[string]interface{})["txID"] = strings.Repeat("a", 64)
		}},
		{"block_changed", blockPath, 2, func(m map[string]interface{}) { m["blockID"] = receiptBlockID(receiptHeight, "77") }},
		{"block_changed_ret", blockPath, 2, func(m map[string]interface{}) {
			m["transactions"].([]interface{})[0].(map[string]interface{})["ret"] = []interface{}{map[string]interface{}{"contractRet": "REVERT"}}
		}},
		{"stale_head", "/walletsolidity/getnowblock", 1, func(m map[string]interface{}) {
			m["block_header"].(map[string]interface{})["raw_data"].(map[string]interface{})["timestamp"] = (input.Now + 779) * 1000
		}},
		{"future_head", "/walletsolidity/getnowblock", 1, func(m map[string]interface{}) {
			m["block_header"].(map[string]interface{})["raw_data"].(map[string]interface{})["timestamp"] = (input.Now + 906) * 1000
		}},
		{"head_regress", "/walletsolidity/getnowblock", 2, func(m map[string]interface{}) {
			m["blockID"] = receiptBlockID(receiptHeight+299, "55")
			m["block_header"].(map[string]interface{})["raw_data"].(map[string]interface{})["number"] = receiptHeight + 299
		}},
		{"head_changed_id", "/walletsolidity/getnowblock", 2, func(m map[string]interface{}) { m["blockID"] = receiptBlockID(receiptHeight+300, "66") }},
		{"head_changed_time", "/walletsolidity/getnowblock", 2, func(m map[string]interface{}) {
			m["block_header"].(map[string]interface{})["raw_data"].(map[string]interface{})["timestamp"] = (input.Now + 901) * 1000
		}},
		{"head_inconsistent_number", "/walletsolidity/getnowblock", 1, func(m map[string]interface{}) {
			m["block_header"].(map[string]interface{})["raw_data"].(map[string]interface{})["number"] = receiptHeight + 299
		}},
		{"wrong_genesis", "/walletsolidity/getblockbynum:0", 1, func(m map[string]interface{}) { m["blockID"] = strings.Repeat("0", 64) }},
		{"wrong_genesis_after", "/walletsolidity/getblockbynum:0", 3, func(m map[string]interface{}) { m["blockID"] = strings.Repeat("0", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server, _ := receiptNode(t, func(key string, count int, reply *executionReply) {
				if key == test.key && count == test.count {
					test.edit(reply.value.(map[string]interface{}))
				}
			})
			observation, err := checkReceipt(t, executionConfig(server.URL))
			if err == nil || observation.Confirmed || observation.BlockID != "" || observation.TransactionID != fixture.TransactionID {
				t.Fatalf("坏证据被放行: %+v %v", observation, err)
			}
		})
	}
}

func TestCheckSettlementTransferFailsClosed(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(map[string]interface{}){
		"asset":     func(e map[string]interface{}) { e["address"] = strings.Repeat("1", 40) },
		"amount":    func(e map[string]interface{}) { e["data"] = fmt.Sprintf("%064x", 999) },
		"overspend": func(e map[string]interface{}) { e["data"] = fmt.Sprintf("%064x", 1001) },
		"payer": func(e map[string]interface{}) {
			e["topics"].([]interface{})[1] = strings.Repeat("0", 24) + strings.Repeat("1", 40)
		},
		"recipient": func(e map[string]interface{}) {
			e["topics"].([]interface{})[2] = strings.Repeat("0", 24) + strings.Repeat("1", 40)
		},
		"bad_padding": func(e map[string]interface{}) {
			value := e["topics"].([]interface{})[1].(string)
			e["topics"].([]interface{})[1] = "1" + value[1:]
		},
		"prefix41":     func(e map[string]interface{}) { e["address"] = "41" + e["address"].(string) },
		"short_data":   func(e map[string]interface{}) { e["data"] = "03e8" },
		"event":        func(e map[string]interface{}) { e["topics"].([]interface{})[0] = strings.Repeat("1", 64) },
		"short_topics": func(e map[string]interface{}) { e["topics"] = e["topics"].([]interface{})[:2] },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			server, _ := receiptNode(t, func(key string, _ int, reply *executionReply) {
				if key == "/walletsolidity/gettransactioninfobyid" {
					edit(reply.value.(map[string]interface{})["log"].([]interface{})[0].(map[string]interface{}))
				}
			})
			if observation, err := checkReceipt(t, executionConfig(server.URL)); err == nil || observation.Confirmed {
				t.Fatal("不匹配的付款事件被接受")
			}
		})
	}
	for _, name := range []string{"duplicate", "roundtrip", "fee", "null_topics", "unrelated"} {
		t.Run(name, func(t *testing.T) {
			server, _ := receiptNode(t, func(key string, _ int, reply *executionReply) {
				if key != "/walletsolidity/gettransactioninfobyid" {
					return
				}
				info := reply.value.(map[string]interface{})
				extra := receiptLog(loadVector(t))
				topics := extra["topics"].([]interface{})
				if name == "roundtrip" {
					topics[1], topics[2] = topics[2], topics[1]
				}
				if name == "fee" {
					topics[2] = strings.Repeat("0", 24) + strings.Repeat("1", 40)
				}
				if name == "unrelated" {
					extra["address"] = strings.Repeat("1", 40)
				}
				if name == "null_topics" {
					extra["topics"] = nil
				}
				info["log"] = append(info["log"].([]interface{}), extra)
			})
			observation, err := checkReceipt(t, executionConfig(server.URL))
			if name == "unrelated" {
				if err != nil || !observation.Confirmed {
					t.Fatal(err)
				}
			} else if err == nil || observation.Confirmed {
				t.Fatal("重复/回转/费用事件被接受")
			}
		})
	}
}

func TestCheckSettlementStrictJSONAndTransport(t *testing.T) {
	t.Parallel()
	for _, test := range []struct{ key, field, value string }{
		{"/walletsolidity/gettransactionbyid", "contractRet", `"REVERT"`},
		{"/walletsolidity/gettransactioninfobyid", "result", `"REVERT"`},
		{"/walletsolidity/gettransactioninfobyid", "address", `"bad"`},
		{"/walletsolidity/getnowblock", "number", "1"},
	} {
		t.Run(test.key+test.field, func(t *testing.T) {
			server, _ := receiptNode(t, func(key string, _ int, reply *executionReply) {
				if key == test.key {
					data, _ := json.Marshal(reply.value)
					needle := `"` + test.field + `":`
					reply.raw = strings.Replace(string(data), needle, needle+test.value+","+needle, 1)
				}
			})
			if observation, err := checkReceipt(t, executionConfig(server.URL)); err == nil || observation.Confirmed {
				t.Fatal("关键嵌套重复键被接受")
			}
		})
	}
	for _, raw := range []string{`[]`, `null`, `{}`, `{"Error":"sensitive-node-detail"}`, `{} {}`, strings.Repeat("x", (1<<20)+1)} {
		server, _ := receiptNode(t, func(key string, _ int, reply *executionReply) {
			if key == "/walletsolidity/getnowblock" {
				reply.raw = raw
			}
		})
		observation, err := checkReceipt(t, executionConfig(server.URL))
		if err == nil || observation.Confirmed || strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), server.URL) {
			t.Fatal("不可信响应或错误未安全处理")
		}
	}
	server, _ := receiptNode(t, func(_ string, _ int, reply *executionReply) { reply.status = 503; reply.raw = "sensitive-node-detail" })
	if observation, err := checkReceipt(t, executionConfig(server.URL)); err == nil || observation.Confirmed || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("HTTP失败未安全处理")
	}
}

func TestCheckSettlementInputCancellationAndConcurrency(t *testing.T) {
	t.Parallel()
	fixture, input := loadSettlementVector(t), loadVector(t)
	prepared := preparedFor(t, fixture, input)
	transaction := exact.SettlementTransaction{ID: fixture.TransactionID, Payload: decodeHex(t, fixture.Payload)}
	server, reads := receiptNode(t, nil)
	client := executionClient(t, executionConfig(server.URL))
	for _, bad := range []*exact.ExecutionClient{nil, {}} {
		if observation, err := bad.CheckSettlement(context.Background(), prepared, transaction, time.Unix(input.Now+900, 0)); !errors.Is(err, exact.ErrExecutionConfig) || observation.Confirmed {
			t.Fatal(err)
		}
	}
	if _, err := client.CheckSettlement(context.Background(), nil, transaction, time.Unix(input.Now+900, 0)); !errors.Is(err, exact.ErrSettlementConfig) {
		t.Fatal(err)
	}
	bad := transaction
	bad.ID = strings.Repeat("a", 64)
	if _, err := client.CheckSettlement(context.Background(), prepared, bad, time.Unix(input.Now+900, 0)); !errors.Is(err, exact.ErrSettlementBytes) {
		t.Fatal(err)
	}
	if _, err := client.CheckSettlement(context.Background(), prepared, transaction, time.Time{}); !errors.Is(err, exact.ErrExecutionInput) {
		t.Fatal(err)
	}
	otherConfig := executionConfig(server.URL)
	otherConfig.Network = tron.NetworkShasta
	chainID, _ := tron.ChainID(tron.NetworkShasta)
	otherConfig.GenesisBlockID = strings.Repeat("00", 8) + strings.Repeat("11", 20) + fmt.Sprintf("%08x", chainID)
	if _, err := executionClient(t, otherConfig).CheckSettlement(context.Background(), prepared, transaction, time.Unix(input.Now+900, 0)); !errors.Is(err, exact.ErrChainMismatch) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if observation, err := client.CheckSettlement(ctx, prepared, transaction, time.Unix(input.Now+900, 0)); !errors.Is(err, context.Canceled) || observation.Confirmed {
		t.Fatal(err)
	}
	if reads.Load() != 0 {
		t.Fatal("输入或取消校验前访问了节点")
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			observation, err := client.CheckSettlement(context.Background(), prepared, transaction, time.Unix(input.Now+900, 0))
			if err != nil || !observation.Confirmed {
				t.Error("并发核验失败")
			}
		})
	}
	wg.Wait()
	slow, _ := receiptNode(t, func(string, int, *executionReply) { time.Sleep(30 * time.Millisecond) })
	config := executionConfig(slow.URL)
	config.Timeout = 10 * time.Millisecond
	if observation, err := checkReceipt(t, config); !errors.Is(err, context.DeadlineExceeded) || observation.Confirmed {
		t.Fatal(err)
	}
}
