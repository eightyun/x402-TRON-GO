package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/eightyun/x402-tron-go/exact"
)

var ErrNotSubmitted = errors.New("原交易尚未进入广播阶段")

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
	if record.State == StateBroadcastUnknown || record.State == StatePending {
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
	if err := store.validateID(id); err != nil {
		return exact.TransactionObservation{}, err
	}
	if client == nil || client.Network() == "" {
		return exact.TransactionObservation{}, exact.ErrExecutionConfig
	}
	if client.Network() != store.network {
		return exact.TransactionObservation{}, exact.ErrChainMismatch
	}
	record, err := store.Get(ctx, id)
	if err != nil {
		return exact.TransactionObservation{}, err
	}
	if (record.State != StateBroadcastUnknown && record.State != StatePending) || record.Transaction == nil {
		return exact.TransactionObservation{}, ErrNotSubmitted
	}
	return client.Lookup(ctx, record.Transaction.ID)
}
