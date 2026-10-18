package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	x402 "github.com/x402-foundation/x402/go/v2"
)

func TestFacilitatorSimulationFailureHasNoPaymentSideEffects(t *testing.T) {
	for _, mode := range []int32{1, 2} {
		t.Run(map[int32]string{1: "revert", 2: "invalid_result"}[mode], func(t *testing.T) {
			fixture := prepareFacilitator(t, 0)
			mechanism := fixture.mechanism(t)
			payload := envelope(t, fixture.input)
			fixture.simulationMode.Store(mode)
			var invalid *x402.VerifyError
			if result, err := mechanism.Verify(fixture.ctx, payload, fixture.input.Requirements, nil); result != nil || !errors.As(err, &invalid) || invalid.InvalidReason != "settlement_simulation_failed" || strings.Contains(err.Error(), "synthetic-sensitive") {
				t.Fatal("Verify 未安全拒绝失败模拟", err)
			}
			var failed *x402.SettleError
			if result, err := mechanism.Settle(fixture.ctx, payload, fixture.input.Requirements, nil); result != nil || !errors.As(err, &failed) || failed.ErrorReason != "settlement_simulation_failed" || failed.Transaction != "" || strings.Contains(err.Error(), "synthetic-sensitive") {
				t.Fatal("Settle 未安全拒绝失败模拟", err)
			}
			var count int
			if err := fixture.db.QueryRow(fixture.ctx, "SELECT count(*) FROM x402_tron_settlements").Scan(&count); err != nil || count != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 || fixture.simulations.Load() != 2 {
				t.Fatal("模拟失败仍占用/签名/广播", err)
			}
			fixture.simulationMode.Store(0)
			result, err := mechanism.Settle(fixture.ctx, payload, fixture.input.Requirements, nil)
			if err != nil || !result.Success || fixture.simulations.Load() != 3 || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
				t.Fatal("模拟恢复后未能独立结算", err)
			}
		})
	}
}

func TestFacilitatorOriginalTransactionRecoverySkipsSimulation(t *testing.T) {
	fixture := prepareFacilitator(t, 0)
	mechanism := fixture.mechanism(t)
	payload := envelope(t, fixture.input)
	fixture.mode.Store(3)
	var pending *x402.SettleError
	if result, err := mechanism.Settle(fixture.ctx, payload, fixture.input.Requirements, nil); result != nil || !errors.As(err, &pending) || pending.ErrorReason != x402.ErrSettlementPending || pending.Transaction == "" || fixture.simulations.Load() != 1 {
		t.Fatal("未保留已广播交易", err)
	}
	// 已消费 nonce 的原交易只查原回执，不能重新模拟后误拒或替换付款。
	fixture.simulationMode.Store(1)
	fixture.mode.Store(1)
	result, err := mechanism.Settle(fixture.ctx, payload, fixture.input.Requirements, nil)
	if err != nil || !result.Success || result.Transaction != pending.Transaction || fixture.simulations.Load() != 1 || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatal("原交易恢复重新模拟/付款", err)
	}
	fixture.server.Close()
	result, err = mechanism.Settle(fixture.ctx, payload, fixture.input.Requirements, nil)
	if err != nil || !result.Success || result.Transaction != pending.Transaction || fixture.simulations.Load() != 1 || fixture.signs.Load() != 1 || fixture.writes.Load() != 1 {
		t.Fatal("已持久化成功仍依赖节点模拟", err)
	}
}

func TestFacilitatorSimulationCancellationDoesNotReserve(t *testing.T) {
	fixture := prepareFacilitator(t, 0)
	mechanism := fixture.mechanism(t)
	ctx, cancel := context.WithCancel(fixture.ctx)
	defer cancel()
	fixture.onSimulation = cancel
	result, err := mechanism.Settle(ctx, envelope(t, fixture.input), fixture.input.Requirements, nil)
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("模拟取消未保留 context 语义", err)
	}
	var count int
	if err := fixture.db.QueryRow(fixture.ctx, "SELECT count(*) FROM x402_tron_settlements").Scan(&count); err != nil || count != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 || fixture.simulations.Load() != 1 {
		t.Fatal("取消模拟后仍占用/付款", err)
	}
}

func TestHTTPSimulationFailureRetainsOfficialProtocolError(t *testing.T) {
	fixture := prepareFacilitator(t, 0)
	client := httpFixtureClient(t, fixture)
	fixture.simulationMode.Store(1)
	payload, _ := json.Marshal(envelope(t, fixture.input))
	requirements, _ := json.Marshal(fixture.input.Requirements)
	var invalid *x402.VerifyError
	if result, err := client.Verify(fixture.ctx, payload, requirements); result != nil || !errors.As(err, &invalid) || invalid.InvalidReason != "settlement_simulation_failed" || strings.Contains(err.Error(), "synthetic-sensitive") {
		t.Fatal("HTTP Verify 模拟错误类型或脱敏失败", err)
	}
	var failed *x402.SettleError
	if result, err := client.Settle(fixture.ctx, payload, requirements); result != nil || !errors.As(err, &failed) || failed.ErrorReason != "settlement_simulation_failed" || failed.Transaction != "" || strings.Contains(err.Error(), "synthetic-sensitive") {
		t.Fatal("HTTP Settle 模拟错误类型或脱敏失败", err)
	}
	var count int
	if err := fixture.db.QueryRow(fixture.ctx, "SELECT count(*) FROM x402_tron_settlements").Scan(&count); err != nil || count != 0 || fixture.signs.Load() != 0 || fixture.writes.Load() != 0 || fixture.simulations.Load() != 2 {
		t.Fatal("HTTP 模拟拒绝仍产生付款副作用", err)
	}
}
