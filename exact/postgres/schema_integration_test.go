package postgres_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
)

func installLegacySchema(t *testing.T, ctx context.Context, db *pgxpool.Pool) {
	t.Helper()
	legacy, err := os.ReadFile("testdata/schema_v1.sql")
	if err != nil {
		t.Fatal(err)
	}
	// setup 刚创建的独立随机 schema 尚无数据，仅替换本用例自己的空表。
	if _, err := db.Exec(ctx, "DROP TABLE x402_tron_settlements"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, string(legacy)); err != nil {
		t.Fatal(err)
	}
}

func journalSnapshot(t *testing.T, ctx context.Context, db *pgxpool.Pool) []byte {
	t.Helper()
	var value []byte
	if err := db.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(t) - 'confirmed_block_number' - 'confirmed_block_id' - 'confirmed_at' ORDER BY id), '[]'::jsonb)
		FROM x402_tron_settlements t`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestUpgradeConfirmationPreservesLegacyJournal(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	installLegacySchema(t, ctx, db)
	permit2, err := tron.NormalizeAddressHex(input.Config.Permit2Address)
	if err != nil {
		t.Fatal(err)
	}
	for index, state := range []string{"reserved", "prepared", "broadcast_unknown", "pending"} {
		var txid, payload interface{}
		if state != "reserved" {
			txid, payload = strings.Repeat(string(rune('a'+index)), 64), []byte("legacy-synthetic-transaction")
		}
		if _, err := db.Exec(ctx, `INSERT INTO x402_tron_settlements
			(id, network, permit2_address, payer, nonce, digest, state, transaction_id, signed_payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, strings.Repeat(string(rune('a'+index)), 64),
			input.Config.Network, permit2, "legacy-synthetic-payer", index, bytes.Repeat([]byte{byte(index)}, 32), state, txid, payload); err != nil {
			t.Fatal(err)
		}
	}
	before := journalSnapshot(t, ctx, db)
	// 未升级的旧库必须报错，不能默默缺失成功审计列。
	if _, err := store.Get(ctx, strings.Repeat("a", 64)); err != postgres.ErrStore {
		t.Fatal("未升级 schema 被当作完整日志读取")
	}
	for range 2 {
		if _, err := db.Exec(ctx, postgres.UpgradeConfirmationSQL); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, journalSnapshot(t, ctx, db)) {
			t.Fatal("升级改变了原状态、交易、授权或时间")
		}
	}
	for index, state := range []postgres.State{postgres.StateReserved, postgres.StatePrepared, postgres.StateBroadcastUnknown, postgres.StatePending} {
		record, err := store.Get(ctx, strings.Repeat(string(rune('a'+index)), 64))
		if err != nil || record.State != state || record.Confirmation != nil {
			t.Fatalf("旧状态未保留: %v", err)
		}
	}
}

func TestUpgradeConfirmationRollsBackIncompatibleSchema(t *testing.T) {
	t.Parallel()
	ctx, db, _, _ := setup(t)
	installLegacySchema(t, ctx, db)
	// 模拟被外部更改过的旧约束，升级不得吞掉不兼容数据或覆盖原约束。
	if _, err := db.Exec(ctx, `ALTER TABLE x402_tron_settlements DROP CONSTRAINT x402_tron_settlements_state_check;
		ALTER TABLE x402_tron_settlements ADD CONSTRAINT x402_tron_settlements_state_check CHECK (state <> 'unsupported');
		INSERT INTO x402_tron_settlements (id,network,permit2_address,payer,nonce,digest,state,transaction_id,signed_payload)
		VALUES (repeat('a',64),'synthetic','41' || repeat('1',40),'synthetic',1,decode(repeat('01',32),'hex'),'external',repeat('b',64),decode('01','hex'))`); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, postgres.UpgradeConfirmationSQL); err == nil {
		t.Fatal("不兼容旧数据被静默升级")
	}
	if _, err := conn.Exec(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'x402_tron_settlements'
		AND column_name IN ('confirmed_block_number','confirmed_block_id','confirmed_at')`).Scan(&count); err != nil || count != 0 {
		t.Fatal("失败升级遗留了部分新增列")
	}
	var definition, state string
	if err := conn.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
		WHERE conrelid = 'x402_tron_settlements'::regclass AND conname = 'x402_tron_settlements_state_check'`).Scan(&definition); err != nil || !strings.Contains(definition, "unsupported") {
		t.Fatal("失败升级未恢复原约束")
	}
	if err := conn.QueryRow(ctx, "SELECT state FROM x402_tron_settlements").Scan(&state); err != nil || state != "external" {
		t.Fatal("失败升级改变了旧数据")
	}
}

func TestConfirmationSchemaRejectsPartialOrInconsistentEvidence(t *testing.T) {
	t.Parallel()
	ctx, db, store, input := setup(t)
	record := reserve(t, ctx, store, input)
	if _, err := store.SaveTransaction(ctx, record.ID, record.Authorization.Digest, transaction("a")); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		"UPDATE x402_tron_settlements SET state = 'succeeded'",
		"UPDATE x402_tron_settlements SET confirmed_at = clock_timestamp()",
		"UPDATE x402_tron_settlements SET state = 'succeeded', confirmed_at = clock_timestamp(), confirmed_block_number = 1",
		"UPDATE x402_tron_settlements SET state = 'succeeded', confirmed_at = clock_timestamp(), confirmed_block_number = 0, confirmed_block_id = repeat('0',64)",
		"UPDATE x402_tron_settlements SET state = 'succeeded', confirmed_at = clock_timestamp(), confirmed_block_number = 1, confirmed_block_id = repeat('a',64)",
		"UPDATE x402_tron_settlements SET state = 'succeeded', confirmed_at = clock_timestamp(), confirmed_block_number = 1, confirmed_block_id = '0000000000000001' || repeat('A',48)",
	} {
		if _, err := db.Exec(ctx, statement); err == nil {
			t.Fatal("约束接受了缺失或不一致的确认字段")
		}
	}
	after, err := store.Get(ctx, record.ID)
	if err != nil || after.State != postgres.StatePrepared || after.Confirmation != nil {
		t.Fatal("约束拒绝后状态被改变")
	}
}
