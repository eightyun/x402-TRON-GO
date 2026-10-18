package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/exact/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLoadSignerRequiresPrivateRegularFile(t *testing.T) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	defer clear(key[:])
	encoded := make([]byte, 64)
	hex.Encode(encoded, key[:])
	defer clear(encoded)
	path := filepath.Join(t.TempDir(), "executor.key")
	if err := os.WriteFile(path, append(append([]byte{}, encoded...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	signer, err := loadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if signature, err := signer.SignTransaction(context.Background(), [32]byte{1}); err != nil || len(signature) != 65 {
		t.Fatal("交易签名失败")
	}
	_ = signer.Close()
	if _, err := signer.SignTransaction(context.Background(), [32]byte{}); !errors.Is(err, exact.ErrEOASigner) {
		t.Fatal("关闭后仍可签名")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"relative.key", filepath.Join(t.TempDir(), "missing"), filepath.Dir(path), link} {
		if _, err := loadSigner(bad); !errors.Is(err, errConfiguration) {
			t.Fatal("非法文件未拒绝")
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSigner(path); !errors.Is(err, errConfiguration) {
		t.Fatal("可被其他用户读取的密钥未拒绝")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Repeat("0", 64), strings.Repeat("f", 64), strings.Repeat("z", 64), "0x" + string(encoded), string(encoded) + " "} {
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadSigner(path); !errors.Is(err, errConfiguration) {
			t.Fatal("无效密钥未拒绝")
		}
	}
}

func TestCheckDatabaseRequiresExplicitCurrentSchema(t *testing.T) {
	dsn := os.Getenv("X402_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("需专用 PostgreSQL 测试库")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("测试库配置无效")
	}
	admin, err := pgxpool.NewWithConfig(ctx, config.Copy())
	if err != nil {
		t.Fatal("测试库连接失败")
	}
	defer admin.Close()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := pgx.Identifier{fmt.Sprintf("x402_test_%x", suffix)}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal("创建独立 schema 失败")
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error("清理本用例独立 schema 失败")
		}
	}()
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("创建隔离连接失败")
	}
	defer pool.Close()
	if err := checkDatabase(ctx, pool); !errors.Is(err, errConfiguration) {
		t.Fatal("未初始化数据库未拒绝")
	}
	// 只有测试部署步骤显式应用 schema，启动检查本身不 DDL。
	if _, err := pool.Exec(ctx, postgres.SchemaSQL); err != nil {
		t.Fatal("测试显式应用 schema 失败")
	}
	if err := checkDatabase(ctx, pool); err != nil {
		t.Fatal("当前 schema 可读性检查失败")
	}
	if _, err := pool.Exec(ctx, "ALTER TABLE x402_tron_settlements DROP COLUMN confirmed_at"); err != nil {
		t.Fatal("构造不兼容测试 schema 失败")
	}
	if err := checkDatabase(ctx, pool); !errors.Is(err, errConfiguration) {
		t.Fatal("缺少当前版本列未阻止启动")
	}
}

func setBasicConfig(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"X402_NETWORK": "", "X402_FACILITATOR_LISTEN_ADDR": "", "X402_MAX_CONCURRENT": "",
		"X402_RPC_BATCH":          "",
		"X402_SETTLEMENT_TIMEOUT": "", "X402_FEE_LIMIT_SUN": "100000000",
		"X402_DATABASE_URL": "postgres://example.invalid/unused", "X402_EXECUTOR_KEY_FILE": "/unused/private.key",
	} {
		t.Setenv(key, value)
	}
}

func TestLoadConfigRejectsUnsafeDeployment(t *testing.T) {
	setBasicConfig(t)
	config, err := loadConfig()
	if err != nil || config.address != "127.0.0.1:8081" || config.mechanism.Policy.Network != "tron:3448148188" {
		t.Fatal("默认配置错误")
	}
	t.Run("batch_opt_in", func(t *testing.T) {
		t.Setenv("X402_RPC_BATCH", "true")
		config, err := loadConfig()
		if err != nil || !config.mechanism.Preflight.BatchRPC {
			t.Fatal("批量配置未生效")
		}
	})
	for _, test := range []struct{ key, value string }{
		{"X402_NETWORK", "tron:728126428"}, {"X402_NETWORK", "tron:0"},
		{"X402_FACILITATOR_LISTEN_ADDR", "0.0.0.0:8081"}, {"X402_FACILITATOR_LISTEN_ADDR", "localhost:8081"},
		{"X402_FACILITATOR_LISTEN_ADDR", "127.0.0.1:0"}, {"X402_FACILITATOR_LISTEN_ADDR", "127.0.0.1:65536"},
		{"X402_FEE_LIMIT_SUN", "0"}, {"X402_FEE_LIMIT_SUN", "unknown"},
		{"X402_MAX_CONCURRENT", "-1"}, {"X402_MAX_CONCURRENT", "65"}, {"X402_MAX_CONCURRENT", "invalid"},
		{"X402_SETTLEMENT_TIMEOUT", "0s"}, {"X402_SETTLEMENT_TIMEOUT", "181s"}, {"X402_SETTLEMENT_TIMEOUT", "invalid"},
		{"X402_DATABASE_URL", ""}, {"X402_EXECUTOR_KEY_FILE", "relative.key"},
		{"X402_RPC_BATCH", "TRUE"}, {"X402_RPC_BATCH", "1"},
	} {
		t.Run(test.key+"/"+test.value, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, err := loadConfig(); !errors.Is(err, errConfiguration) {
				t.Fatal("危险配置未拒绝")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx); !errors.Is(err, errConfiguration) {
		t.Fatal("不存在的密钥没有在连接前拒绝")
	}
}
