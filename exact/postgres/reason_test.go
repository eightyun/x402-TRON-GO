package postgres

import (
	"errors"
	"fmt"
	"testing"

	"github.com/eightyun/x402-tron-go/exact"
)

func TestFacilitatorChainFailureReasons(t *testing.T) {
	for err, want := range map[error]string{
		exact.ErrChainChanged: "chain_state_changed", exact.ErrStaleHead: "chain_head_stale",
		exact.ErrChainMismatch: "chain_network_mismatch", exact.ErrContractMismatch: "chain_contract_mismatch",
		exact.ErrNodeRead: "chain_read_failed", errors.New("private-node-detail"): "settlement_unavailable",
	} {
		if got := facilitatorReason(fmt.Errorf("private-wrapper: %w", err)); got != want {
			t.Fatalf("固定分类不符: %q want %q", got, want)
		}
	}
	if got := facilitatorReason(errors.Join(exact.ErrSettlementSimulation, exact.ErrNodeRead)); got != "settlement_simulation_failed" {
		t.Fatal("完整模拟失败分类优先级改变")
	}
}
