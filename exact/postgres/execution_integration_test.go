package postgres_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
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

type executionFixture struct {
	Config        exact.SettlementBuilderConfig `json:"config"`
	Reference     exact.ReferenceBlock          `json:"reference"`
	Signature     string                        `json:"signature"`
	TransactionID string                        `json:"transactionID"`
	RawDataHex    string                        `json:"rawDataHex"`
	Payload       string                        `json:"payload"`
}

func prepareExecution(t *testing.T, ctx context.Context, store *postgres.Store, input vector) (executionFixture, postgres.Record, *exact.PreparedSettlement, exact.SettlementTransaction) {
	t.Helper()
	data, err := os.ReadFile("../testdata/tronweb-settlement.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture executionFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	record := reserve(t, ctx, store, input)
	builder, err := exact.NewSettlementBuilder(fixture.Config)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(envelope(t, input), input.Requirements, fixture.Reference, time.Unix(input.Now, 0))
	if err != nil {
		t.Fatal(err)
	}
	signature, err := hex.DecodeString(fixture.Signature)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := prepared.Sign(ctx, func(context.Context, [32]byte) ([]byte, error) { return signature, nil }, time.Unix(input.Now, 0))
	if err != nil {
		t.Fatal(err)
	}
	return fixture, record, prepared, signed
}

func executionGenesis() string {
	return strings.Repeat("00", 8) + strings.Repeat("11", 20) + "cd8690dc"
}

func journalNode(t *testing.T, ctx context.Context, db *pgxpool.Pool, record postgres.Record, fixture executionFixture, response func() interface{}) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	writes := new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var params map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&params) != nil {
			t.Error("原生请求参数错误")
			w.WriteHeader(400)
			return
		}
		switch r.URL.Path {
		case "/wallet/getblockbynum":
			_ = json.NewEncoder(w).Encode(map[string]string{"blockID": executionGenesis()})
		case "/wallet/broadcasthex":
			writes.Add(1)
			var state, txid string
			var payload []byte
			if err := db.QueryRow(ctx, "SELECT state, transaction_id, signed_payload FROM x402_tron_settlements WHERE id = $1", record.ID).Scan(&state, &txid, &payload); err != nil || state != string(postgres.StateBroadcastUnknown) || txid != fixture.TransactionID || hex.EncodeToString(payload) != fixture.Payload {
				t.Error("网络写入前没有提交原始交易和 unknown")
			}
			var sent string
			if json.Unmarshal(params["transaction"], &sent) != nil || sent != fixture.Payload {
				t.Error("发送的不是数据库原始字节")
			}
			_ = json.NewEncoder(w).Encode(response())
		case "/wallet/gettransactionbyid":
			var id string
			if json.Unmarshal(params["value"], &id) != nil || id != fixture.TransactionID {
				t.Error("恢复后没有查询原 txid")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"txID": fixture.TransactionID, "raw_data_hex": fixture.RawDataHex})
		default:
			t.Error("调用了非预期接口")
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	return server, writes
}

func newExecutionClient(t *testing.T, endpoint string, httpClient *http.Client) *exact.ExecutionClient {
	t.Helper()
	client, err := exact.NewExecutionClient(exact.ExecutionConfig{
		Network: tron.NetworkNile, NodeURL: endpoint, GenesisBlockID: executionGenesis(), HTTPClient: httpClient, AllowInsecureLocalHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestSubmitConcurrentSingleBroadcast(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	fixture, record, prepared, signed := prepareExecution(t, ctx, store, input)
	if _, err := store.SaveTransaction(ctx, record.ID, record.Authorization.Digest, postgres.Transaction{ID: signed.ID, Payload: signed.Payload}); err != nil {
		t.Fatal(err)
	}
	server, writes := journalNode(t, ctx, db, record, fixture, func() interface{} {
		return map[string]interface{}{"result": true, "code": "SUCCESS", "txid": fixture.TransactionID}
	})
	client := newExecutionClient(t, server.URL, nil)
	other, err := postgres.New(db, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	stores := []*postgres.Store{store, other}
	var wg sync.WaitGroup
	var successful atomic.Int32
	for n := range 16 {
		wg.Go(func() {
			result, err := stores[n%2].Submit(ctx, record.ID, prepared, client, time.Unix(input.Now, 0))
			if err == nil {
				successful.Add(1)
				if result.State != postgres.StatePending || result.Transaction.ID != signed.ID {
					t.Error("受理后状态或 txid 不符")
				}
			} else if !errors.Is(err, postgres.ErrBroadcastStarted) {
				t.Errorf("并发 Submit 错误类别：%v", err)
			}
		})
	}
	wg.Wait()
	if writes.Load() != 1 || successful.Load() != 1 {
		t.Fatalf("写次数/成功领取次数：%d/%d", writes.Load(), successful.Load())
	}
	observed, err := other.Track(ctx, record.ID, client)
	if err != nil || !observed.Found || observed.TransactionID != signed.ID || writes.Load() != 1 {
		t.Fatalf("原交易查询失败或重播：%v", err)
	}
}

type lostBroadcastResponse struct{ base http.RoundTripper }

func (transport lostBroadcastResponse) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err == nil && request.URL.Path == "/wallet/broadcasthex" {
		response.Body.Close()
		// 节点实际收到交易，但调用方未收到 ACK，模拟响应丢失而不是提前阻止写入。
		return nil, context.DeadlineExceeded
	}
	return response, err
}

func TestSubmitLostResponseRecoveryDoesNotRebroadcast(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	fixture, record, prepared, signed := prepareExecution(t, ctx, store, input)
	if _, err := store.SaveTransaction(ctx, record.ID, record.Authorization.Digest, postgres.Transaction{ID: signed.ID, Payload: signed.Payload}); err != nil {
		t.Fatal(err)
	}
	server, writes := journalNode(t, ctx, db, record, fixture, func() interface{} {
		return map[string]interface{}{"result": true, "code": "SUCCESS", "txid": fixture.TransactionID}
	})
	client := newExecutionClient(t, server.URL, &http.Client{Transport: lostBroadcastResponse{base: http.DefaultTransport}})
	unknown, err := store.Submit(ctx, record.ID, prepared, client, time.Unix(input.Now, 0))
	if !errors.Is(err, exact.ErrBroadcastUnknown) || !errors.Is(err, context.DeadlineExceeded) || unknown.State != postgres.StateBroadcastUnknown || unknown.Transaction.ID != signed.ID || writes.Load() != 1 {
		t.Fatalf("结果未知未保留原交易：%v", err)
	}
	restarted, err := postgres.New(db, input.Config)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := restarted.Submit(ctx, record.ID, nil, client, time.Unix(input.Now+1000, 0)); !errors.Is(err, postgres.ErrBroadcastStarted) {
			t.Fatal("恢复流程重新获得了发送许可")
		}
		observation, err := restarted.Track(ctx, record.ID, client)
		if err != nil || !observation.Found || observation.TransactionID != signed.ID {
			t.Fatalf("无法无 Prepare 查询原交易：%v", err)
		}
	}
	after, err := restarted.Get(ctx, record.ID)
	if err != nil || after.State != postgres.StateBroadcastUnknown || !bytes.Equal(after.Transaction.Payload, signed.Payload) || writes.Load() != 1 {
		t.Fatal("只读查找修改了 unknown 或再次广播")
	}
}

func TestSubmitRejectedAndAcceptedPersistenceFailureRemainUnknown(t *testing.T) {
	for _, failure := range []string{"rejected", "wrong_id", "save_ack_failed"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			ctx, db, store, input := setup(t)
			fixture, record, prepared, signed := prepareExecution(t, ctx, store, input)
			if _, err := store.SaveTransaction(ctx, record.ID, record.Authorization.Digest, postgres.Transaction{ID: signed.ID, Payload: signed.Payload}); err != nil {
				t.Fatal(err)
			}
			server, writes := journalNode(t, ctx, db, record, fixture, func() interface{} {
				if failure == "save_ack_failed" {
					if _, err := db.Exec(ctx, "ALTER TABLE x402_tron_settlements RENAME TO failed_ack_journal"); err != nil {
						t.Error("模拟 ACK 持久化异常失败")
					}
				}
				id := fixture.TransactionID
				if failure == "wrong_id" {
					id = strings.Repeat("a", 64)
				}
				return map[string]interface{}{"result": failure != "rejected", "txid": id}
			})
			client := newExecutionClient(t, server.URL, nil)
			unknown, err := store.Submit(ctx, record.ID, prepared, client, time.Unix(input.Now, 0))
			want := exact.ErrBroadcastUnknown
			if failure == "rejected" {
				want = exact.ErrBroadcastRejected
			}
			if !errors.Is(err, want) || unknown.State != postgres.StateBroadcastUnknown || unknown.Transaction.ID != signed.ID || writes.Load() != 1 {
				t.Fatalf("失败路径丢失了原交易：%v", err)
			}
			if failure == "save_ack_failed" {
				var state, id string
				if err := db.QueryRow(ctx, "SELECT state, transaction_id FROM failed_ack_journal WHERE id = $1", record.ID).Scan(&state, &id); err != nil || state != string(postgres.StateBroadcastUnknown) || id != signed.ID {
					t.Fatal("受理后 SQL 失败没有保留 unknown")
				}
			}
		})
	}
}

func TestSubmitGuardsDoNotStartBroadcast(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	fixture, record, prepared, signed := prepareExecution(t, ctx, store, input)
	server, writes := journalNode(t, ctx, db, record, fixture, func() interface{} { return nil })
	client := newExecutionClient(t, server.URL, nil)
	if _, err := store.Submit(ctx, record.ID, prepared, client, time.Unix(input.Now, 0)); !errors.Is(err, postgres.ErrNotPrepared) {
		t.Fatal("未保存签名数据仍允许提交")
	}
	if _, err := store.Track(ctx, record.ID, client); !errors.Is(err, postgres.ErrNotSubmitted) {
		t.Fatal("未提交记录进入了追踪")
	}
	if _, err := store.SaveTransaction(ctx, record.ID, record.Authorization.Digest, postgres.Transaction{ID: signed.ID, Payload: signed.Payload}); err != nil {
		t.Fatal(err)
	}
	for _, call := range []func() error{
		func() error { _, err := store.Submit(ctx, record.ID, nil, client, time.Unix(input.Now, 0)); return err },
		func() error {
			_, err := store.Submit(ctx, record.ID, prepared, nil, time.Unix(input.Now, 0))
			return err
		},
		func() error {
			_, err := store.Submit(ctx, record.ID, prepared, client, time.Unix(input.Now+1000, 0))
			return err
		},
	} {
		if err := call(); err == nil {
			t.Fatal("无效提交通过")
		}
	}
	after, err := store.Get(ctx, record.ID)
	if err != nil || after.State != postgres.StatePrepared || writes.Load() != 0 {
		t.Fatal("本地校验失败仍推进了广播状态")
	}
}
