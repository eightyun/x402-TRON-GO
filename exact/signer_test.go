package exact_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/eightyun/x402-tron-go/exact"
)

func TestEOASignerValidationAndClose(t *testing.T) {
	t.Parallel()
	order := secp256k1.S256().Params().N.FillBytes(make([]byte, 32))
	for _, bad := range [][]byte{nil, {1}, make([]byte, 32), make([]byte, 33), order} {
		if signer, err := exact.NewEOASigner(bad); !errors.Is(err, exact.ErrEOASigner) || signer != nil {
			t.Fatal("无效标量未拒绝")
		}
	}
	for _, signer := range []*exact.EOASigner{nil, {}} {
		if _, err := signer.SignAuthorization(context.Background(), [32]byte{}); !errors.Is(err, exact.ErrEOASigner) {
			t.Fatal(err)
		}
		if err := signer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if (*exact.EOASigner)(nil).Address() != "" {
		t.Fatal("nil 地址不为空")
	}
	key, err := secp256k1.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	defer key.Zero()
	encoded := key.Serialize()
	signer, err := exact.NewEOASigner(encoded)
	clear(encoded)
	if err != nil {
		t.Fatal(err)
	}
	defer signer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := signer.SignAuthorization(ctx, [32]byte{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if signature, err := signer.SignAuthorization(context.Background(), [32]byte{1}); err != nil || len(signature) != 65 {
		t.Fatal("输入副本被改写影响签名器")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if got := fmt.Sprintf(format, signer); got != "EOASigner(<redacted>)" {
			t.Fatal("格式化暴露内部字段")
		}
	}
	data, err := json.Marshal(signer)
	if err != nil || string(data) != "{}" {
		t.Fatal("JSON 暴露内部字段")
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, err := signer.SignAuthorization(context.Background(), [32]byte{2})
			if err != nil && !errors.Is(err, exact.ErrEOASigner) {
				t.Error(err)
			}
		})
		wg.Go(func() { _ = signer.Close() })
	}
	wg.Wait()
	if signature, err := signer.SignAuthorization(context.Background(), [32]byte{}); signature != nil || !errors.Is(err, exact.ErrEOASigner) || strings.Contains(err.Error(), "private") {
		t.Fatal("关闭后仍可签名")
	}
}
