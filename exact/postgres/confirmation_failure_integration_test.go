package postgres_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"

	"github.com/eightyun/x402-tron-go/exact/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// 仅用于专用明文测试连接：等待 PostgreSQL 的 idle ReadyForQuery 确认提交完成，
// 随后丢弃整份响应，使调用者不能看到返回行；不只是阻止 SQL 发出或取消未提交事务。
type lostConfirmationConn struct {
	net.Conn
	armed atomic.Bool
	lost  *atomic.Bool
}

func (conn *lostConfirmationConn) Write(value []byte) (int, error) {
	if bytes.Contains(value, []byte("UPDATE x402_tron_settlements")) && bytes.Contains(value, []byte("SET state = 'succeeded'")) {
		conn.armed.Store(true)
	}
	return conn.Conn.Write(value)
}

func (conn *lostConfirmationConn) Read(value []byte) (int, error) {
	if !conn.armed.Load() {
		return conn.Conn.Read(value)
	}
	for {
		var header [5]byte
		if _, err := io.ReadFull(conn.Conn, header[:]); err != nil {
			return 0, err
		}
		length := binary.BigEndian.Uint32(header[1:])
		if length < 4 || length > 1<<20 {
			return 0, io.ErrUnexpectedEOF
		}
		body := make([]byte, int(length)-4)
		if _, err := io.ReadFull(conn.Conn, body); err != nil {
			return 0, err
		}
		if header[0] == 'Z' {
			if bytes.Equal(body, []byte{'I'}) {
				conn.lost.Store(true)
			}
			_ = conn.Conn.Close()
			return 0, io.ErrUnexpectedEOF
		}
	}
}

func TestConfirmSettlementLostCommitResponseRequiresReadback(t *testing.T) {
	t.Parallel()
	input := prepareConfirmation(t, postgres.StatePending)
	config := input.db.Config()
	if config.ConnConfig.TLSConfig != nil {
		t.Skip("PostgreSQL 协议故障注入要求 sslmode=disable 的隔离测试库")
	}
	baseDial := config.ConnConfig.DialFunc
	if baseDial == nil {
		baseDial = (&net.Dialer{}).DialContext
	}
	var lost atomic.Bool
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := baseDial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &lostConfirmationConn{Conn: conn, lost: &lost}, nil
	}
	config.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	config.MaxConns = 1
	faultDB, err := pgxpool.NewWithConfig(input.ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(faultDB.Close)
	faultStore, err := postgres.New(faultDB, input.input.Config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := faultStore.ConfirmSettlement(input.ctx, input.before.ID, input.fixture.Config.ExecutorAddress, input.client, input.now)
	if !lost.Load() || !errors.Is(err, postgres.ErrStore) || result.State == postgres.StateSucceeded || result.Confirmation != nil {
		t.Fatalf("提交响应丢失未保守报告未知结果: %v", err)
	}
	// 正常连接读回，证明 SQL 确实已提交；失败返回值绝不能当作重新付款许可。
	stored, err := input.store.Get(input.ctx, input.before.ID)
	if err != nil || stored.State != postgres.StateSucceeded || stored.Confirmation == nil {
		t.Fatal("提交响应丢失用例并未实际保存成功结果")
	}
	reads := input.reads.Load()
	again, err := input.confirm()
	if err != nil || again.State != postgres.StateSucceeded || input.reads.Load() != reads {
		t.Fatal("提交未知后的读回恢复依赖再次 RPC 或未保留原结果")
	}
}
