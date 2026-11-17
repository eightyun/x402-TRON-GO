package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/eightyun/x402-tron-go/exact"
)

var (
	ErrNotSubmitted          = errors.New("postgres: settlement has not been submitted for broadcast")
	ErrSettlementUnconfirmed = errors.New("postgres: settlement not yet confirmed as solidified with the exact transfer")
)

// Submit reads and validates the persisted transaction, atomically commits the unknown state, and
// then broadcasts only the permitted original bytes. Repeated calls never rebroadcast; every
// uncertain outcome keeps the original txid. The caller must still perform on-chain preflight,
// permission and fee risk controls. now is the trusted server time; database latency counts
// toward the validity check before broadcasting.
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

// Track looks up the transaction after a restart using only the original txid from the journal,
// without rebuilding the expired authorization or prepared object. It does not modify the
// journal, does not broadcast, does not treat Found as success or solidification, and does not
// release the reservation when the transaction is not found.
func (store *Store) Track(ctx context.Context, id string, client *exact.ExecutionClient) (exact.TransactionObservation, error) {
	record, err := store.submittedRecord(ctx, id, client)
	if err != nil {
		return exact.TransactionObservation{}, err
	}
	return client.Lookup(ctx, record.Transaction.ID)
}

// CheckSettlement restores the trusted binding from the persisted original transaction and then
// verifies, read-only, that the original transaction is solidified and paid the exact amount.
// executorAddress comes from trusted deployment configuration; the original prepared object need
// not be kept and the expired quote is not revalidated. It does not change the journal state,
// broadcast permission or reservation; Confirmed is not the persisted succeeded terminal state.
// The caller must bound the whole SQL, decoding and RPC round with a context; now comes from a
// trusted server.
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

// ConfirmSettlement atomically saves the succeeded result only after it internally verifies that
// the original transaction is solidified and paid the exact amount. For an already succeeded
// record it verifies the original binding locally and returns idempotently without querying the
// node again. It does not grant permission to deliver the requested resource. An unconfirmed
// result, an error or an unknown commit outcome must never be reported as success or lead to a
// rebroadcast; after a database error, call Get to re-check. No database lock is held during
// RPC; the caller should bound SQL, restoration, RPC and commit with a single context.
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
