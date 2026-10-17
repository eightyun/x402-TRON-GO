package exact_test

import (
	"errors"
	"testing"
	"time"

	"github.com/eightyun/x402-tron-go/exact"
)

func TestAuthorizationBindingIdentifiesHistoryButDoesNotAdmitExpiredPayment(t *testing.T) {
	t.Parallel()
	input := loadVector(t)
	verifier := verifierFor(t, input.Config)
	original, err := verifier.VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
	if err != nil {
		t.Fatal(err)
	}
	historical, err := verifier.VerifyAuthorizationBinding(envelope(t, input), input.Requirements)
	if err != nil || historical != original {
		t.Fatalf("历史绑定不符: %v", err)
	}
	if _, err := verifier.VerifyAuthorization(envelope(t, input), input.Requirements, time.Unix(input.Now+900, 0)); !errors.Is(err, exact.ErrInvalidValidity) {
		t.Fatal("历史绑定改变了新请求的有效期规则")
	}
	for _, mutate := range []func(*vector){
		func(v *vector) { v.Signature = "0x00" },
		func(v *vector) { v.Requirements.Amount = "999" },
		func(v *vector) { v.Requirements.PayTo = v.Config.Permit2Address },
		func(v *vector) { v.Config.Network = "tron:728126428" },
	} {
		changed := input
		mutate(&changed)
		if _, err := verifierFor(t, changed.Config).VerifyAuthorizationBinding(envelope(t, changed), changed.Requirements); err == nil {
			t.Fatal("历史恢复放宽了签名、报价或签名域")
		}
	}
	for _, zero := range []*exact.OfflineVerifier{nil, {}} {
		if _, err := zero.VerifyAuthorizationBinding(envelope(t, input), input.Requirements); !errors.Is(err, exact.ErrInvalidConfig) {
			t.Fatal("零值历史绑定校验器被接受")
		}
	}
}
