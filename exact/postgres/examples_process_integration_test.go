package postgres_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/eightyun/x402-tron-go/exact"
)

func exampleEnvironment(values map[string]string) []string {
	var env []string
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		_, overridden := values[key]
		if !overridden && !strings.HasPrefix(item, "X402_") && !strings.HasPrefix(item, "SSL_CERT_FILE=") {
			env = append(env, item)
		}
	}
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	return env
}

func freeExampleAddress(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal("分配测试端口失败")
	}
	address := l.Addr().String()
	_ = l.Close()
	return address
}

func exampleProcess(t *testing.T, ctx context.Context, binary string, env []string, readyURL, token string) func() {
	t.Helper()
	cmd := exec.Command(binary)
	cmd.Env = env
	f, e := os.OpenFile(filepath.Join(t.TempDir(), "service.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal("创建临时日志失败")
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if cmd.Start() != nil {
		_ = f.Close()
		t.Fatal("示例子进程未启动")
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				_ = cmd.Process.Kill()
				<-exited
			}
			_ = f.Close()
		})
	}
	t.Cleanup(stop)
	client := http.Client{Timeout: time.Second}
	timer := time.NewTicker(50 * time.Millisecond)
	defer timer.Stop()
	for {
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet, readyURL, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response, e := client.Do(request)
		if e == nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
			_ = response.Body.Close()
			if (token != "" && response.StatusCode == 200) || (token == "" && response.StatusCode == 402) {
				return stop
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal("示例服务未就绪")
		case <-exited:
			once.Do(func() { _ = f.Close() })
			t.Fatal("示例服务提前退出；请核对本测试进程的隔离配置")
		case <-timer.C:
		}
	}
}

// 实际构建/启动三个公开可执行示例；只有节点/区块/回执为合成夹具。
func TestRunnableExamplesPayAndRestartRecovery(t *testing.T) {
	// macOS使用系统证书验证器，不读取SSL_CERT_FILE；不为测试给生产示例加跳过TLS校验。
	if runtime.GOOS != "linux" {
		t.Skip("自签名节点子进程验收在Linux CI执行；macOS实链使用公开可信TLS节点")
	}
	if os.Getenv("X402_TEST_DATABASE_URL") == "" {
		t.Skip("需专用PostgreSQL测试库")
	}
	buildCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bin := t.TempDir()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", bin+string(filepath.Separator), "./examples/facilitator", "./examples/server", "./examples/payer")
	build.Dir = "../.."
	if build.Run() != nil {
		t.Fatal("构建公开示例失败")
	}
	fixture := prepareFacilitator(t, 0)
	state := t.TempDir()
	if os.Chmod(state, 0700) != nil {
		t.Fatal("状态目录权限失败")
	}
	keyFile := func(name string) (string, *exact.EOASigner) {
		key, e := secp256k1.GeneratePrivateKey()
		if e != nil {
			t.Fatal(e)
		}
		defer key.Zero()
		b := key.Serialize()
		defer clear(b)
		signer, e := exact.NewEOASigner(b)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = signer.Close() })
		path := filepath.Join(t.TempDir(), name)
		if os.WriteFile(path, []byte(hex.EncodeToString(b)), 0600) != nil {
			t.Fatal("临时测试密钥写入失败")
		}
		return path, signer
	}
	executorPath, executor := keyFile("executor.key")
	payerPath, payer := keyFile("payer.key")
	fixture.config.ExecutorAddress = executor.Address()
	var initial sync.Once
	tlsNode := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		initial.Do(func() {
			// 付款程序已持久化原授权后才会触发节点访问，读取它仅用于设置合成事件/nonce。
			header, e := os.ReadFile(filepath.Join(state, "payment-signature"))
			if e != nil {
				t.Error("节点访问前未保存授权")
				return
			}
			b, e := base64.StdEncoding.DecodeString(string(header))
			var p struct {
				Payload exact.Permit2Payload `json:"payload"`
			}
			if e != nil || json.Unmarshal(b, &p) != nil {
				t.Error("读取临时授权失败")
				return
			}
			fixture.input.Authorization = p.Payload.Authorization
			fixture.input.Signature = p.Payload.Signature
		})
		fixture.node(t, w, r)
	}))
	defer tlsNode.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	if os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsNode.Certificate().Raw}), 0600) != nil {
		t.Fatal("保存测试CA失败")
	}
	facAddress, resourceAddress := freeExampleAddress(t), freeExampleAddress(t)
	values := map[string]string{
		"PGOPTIONS":     "-c search_path=" + strings.Trim(fixture.db.Config().ConnConfig.RuntimeParams["search_path"], `"`),
		"SSL_CERT_FILE": ca, "X402_NETWORK": fixture.config.Policy.Network, "X402_ASSET": fixture.config.Policy.Asset, "X402_PAY_TO": fixture.config.Policy.PayTo, "X402_MAX_AMOUNT_ATOMIC": "1000", "X402_PRICE_ATOMIC": "1000",
		"X402_PERMIT2_ADDRESS": fixture.input.Config.Permit2Address, "X402_PROXY_ADDRESS": fixture.input.Config.ProxyAddress, "X402_PERMIT2_CODE_HASH": fixture.config.Preflight.Permit2CodeHash, "X402_PROXY_CODE_HASH": fixture.config.Preflight.ProxyCodeHash,
		"X402_JSON_RPC_URL": tlsNode.URL + "/jsonrpc", "X402_NATIVE_NODE_URL": tlsNode.URL, "X402_GENESIS_BLOCK_ID": executionGenesis(), "X402_DATABASE_URL": fixture.db.Config().ConnConfig.ConnString(), "X402_EXECUTOR_KEY_FILE": executorPath, "X402_FEE_LIMIT_SUN": "100000000", "X402_SETTLEMENT_TIMEOUT": "15s",
		"X402_FACILITATOR_LISTEN_ADDR": facAddress, "X402_LISTEN_ADDR": resourceAddress, "X402_FACILITATOR_URL": "http://" + facAddress, "X402_FACILITATOR_TOKEN": httpTestToken, "X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP": "true",
		"X402_RESOURCE_URL": "http://" + resourceAddress + "/resource", "X402_ALLOW_INSECURE_LOCAL_RESOURCE_HTTP": "true", "X402_ENABLE_TESTNET_PAYMENTS": "true", "X402_PAYMENT_STATE_DIR": state, "X402_PAYER_ADDRESS": payer.Address(), "X402_PAYER_KEY_FILE": payerPath,
	}
	stopFac := exampleProcess(t, fixture.ctx, filepath.Join(bin, "facilitator"), exampleEnvironment(values), "http://"+facAddress+"/supported", httpTestToken)
	exampleProcess(t, fixture.ctx, filepath.Join(bin, "server"), exampleEnvironment(values), values["X402_RESOURCE_URL"], "")
	runPayer := func(mode string) []byte {
		t.Helper()
		cmd := exec.CommandContext(fixture.ctx, filepath.Join(bin, "payer"), mode)
		cmd.Env = exampleEnvironment(values)
		b, e := cmd.Output()
		if e != nil {
			t.Fatal("付款示例操作未完成")
		}
		return b
	}
	var result struct {
		Success       bool
		TransactionID string
	}
	if json.Unmarshal(runPayer("pay"), &result) != nil || !result.Success || len(result.TransactionID) != 64 {
		t.Fatal("付款示例未收到匹配成功回执")
	}
	var stateBefore, id string
	var signedBefore []byte
	if fixture.db.QueryRow(fixture.ctx, "SELECT state,transaction_id,signed_payload FROM x402_tron_settlements").Scan(&stateBefore, &id, &signedBefore) != nil || stateBefore != "succeeded" || id != result.TransactionID || fixture.writes.Load() != 1 {
		t.Fatal("可执行三端未持久化唯一付款")
	}
	headerBefore, e := os.ReadFile(filepath.Join(state, "payment-signature"))
	if e != nil {
		t.Fatal("原请求未保留")
	}
	stopFac()
	tlsNode.Close()
	exampleProcess(t, fixture.ctx, filepath.Join(bin, "facilitator"), exampleEnvironment(values), "http://"+facAddress+"/supported", httpTestToken)
	values["X402_PAYER_KEY_FILE"] = "/not-present-must-not-load-during-recovery"
	if json.Unmarshal(runPayer("recover"), &result) != nil || !result.Success || result.TransactionID != id {
		t.Fatal("新facilitator/付款进程无法恢复原成功记录")
	}
	var signedAfter []byte
	var count int
	if fixture.db.QueryRow(fixture.ctx, "SELECT signed_payload,count(*) OVER () FROM x402_tron_settlements").Scan(&signedAfter, &count) != nil || count != 1 || !bytes.Equal(signedBefore, signedAfter) || fixture.writes.Load() != 1 {
		t.Fatal("恢复替换或重复了原交易")
	}
	headerAfter, e := os.ReadFile(filepath.Join(state, "payment-signature"))
	if e != nil || !bytes.Equal(headerBefore, headerAfter) {
		t.Fatal("恢复创建了新授权")
	}
}
