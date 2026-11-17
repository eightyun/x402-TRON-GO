// Package postgres provides an optional idempotent settlement journal, controlled submission and
// persistence of trusted-node verification results. It holds no keys.
package postgres

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/x402-foundation/x402/go/v2/types"
)

// SchemaSQL must be applied explicitly by the deployer; constructing a Store does not modify the
// database schema. Journal records that were ever used for a broadcast must never be deleted;
// the runtime account should not hold DELETE or TRUNCATE privileges.
//
//go:embed schema.sql
var SchemaSQL string

// UpgradeConfirmationSQL explicitly enables the succeeded terminal state on an existing journal
// table. Back up first and stop writes from older versions. The script is re-runnable and does
// not delete history; if its transaction fails, the caller should roll back the unfinished
// transaction on that connection.
//
//go:embed upgrade_confirmation.sql
var UpgradeConfirmationSQL string

var (
	ErrInvalidStore     = errors.New("postgres: invalid settlement journal configuration")
	ErrInvalidInput     = errors.New("postgres: invalid settlement journal input")
	ErrStore            = errors.New("postgres: settlement journal database operation failed")
	ErrNotFound         = errors.New("postgres: settlement journal record not found")
	ErrConflict         = errors.New("postgres: authorization nonce or transaction already used by another settlement")
	ErrNotPrepared      = errors.New("postgres: settlement has no saved signed transaction")
	ErrBroadcastStarted = errors.New("postgres: settlement already broadcast or pending; only tracking the original transaction is allowed")
)

// State represents local execution progress and persisted trusted-node verification results.
// It is not independent on-chain proof.
type State string

const (
	StateReserved         State = "reserved"
	StatePrepared         State = "prepared"
	StateBroadcastUnknown State = "broadcast_unknown"
	StatePending          State = "pending"
	StateSucceeded        State = "succeeded"
)

// Confirmation is the audit locator recorded after the original transaction passed solidification
// and exact-payment verification. It is not proof of state against a malicious node.
type Confirmation struct {
	BlockNumber uint64
	BlockID     string
	ConfirmedAt time.Time
}

// Transaction holds the transaction ID and opaque signed bytes supplied by a trusted transaction
// builder. The journal only enforces that they cannot be replaced; it does not verify the
// signature, the binding between content and authorization, or on-chain validity.
// Payload is broadcastable sensitive data and must never be logged or returned from merchant
// APIs.
type Transaction struct {
	ID      string
	Payload []byte `json:"-"`
}

// Record can be used to recover the original transaction after a process restart. It is not a
// credential for delivering the resource.
type Record struct {
	ID             string
	Permit2Address string
	Authorization  exact.VerifiedAuthorization
	State          State
	Transaction    *Transaction
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Confirmation   *Confirmation `json:",omitempty"`
}

// Store is safe for concurrent use. All instances must share the same journal, and broadcasting
// outside the journal is forbidden. The deployer manages the db lifecycle as well as a trusted
// search_path, TLS, privileges and backups.
type Store struct {
	db       *pgxpool.Pool
	verifier *exact.OfflineVerifier
	network  string
	permit2  string
	config   exact.Config
}

// New pins one trusted network and Permit2 domain. It does not contact nodes or run DDL
// automatically.
func New(db *pgxpool.Pool, config exact.Config) (*Store, error) {
	verifier, err := exact.NewOfflineVerifier(config)
	if db == nil || err != nil {
		return nil, ErrInvalidStore
	}
	network, err := tron.NormalizeNetwork(config.Network)
	if err != nil {
		return nil, ErrInvalidStore
	}
	permit2, err := tron.NormalizeAddressHex(config.Permit2Address)
	if err != nil {
		return nil, ErrInvalidStore
	}
	return &Store{db: db, verifier: verifier, network: network, permit2: permit2, config: config}, nil
}

// Reserve verifies the authorization offline and then durably reserves the payer's Permit2 nonce.
// A different digest for the same nonce conflicts; a repeated request with the same
// authorization returns the original record. It does not reserve on-chain balance and does not
// replace preflight checks; now must come from a trusted server clock.
func (store *Store) Reserve(ctx context.Context, payload types.PaymentPayload, requirements types.PaymentRequirements, now time.Time) (Record, error) {
	if !store.valid() {
		return Record{}, ErrInvalidStore
	}
	verified, err := store.verifier.VerifyAuthorization(payload, requirements, now)
	if err != nil {
		return Record{}, err
	}
	id := store.authorizationID(verified)
	_, err = store.db.Exec(ctx, `
		INSERT INTO x402_tron_settlements (id, network, permit2_address, payer, nonce, digest)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT DO NOTHING
	`, id, store.network, store.permit2, verified.Payer, verified.Nonce, verified.Digest[:])
	if err != nil {
		return Record{}, storageError(ctx, err)
	}
	record, err := store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Record{}, ErrConflict
	}
	if err != nil {
		return Record{}, err
	}
	if record.Authorization != verified {
		return Record{}, ErrConflict
	}
	return record, nil
}

func (store *Store) authorizationID(verified exact.VerifiedAuthorization) string {
	identity := sha256.Sum256([]byte(store.network + "\x00" + store.permit2 + "\x00" + verified.Payer + "\x00" + verified.Nonce))
	return hex.EncodeToString(identity[:])
}

const recordColumns = `id, network, permit2_address, payer, nonce::TEXT, digest, state,
	transaction_id, signed_payload, created_at, updated_at, confirmed_block_number, confirmed_block_id, confirmed_at`

// Get reads only records in this Store's network and Permit2 domain. The original transaction
// can still be tracked even after the original authorization has expired.
func (store *Store) Get(ctx context.Context, id string) (Record, error) {
	if err := store.validateID(id); err != nil {
		return Record{}, err
	}
	return scanRecord(ctx, store.db.QueryRow(ctx, `SELECT `+recordColumns+`
		FROM x402_tron_settlements WHERE id = $1 AND network = $2 AND permit2_address = $3
	`, id, store.network, store.permit2))
}

// SaveTransaction atomically saves the single signed result. It must succeed before broadcasting,
// and the caller must use the persisted bytes it returns. Retrying with the same result is
// idempotent; a different result, a different digest or a txid already used by another
// authorization conflicts. The caller must verify that the ID matches the Payload, the
// authorization binding and the validity window; this function does not interpret the
// transaction encoding.
func (store *Store) SaveTransaction(ctx context.Context, id string, digest [32]byte, transaction Transaction) (Record, error) {
	if err := store.validateID(id); err != nil {
		return Record{}, err
	}
	if digest == [32]byte{} || !validTransactionID(transaction.ID) || len(transaction.Payload) == 0 || len(transaction.Payload) > 1<<20 {
		return Record{}, ErrInvalidInput
	}
	record, err := store.saveTransaction(ctx, store.db, id, digest, transaction)
	if errors.Is(err, ErrNotFound) {
		return Record{}, store.missingOrConflict(ctx, id)
	}
	return record, err
}

// rowQuerier 仅用于连接池与显式事务共享同一保存语句，不将事务内写入旁路到连接池。
type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (store *Store) saveTransaction(ctx context.Context, db rowQuerier, id string, digest [32]byte, transaction Transaction) (Record, error) {
	return scanRecord(ctx, db.QueryRow(ctx, `
		UPDATE x402_tron_settlements
		SET state = CASE WHEN state = 'reserved' THEN 'prepared' ELSE state END,
		    transaction_id = $5, signed_payload = $6,
		    updated_at = CASE WHEN state = 'reserved' THEN clock_timestamp() ELSE updated_at END
		WHERE id = $1 AND network = $2 AND permit2_address = $3 AND digest = $4
		  AND (state = 'reserved' OR (transaction_id = $5 AND signed_payload = $6))
		RETURNING `+recordColumns, id, store.network, store.permit2, digest[:], transaction.ID, transaction.Payload))
}

// BeginBroadcast is the only broadcast permission: it first commits the unknown state and then
// returns the original signed result stored in the database. Only one call can succeed. The
// caller may broadcast the returned value only after this call succeeds, and must never
// broadcast a transaction that was not saved locally. An unknown database commit outcome, a
// process exit or an RPC timeout must never lead to creating a new transaction; call Get and
// track the original txid. A crash before the actual broadcast may also leave the unknown state;
// manual recovery is preferred over risking a double payment.
func (store *Store) BeginBroadcast(ctx context.Context, id string, digest [32]byte) (Record, error) {
	if err := store.validateID(id); err != nil {
		return Record{}, err
	}
	if digest == [32]byte{} {
		return Record{}, ErrInvalidInput
	}
	record, err := scanRecord(ctx, store.db.QueryRow(ctx, `
		UPDATE x402_tron_settlements SET state = 'broadcast_unknown', updated_at = clock_timestamp()
		WHERE id = $1 AND network = $2 AND permit2_address = $3 AND digest = $4 AND state = 'prepared'
		RETURNING `+recordColumns, id, store.network, store.permit2, digest[:]))
	if errors.Is(err, ErrNotFound) {
		existing, readErr := store.Get(ctx, id)
		if readErr != nil {
			return Record{}, readErr
		}
		if existing.Authorization.Digest != digest {
			return Record{}, ErrConflict
		}
		if existing.State == StateReserved {
			return Record{}, ErrNotPrepared
		}
		return Record{}, ErrBroadcastStarted
	}
	return record, err
}

// RecordAccepted only records that a node accepted the original txid; it must not be used to
// deliver the resource or treated as on-chain success. Rejection, timeout or a missing
// transaction must never release the nonce or replace the original transaction; while
// unconfirmed, the record stays unknown or pending. A matching callback on an already succeeded
// record only returns the original result, without rolling back the state or changing the first
// confirmation time.
func (store *Store) RecordAccepted(ctx context.Context, id string, digest [32]byte, transactionID string) (Record, error) {
	if err := store.validateID(id); err != nil {
		return Record{}, err
	}
	if digest == [32]byte{} || !validTransactionID(transactionID) {
		return Record{}, ErrInvalidInput
	}
	record, err := scanRecord(ctx, store.db.QueryRow(ctx, `
		UPDATE x402_tron_settlements
		SET state = CASE WHEN state = 'succeeded' THEN state ELSE 'pending' END,
		    updated_at = CASE WHEN state = 'broadcast_unknown' THEN clock_timestamp() ELSE updated_at END
		WHERE id = $1 AND network = $2 AND permit2_address = $3 AND digest = $4
		  AND transaction_id = $5 AND state IN ('broadcast_unknown', 'pending', 'succeeded')
		RETURNING `+recordColumns, id, store.network, store.permit2, digest[:], transactionID))
	if errors.Is(err, ErrNotFound) {
		return Record{}, store.missingOrConflict(ctx, id)
	}
	return record, err
}

func (store *Store) valid() bool {
	return store != nil && store.db != nil && store.verifier != nil
}

func (store *Store) validateID(id string) error {
	if !store.valid() {
		return ErrInvalidStore
	}
	if !validHexID(id) {
		return ErrInvalidInput
	}
	return nil
}

func validHexID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validTransactionID(id string) bool {
	return validHexID(id) && id != strings.Repeat("0", 64)
}

func (store *Store) missingOrConflict(ctx context.Context, id string) error {
	_, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	return ErrConflict
}

func scanRecord(ctx context.Context, row pgx.Row) (Record, error) {
	var record Record
	var digest, payload []byte
	var transactionID *string
	var blockNumber *int64
	var blockID *string
	var confirmedAt *time.Time
	err := row.Scan(&record.ID, &record.Authorization.Network, &record.Permit2Address,
		&record.Authorization.Payer, &record.Authorization.Nonce, &digest, &record.State,
		&transactionID, &payload, &record.CreatedAt, &record.UpdatedAt, &blockNumber, &blockID, &confirmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, storageError(ctx, err)
	}
	if len(digest) != 32 {
		return Record{}, ErrStore
	}
	copy(record.Authorization.Digest[:], digest)
	if transactionID != nil {
		record.Transaction = &Transaction{ID: *transactionID, Payload: payload}
	}
	if record.State == StateSucceeded {
		if blockNumber == nil || *blockNumber <= 0 || blockID == nil || !validHexID(*blockID) || confirmedAt == nil || transactionID == nil {
			return Record{}, ErrStore
		}
		record.Confirmation = &Confirmation{BlockNumber: uint64(*blockNumber), BlockID: *blockID, ConfirmedAt: *confirmedAt}
	} else if blockNumber != nil || blockID != nil || confirmedAt != nil {
		return Record{}, ErrStore
	}
	return record, nil
}

func storageError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		if errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		return context.DeadlineExceeded
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	// 数据库原始错误可能包含 SQL 参数、业务数据和连接信息，不向调用方透传。
	return ErrStore
}
