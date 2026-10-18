// facilitator 示例仅允许测试网和本机监听；真实转账须由部署方显式准备可信合约、节点、资金和授权。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/eightyun/x402-tron-go/exact/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		// 不输出数据库、节点、签名器或请求原文，避免凭据和可广播数据泄漏。
		_, _ = fmt.Fprintln(os.Stderr, "facilitator 启动或运行失败；请核对可信配置、数据库结构和服务状态。")
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	config, err := loadConfig()
	if err != nil {
		return err
	}
	signer, err := loadSigner(config.keyFile)
	if err != nil {
		return err
	}
	defer signer.Close()
	config.mechanism.ExecutorAddress = signer.Address()
	config.mechanism.Signer = signer.SignTransaction
	// 所有纯配置校验先于连接数据库，不会在启动时签名、预检付款或广播。
	poolConfig, err := pgxpool.ParseConfig(config.databaseURL)
	if err != nil {
		return errConfiguration
	}
	poolConfig.MaxConns = int32(config.http.MaxConcurrent + 4)
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return errConfiguration
	}
	defer pool.Close()
	store, err := postgres.New(pool, config.mechanism.Preflight.Authorization)
	if err != nil {
		return err
	}
	mechanism, err := postgres.NewFacilitator(store, config.mechanism)
	if err != nil {
		return err
	}
	handler, err := postgres.NewHTTPHandler(mechanism, config.http)
	if err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, 15*time.Second)
	err = pool.Ping(startup)
	if err == nil {
		err = checkDatabase(startup, pool)
	}
	cancel()
	if err != nil {
		return errConfiguration
	}
	server := &http.Server{
		Addr: config.address, Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: config.mechanism.Timeout + 15*time.Second,
		IdleTimeout: time.Minute, MaxHeaderBytes: 16 << 10,
	}
	defer server.Close()
	result := make(chan error, 1)
	go func() { result <- server.ListenAndServe() }()
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		// 给已接收的原交易足够时间保存 pending/成功；超时强制关闭也不允许重签重播。
		shutdown, done := context.WithTimeout(context.WithoutCancel(ctx), config.mechanism.Timeout+10*time.Second)
		defer done()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

func checkDatabase(ctx context.Context, pool *pgxpool.Pool) error {
	// 只验证当前版本列的可读性，不能替代部署方显式应用/核对官方 schema、约束及最小权限。
	rows, err := pool.Query(ctx, `SELECT id, network, permit2_address, payer, nonce, digest, state,
		transaction_id, signed_payload, created_at, updated_at,
		confirmed_block_number, confirmed_block_id, confirmed_at FROM x402_tron_settlements LIMIT 0`)
	if err != nil {
		return errConfiguration
	}
	rows.Close()
	if rows.Err() != nil {
		return errConfiguration
	}
	return nil
}
