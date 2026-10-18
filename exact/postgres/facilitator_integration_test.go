package postgres_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/exact/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
	"golang.org/x/crypto/sha3"
)

const facilitatorHeight = uint64(71846434)

type facilitatorTest struct {
	ctx            context.Context
	db             *pgxpool.Pool
	store          *postgres.Store
	input          vector
	config         postgres.FacilitatorConfig
	server         *httptest.Server
	signs          atomic.Int32
	writes         atomic.Int32
	jsonRPCs       atomic.Int32
	simulations    atomic.Int32
	simulationMode atomic.Int32
	onSimulation   func()
	mode           atomic.Int32
	used           atomic.Bool
	onMissing      func()
	mu             sync.Mutex
	signed         exact.SettlementTransaction
	raw            []byte
	stamp          int64
	headTime       int64
	simulatedData  []byte
}

func prepareFacilitator(t *testing.T, age time.Duration) *facilitatorTest {
	t.Helper()
	ctx, db, store, input := setup(t)
	payer, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(payer.Zero)
	executor, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(executor.Zero)
	hash := sha3.NewLegacyKeccak256()
	hash.Write(executor.PubKey().SerializeUncompressed()[1:])
	executorAddress, err := tron.NormalizeAddressBase58("41" + hex.EncodeToString(hash.Sum(nil)[12:]))
	if err != nil {
		t.Fatal(err)
	}
	input.Now = time.Now().Add(-age).Unix()
	input.Authorization.Deadline = fmt.Sprint(input.Now + 60)
	input.Requirements.Extra = map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"}
	input = signedVariant(t, input, payer)
	fixture := &facilitatorTest{ctx: ctx, db: db, store: store, input: input, headTime: time.Now().UnixMilli()}
	fixture.mode.Store(1)
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fixture.node(t, w, r) }))
	t.Cleanup(fixture.server.Close)
	fixture.config = postgres.FacilitatorConfig{
		Preflight: exact.PreflightConfig{
			Authorization: input.Config, NodeURL: fixture.server.URL + "/jsonrpc", AllowInsecureLocalHTTP: true,
			Permit2CodeHash: "0xa9c7ddf1ae0410c3a502ed7c7152ebd190fad85ab7fe3497c68869d5079c5219",
			ProxyCodeHash:   "0x7efcce47028dabcb0d42f3a7eda8820bf6f7f4e618398c2547d52f703cafb073",
		},
		Execution: exact.ExecutionConfig{Network: input.Config.Network, NodeURL: fixture.server.URL,
			GenesisBlockID: executionGenesis(), AllowInsecureLocalHTTP: true},
		Policy:          exact.ServerConfig{Network: input.Config.Network, Asset: input.Requirements.Asset, PayTo: input.Requirements.PayTo},
		ExecutorAddress: executorAddress, MaxAmount: "1000", FeeLimitSUN: 100_000_000, Lifetime: 30 * time.Second,
		Timeout: 15 * time.Second, PollInterval: time.Second,
		Signer: func(ctx context.Context, digest [32]byte) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			fixture.signs.Add(1)
			compact := ecdsa.SignCompact(executor, digest[:], false)
			return append(append([]byte{}, compact[1:]...), compact[0]-27), nil
		},
	}
	return fixture
}

func (fixture *facilitatorTest) mechanism(t *testing.T) *postgres.Facilitator {
	t.Helper()
	mechanism, err := postgres.NewFacilitator(fixture.store, fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	return mechanism
}

func (fixture *facilitatorTest) capture(t *testing.T, signed exact.SettlementTransaction, stamp int64) {
	t.Helper()
	if len(signed.Payload) < 3 || signed.Payload[0] != 0x0a {
		t.Fatal("测试交易格式错误")
	}
	length, n := binary.Uvarint(signed.Payload[1:])
	if n <= 0 || length > uint64(len(signed.Payload)-1-n) {
		t.Fatal("测试 raw 长度错误")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	fixture.signed, fixture.raw = signed, append([]byte(nil), signed.Payload[1+n:1+n+int(length)]...)
	fixture.stamp, fixture.headTime = stamp, time.Now().UnixMilli()
	if fixture.headTime < stamp {
		fixture.headTime = stamp
	}
}

// 节点、区块和回执均为合成数据；签名来自本次测试临时生成的密钥，不读取现有钱包。
func (fixture *facilitatorTest) node(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	if fixture.mode.Load() == 3 && strings.HasPrefix(r.URL.Path, "/walletsolidity/") {
		w.WriteHeader(503)
		return
	}
	var params map[string]json.RawMessage
	if json.NewDecoder(r.Body).Decode(&params) != nil {
		t.Error("请求无效")
		w.WriteHeader(400)
		return
	}
	if r.URL.Path == "/jsonrpc" {
		fixture.jsonRPCs.Add(1)
		var method string
		_ = json.Unmarshal(params["method"], &method)
		var arguments []json.RawMessage
		_ = json.Unmarshal(params["params"], &arguments)
		var result interface{}
		switch method {
		case "eth_chainId":
			result = "0xcd8690dc"
		case "eth_getBlockByNumber":
			result = map[string]string{"number": fmt.Sprintf("0x%x", facilitatorHeight), "hash": fmt.Sprintf("0x%016x%s", facilitatorHeight, strings.Repeat("11", 24)), "timestamp": fmt.Sprintf("0x%x", fixture.input.Now)}
		case "eth_getCode":
			var address string
			_ = json.Unmarshal(arguments[0], &address)
			permit2, _ := tron.AddressForSigning(fixture.input.Config.Permit2Address)
			switch address {
			case permit2:
				result = "0x6000600055"
			case fixture.input.Authorization.Spender:
				result = "0x6001600055"
			case fixture.input.Authorization.Permitted.Token:
				result = "0x6002600055"
			default:
				t.Error("非预期代码地址")
			}
		case "eth_call":
			var call map[string]string
			_ = json.Unmarshal(arguments[0], &call)
			data := call["data"]
			if len(data) < 10 {
				t.Error("无效 ABI")
				return
			}
			switch data[:10] {
			case "0x13cd3b53":
				fixture.simulations.Add(1)
				executor, _ := tron.AddressForSigning(fixture.config.ExecutorAddress)
				encoded, err := hex.DecodeString(data[2:])
				if err != nil || len(encoded) != 388 || len(call) != 4 || call["from"] != executor || call["to"] != fixture.input.Authorization.Spender || call["value"] != "0x0" || len(arguments) != 2 || string(arguments[1]) != `"latest"` {
					t.Error("模拟未使用完整受限settle调用/可信费用付款方")
				}
				fixture.mu.Lock()
				fixture.simulatedData = append([]byte(nil), encoded...)
				fixture.mu.Unlock()
				if fixture.onSimulation != nil {
					fixture.onSimulation()
				}
				if fixture.simulationMode.Load() == 1 {
					_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "error": map[string]interface{}{"code": 3, "message": "synthetic-sensitive-revert"}})
					return
				}
				if fixture.simulationMode.Load() == 2 {
					result = true
				} else {
					result = "0x"
				}
			case "0x6afdd850":
				permit2, _ := tron.AddressForSigning(fixture.input.Config.Permit2Address)
				result = "0x" + strings.Repeat("0", 24) + permit2[2:]
			case "0x2c03ae6a":
				result = "0xd97b3239a7f32295517bd14cb074edfdd188dfe5eb42f802bb26d4fd1eb12c37"
			case "0x3644e515":
				result = "0xe7b78607f21dc61c0f1743639a2e5e0389476396bb887563161db36f84149bb6"
			case "0x70a08231", "0xdd62ed3e":
				result = fmt.Sprintf("0x%064x", 1000)
			case "0x4fe02b44":
				bitmap := new(big.Int)
				if fixture.used.Load() {
					nonce, _ := new(big.Int).SetString(fixture.input.Authorization.Nonce[2:], 16)
					bitmap.SetBit(bitmap, int(nonce.Uint64()&255), 1)
				}
				result = fmt.Sprintf("0x%064x", bitmap)
			default:
				t.Error("非预期只读 ABI")
			}
		default:
			t.Error("出现非预期 JSON-RPC 写方法")
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": result})
		return
	}
	if r.URL.Path == "/wallet/broadcasthex" {
		fixture.writes.Add(1)
		var encoded string
		_ = json.Unmarshal(params["transaction"], &encoded)
		payload, err := hex.DecodeString(encoded)
		if err != nil || len(payload) < 3 {
			t.Error("广播字节无效")
			return
		}
		length, n := binary.Uvarint(payload[1:])
		if n <= 0 || length > uint64(len(payload)-1-n) {
			t.Error("广播 raw 长度无效")
			return
		}
		digest := sha256.Sum256(payload[1+n : 1+n+int(length)])
		fixture.mu.Lock()
		simulated := append([]byte(nil), fixture.simulatedData...)
		fixture.mu.Unlock()
		if len(simulated) != 388 || !bytes.Contains(payload[1+n:1+n+int(length)], simulated) {
			t.Error("实际广播未使用模拟过的原授权调用")
		}
		id := hex.EncodeToString(digest[:])
		var state string
		var stored []byte
		if err := fixture.db.QueryRow(fixture.ctx, "SELECT state, signed_payload FROM x402_tron_settlements WHERE transaction_id=$1", id).Scan(&state, &stored); err != nil || state != "broadcast_unknown" || !bytes.Equal(stored, payload) {
			t.Error("广播前未提交同一原始交易及 unknown")
		}
		fixture.capture(t, exact.SettlementTransaction{ID: id, Payload: payload}, time.Now().UnixMilli())
		fixture.used.Store(true)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"result": true, "txid": id})
		return
	}
	fixture.mu.Lock()
	id, raw, stamp, headTime := fixture.signed.ID, hex.EncodeToString(fixture.raw), fixture.stamp, fixture.headTime
	fixture.mu.Unlock()
	body := map[string]interface{}{"txID": id, "raw_data_hex": raw, "ret": []interface{}{map[string]string{"contractRet": "SUCCESS"}}}
	block := func(height uint64, millis int64, suffix string) map[string]interface{} {
		return map[string]interface{}{"blockID": fmt.Sprintf("%016x%s", height, strings.Repeat(suffix, 24)), "block_header": map[string]interface{}{"raw_data": map[string]interface{}{"number": height, "timestamp": millis}}}
	}
	var result interface{}
	switch r.URL.Path {
	case "/wallet/getblockbynum", "/walletsolidity/getblockbynum":
		if string(params["num"]) == "0" {
			result = map[string]string{"blockID": executionGenesis()}
		} else {
			value := block(facilitatorHeight+1, stamp, "44")
			value["transactions"] = []interface{}{body}
			result = value
		}
	case "/walletsolidity/getnowblock":
		height := facilitatorHeight
		if id != "" {
			height += 301
		}
		result = block(height, headTime, "55")
	case "/wallet/gettransactionbyid", "/walletsolidity/gettransactionbyid", "/walletsolidity/gettransactioninfobyid":
		if id != "" && string(params["value"]) != `"`+id+`"` {
			t.Error("未查询原 txid")
		}
		if id == "" || fixture.mode.Load() == 0 {
			result = map[string]interface{}{}
		} else if r.URL.Path != "/walletsolidity/gettransactioninfobyid" {
			result = body
		} else {
			amount, _ := new(big.Int).SetString(fixture.input.Requirements.Amount, 10)
			result = map[string]interface{}{
				"id": id, "blockNumber": facilitatorHeight + 1, "blockTimeStamp": stamp,
				"receipt": map[string]string{"result": "SUCCESS"},
				"log": []interface{}{map[string]interface{}{
					"address": fixture.input.Authorization.Permitted.Token[2:],
					"topics":  []string{"ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef", strings.Repeat("0", 24) + fixture.input.Authorization.From[2:], strings.Repeat("0", 24) + fixture.input.Authorization.Witness.To[2:]},
					"data":    fmt.Sprintf("%064x", amount),
				}},
			}
		}
	default:
		t.Errorf("非预期原生方法: %s", r.URL.Path)
		w.WriteHeader(400)
		return
	}
	_ = json.NewEncoder(w).Encode(result)
	if r.URL.Path == "/walletsolidity/gettransactioninfobyid" && fixture.mode.Load() == 0 && fixture.onMissing != nil {
		fixture.onMissing()
	}
}

func TestFacilitatorOfficialSDKVerifyAndIndependentSettle(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	mechanism := fixture.mechanism(t)
	core := x402.Newx402Facilitator().Register([]x402.Network{x402.Network(mechanism.Network())}, mechanism)
	payload, _ := json.Marshal(envelope(t, fixture.input))
	requirements, _ := json.Marshal(fixture.input.Requirements)
	verified, err := core.Verify(fixture.ctx, payload, requirements)
	if err != nil || !verified.IsValid || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatalf("官方 Verify 失败或触发写入: %v", err)
	}
	var count int
	if err := fixture.db.QueryRow(fixture.ctx, "SELECT count(*) FROM x402_tron_settlements").Scan(&count); err != nil || count != 0 {
		t.Fatal("Verify 占用了日志 nonce")
	}
	// 另一个实例没有任何 Verify 内存状态，仍须独立完成结算。
	other := x402.Newx402Facilitator().Register([]x402.Network{x402.Network(mechanism.Network())}, fixture.mechanism(t))
	result, err := other.Settle(fixture.ctx, payload, requirements)
	if err != nil || !result.Success || result.Amount != "1000" || result.Transaction == "" || string(result.Network) != mechanism.Network() || result.Payer != verified.Payer {
		t.Fatalf("官方 Settle 未完成原付款: %v", err)
	}
	fixture.server.Close()
	again, err := other.Settle(fixture.ctx, payload, requirements)
	if err != nil || !again.Success || again.Transaction != result.Transaction || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatalf("成功恢复依赖节点或重复付款: %v", err)
	}
	if kinds := core.GetSupported().Kinds; len(kinds) != 1 || kinds[0].Network != mechanism.Network() || kinds[0].Scheme != "exact" {
		t.Fatal("官方 supported 注册错误")
	}
	mechanism.GetExtra(x402.Network(mechanism.Network()))["supportedAssetTransferMethods"] = []string{"invalid"}
	mechanism.GetSigners(x402.Network(mechanism.Network()))[0] = "invalid"
	if mechanism.GetSigners(x402.Network(mechanism.Network()))[0] != fixture.config.ExecutorAddress || mechanism.GetExtra("tron:1") != nil || mechanism.GetSigners("tron:1") != nil || mechanism.CaipFamily() != "tron:*" {
		t.Fatal("机制元数据引用了可变配置或越界网络")
	}
}

func persistFacilitatorTransaction(t *testing.T, fixture *facilitatorTest) (postgres.Record, exact.SettlementTransaction) {
	t.Helper()
	record := reserve(t, fixture.ctx, fixture.store, fixture.input)
	builder, err := exact.NewSettlementBuilder(exact.SettlementBuilderConfig{Authorization: fixture.input.Config, ExecutorAddress: fixture.config.ExecutorAddress, FeeLimitSUN: fixture.config.FeeLimitSUN, Lifetime: fixture.config.Lifetime})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := builder.Build(envelope(t, fixture.input), fixture.input.Requirements, exact.ReferenceBlock{
		Network: fixture.input.Config.Network, Number: facilitatorHeight, ID: fmt.Sprintf("0x%016x%s", facilitatorHeight, strings.Repeat("11", 24)), Timestamp: time.Unix(fixture.input.Now, 0),
	}, time.Unix(fixture.input.Now, 0))
	if err != nil {
		t.Fatal(err)
	}
	signed, err := prepared.Sign(fixture.ctx, fixture.config.Signer, time.Unix(fixture.input.Now, 0))
	if err != nil {
		t.Fatal(err)
	}
	record, err = fixture.store.SaveTransaction(fixture.ctx, record.ID, record.Authorization.Digest, postgres.Transaction{ID: signed.ID, Payload: signed.Payload})
	if err != nil {
		t.Fatal(err)
	}
	return record, signed
}

func TestFacilitatorHistoricalRecoveryDoesNotReadConsumedNonceOrResign(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 5*time.Minute)
	record, signed := persistFacilitatorTransaction(t, fixture)
	if _, err := fixture.store.BeginBroadcast(fixture.ctx, record.ID, record.Authorization.Digest); err != nil {
		t.Fatal(err)
	}
	fixture.capture(t, signed, (fixture.input.Now+3)*1000)
	fixture.used.Store(true)
	result, err := fixture.mechanism(t).Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	if err != nil || !result.Success || result.Transaction != signed.ID || fixture.jsonRPCs.Load() != 0 || fixture.signs.Load() != 1 || fixture.writes.Load() != 0 {
		t.Fatalf("过期/已消费 nonce 的原交易恢复失败或重播: %v", err)
	}
}

func TestFacilitatorLoweredFeePolicyNeverBroadcastsOldHigherLimit(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	record, _ := persistFacilitatorTransaction(t, fixture)
	fixture.config.FeeLimitSUN--
	result, err := fixture.mechanism(t).Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	var rejected *x402.SettleError
	if result != nil || !errors.As(err, &rejected) || rejected.ErrorReason != "settlement_policy_rejected" || fixture.signs.Load() != 1 || fixture.writes.Load() != 0 {
		t.Fatalf("恢复忽略新费用上限或重签/广播了旧交易: %v", err)
	}
	after, err := fixture.store.Get(fixture.ctx, record.ID)
	if err != nil || after.State != postgres.StatePrepared || !bytes.Equal(after.Transaction.Payload, record.Transaction.Payload) {
		t.Fatal("费用策略拒绝修改了已存交易")
	}
}

func TestFacilitatorPendingWaitAndRetryUsesOriginalTransaction(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	fixture.mode.Store(0)
	mechanism := fixture.mechanism(t)
	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	fixture.onMissing = cancel
	result, err := mechanism.Settle(ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	var pending *x402.SettleError
	if result != nil || !errors.As(err, &pending) || pending.ErrorReason != x402.ErrSettlementPending || pending.Transaction == "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("未确认或取消未返回原 txid pending: %v", err)
	}
	fixture.mode.Store(1)
	result, err = fixture.mechanism(t).Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	if err != nil || !result.Success || result.Transaction != pending.Transaction || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatalf("pending 重试重复签名/付款: %v", err)
	}
}

func TestFacilitatorConcurrentSettleOnlySignsAndBroadcastsOnce(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	otherDB, err := pgxpool.NewWithConfig(fixture.ctx, fixture.db.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(otherDB.Close)
	otherStore, err := postgres.New(otherDB, fixture.input.Config)
	if err != nil {
		t.Fatal(err)
	}
	other, err := postgres.NewFacilitator(otherStore, fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	mechanisms := []*postgres.Facilitator{fixture.mechanism(t), other}
	var wg sync.WaitGroup
	for index := range 16 {
		wg.Go(func() {
			result, err := mechanisms[index%2].Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
			if err != nil || !result.Success {
				t.Errorf("并发 Settle 失败: %v", err)
			}
		})
	}
	wg.Wait()
	if fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatalf("签名/广播次数为 %d/%d", fixture.signs.Load(), fixture.writes.Load())
	}
}

func TestFacilitatorRejectsPolicyAndSignerFailures(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	mechanism := fixture.mechanism(t)
	for _, mutate := range []func(*types.PaymentRequirements){
		func(r *types.PaymentRequirements) { r.Amount = "1001" },
		func(r *types.PaymentRequirements) { r.Asset = fixture.input.Config.Permit2Address },
		func(r *types.PaymentRequirements) { r.PayTo = fixture.input.Config.Permit2Address },
		func(r *types.PaymentRequirements) { r.Network = tron.NetworkMainnet },
		func(r *types.PaymentRequirements) { r.Extra = map[string]interface{}{"assetTransferMethod": "permit2"} },
	} {
		requirements := fixture.input.Requirements
		mutate(&requirements)
		if _, err := mechanism.Settle(fixture.ctx, envelope(t, fixture.input), requirements, nil); err == nil {
			t.Fatal("不符合费用策略的付款进入结算")
		}
		if _, err := mechanism.Verify(fixture.ctx, envelope(t, fixture.input), requirements, nil); err == nil {
			t.Fatal("不符合费用策略的付款进入预检")
		}
	}
	if fixture.jsonRPCs.Load() != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatal("策略拒绝仍触发 RPC/签名/广播")
	}
	fixture.config.Signer = func(context.Context, [32]byte) ([]byte, error) {
		return nil, errors.New("synthetic-private-signer-detail")
	}
	if _, err := fixture.mechanism(t).Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil); err == nil || strings.Contains(err.Error(), "synthetic-private") || fixture.writes.Load() != 0 {
		t.Fatal("签名错误泄漏原文或触发广播")
	}
	var state string
	if err := fixture.db.QueryRow(fixture.ctx, "SELECT state FROM x402_tron_settlements").Scan(&state); err != nil || state != "reserved" {
		t.Fatal("签名失败事务未保留可审计占用")
	}
}

func TestFacilitatorWaitsForSolidifiedReceipt(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	fixture.mode.Store(0)
	fixture.onMissing = func() { fixture.mode.Store(1) }
	result, err := fixture.mechanism(t).Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	if err != nil || !result.Success || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatalf("等待固化期间重复付款或未取得最终结果: %v", err)
	}
}

func TestFacilitatorConfigurationAndZeroValues(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	for _, mutate := range []func(*postgres.FacilitatorConfig){
		func(c *postgres.FacilitatorConfig) { c.Signer = nil },
		func(c *postgres.FacilitatorConfig) { c.FeeLimitSUN = 0 },
		func(c *postgres.FacilitatorConfig) { c.Lifetime = 15 * time.Second },
		func(c *postgres.FacilitatorConfig) { c.MaxAmount = "01" },
		func(c *postgres.FacilitatorConfig) { c.Policy.Network = tron.NetworkMainnet },
		func(c *postgres.FacilitatorConfig) { c.Preflight.Authorization.ProxyAddress = c.ExecutorAddress },
		func(c *postgres.FacilitatorConfig) { c.Preflight.Authorization.Permit2Address = c.ExecutorAddress },
		func(c *postgres.FacilitatorConfig) { c.Execution.GenesisBlockID = "" },
		func(c *postgres.FacilitatorConfig) { c.Preflight.Permit2CodeHash = "" },
		func(c *postgres.FacilitatorConfig) { c.Timeout = -time.Second },
		func(c *postgres.FacilitatorConfig) { c.Timeout = 4 * time.Minute },
		func(c *postgres.FacilitatorConfig) { c.PollInterval = time.Millisecond },
		func(c *postgres.FacilitatorConfig) { c.PollInterval = 11 * time.Second },
	} {
		config := fixture.config
		mutate(&config)
		if mechanism, err := postgres.NewFacilitator(fixture.store, config); mechanism != nil || !errors.Is(err, postgres.ErrFacilitatorConfig) {
			t.Fatalf("坏配置被接受: %v", err)
		}
	}
	if _, err := postgres.NewFacilitator(nil, fixture.config); !errors.Is(err, postgres.ErrFacilitatorConfig) {
		t.Fatal("nil Store 被接受")
	}
	defaults := fixture.config
	defaults.Timeout, defaults.PollInterval = 0, 0
	if _, err := postgres.NewFacilitator(fixture.store, defaults); err != nil {
		t.Fatalf("有界默认配置无效: %v", err)
	}
	for _, mechanism := range []*postgres.Facilitator{nil, {}} {
		if mechanism.Network() != "" || mechanism.GetExtra(tron.NetworkNile) != nil || mechanism.GetSigners(tron.NetworkNile) != nil {
			t.Fatal("零值机制发布了能力")
		}
		if _, err := mechanism.Verify(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil); !errors.Is(err, postgres.ErrFacilitatorConfig) {
			t.Fatal("零值 Verify 被接受")
		}
		if _, err := mechanism.Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil); !errors.Is(err, postgres.ErrFacilitatorConfig) {
			t.Fatal("零值 Settle 被接受")
		}
	}
	if fixture.jsonRPCs.Load() != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatal("构造器或零值调用产生了外部操作")
	}
}

func TestFacilitatorRejectsNewExpiredAuthorization(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 5*time.Minute)
	result, err := fixture.mechanism(t).Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	var rejected *x402.SettleError
	if result != nil || !errors.As(err, &rejected) || rejected.ErrorReason != "payment_expired" || fixture.jsonRPCs.Load() != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatalf("历史识别放行了新的过期付款: %v", err)
	}
}

func TestFacilitatorUnavailableSolidityNodePreservesOriginalTransaction(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	fixture.mode.Store(3)
	mechanism := fixture.mechanism(t)
	result, err := mechanism.Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	var pending *x402.SettleError
	if result != nil || !errors.As(err, &pending) || pending.ErrorReason != x402.ErrSettlementPending || pending.Transaction == "" {
		t.Fatalf("固化节点不可用时误判成功/终态或遗漏原交易: %v", err)
	}
	fixture.mode.Store(1)
	result, err = mechanism.Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	if err != nil || !result.Success || result.Transaction != pending.Transaction || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatalf("节点恢复后重新付款: %v", err)
	}
}

func TestFacilitatorFailedPrepareNeverBroadcastsUncommittedBytes(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	if _, err := fixture.db.Exec(fixture.ctx, `CREATE FUNCTION reject_prepared() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN RAISE EXCEPTION 'synthetic-private-storage-detail'; END $$;
		CREATE TRIGGER reject_prepared BEFORE UPDATE ON x402_tron_settlements
		FOR EACH ROW WHEN (NEW.state = 'prepared') EXECUTE FUNCTION reject_prepared()`); err != nil {
		t.Fatal(err)
	}
	result, err := fixture.mechanism(t).Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	var rejected *x402.SettleError
	if result != nil || !errors.As(err, &rejected) || rejected.ErrorReason != "settlement_recovery_required" || strings.Contains(err.Error(), "synthetic-private") || fixture.writes.Load() != 0 {
		t.Fatal("未提交的签名字节被广播或错误未脱敏")
	}
	var state string
	var transactionID *string
	if err := fixture.db.QueryRow(fixture.ctx, "SELECT state, transaction_id FROM x402_tron_settlements").Scan(&state, &transactionID); err != nil || state != "reserved" || transactionID != nil {
		t.Fatal("失败签名保存未回滚")
	}
	if _, err := fixture.db.Exec(fixture.ctx, "DROP TRIGGER reject_prepared ON x402_tron_settlements"); err != nil {
		t.Fatal(err)
	}
	result, err = fixture.mechanism(t).Settle(fixture.ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	if err != nil || !result.Success || fixture.signs.Load() != 2 || fixture.writes.Load() != 1 {
		t.Fatalf("未提交签名的安全重试失败: %v", err)
	}
}
