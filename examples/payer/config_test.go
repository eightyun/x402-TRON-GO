package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func environment(t *testing.T) {
	t.Helper()
	c, _ := fixtureConfig(t)
	for name, value := range map[string]string{"X402_NETWORK": c.Network, "X402_RESOURCE_URL": "https://example.invalid/resource", "X402_PERMIT2_ADDRESS": c.Permit2, "X402_PROXY_ADDRESS": c.Proxy, "X402_PAYER_ADDRESS": c.Payer, "X402_ASSET": c.Asset, "X402_PAY_TO": c.PayTo, "X402_MAX_AMOUNT_ATOMIC": "1000", "X402_PAYMENT_STATE_DIR": c.stateDirectory, "X402_PAYER_KEY_FILE": c.keyFile, "X402_ENABLE_TESTNET_PAYMENTS": "true", "X402_ALLOW_INSECURE_LOCAL_RESOURCE_HTTP": ""} {
		t.Setenv(name, value)
	}
}

func TestConfigRejectsUnsafePaymentPolicy(t *testing.T) {
	environment(t)
	if _, e := loadConfig(); e != nil {
		t.Fatal(e)
	}
	for _, test := range []struct{ key, value string }{
		{"X402_NETWORK", "tron:728126428"}, {"X402_NETWORK", "tron:0x2b6653dc"}, {"X402_MAX_AMOUNT_ATOMIC", "1000.0"}, {"X402_MAX_AMOUNT_ATOMIC", "0"}, {"X402_PAYER_ADDRESS", "invalid"}, {"X402_PROXY_ADDRESS", os.Getenv("X402_PERMIT2_ADDRESS")},
		{"X402_RESOURCE_URL", "http://example.invalid/resource"}, {"X402_RESOURCE_URL", "https://user:password@example.invalid/resource"}, {"X402_RESOURCE_URL", "https://example.invalid/?key=secret"}, {"X402_RESOURCE_URL", "https://example.invalid/?"}, {"X402_RESOURCE_URL", "https://example.invalid/#secret"}, {"X402_RESOURCE_URL", "file:///tmp/input"}, {"X402_PAYMENT_STATE_DIR", "relative"}, {"X402_PAYMENT_STATE_DIR", "/"}, {"X402_ENABLE_TESTNET_PAYMENTS", "yes"},
	} {
		t.Run(test.key+test.value, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			if _, e := loadConfig(); !errors.Is(e, errConfiguration) {
				t.Fatal("不安全配置未拒绝")
			}
		})
	}
	t.Setenv("X402_ALLOW_INSECURE_LOCAL_RESOURCE_HTTP", "true")
	t.Setenv("X402_RESOURCE_URL", "http://127.0.0.1:8080/resource")
	if _, e := loadConfig(); e != nil {
		t.Fatal("显式loopback未允许")
	}
	for _, url := range []string{"http://localhost:8080/resource", "http://0.0.0.0:8080/resource", "http://example.invalid/resource"} {
		t.Setenv("X402_RESOURCE_URL", url)
		if _, e := loadConfig(); !errors.Is(e, errConfiguration) {
			t.Fatal("非字面loopback明文未拒绝")
		}
	}
}

func TestStatePrivacyLockAndNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	if os.Chmod(dir, 0700) != nil {
		t.Fatal("临时状态目录权限设置失败")
	}
	f, e := lockState(dir)
	if e != nil {
		t.Fatal(e)
	}
	if second, e := lockState(dir); e == nil {
		_ = second.Close()
		t.Fatal("同时持锁")
	}
	_ = f.Close()
	f, e = lockState(dir)
	if e != nil {
		t.Fatal("关闭后锁未释放")
	}
	_ = f.Close()
	if writeState(dir, "original", []byte("first")) != nil || writeState(dir, "original", []byte("replacement")) == nil {
		t.Fatal("原文件可被覆盖")
	}
	if e := os.Symlink(filepath.Join(dir, "original"), filepath.Join(dir, "link")); e != nil {
		t.Fatal(e)
	}
	if _, e := readState(dir, "link", 100); !errors.Is(e, errState) {
		t.Fatal("符号链接被读取")
	}
	if e := os.Chmod(filepath.Join(dir, "original"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := readState(dir, "original", 100); !errors.Is(e, errState) {
		t.Fatal("宽权限文件被读取")
	}
	if e := os.Chmod(dir, 0755); e != nil {
		t.Fatal(e)
	}
	if f, e := lockState(dir); e == nil {
		_ = f.Close()
		t.Fatal("宽权限目录被允许")
	}
}
