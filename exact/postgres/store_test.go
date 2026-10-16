package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eightyun/x402-tron-go/exact/postgres"
)

func TestInvalidStore(t *testing.T) {
	input := loadVector(t)
	if _, err := postgres.New(nil, input.Config); !errors.Is(err, postgres.ErrInvalidStore) {
		t.Fatal("nil 数据库被接受")
	}
	for _, store := range []*postgres.Store{nil, {}} {
		ctx := context.Background()
		for _, call := range []func() error{
			func() error {
				_, err := store.Reserve(ctx, envelope(t, input), input.Requirements, time.Unix(input.Now, 0))
				return err
			},
			func() error { _, err := store.Get(ctx, strings.Repeat("a", 64)); return err },
			func() error { _, err := store.SaveTransaction(ctx, "", [32]byte{}, postgres.Transaction{}); return err },
			func() error { _, err := store.BeginBroadcast(ctx, "", [32]byte{}); return err },
			func() error { _, err := store.RecordAccepted(ctx, "", [32]byte{}, ""); return err },
		} {
			if err := call(); !errors.Is(err, postgres.ErrInvalidStore) {
				t.Fatalf("零值存储器错误类别：%v", err)
			}
		}
	}
}

func TestInvalidInputAndAuthorization(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	invalidConfig := input.Config
	invalidConfig.Network = "tron:1"
	if _, err := postgres.New(db, invalidConfig); !errors.Is(err, postgres.ErrInvalidStore) {
		t.Fatal("错误网络配置被接受")
	}
	for _, id := range []string{"", strings.Repeat("a", 63), strings.Repeat("G", 64), "' OR 1=1 --"} {
		if _, err := store.Get(ctx, id); !errors.Is(err, postgres.ErrInvalidInput) {
			t.Fatalf("错误 ID 被接受：%v", err)
		}
	}
	first := reserve(t, ctx, store, input)
	for _, candidate := range []postgres.Transaction{
		{},
		{ID: strings.Repeat("0", 64), Payload: []byte{1}},
		{ID: strings.Repeat("A", 64), Payload: []byte{1}},
		{ID: strings.Repeat("a", 64)},
		{ID: strings.Repeat("a", 64), Payload: make([]byte, (1<<20)+1)},
	} {
		if _, err := store.SaveTransaction(ctx, first.ID, first.Authorization.Digest, candidate); !errors.Is(err, postgres.ErrInvalidInput) {
			t.Fatalf("错误交易被接受：%v", err)
		}
	}
	if _, err := store.BeginBroadcast(ctx, first.ID, [32]byte{}); !errors.Is(err, postgres.ErrInvalidInput) {
		t.Fatal("零摘要被接受")
	}
	if _, err := store.RecordAccepted(ctx, first.ID, [32]byte{}, ""); !errors.Is(err, postgres.ErrInvalidInput) {
		t.Fatal("错误受理参数被接受")
	}
	if _, err := store.RecordAccepted(ctx, first.ID, first.Authorization.Digest, strings.Repeat("0", 64)); !errors.Is(err, postgres.ErrInvalidInput) {
		t.Fatal("零交易 ID 被接受")
	}
	input.Signature = "0x00"
	if _, err := store.Reserve(ctx, envelope(t, input), input.Requirements, time.Unix(input.Now, 0)); err == nil {
		t.Fatal("非法授权占用了 nonce")
	}
	var count int
	if err := db.QueryRow(ctx, "SELECT count(*) FROM x402_tron_settlements").Scan(&count); err != nil || count != 1 {
		t.Fatal("非法请求写入了记录")
	}
}
