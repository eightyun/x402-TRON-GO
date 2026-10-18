package exact_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/eightyun/x402-tron-go/exact"
)

func contractRead(query nodeRequest) bool {
	switch selector(query) {
	case "0x6afdd850", "0x2c03ae6a", "0x3644e515":
		return true
	}
	return query.Method == "eth_getCode"
}

// 在真实HTTP边界设置测试等待点，不依靠短授权TTL或生产埋点验证并发。
func observedNode(t *testing.T, handler http.Handler, observe func(*http.Request, nodeRequest)) (*httptest.Server, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	received, active, peak := new(atomic.Int64), new(atomic.Int64), new(atomic.Int64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		var query nodeRequest
		if err != nil || json.Unmarshal(data, &query) != nil {
			t.Error("并发节点收到无效请求")
			return
		}
		received.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for previous := peak.Load(); current > previous; previous = peak.Load() {
			if peak.CompareAndSwap(previous, current) {
				break
			}
		}
		observe(r, query)
		if r.Context().Err() != nil {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, received, peak
}

func awaitReads(t *testing.T, entered <-chan struct{}, count int) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for range count {
		select {
		case <-entered:
		case <-deadline.C:
			t.Fatal("独立读取未能并发到达等待点")
		}
	}
}

func TestParallelDeploymentSharedLimitAndQueuedCancellation(t *testing.T) {
	input := loadVector(t)
	input.Authorization.From = input.Authorization.Spender
	base, reads := testNode(t, input, nil)
	entered, release := make(chan struct{}, 16), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var headReads atomic.Int64
	server, received, peak := observedNode(t, base.Config.Handler, func(r *http.Request, query nodeRequest) {
		if query.Method == "eth_getBlockByNumber" {
			headReads.Add(1)
		}
		if contractRead(query) {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	verifier := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc"))
	finished := make(chan error, 1)
	go func() {
		_, err := verifier.CheckDeployment(ctx, input.Requirements.Asset, time.Unix(input.Now, 0))
		finished <- err
	}()
	awaitReads(t, entered, 6)
	if received.Load() != 8 || peak.Load() != 6 || headReads.Load() != 1 {
		t.Fatal("未保持6个并发固定读，或提前进入后置链头检查")
	}
	// 另一个调用的两项边界读只能等待同一额度，预算必须包含排队时间。
	waiting, stop := context.WithTimeout(ctx, 250*time.Millisecond)
	defer stop()
	if _, err := verifier.CheckDeployment(waiting, input.Requirements.Asset, time.Unix(input.Now, 0)); !errors.Is(err, context.DeadlineExceeded) || received.Load() != 8 {
		t.Fatal("并发调用绕过共享上限，或排队不计入预算", err)
	}
	unblock()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("释放读取后检查未完成", err)
		}
	case <-ctx.Done():
		t.Fatal("读取组未退出")
	}
	if _, err := verifier.CheckDeployment(ctx, input.Requirements.Asset, time.Unix(input.Now, 0)); err != nil || reads.Load() != 20 || received.Load() != 20 || peak.Load() > 6 {
		t.Fatal("额度泄漏、取消后额外读取或后续检查失败", err)
	}
}

func TestParallelPaymentStateCompletesBeforeSimulation(t *testing.T) {
	input := loadVector(t)
	base, reads, simulations := simulationNode(t, input, nil)
	entered, release := make(chan struct{}, 3), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var released atomic.Bool
	server, _, _ := observedNode(t, base.Config.Handler, func(r *http.Request, query nodeRequest) {
		switch selector(query) {
		case "0x70a08231", "0xdd62ed3e", "0x4fe02b44":
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
			}
		case "0x13cd3b53":
			if !released.Load() {
				t.Error("付款状态未全部返回就开始模拟")
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	verifier := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc"))
	executor := loadSettlementVector(t).Config.ExecutorAddress
	payload := envelope(t, input)
	go func() {
		_, err := verifier.CheckWithSimulation(ctx, payload, input.Requirements, executor, time.Unix(input.Now, 0))
		finished <- err
	}()
	awaitReads(t, entered, 3)
	if simulations.Load() != 0 {
		t.Fatal("付款状态仍在检查时调用模拟")
	}
	released.Store(true)
	unblock()
	select {
	case err := <-finished:
		if err != nil || reads.Load() != 13 || simulations.Load() != 1 {
			t.Fatal("并发付款状态检查改变了原13+1读或模拟结果", err)
		}
	case <-ctx.Done():
		t.Fatal("付款状态读取未退出")
	}
}

func TestParallelChecksKeepLogicalErrorPriorityAndStageGates(t *testing.T) {
	for _, contractsFail := range []bool{true, false} {
		t.Run(fmt.Sprint(contractsFail), func(t *testing.T) {
			input := loadVector(t)
			var paymentReads atomic.Int64
			server, _, simulations := simulationNode(t, input, func(query nodeRequest, _ int, reply *nodeReply) {
				if contractsFail {
					if codeAddress(query) == permitWire {
						reply.Result = "0x6003"
					}
					if selector(query) == "0x3644e515" {
						reply.Error = map[string]interface{}{"code": 3, "message": "synthetic-sensitive"}
					}
				}
				switch selector(query) {
				case "0x70a08231":
					paymentReads.Add(1)
					reply.Result = fmt.Sprintf("0x%064x", 0)
				case "0xdd62ed3e":
					paymentReads.Add(1)
					reply.Error = map[string]interface{}{"code": 3}
				case "0x4fe02b44":
					paymentReads.Add(1)
					reply.Result = "0x" + string(bytes.Repeat([]byte("f"), 64))
				}
			})
			verifier := preflightFor(t, preflightConfig(input, server.URL+"/jsonrpc"))
			want := exact.ErrInsufficientBalance
			if contractsFail {
				want = exact.ErrContractMismatch
			}
			for range 5 {
				if _, err := verifier.CheckWithSimulation(context.Background(), envelope(t, input), input.Requirements, loadSettlementVector(t).Config.ExecutorAddress, time.Unix(input.Now, 0)); !errors.Is(err, want) {
					t.Fatal("错误类型随并发响应到达顺序变化", err)
				}
			}
			if simulations.Load() != 0 || (contractsFail && paymentReads.Load() != 0) || (!contractsFail && paymentReads.Load() != 15) {
				t.Fatal("并发错误绕过了后续阶段闸门")
			}
		})
	}
}
