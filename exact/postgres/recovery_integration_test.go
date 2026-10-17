package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/exact/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 公开签名向量及合成回执，没有真实 Nile 节点或资产。
func recoveryNode(t *testing.T, fixture executionFixture, input vector) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	reads, mode := new(atomic.Int32), new(atomic.Int32)
	const height = 71846435
	block := func(number int, stamp int64) map[string]interface{} {
		return map[string]interface{}{
			"blockID": fmt.Sprintf("%016x", number) + strings.Repeat("44", 24),
			"block_header": map[string]interface{}{"raw_data": map[string]interface{}{
				"number": number, "timestamp": stamp,
			}},
		}
	}
	body := func() map[string]interface{} {
		return map[string]interface{}{
			"txID": fixture.TransactionID, "raw_data_hex": fixture.RawDataHex,
			"ret": []interface{}{map[string]interface{}{"contractRet": "SUCCESS"}},
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		var params map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&params) != nil {
			t.Error("核验参数无效")
			w.WriteHeader(400)
			return
		}
		var reply interface{}
		switch r.URL.Path {
		case "/walletsolidity/getblockbynum":
			if string(params["num"]) == "0" {
				reply = map[string]string{"blockID": executionGenesis()}
			} else {
				if string(params["num"]) != fmt.Sprint(height) {
					t.Error("未核验原交易块")
				}
				value := block(height, (input.Now+3)*1000)
				value["transactions"] = []interface{}{body()}
				reply = value
			}
		case "/walletsolidity/getnowblock":
			reply = block(height+300, (input.Now+900)*1000)
		case "/walletsolidity/gettransactionbyid", "/walletsolidity/gettransactioninfobyid":
			if string(params["value"]) != `"`+fixture.TransactionID+`"` {
				t.Error("没有查询持久化原交易")
			}
			if mode.Load() == 0 {
				reply = map[string]interface{}{}
			} else if r.URL.Path == "/walletsolidity/gettransactionbyid" {
				reply = body()
			} else {
				amount := 1000
				if mode.Load() == 2 {
					amount--
				}
				reply = map[string]interface{}{
					"id": fixture.TransactionID, "blockNumber": height, "blockTimeStamp": (input.Now + 3) * 1000,
					"receipt": map[string]string{"result": "SUCCESS"},
					"log": []interface{}{map[string]interface{}{
						"address": input.Authorization.Permitted.Token[2:],
						"topics": []string{
							"ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
							strings.Repeat("0", 24) + input.Authorization.From[2:],
							strings.Repeat("0", 24) + input.Authorization.Witness.To[2:],
						},
						"data": fmt.Sprintf("%064x", amount),
					}},
				}
			}
		default:
			t.Errorf("恢复核验执行了非只读操作: %s", r.URL.Path)
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(server.Close)
	return server, reads, mode
}

func TestCheckSettlementAfterJournalRestart(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	fixture, record, _, signed := prepareExecution(t, ctx, store, input)
	server, reads, mode := recoveryNode(t, fixture, input)
	client, now := newExecutionClient(t, server.URL, nil), time.Unix(input.Now+900, 0)
	check := func(store *postgres.Store) (exact.SettlementObservation, error) {
		return store.CheckSettlement(ctx, record.ID, fixture.Config.ExecutorAddress, client, now)
	}
	if _, err := check(store); !errors.Is(err, postgres.ErrNotSubmitted) {
		t.Fatal("reserved 进入了固化核验")
	}
	if _, err := store.SaveTransaction(ctx, record.ID, record.Authorization.Digest, postgres.Transaction{ID: signed.ID, Payload: signed.Payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := check(store); !errors.Is(err, postgres.ErrNotSubmitted) || reads.Load() != 0 {
		t.Fatal("prepared 进入了节点核验")
	}
	before, err := store.BeginBroadcast(ctx, record.ID, record.Authorization.Digest)
	if err != nil {
		t.Fatal(err)
	}
	// 使用新的连接池和 Store，恢复入口不传入原 PreparedSettlement。
	reopened, err := pgxpool.NewWithConfig(ctx, db.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.Close)
	restarted, err := postgres.New(reopened, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	for _, state := range []postgres.State{postgres.StateBroadcastUnknown, postgres.StatePending} {
		if state == postgres.StatePending {
			before, err = restarted.RecordAccepted(ctx, record.ID, record.Authorization.Digest, signed.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, value := range []int32{0, 2, 1} {
			mode.Store(value)
			observation, err := check(restarted)
			if value == 0 && (err != nil || observation.Confirmed) || value == 2 && (!errors.Is(err, exact.ErrSettlementTransfer) || observation.Confirmed) || value == 1 && (err != nil || !observation.Confirmed || observation.TransactionID != signed.ID || observation.BlockNumber != 71846435) {
				t.Fatalf("重启后 %s 模式 %d 核验结果无效: %+v %v", state, value, observation, err)
			}
		}
		after, err := restarted.Get(ctx, record.ID)
		if err != nil || after.State != before.State || !after.UpdatedAt.Equal(before.UpdatedAt) || after.Transaction.ID != before.Transaction.ID || !bytes.Equal(after.Transaction.Payload, before.Transaction.Payload) {
			t.Fatal("只读核验改变了状态或原始交易")
		}
	}
	mode.Store(1)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			observation, err := check(restarted)
			if err != nil || !observation.Confirmed {
				t.Errorf("并发恢复核验失败: %v", err)
			}
		})
	}
	wg.Wait()
	if _, err := restarted.BeginBroadcast(ctx, record.ID, record.Authorization.Digest); !errors.Is(err, postgres.ErrBroadcastStarted) {
		t.Fatal("核验后重新获得广播许可")
	}
}

func TestCheckSettlementRecoveryRejectsUntrustedBinding(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	fixture, record, _, signed := prepareExecution(t, ctx, store, input)
	if _, err := store.SaveTransaction(ctx, record.ID, record.Authorization.Digest, postgres.Transaction{ID: signed.ID, Payload: signed.Payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginBroadcast(ctx, record.ID, record.Authorization.Digest); err != nil {
		t.Fatal(err)
	}
	server, reads, _ := recoveryNode(t, fixture, input)
	client, now := newExecutionClient(t, server.URL, nil), time.Unix(input.Now+900, 0)
	if _, err := store.CheckSettlement(ctx, record.ID, input.Config.Permit2Address, client, now); !errors.Is(err, exact.ErrSettlementBytes) {
		t.Fatal("未拒绝错误费用付款方")
	}
	changed := input.Config
	changed.ProxyAddress = fixture.Config.ExecutorAddress
	other, err := postgres.New(db, changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.CheckSettlement(ctx, record.ID, fixture.Config.ExecutorAddress, client, now); !errors.Is(err, exact.ErrSettlementBytes) {
		t.Fatal("另一代理解释了原交易")
	}
	if _, err := store.CheckSettlement(ctx, record.ID, fixture.Config.ExecutorAddress, nil, now); !errors.Is(err, exact.ErrExecutionConfig) {
		t.Fatal("接受了 nil 执行客户端")
	}
	if _, err := store.CheckSettlement(ctx, "invalid", fixture.Config.ExecutorAddress, client, now); !errors.Is(err, postgres.ErrInvalidInput) {
		t.Fatal("接受了非法日志 ID")
	}
	if _, err := store.CheckSettlement(ctx, strings.Repeat("f", 64), fixture.Config.ExecutorAddress, client, now); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatal("未保留不存在错误")
	}
	otherClient, err := exact.NewExecutionClient(exact.ExecutionConfig{
		Network: tron.NetworkMainnet, NodeURL: server.URL, AllowInsecureLocalHTTP: true,
		GenesisBlockID: strings.Repeat("00", 8) + strings.Repeat("11", 20) + fmt.Sprintf("%08x", 728126428),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckSettlement(ctx, record.ID, fixture.Config.ExecutorAddress, otherClient, now); !errors.Is(err, exact.ErrChainMismatch) {
		t.Fatal("使用另一网络的客户端核验了日志")
	}
	// 仅修改本用例随机 schema，验证历史授权标识与签名原交易必须一致。
	differentDigest := record.Authorization.Digest
	differentDigest[0] ^= 1
	if _, err := db.Exec(ctx, "UPDATE x402_tron_settlements SET digest = $2 WHERE id = $1", record.ID, differentDigest[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CheckSettlement(ctx, record.ID, fixture.Config.ExecutorAddress, client, now); !errors.Is(err, postgres.ErrConflict) {
		t.Fatal("核验忽略了持久化授权摘要")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.CheckSettlement(canceled, record.ID, fixture.Config.ExecutorAddress, client, now); !errors.Is(err, context.Canceled) {
		t.Fatal("未保留取消错误")
	}
	if reads.Load() != 0 {
		t.Fatal("本地绑定校验失败后仍读取了节点")
	}
}

func TestCheckSettlementDoesNotInterpretOpaqueJournalBytes(t *testing.T) {
	t.Parallel()
	ctx, _, store, input := setup(t)
	record := reserve(t, ctx, store, input)
	tx := transaction("a")
	if _, err := store.SaveTransaction(ctx, record.ID, record.Authorization.Digest, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginBroadcast(ctx, record.ID, record.Authorization.Digest); err != nil {
		t.Fatal(err)
	}
	server, reads, _ := recoveryNode(t, executionFixture{}, input)
	client := newExecutionClient(t, server.URL, nil)
	if result, err := store.CheckSettlement(ctx, record.ID, input.Config.Permit2Address, client, time.Unix(input.Now+900, 0)); !errors.Is(err, exact.ErrSettlementBytes) || result.Confirmed || reads.Load() != 0 {
		t.Fatalf("不透明旧记录被当成了付款: %v", err)
	}
}
