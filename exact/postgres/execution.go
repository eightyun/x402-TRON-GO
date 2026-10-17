package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/eightyun/x402-tron-go/exact"
)

var (
	ErrNotSubmitted          = errors.New("原交易尚未进入广播阶段")
	ErrSettlementUnconfirmed = errors.New("原交易尚未通过固化及精确到账核验")
)

// Submit 读取并验证已持久化交易，先原子提交 unknown，再只广播获得许可的原始字节。
// 重复调用不能重播；所有不确定结果保留原 txid。调用方仍须完成链上预检/权限/费用风控。
// now 是可信服务端时间，数据库耗时计入广播前的有效期检查。
func (store *Store) Submit(ctx context.Context, id string, prepared *exact.PreparedSettlement, client *exact.ExecutionClient, now time.Time) (Record, error) {
	started := time.Now()
	if err := store.validateID(id); err != nil {
		return Record{}, err
	}
	if client == nil || client.Network() == "" {
		return Record{}, exact.ErrExecutionConfig
	}
	if client.Network() != store.network {
		return Record{}, exact.ErrChainMismatch
	}
	record, err := store.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if record.State == StateBroadcastUnknown || record.State == StatePending || record.State == StateSucceeded {
		return record, ErrBroadcastStarted
	}
	if record.State != StatePrepared || record.Transaction == nil {
		return record, ErrNotPrepared
	}
	if prepared == nil || prepared.Authorization() != record.Authorization {
		return record, ErrConflict
	}
	transaction := exact.SettlementTransaction{ID: record.Transaction.ID, Payload: record.Transaction.Payload}
	if err := prepared.ValidateBroadcast(transaction, now.Add(time.Since(started))); err != nil {
		return record, err
	}
	record, err = store.BeginBroadcast(ctx, id, record.Authorization.Digest)
	if err != nil {
		// SQL 提交结果也可能未知，必须 Get 原记录核对，不能假定仍可再次广播。
		return Record{}, err
	}
	transaction = exact.SettlementTransaction{ID: record.Transaction.ID, Payload: record.Transaction.Payload}
	if err := client.Broadcast(ctx, prepared, transaction, now.Add(time.Since(started))); err != nil {
		return record, err
	}
	accepted, err := store.RecordAccepted(ctx, id, record.Authorization.Digest, record.Transaction.ID)
	if err != nil {
		return record, errors.Join(exact.ErrBroadcastUnknown, err)
	}
	return accepted, nil
}

// Track 重启后仅用日志中的原 txid 查找主体，不需重建过期授权或准备对象。
// 该方法不修改日志，不广播，不把 Found 视为成功或固化，不因未找到而释放占用。
func (store *Store) Track(ctx context.Context, id string, client *exact.ExecutionClient) (exact.TransactionObservation, error) {
	record, err := store.submittedRecord(ctx, id, client)
	if err != nil {
		return exact.TransactionObservation{}, err
	}
	return client.Lookup(ctx, record.Transaction.ID)
}

// CheckSettlement 从持久化原交易恢复可信绑定，再只读核验原交易固化与精确到账。
// executorAddress 来自可信部署配置；无须保留原准备对象或重新验证已过期报价。
// 不改变日志状态、广播许可或占用；Confirmed 不是持久化成功终态。
// 调用方须为整轮 SQL/解析/RPC 设置有界 context，now 来自可信服务端。
func (store *Store) CheckSettlement(ctx context.Context, id, executorAddress string, client *exact.ExecutionClient, now time.Time) (exact.SettlementObservation, error) {
	started := time.Now()
	record, err := store.submittedRecord(ctx, id, client)
	if err != nil {
		return exact.SettlementObservation{}, err
	}
	prepared, err := store.restoreRecord(record, executorAddress)
	if err != nil {
		return exact.SettlementObservation{}, err
	}
	transaction := exact.SettlementTransaction{ID: record.Transaction.ID, Payload: record.Transaction.Payload}
	return client.CheckSettlement(ctx, prepared, transaction, now.Add(time.Since(started)))
}

// ConfirmSettlement 只允许在内部完成原交易固化/精确到账核验后原子保存成功结果。
// 已成功记录先本地验证原绑定，再幂等返回，不重新查询节点。它不授予资源请求交付许可。
// 未确认/错误/提交结果未知均不能宣称成功或重播；数据库错误后必须 Get 复核。
// RPC 不持有数据库锁，调用方应为 SQL/恢复/RPC/提交设置统一有界 context。
func (store *Store) ConfirmSettlement(ctx context.Context, id, executorAddress string, client *exact.ExecutionClient, now time.Time) (Record, error) {
	started := time.Now()
	record, err := store.submittedRecord(ctx, id, client)
	if err != nil {
		return Record{}, err
	}
	prepared, err := store.restoreRecord(record, executorAddress)
	if err != nil {
		return Record{}, err
	}
	if err := ctx.Err(); err != nil {
		return Record{}, err
	}
	if record.State == StateSucceeded {
		return record, nil
	}
	transaction := exact.SettlementTransaction{ID: record.Transaction.ID, Payload: record.Transaction.Payload}
	observation, err := client.CheckSettlement(ctx, prepared, transaction, now.Add(time.Since(started)))
	if err != nil {
		return record, err
	}
	if !observation.Confirmed {
		return record, ErrSettlementUnconfirmed
	}
	confirmed, err := scanRecord(ctx, store.db.QueryRow(ctx, `
		UPDATE x402_tron_settlements
		SET state = 'succeeded',
		    confirmed_block_number = $9, confirmed_block_id = $10,
		    confirmed_at = CASE WHEN state = 'succeeded' THEN confirmed_at ELSE clock_timestamp() END,
		    updated_at = CASE WHEN state = 'succeeded' THEN updated_at ELSE clock_timestamp() END
		WHERE id = $1 AND network = $2 AND permit2_address = $3 AND digest = $4
		  AND transaction_id = $5 AND signed_payload = $6 AND payer = $7 AND nonce = $8::NUMERIC
		  AND (state IN ('broadcast_unknown', 'pending') OR
		       (state = 'succeeded' AND confirmed_block_number = $9 AND confirmed_block_id = $10))
		RETURNING `+recordColumns,
		id, store.network, store.permit2, record.Authorization.Digest[:], transaction.ID, transaction.Payload,
		record.Authorization.Payer, record.Authorization.Nonce, int64(observation.BlockNumber), observation.BlockID))
	if errors.Is(err, ErrNotFound) {
		return record, store.missingOrConflict(ctx, id)
	}
	if err != nil {
		// UPDATE 可能已提交，但响应丢失；只返回提交前快照和错误，不能虚构成功。
		return record, err
	}
	return confirmed, nil
}

func (store *Store) submittedRecord(ctx context.Context, id string, client *exact.ExecutionClient) (Record, error) {
	if err := store.validateID(id); err != nil {
		return Record{}, err
	}
	if client == nil || client.Network() == "" {
		return Record{}, exact.ErrExecutionConfig
	}
	if client.Network() != store.network {
		return Record{}, exact.ErrChainMismatch
	}
	record, err := store.Get(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if (record.State != StateBroadcastUnknown && record.State != StatePending && record.State != StateSucceeded) || record.Transaction == nil {
		return Record{}, ErrNotSubmitted
	}
	return record, nil
}

func (store *Store) restoreRecord(record Record, executorAddress string) (*exact.PreparedSettlement, error) {
	transaction := exact.SettlementTransaction{ID: record.Transaction.ID, Payload: record.Transaction.Payload}
	prepared, err := exact.RestoreSettlement(store.config, executorAddress, transaction)
	if err != nil {
		return nil, err
	}
	if prepared.Authorization() != record.Authorization {
		return nil, ErrConflict
	}
	return prepared, nil
}
