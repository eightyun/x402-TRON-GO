package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eightyun/x402-tron-go/exact/postgres"
	x402 "github.com/x402-foundation/x402/go/v2"
)

func TestLocalClientConfigurationAndCapabilities(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	mechanism := fixture.mechanism(t)
	for _, capacity := range []int{-1, 65} {
		if client, err := postgres.NewLocalClient(mechanism, capacity); client != nil || !errors.Is(err, postgres.ErrLocalClientConfig) {
			t.Fatal("无效本地容量未拒绝")
		}
	}
	if client, err := postgres.NewLocalClient(nil, 0); client != nil || !errors.Is(err, postgres.ErrLocalClientConfig) {
		t.Fatal("nil机制未拒绝")
	}
	for _, capacity := range []int{0, 1, 64} {
		client, err := postgres.NewLocalClient(mechanism, capacity)
		if err != nil {
			t.Fatal(err)
		}
		capabilities, err := client.GetSupported(fixture.ctx)
		if err != nil || len(capabilities.Kinds) != 1 || capabilities.Kinds[0].Network != mechanism.Network() {
			t.Fatal("本地能力不符", err)
		}
		capabilities.Kinds[0].Network = "changed"
		capabilities.Kinds[0].Extra["supportedAssetTransferMethods"] = []string{"invalid"}
		again, err := client.GetSupported(fixture.ctx)
		if err != nil || again.Kinds[0].Network != mechanism.Network() || again.Kinds[0].Extra["supportedAssetTransferMethods"].([]string)[0] != "permit2" {
			t.Fatal("本地能力共享可变引用")
		}
	}
	if fixture.jsonRPCs.Load() != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatal("构造/发现能力触发链操作")
	}
}

func TestLocalClientRejectsMalformedInputBeforeMechanism(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	client, err := postgres.NewLocalClient(fixture.mechanism(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(envelope(t, fixture.input))
	requirements, _ := json.Marshal(fixture.input.Requirements)
	for _, test := range []struct {
		name                  string
		payload, requirements []byte
	}{
		{"empty", nil, requirements},
		{"null", []byte(`null`), requirements},
		{"array", []byte(`[]`), requirements},
		{"v1", []byte(strings.Replace(string(payload), `"x402Version":2`, `"x402Version":1`, 1)), requirements},
		{"duplicate", []byte(strings.Replace(string(payload), `"x402Version":2`, `"x402Version":1,"x402Version":2`, 1)), requirements},
		{"case_duplicate", []byte(strings.Replace(string(payload), `"x402Version":2`, `"X402Version":1,"x402Version":2`, 1)), requirements},
		{"unknown_field", append([]byte(`{"private-marker":true,`), payload[1:]...), requirements},
		{"nested_duplicate", []byte(strings.Replace(string(payload), `"amount":"1000"`, `"amount":"999","amount":"1000"`, 1)), requirements},
		{"trailing", append(append([]byte{}, payload...), []byte(` {}`)...), requirements},
		{"oversize", []byte(strings.Repeat(" ", 64<<10)), requirements},
		{"quote_duplicate", payload, []byte(strings.Replace(string(requirements), `"amount":"1000"`, `"amount":"999","amount":"1000"`, 1))},
		{"quote_unknown", payload, append([]byte(`{"private-marker":true,`), requirements[1:]...)},
		{"quote_empty", payload, nil},
		{"quote_null", payload, []byte(`null`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if result, err := client.Verify(fixture.ctx, test.payload, test.requirements); result != nil || err == nil || !strings.Contains(err.Error(), "invalid_request") || strings.Contains(err.Error(), "private-marker") {
				t.Fatal("本地无效Verify未被安全拒绝")
			}
			if result, err := client.Settle(fixture.ctx, test.payload, test.requirements); result != nil || err == nil || !strings.Contains(err.Error(), "invalid_request") || strings.Contains(err.Error(), "private-marker") {
				t.Fatal("本地无效Settle未被安全拒绝")
			}
		})
	}
	if fixture.jsonRPCs.Load() != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatal("无效本地请求进入机制")
	}
	var count int
	if err := fixture.db.QueryRow(fixture.ctx, "SELECT count(*) FROM x402_tron_settlements").Scan(&count); err != nil || count != 0 {
		t.Fatal("无效请求占用日志")
	}
}

func TestLocalClientCancellationPrecedesInput(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	client, err := postgres.NewLocalClient(fixture.mechanism(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(fixture.ctx)
	cancel()
	if _, err := client.Verify(ctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("Verify取消语义丢失", err)
	}
	if _, err := client.Settle(ctx, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("Settle取消语义丢失", err)
	}
	if _, err := client.GetSupported(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("发现能力忽略取消", err)
	}
	deadline, done := context.WithDeadline(fixture.ctx, time.Now().Add(-time.Second))
	defer done()
	if _, err := client.Settle(deadline, nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("期限错误丢失", err)
	}
	if fixture.jsonRPCs.Load() != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatal("取消本地请求进入机制")
	}
}

func TestLocalClientCapacityIsSharedAndReleased(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	fixture.onSimulation = func() {
		enterOnce.Do(func() { close(entered) })
		select {
		case <-release:
		case <-fixture.ctx.Done():
		}
	}
	client, err := postgres.NewLocalClient(fixture.mechanism(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(envelope(t, fixture.input))
	requirements, _ := json.Marshal(fixture.input.Requirements)
	finished := make(chan error, 1)
	go func() {
		response, err := client.Verify(fixture.ctx, payload, requirements)
		if err == nil && (response == nil || !response.IsValid) {
			err = errors.New("未返回有效预检")
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-fixture.ctx.Done():
		t.Fatal("预检未进入完整模拟")
	}
	if _, err := client.Verify(fixture.ctx, payload, requirements); !errors.Is(err, postgres.ErrLocalClientCapacity) {
		t.Fatal("Verify未共享容量")
	}
	if _, err := client.Settle(fixture.ctx, payload, requirements); !errors.Is(err, postgres.ErrLocalClientCapacity) {
		t.Fatal("Settle未共享容量")
	}
	if _, err := client.GetSupported(fixture.ctx); err != nil {
		t.Fatal("满载阻断廉价能力发现")
	}
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-fixture.ctx.Done():
		t.Fatal("预检未退出")
	}
	if response, err := client.Verify(fixture.ctx, payload, requirements); err != nil || response == nil || !response.IsValid {
		t.Fatal("并发额度未释放", err)
	}
	if fixture.signs.Load() != 0 || fixture.writes.Load() != 0 {
		t.Fatal("Verify或被拒绝Settle发起付款")
	}
}

func TestLocalClientHistoricalRecoveryNeverResigns(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 5*time.Minute)
	record, signed := persistFacilitatorTransaction(t, fixture)
	if _, err := fixture.store.BeginBroadcast(fixture.ctx, record.ID, record.Authorization.Digest); err != nil {
		t.Fatal(err)
	}
	fixture.capture(t, signed, (fixture.input.Now+3)*1000)
	fixture.used.Store(true)
	client, err := postgres.NewLocalClient(fixture.mechanism(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(envelope(t, fixture.input))
	requirements, _ := json.Marshal(fixture.input.Requirements)
	result, err := client.Settle(fixture.ctx, payload, requirements)
	if err != nil || !result.Success || result.Transaction != signed.ID || fixture.jsonRPCs.Load() != 0 || fixture.signs.Load() != 1 || fixture.writes.Load() != 0 {
		t.Fatal("进程内历史恢复错误重验时效、读取nonce、签名或广播", err)
	}
}

func TestLocalClientConcurrentSettleDeduplicatesAcrossClients(t *testing.T) {
	t.Parallel()
	fixture := prepareFacilitator(t, 0)
	clients := make([]x402.FacilitatorClient, 2)
	for index := range clients {
		client, err := postgres.NewLocalClient(fixture.mechanism(t), 8)
		if err != nil {
			t.Fatal(err)
		}
		clients[index] = client
	}
	payload, _ := json.Marshal(envelope(t, fixture.input))
	requirements, _ := json.Marshal(fixture.input.Requirements)
	var workers sync.WaitGroup
	results := make(chan string, 16)
	for index := range 16 {
		workers.Go(func() {
			result, err := clients[index%2].Settle(fixture.ctx, payload, requirements)
			if err != nil || result == nil || !result.Success {
				t.Error("本地并发结算失败", err)
				return
			}
			results <- result.Transaction
		})
	}
	workers.Wait()
	close(results)
	var transaction string
	var count int
	for id := range results {
		count++
		if transaction == "" {
			transaction = id
		}
		if id != transaction {
			t.Error("并发恢复创建不同交易")
		}
	}
	if count != 16 || transaction == "" || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatal("跨本地client重复签名或广播")
	}
	var rows int
	if err := fixture.db.QueryRow(fixture.ctx, "SELECT count(*) FROM x402_tron_settlements").Scan(&rows); err != nil || rows != 1 {
		t.Fatal("本地并发创建重复日志")
	}
}
