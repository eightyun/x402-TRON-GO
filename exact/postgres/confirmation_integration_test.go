package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/exact/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

type confirmationTest struct {
	ctx     context.Context
	db      *pgxpool.Pool
	store   *postgres.Store
	input   vector
	fixture executionFixture
	before  postgres.Record
	server  *httptest.Server
	reads   *atomic.Int32
	mode    *atomic.Int32
	client  *exact.ExecutionClient
	now     time.Time
}

func prepareConfirmation(t *testing.T, state postgres.State) confirmationTest {
	t.Helper()
	ctx, db, store, input := setup(t)
	fixture, record, _, signed := prepareExecution(t, ctx, store, input)
	if _, err := store.SaveTransaction(ctx, record.ID, record.Authorization.Digest, postgres.Transaction{ID: signed.ID, Payload: signed.Payload}); err != nil {
		t.Fatal(err)
	}
	record, err := store.BeginBroadcast(ctx, record.ID, record.Authorization.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if state == postgres.StatePending {
		record, err = store.RecordAccepted(ctx, record.ID, record.Authorization.Digest, signed.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	server, reads, mode := recoveryNode(t, fixture, input)
	mode.Store(1)
	return confirmationTest{ctx, db, store, input, fixture, record, server, reads, mode,
		newExecutionClient(t, server.URL, nil), time.Unix(input.Now+900, 0)}
}

func (test confirmationTest) confirm() (postgres.Record, error) {
	return test.store.ConfirmSettlement(test.ctx, test.before.ID, test.fixture.Config.ExecutorAddress, test.client, test.now)
}

func TestConfirmSettlementPersistsAndSurvivesRestart(t *testing.T) {
	for _, state := range []postgres.State{postgres.StateBroadcastUnknown, postgres.StatePending} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			input := prepareConfirmation(t, state)
			confirmed, err := input.confirm()
			if err != nil || confirmed.State != postgres.StateSucceeded || confirmed.Confirmation == nil {
				t.Fatalf("未持久化成功终态: %v", err)
			}
			if confirmed.Confirmation.BlockNumber != 71846435 || confirmed.Confirmation.BlockID != "0000000004484a23"+strings.Repeat("44", 24) || confirmed.Confirmation.ConfirmedAt.IsZero() || !confirmed.CreatedAt.Equal(input.before.CreatedAt) || confirmed.Authorization != input.before.Authorization || !reflect.DeepEqual(confirmed.Transaction, input.before.Transaction) {
				t.Fatal("确认结果没有绑定原交易/区块/授权")
			}
			for range 2 {
				if _, err := input.db.Exec(input.ctx, postgres.UpgradeConfirmationSQL); err != nil {
					t.Fatal(err)
				}
			}
			input.mode.Store(0)
			audit, err := input.store.CheckSettlement(input.ctx, confirmed.ID, input.fixture.Config.ExecutorAddress, input.client, input.now)
			if err != nil || audit.Confirmed {
				t.Fatal("成功后的显式审计将缺失证据当作当前确认")
			}
			unchanged, err := input.store.Get(input.ctx, confirmed.ID)
			if err != nil || !reflect.DeepEqual(unchanged, confirmed) {
				t.Fatal("后续缺失证据降级了持久化成功")
			}
			lookupServer, writes := journalNode(t, input.ctx, input.db, confirmed, input.fixture, func() interface{} { return nil })
			tracked, err := input.store.Track(input.ctx, confirmed.ID, newExecutionClient(t, lookupServer.URL, nil))
			if err != nil || !tracked.Found || writes.Load() != 0 {
				t.Fatal("成功后无法只读追踪原交易")
			}
			otherDB, err := pgxpool.NewWithConfig(input.ctx, input.db.Config())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(otherDB.Close)
			restarted, err := postgres.New(otherDB, input.input.Config)
			if err != nil {
				t.Fatal(err)
			}
			input.db.Close()
			input.store = restarted
			reads := input.reads.Load()
			input.server.Close()
			for range 2 {
				again, err := input.confirm()
				if err != nil || !reflect.DeepEqual(again, confirmed) || input.reads.Load() != reads {
					t.Fatalf("已确认结果依赖节点或时间被重写: %v", err)
				}
				accepted, err := restarted.RecordAccepted(input.ctx, confirmed.ID, confirmed.Authorization.Digest, confirmed.Transaction.ID)
				if err != nil || !reflect.DeepEqual(accepted, confirmed) {
					t.Fatal("迟到的受理回调降级或改变了成功记录")
				}
				saved, err := restarted.SaveTransaction(input.ctx, confirmed.ID, confirmed.Authorization.Digest, *confirmed.Transaction)
				if err != nil || !reflect.DeepEqual(saved, confirmed) {
					t.Fatal("幂等保存原交易改变了成功记录")
				}
			}
			if _, err := restarted.BeginBroadcast(input.ctx, confirmed.ID, confirmed.Authorization.Digest); !errors.Is(err, postgres.ErrBroadcastStarted) {
				t.Fatal("成功后重新获取了广播许可")
			}
			if _, err := restarted.Submit(input.ctx, confirmed.ID, nil, input.client, input.now); !errors.Is(err, postgres.ErrBroadcastStarted) {
				t.Fatal("成功后仍试图提交")
			}
			if _, err := restarted.SaveTransaction(input.ctx, confirmed.ID, confirmed.Authorization.Digest, transaction("f")); !errors.Is(err, postgres.ErrConflict) {
				t.Fatal("成功后替换了交易")
			}
			if _, err := restarted.RecordAccepted(input.ctx, confirmed.ID, confirmed.Authorization.Digest, transaction("f").ID); !errors.Is(err, postgres.ErrConflict) {
				t.Fatal("成功后接受了另一个 txid 的迟到回调")
			}
			if _, err := restarted.ConfirmSettlement(input.ctx, confirmed.ID, input.input.Config.Permit2Address, input.client, input.now); err == nil {
				t.Fatal("读取成功缓存跳过了可信费用付款方校验")
			}
			otherConfig := input.input.Config
			otherConfig.ProxyAddress = input.fixture.Config.ExecutorAddress
			other, err := postgres.New(otherDB, otherConfig)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := other.ConfirmSettlement(input.ctx, confirmed.ID, input.fixture.Config.ExecutorAddress, input.client, input.now); err == nil {
				t.Fatal("另一代理读取成功缓存跳过了原绑定校验")
			}
			serialized, err := json.Marshal(confirmed)
			if err != nil || bytes.Contains(serialized, []byte("Payload")) || !bytes.Contains(serialized, []byte("Confirmation")) {
				t.Fatal("成功记录序列化泄漏签名字节或遗漏确认")
			}
		})
	}
}

func TestConfirmSettlementFailuresDoNotChangeJournal(t *testing.T) {
	t.Parallel()
	input := prepareConfirmation(t, postgres.StatePending)
	for _, test := range []struct {
		mode int32
		want error
	}{{0, postgres.ErrSettlementUnconfirmed}, {2, exact.ErrSettlementTransfer}} {
		input.mode.Store(test.mode)
		result, err := input.confirm()
		if !errors.Is(err, test.want) || result.State == postgres.StateSucceeded || result.Confirmation != nil {
			t.Fatalf("失败或未确认被当作成功: %v", err)
		}
	}
	input.mode.Store(1)
	// 仅在本用例随机 schema 注入数据库故障，不改变生产代码或外部库。
	if _, err := input.db.Exec(input.ctx, `CREATE FUNCTION reject_confirmation() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic-sensitive-database-message'; END $$;
		CREATE TRIGGER reject_confirmation BEFORE UPDATE ON x402_tron_settlements
		FOR EACH ROW WHEN (NEW.state = 'succeeded') EXECUTE FUNCTION reject_confirmation()`); err != nil {
		t.Fatal(err)
	}
	result, err := input.confirm()
	if !errors.Is(err, postgres.ErrStore) || result.State == postgres.StateSucceeded || result.Confirmation != nil || strings.Contains(err.Error(), "synthetic-sensitive") {
		t.Fatal("失败的数据库写入被当作成功或泄漏原始错误")
	}
	after, err := input.store.Get(input.ctx, input.before.ID)
	if err != nil || !reflect.DeepEqual(after, input.before) {
		t.Fatal("失败确认改变了原日志")
	}
}

// 确保所有调用都读取 pending 后再返回证据，实际覆盖并发 CAS，而非只读成功缓存。
func confirmationBarrierNode(t *testing.T, source *httptest.Server, workers int32, conflicting bool) *httptest.Server {
	t.Helper()
	var entered atomic.Int32
	gate := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/walletsolidity/gettransactionbyid" {
			if entered.Add(1) == workers {
				close(gate)
			}
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		if !conflicting || r.Header.Get("TRON-PRO-API-KEY") != "synthetic-alternate-view" {
			source.Config.Handler.ServeHTTP(w, r)
			return
		}
		recorder := httptest.NewRecorder()
		source.Config.Handler.ServeHTTP(recorder, r)
		var value map[string]interface{}
		if json.Unmarshal(recorder.Body.Bytes(), &value) != nil {
			t.Error("合成回执无效")
			w.WriteHeader(500)
			return
		}
		if id, ok := value["blockID"].(string); ok && id != executionGenesis() {
			value["blockID"] = id[:16] + strings.Repeat("55", 24)
		}
		_ = json.NewEncoder(w).Encode(value)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestConfirmSettlementConcurrentIdempotency(t *testing.T) {
	t.Parallel()
	input := prepareConfirmation(t, postgres.StatePending)
	const workers = 16
	server := confirmationBarrierNode(t, input.server, workers, false)
	input.client = newExecutionClient(t, server.URL, nil)
	otherDB, err := pgxpool.NewWithConfig(input.ctx, input.db.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(otherDB.Close)
	other, err := postgres.New(otherDB, input.input.Config)
	if err != nil {
		t.Fatal(err)
	}
	stores := []*postgres.Store{input.store, other}
	results := make(chan postgres.Record, workers)
	var wg sync.WaitGroup
	for index := range workers {
		wg.Go(func() {
			record, err := stores[index%2].ConfirmSettlement(input.ctx, input.before.ID, input.fixture.Config.ExecutorAddress, input.client, input.now)
			if err != nil {
				t.Errorf("并发确认失败: %v", err)
				return
			}
			results <- record
		})
	}
	for range workers {
		wg.Go(func() {
			if _, err := input.store.RecordAccepted(input.ctx, input.before.ID, input.before.Authorization.Digest, input.before.Transaction.ID); err != nil {
				t.Errorf("并发受理回调失败: %v", err)
			}
		})
	}
	wg.Wait()
	close(results)
	final, err := input.store.Get(input.ctx, input.before.ID)
	if err != nil || final.State != postgres.StateSucceeded || final.Confirmation == nil {
		t.Fatal("并发确认没有持久化成功")
	}
	for result := range results {
		if !reflect.DeepEqual(result, final) {
			t.Fatal("并发确认覆盖了首个区块或时间")
		}
	}
}

func TestConfirmSettlementConflictingEvidenceNeverOverwrites(t *testing.T) {
	t.Parallel()
	input := prepareConfirmation(t, postgres.StatePending)
	server := confirmationBarrierNode(t, input.server, 2, true)
	first := newExecutionClient(t, server.URL, nil)
	second, err := exact.NewExecutionClient(exact.ExecutionConfig{
		Network: input.input.Config.Network, NodeURL: server.URL, AllowInsecureLocalHTTP: true,
		GenesisBlockID: executionGenesis(), APIKey: "synthetic-alternate-view",
	})
	if err != nil {
		t.Fatal(err)
	}
	var successes, conflicts atomic.Int32
	var wg sync.WaitGroup
	for _, client := range []*exact.ExecutionClient{first, second} {
		wg.Go(func() {
			record, err := input.store.ConfirmSettlement(input.ctx, input.before.ID, input.fixture.Config.ExecutorAddress, client, input.now)
			if err == nil && record.State == postgres.StateSucceeded {
				successes.Add(1)
			} else if errors.Is(err, postgres.ErrConflict) && record.State != postgres.StateSucceeded {
				conflicts.Add(1)
			} else {
				t.Errorf("冲突核验结果无效: %v", err)
			}
		})
	}
	wg.Wait()
	if successes.Load() != 1 || conflicts.Load() != 1 {
		t.Fatalf("冲突区块成功/拒绝次数为 %d/%d", successes.Load(), conflicts.Load())
	}
	final, err := input.store.Get(input.ctx, input.before.ID)
	if err != nil || final.State != postgres.StateSucceeded || final.Confirmation == nil {
		t.Fatal("冲突核验丢失首个成功记录")
	}
}
