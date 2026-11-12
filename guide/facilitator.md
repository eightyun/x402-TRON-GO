# Self-hosted facilitator

[简体中文](facilitator.zh-CN.md) | English

This guide explains how to run the durable, PostgreSQL-backed facilitator from `exact/postgres`. It covers embedding it in your resource server, serving it over HTTP, the testnet payer example, the official mechanism contract and the settlement journal that makes recovery safe.

## Deployment options

| Option | Use it when | Entry point |
| --- | --- | --- |
| Embed in-process | The facilitator runs inside your resource server | `postgres.NewLocalClient` |
| Serve over HTTP | The facilitator runs as a separate service | `postgres.NewHTTPHandler` |
| Existing facilitator | You already operate a separate facilitator | Official `HTTPFacilitatorClient` |

The HTTP executables in `examples/` are optional. You do not need them to import the SDK.

### Embed in your application (recommended)

1. Create the store with `postgres.New(pool, config.Preflight.Authorization)`. An administrator must apply the schema beforehand (see [Durable settlement journal](#durable-settlement-journal)).
2. Create the mechanism with `postgres.NewFacilitator(store, config)`. It implements the official facilitator mechanism.
3. Wrap it with `postgres.NewLocalClient(mechanism, 8)`.
4. Pass the result to the official `x402.WithFacilitatorClient(client)` in your resource server.

No facilitator URL, Bearer token, listener or separate service is required. Your application manages the database pool and the transaction signer. The adapter reuses the official core and the existing settlement and recovery logic. It does not duplicate protocol code or weaken any check.

The in-process client behaves as follows:

- It accepts x402 v2 only, with a combined input limit of 64 KiB.
- It rejects duplicate or ambiguous keys, unknown structural fields and trailing JSON.
- `maxConcurrent` accepts 1 to 64. A value of `0` defaults to 8.
- When all slots are busy, it returns `ErrLocalClientCapacity` immediately. There is no unbounded queue.
- Cancellation and deadlines keep their normal `context` semantics.
- Requests cannot select nodes, signers or trusted contracts.

The concurrency limit applies to this client only. It is not a spending budget and not application authentication.

A failed or pending payment never permits delivery. Recovery uses only the original authorization and the same durable journal.

### Serve over HTTP

```go
handler, err := postgres.NewHTTPHandler(mechanism, postgres.HTTPConfig{BearerToken: token, MaxConcurrent: 8})
```

The handler registers the official SDK core and exposes three endpoints: `GET /supported`, `POST /verify` and `POST /settle`.

- Every endpoint requires a Bearer token of 32 to 256 bytes of non-whitespace ASCII.
- JSON bodies are limited to 64 KiB.
- Duplicate or ambiguous keys, unknown envelope fields, trailing values and non-v2 requests are rejected.
- Concurrent verification and settlement defaults to 8 and accepts 1 to 64.
- Requests over the limit receive `503` without any signing or broadcasting.

The concurrency limit is per process. It is not an aggregate spending budget.

### Run the example facilitator service

The [runnable example](../examples/facilitator/main.go) wraps `NewHTTPHandler` in a service. It targets Linux and macOS. It allows only Nile and Shasta, listens only on a literal loopback address, and **rejects mainnet and public binding**.

Configure these variables, then run `go run ./examples/facilitator`:

| Variable | Required | Meaning |
| --- | --- | --- |
| `X402_NETWORK` | No | Defaults to Nile. Shasta is also allowed. |
| `X402_DATABASE_URL` | Yes | Isolated journal database. Use a trusted `search_path`, a least-privilege runtime role, and TLS for remote connections. |
| `X402_EXECUTOR_KEY_FILE` | Yes | Absolute path to a regular file owned by the service user with mode 0400 or 0600. It contains 64 hex characters, optionally followed by one newline. A symlink as the final path component is rejected. |
| `X402_FACILITATOR_TOKEN` | Yes | Must match the resource server. Inject it through the deployment environment. Never put it in source code, URLs or logs. |
| `X402_PERMIT2_ADDRESS`, `X402_PROXY_ADDRESS` | Yes | Independently verified deployments. There are no default addresses. |
| `X402_PERMIT2_CODE_HASH`, `X402_PROXY_CODE_HASH` | Yes | Trusted Keccak-256 hashes of the runtime bytecode: `0x` followed by 32 bytes. |
| `X402_JSON_RPC_URL` | Yes | Trusted HTTPS JSON-RPC endpoint. |
| `X402_RPC_BATCH` | No | Defaults to `false`. Only the literal value `true` batches independent reads. The node must support batching; there is no fallback. |
| `X402_NATIVE_NODE_URL` | Yes | HTTPS root URL of a native node. The `wallet` and `walletsolidity` routes are required. |
| `X402_GENESIS_BLOCK_ID` | Yes | Independently verified full genesis block ID. |
| `X402_NODE_API_KEY` | No | Service credential for these nodes. It is never taken from requests. |
| `X402_ASSET`, `X402_PAY_TO` | Yes | Fixed token and recipient. |
| `X402_MAX_AMOUNT_ATOMIC` | Yes | Per-payment cap as a positive integer in atomic units. |
| `X402_FEE_LIMIT_SUN` | Yes | Positive per-transaction fee cap. It is **not** an aggregate gas budget. |
| `X402_SETTLEMENT_TIMEOUT` | No | Defaults to `90s`. Accepts 1 second to 3 minutes. The original transaction lifetime is fixed at 60 seconds. Transactions already submitted remain recoverable after the timeout. |
| `X402_MAX_CONCURRENT` | No | Defaults to 8. |
| `X402_FACILITATOR_LISTEN_ADDR` | No | Defaults to `127.0.0.1:8081`. |

#### Database setup

- An administrator applies [schema.sql](../exact/postgres/schema.sql) explicitly, using a separate migration role.
- Existing tables need the upgrade described in [Durable settlement journal](#durable-settlement-journal).
- Do not put database URLs or passwords in command-line arguments.
- The runtime role should not have DDL, `DELETE` or `TRUNCATE` privileges.

#### What startup checks

Startup checks the configuration, database connectivity and that the current-version columns are readable.

Startup **does not** create or migrate tables, inspect every constraint, check live deployments, sign, approve tokens or submit payments. It is not a full deployment acceptance test. Verify these yourself:

- the complete database schema;
- the executor's default-owner permission, TRX balance and energy;
- the token's semantics;
- the payer's Permit2 allowance.

#### Connect the example resource server

For a local integration, set these on the resource server and run `go run ./examples/server`:

- `X402_FACILITATOR_URL=http://127.0.0.1:8081`
- the same `X402_FACILITATOR_TOKEN`
- `X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP=true`

Cross-machine deployments require trusted TLS ingress that forwards to the loopback listener, ingress rate limits and aggregate spending budgets.

#### Key handling

- The example loads only the explicitly configured key file. It does not discover or modify existing wallets.
- The key stays in process memory and is cleared on shutdown on a best-effort basis. This is not HSM or MPC security.
- SDK integrations can supply an external signer through `FacilitatorConfig.Signer` instead.

#### Responses and shutdown

- Responses use the official formats.
- An unknown or unconfirmed result is never reported as success.
- HTTP failures keep the `settlement_pending` reason and the original txid, so the identical payload can be used for recovery.
- A disconnect or a `503` does not prove that the payment failed. Never generate a replacement authorization.
- `SIGTERM` drains requests that were already accepted. Interrupted work recovers from the same journal. There is no automatic background executor and no automatic rebroadcast.

## Testnet payer example

The [payer example](../examples/payer/main.go) makes real testnet payments. It must be enabled explicitly and runs on Linux and macOS. It is separate from `examples/client`, which only performs an offline handshake.

### Configure

Use [.env.example](../.env.example) as a template for:

- the trusted network, Permit2 address and proxy address;
- the token and recipient;
- `X402_MAX_AMOUNT_ATOMIC`, `X402_PAYER_ADDRESS` and `X402_PAYER_KEY_FILE`;
- the resource URL.

The template is not loaded automatically. Never commit real credentials, keys or authorizations.

Then prepare the state directory:

1. Create a **0700 state directory owned by the service user, under parent directories that you control privately**.
2. Set its absolute path as `X402_PAYMENT_STATE_DIR`.
3. Set `X402_ENABLE_TESTNET_PAYMENTS=true`.

### Pay and recover

```bash
go run ./examples/payer pay
# After a disconnect, failure, pending result or interruption, reuse the same directory and configuration. Never pay again.
go run ./examples/payer recover
```

`pay` works in this order:

1. It exclusively persists a payment intent. If an intent already exists, `pay` refuses to run.
2. The official client signs one authorization from the 402 quote.
3. The original `PAYMENT-SIGNATURE` header is saved before it is sent.

`recover` checks that the original configuration and signature still match. It then sends the same header again, without generating a new nonce. It can complete an authorization that was never submitted, so it **is not a read-only lookup**. Recovery never loads the payer key.

Each attempt and the content of a successful response are kept in private files. Standard output contains only restricted metadata.

### Request restrictions

- Only `GET` to the fixed URL on Nile or Shasta is supported.
- HTTPS is the default. Plain HTTP requires `X402_ALLOW_INSECURE_LOCAL_RESOURCE_HTTP=true` and a literal loopback IP.
- Mainnet, credentials in the URL, query strings, fragments and redirects are rejected.
- The key must be a regular file owned by the service user, with mode 0400 or 0600.

### Receipt check

The payer reports success only when the response is `200` with a successful `PAYMENT-RESPONSE` receipt that matches:

- the configured network;
- the configured payer, either in Base58 form or the same address in `0x` signing form;
- the quoted amount, if the receipt includes one (the amount field is optional in x402);
- a nonempty txid.

This check trusts the facilitator service. It is not an independent on-chain proof.

### Recovery rules

> [!WARNING]
> Never clear the state directory to retry, and never use another directory to replace a payment whose outcome is unknown. Doing so can pay twice.

If an intent exists but no header was saved, investigate manually whether an authorization was signed or sent. Do not re-sign automatically.

The example does not:

- fund wallets or approve tokens;
- retry automatically;
- enforce a cumulative gas budget;
- deduplicate business fulfillment.

Replaying a resource `GET` may run the handler again.

## Official facilitator mechanism

Construct the mechanism with `postgres.NewFacilitator(store, config)` and register it with the official SDK:

```go
x402.Newx402Facilitator().Register([]x402.Network{x402.Network(mechanism.Network())}, mechanism)
```

You do not need custom TRON verification, transaction encoding or a settlement state machine. Construction does not connect to anything, migrate the database, sign, broadcast or start HTTP or background services.

### Configuration

Supply every trusted `FacilitatorConfig` value explicitly:

| Field | Meaning |
| --- | --- |
| `Preflight` | Authorization domain, JSON-RPC endpoint and independently verified runtime-code hashes for Permit2 and the proxy. |
| `Execution` | Native-node root on the same network and the pinned full genesis ID. The `wallet` and `walletsolidity` APIs are required. |
| `Policy`, `MaxAmount` | Fixed network, a single asset and recipient, and the per-payment cap in atomic units. Requests cannot override them. |
| `ExecutorAddress`, `Signer` | A default-owner EOA that pays the gas, and an external digest signer. The signer must honor its context and must never broadcast by itself. |
| `FeeLimitSUN` | Positive per-transaction fee limit. |
| `Lifetime` | Transaction lifetime in whole milliseconds, greater than 15 seconds and at most 10 minutes. |
| `Timeout` | Total settlement budget. Defaults to 90 seconds, at most 3 minutes. |
| `PollInterval` | Confirmation polling interval. Defaults to 3 seconds, range 1 to 10 seconds. |

### Scope

- Only x402 v2 with the `exact` scheme, Permit2 and the `upfront` flow is supported.
- Quote timeouts must be 6 to 600 seconds. A new transaction still requires at least 15 seconds of remaining validity.
- The mechanism declares `supportedAssetTransferMethods: ["permit2"]` and the configured executor as signer.
- All deployment and network settings must match the store.
- Dynamic multi-merchant routing, other schemes, extensions, multisig accounts and contract wallets are not supported.

### Verify

`Verify` is a read-only check of a fresh payment. It does not reserve the journal, sign or broadcast. A successful `Verify` does not promise that settlement will succeed.

### Settle

`Settle` does not depend on a preceding `Verify`.

For a new payment:

1. The signature, policy, time window and on-chain state are checked.
2. The journal entry is reserved.
3. Inside a row-locked database transaction, the state is checked again, and the unique original transaction is built, signed and committed.
4. Only after that commit is the transaction broadcast.

Preflight, broadcast and finality waiting run outside the database transaction. When instances compete, the losers reconcile with the winner's transaction instead of creating another payment.

For an authorization that is already `broadcast_unknown`, `pending` or `succeeded`:

- The request must match the trusted requirements, the signature and the durable identity. The original transaction is then recovered.
- Current-validity admission is not rerun, and the already-consumed nonce is not queried.
- `VerifyAuthorizationBinding` only identifies historical records. It never replaces fresh `VerifyAuthorization` and `Preflight.Check` admission.

### Settlement results

| Result | When |
| --- | --- |
| `success=true` with the original txid, network, payer and amount | Only after `ConfirmSettlement` has persisted success. |
| `settlement_pending` with the original txid | The budget ran out, the broadcast outcome is unknown, or the verification nodes are unavailable. Missing solidified evidence is polled within the budget first. |
| `settlement_recovery_required` | Persistence was ambiguous. Read the journal back before doing anything else. |

Neither `settlement_pending` nor `settlement_recovery_required` permits releasing the reservation, re-signing the saved transaction or creating another payment.

### Retries and timeouts

- The official resource server retries a pending result with an original txid **once, using identical bytes**. It does not retry indefinitely.
- The mechanism owns the bounded waiting.
- The paired HTTP examples align the remote, middleware and write timeouts. Your proxies and callers must cover the same budget.
- After an interruption, recover with the same payment payload. Never authorize a new payment.

### Upfront flow

- In the `upfront` flow, payment is settled before the handler runs. If the handler later fails, the payment is not refunded automatically. Do not settle again.
- Your application needs fulfillment idempotency, lookup and compensation.
- New `upfront` quotes require the updated client. Existing quotes and signatures without `paymentFlow` remain compatible.
- The signed digest does not bind `paymentFlow`. The verifier and the official server compare the accepted terms with the trusted quote, so the field cannot be stripped to downgrade the flow.

### External signing

- During external signing, the mechanism holds one database connection and a row lock. Use short signing deadlines and size the pool accordingly.
- If the process crashes after signing but before the save commits, a later attempt may invoke the signer again. This library never broadcasts uncommitted bytes.

> [!WARNING]
> **Exactly-once HSM invocation is not guaranteed.**

## Durable settlement journal

The journal is the PostgreSQL table behind the facilitator. It ensures that each authorization has at most one signed transaction and one broadcast, and that every recovery reuses that original transaction.

### Set up

- `exact/postgres.New` accepts a caller-managed `pgxpool.Pool` and a trusted `exact.Config`.
- Apply [schema.sql](../exact/postgres/schema.sql) explicitly under a dedicated, trusted `search_path`. The same SQL is available as `SchemaSQL`.
- Construction never runs DDL.
- Other packages do not need a database. The journal does not depend on any payment gateway or merchant ledger.

### Upgrade an existing journal

> [!WARNING]
> **Existing journals must be upgraded before the new version starts.** Reapplying `SchemaSQL` alone does not upgrade an existing table.

1. Back up the database and stop all old writers.
2. Run [upgrade_confirmation.sql](../exact/postgres/upgrade_confirmation.sql) explicitly in the same trusted `search_path`. The same SQL is available as `UpgradeConfirmationSQL`.
3. Start the new version.

The script runs in a transaction and adds the confirmation columns and constraints. It is repeatable and preserves historical transactions and states. If it fails, roll back any open transaction.

Runtime accounts need no DDL privileges. Do not run old binaries that do not understand the success state alongside new ones. Do not roll back by dropping columns, clearing confirmations or downgrading states.

### States

```
reserved ──SaveTransaction──▶ prepared ──BeginBroadcast──▶ broadcast_unknown ──RecordAccepted──▶ pending
                                                                  │                                 │
                                                                  └────────ConfirmSettlement────────┴──▶ succeeded
```

| State | Meaning |
| --- | --- |
| `reserved` | The authorization is reserved. No transaction is saved yet. |
| `prepared` | The signed transaction and its ID are saved. Nothing has been broadcast. |
| `broadcast_unknown` | Broadcast permission was granted. The transaction may or may not have reached a node. |
| `pending` | A node accepted the transaction. This is not success. |
| `succeeded` | The solidified transaction and the exact payment were verified by a trusted node and persisted. |

Once a record reaches `broadcast_unknown`, `pending` or `succeeded`, the transaction is never sent again.

### Store operations

**`Reserve`** verifies the authorization offline, then reserves the combination of network, Permit2, payer and canonical nonce.

- An identical authorization returns the original record.
- A different digest for the same key conflicts.
- Different proxies share the same Permit2 nonce namespace.

**`SaveTransaction`** atomically persists a single transaction ID and signed payload.

- Under concurrent writes, only one distinct result wins. Identical writes are idempotent.
- Saved data cannot be replaced.
- The same network and txid cannot be assigned to another authorization.

**`BeginBroadcast`** commits `broadcast_unknown` **before** it returns the sole broadcast permission and the persisted transaction.

- Never broadcast unsaved local data.
- Never broadcast unless this call succeeded.

**`RecordAccepted`** records node acceptance as `pending`. Acceptance alone never marks success.

- If the record is already `succeeded`, a matching txid and digest keep the success state and confirmation timestamps.
- After a timeout, process exit, missing transaction or ambiguous database commit, call `Get` and track the original txid. Never re-sign, replace or create a new payment.
- A crash before the actual submission can also leave a record in `broadcast_unknown`. Automatic rebroadcast and reservation release are not implemented.

**`Submit(ctx, id, prepared, executionClient, now)`** is the preferred way to broadcast. It loads the original bytes, validates them, moves the record to `broadcast_unknown` with a compare-and-swap, broadcasts only that durable snapshot and records acceptance.

- Concurrent or repeated calls get only one permission.
- A record in `broadcast_unknown`, `pending` or `succeeded` is never sent again.
- If the node accepted the transaction but the acknowledgement could not be persisted, the record stays `broadcast_unknown` with the original txid.
- If the SQL commit itself is ambiguous, call `Get`. Never assume the permission is still unused.

**`Track(ctx, id, executionClient)`** performs read-only recovery using the txid in the journal.

- It does not need the expired authorization or a `Prepare` object.
- It never marks success or rebroadcasts based on a lookup.
- Bound the whole database and network operation with the caller's context.
- No background executor, polling or queue is started.

**`store.CheckSettlement(ctx, id, executorAddress, executionClient, now)`** audits a record without changing it.

- It recovers the prepared binding from the original record in `broadcast_unknown`, `pending` or `succeeded`.
- It checks the trusted contracts, the executor and the original authorization identity, then verifies the solidified transaction and the exact payment.
- No in-memory prepared object is needed, even after expiry.
- It never changes state or timestamps and never broadcasts.
- Invalid or mismatched legacy bytes fail before any node read.
- If a later audit finds missing evidence or fails, durable success is not revoked automatically.

**`store.ConfirmSettlement(ctx, id, executorAddress, executionClient, now)`** is the only way to record success.

- It performs the same check as `CheckSettlement`, then atomically moves the record from `broadcast_unknown` or `pending` to `succeeded`. The compare-and-swap is keyed on the original authorization, txid and signed bytes.
- `Record.Confirmation` stores the block height, block ID and database confirmation time.
- There is no public setter that accepts a success flag or a caller-provided receipt.
- Concurrent matching results are idempotent. A conflicting block never overwrites an existing confirmation.
- Unconfirmed evidence returns `ErrSettlementUnconfirmed`.
- After any error, including a lost commit response, call `Get` before claiming success. An error never permits a new payment.

For a record that is already `succeeded`:

- `ConfirmSettlement` restores and checks the original binding and trusted configuration, then returns the persisted result. It does not read from nodes or change timestamps. Use `CheckSettlement` explicitly for a fresh node audit.
- Re-saving identical bytes and late acceptance callbacks keep the success state.
- `BeginBroadcast` and `Submit` never grant another permission.

Bound the SQL, recovery, RPC and commit steps with one caller context.

### Guarantees and limits

- These guarantees apply only to cooperating instances that share the same trusted database and follow this workflow.
- A reservation does not lock the on-chain nonce or funds.
- `SaveTransaction` treats the bytes as opaque. Use the trusted builder to bind the ID, signatures, authorization and expiry. Never accept arbitrary transaction bytes from clients.
- `succeeded` stores a verification result from a trusted node. It is not an independent chain proof. Write access to the database remains a trust boundary.
- Contract preflight, account-permission checks, fee-resource checks and risk gates are still required.

> [!WARNING]
> **A successful record is not bound to a URL or business request, and it does not authorize repeated delivery.** Neither exactly-once on-chain payment nor exactly-once delivery is guaranteed. HTTP authentication, linking business requests to quotes, and fulfillment idempotency are the application's responsibility.

### Protect the journal

- Signed payloads can be broadcast as they are. Use least-privilege database access, encryption and backups.
- `Transaction.Payload` is excluded from JSON. Never log it manually.
- Runtime accounts should not have `DELETE` or `TRUNCATE` privileges.
- Do not purge expired reservations automatically, and do not delete history when rolling back code.
- The temporary localhost database in CI uses trust authentication. It is **not a deployment configuration**.

## Operator responsibilities

- Per-payment caps and fee caps are not aggregate sponsorship budgets.
- Public endpoints still need authentication, rate limits and spending budgets.
- Verify deployments, token semantics, gas resources and account permissions independently.
