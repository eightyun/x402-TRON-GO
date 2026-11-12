# Self-hosted facilitator

[简体中文](facilitator.zh-CN.md) | English

Running the durable PostgreSQL-backed facilitator: embedding it in-process, exposing it over HTTP, the testnet payer example, the settlement journal and the official mechanism contract.

## Deployment options

### Embed the library in your application first

`postgres.NewFacilitator(store, config)` implements the official mechanism. Pass it to `postgres.NewLocalClient(mechanism, 8)`, then supply the result to official `x402.WithFacilitatorClient(client)` inside your resource server. No facilitator URL, Bearer token, listener or separate service is required. Create the store with `postgres.New(pool, config.Preflight.Authorization)`; the application manages the pool and transaction signer, and an administrator explicitly applies the schema beforehand. The adapter reuses the official core and existing settlement/recovery rather than duplicating protocol or weakening checks.

The local entry accepts v2 with a 64KiB combined-input bound, rejecting duplicate/ambiguous keys, unknown structural fields and trailing JSON. `maxConcurrent=0` defaults to eight; 1–64 is accepted. Full capacity returns `ErrLocalClientCapacity` without an unbounded queue. This limit is per client, not aggregate spending or application authentication. Cancellation/deadlines preserve context semantics. Failed/pending payments do not permit delivery; recovery uses only the original authorization and the same durable journal. Requests must not select nodes, signers or trusted contracts. For an existing separate facilitator, continue using the official `HTTPFacilitatorClient`.

The HTTP executables below are optional examples, not prerequisites for importing the SDK.

### Real payment client and identical-request recovery

The [payer example](../examples/payer/main.go) is an explicitly enabled Linux/macOS testnet client, separate from the offline-handshake-only `examples/client`. Use [.env.example](../.env.example) to configure trusted network/Permit2/proxy, token/recipient, `X402_MAX_AMOUNT_ATOMIC`, `X402_PAYER_ADDRESS`, `X402_PAYER_KEY_FILE` and resource URL. The template is not loaded automatically. Never commit actual credentials, keys or authorizations.

Pre-create a service-user-owned **0700 state directory under privately controlled parents**, set its absolute path as `X402_PAYMENT_STATE_DIR`, then explicitly set `X402_ENABLE_TESTNET_PAYMENTS=true`:

```bash
go run ./examples/payer pay
# After disconnect, failure, pending or interruption, reuse the same directory/config; never pay again.
go run ./examples/payer recover
```

Only GET to the fixed URL on Nile/Shasta is supported. HTTPS is the default. Plaintext requires explicit `X402_ALLOW_INSECURE_LOCAL_RESOURCE_HTTP=true` and a literal loopback IP. Mainnet, URL credentials/query/fragment and redirects are rejected. Keys must be service-user-owned 0400/0600 regular files. Recovery never loads the key.

`pay` exclusively persists an intent before the official client signs one authorization from the 402 quote, then saves the original `PAYMENT-SIGNATURE` before transmission. Existing intents reject another pay. `recover` checks the original configuration/signature binding and sends that same header without generating a nonce; it can complete a previously unsubmitted authorization and **is not a read-only lookup**. Private files retain each attempt and successful resource content; stdout contains restricted metadata only. HTTP success must match network, payer, amount and a nonempty txid, but relies on the trusted service rather than independent chain proof.

Never clear the state directory to retry, or replace an unknown payment using another directory. An intent without a saved header requires manual investigation of whether authorization was signed/sent, not automatic re-signing. The example does not fund wallets, approve tokens, automatically retry, enforce cumulative gas budgets or deduplicate business fulfillment. Replayed resource GETs may execute the handler again.

`postgres.NewHTTPHandler(mechanism, postgres.HTTPConfig{BearerToken: token, MaxConcurrent: 8})` registers the official SDK core and exposes `GET /supported`, `POST /verify`, `POST /settle`. All endpoints require a 32–256-byte non-whitespace ASCII Bearer token. JSON is limited to 64 KiB; duplicate/ambiguous keys, unknown envelope fields, trailing values and non-v2 requests are rejected. Concurrent verification/settlement defaults to 8 (configurable 1–64). Excess requests receive 503 without signing/broadcasting. This is a per-process concurrency limit, not an aggregate expense budget.

The [runnable example](../examples/facilitator/main.go) targets Linux/macOS, allows only Nile/Shasta and literal loopback listeners, and **rejects mainnet/public binding**. Configure these variables and run `go run ./examples/facilitator`:

| Variable | Meaning |
| --- | --- |
| `X402_NETWORK` | Defaults to Nile; Shasta also allowed |
| `X402_DATABASE_URL` | Required isolated journal database; trusted `search_path`, least-privilege runtime role, TLS for remote connections |
| `X402_EXECUTOR_KEY_FILE` | Required absolute path; service-user-owned 0400/0600 regular file containing 64 hex characters, optionally one newline; final-component symlinks rejected |
| `X402_FACILITATOR_TOKEN` | Required, matching the resource server; inject through deployment environment, never source, URL or logs |
| `X402_PERMIT2_ADDRESS` / `X402_PROXY_ADDRESS` | Required independently verified deployments; no default addresses |
| `X402_PERMIT2_CODE_HASH` / `X402_PROXY_CODE_HASH` | Required trusted runtime-bytecode Keccak-256 hashes, `0x` plus 32 bytes |
| `X402_JSON_RPC_URL` | Required trusted HTTPS JSON-RPC endpoint |
| `X402_RPC_BATCH` | Optional, defaults to `false`; only literal `true` batches independent reads. Requires node batch support; no fallback |
| `X402_NATIVE_NODE_URL` / `X402_GENESIS_BLOCK_ID` | Required HTTPS native-node root URL and independently verified full genesis block ID; wallet/walletsolidity routes required |
| `X402_NODE_API_KEY` | Optional service credential for these nodes, never request-supplied |
| `X402_ASSET` / `X402_PAY_TO` / `X402_MAX_AMOUNT_ATOMIC` | Required fixed token/recipient/per-payment cap; positive atomic-unit integer cap |
| `X402_FEE_LIMIT_SUN` | Required positive per-transaction fee cap, **not** an aggregate gas budget |
| `X402_SETTLEMENT_TIMEOUT` | Defaults to `90s`, accepts 1 second–3 minutes; original transaction lifetime is fixed at 60 seconds; already-submitted transactions remain recoverable |
| `X402_MAX_CONCURRENT` / `X402_FACILITATOR_LISTEN_ADDR` | Defaults to 8 / `127.0.0.1:8081` |

Administrators explicitly apply [schema.sql](../exact/postgres/schema.sql) using a separate migration role; existing tables need the upgrade described below. Do not put database URLs/passwords in command-line arguments. Runtime roles should lack DDL, DELETE and TRUNCATE privileges. Startup checks configuration, database connectivity and readability of current-version columns. It **does not create/migrate tables, exhaustively inspect constraints, check live deployments, sign, approve or submit payments**. This is not full deployment acceptance: verify the complete schema, executor default-owner permissions/TRX/energy, token semantics and payer Permit2 allowance independently.

For local resource-server integration set `X402_FACILITATOR_URL=http://127.0.0.1:8081`, the matching token and `X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP=true`, then run `go run ./examples/server`. Cross-machine deployments require trusted TLS ingress forwarding to loopback, ingress rate limits and aggregate expense budgets. The key stays in process memory and is cleared best-effort on shutdown, not HSM/MPC security. SDK integrations can supply an external `FacilitatorConfig.Signer`. The example loads only the explicitly configured key file, without discovering or modifying existing wallets.

Responses follow official formats. Unknown/unconfirmed results remain unsuccessful; HTTP failures preserve `settlement_pending` and the original txid for identical-payload recovery. Disconnect/503 is not proof of payment failure: never generate a replacement authorization. SIGTERM drains accepted requests; interrupted work recovers from the same journal, without automatic background executors or rebroadcast.

Upfront payment is not automatically refunded if the handler later fails; do not settle again. Applications need fulfillment idempotency, lookup and compensation. New upfront quotes require the updated client; existing no-paymentFlow quotes/signatures remain compatible. The digest does not bind paymentFlow; the verifier and official server compare accepted terms with the trusted quote to prevent stripping/downgrading it.

## Durable settlement journal

`exact/postgres.New` accepts a caller-managed `pgxpool.Pool` and trusted `exact.Config`. Explicitly apply [schema.sql](../exact/postgres/schema.sql), also available as `SchemaSQL`, under a dedicated trusted `search_path`; construction never runs DDL. Other packages do not require a database. No dependency on the payment gateway or merchant ledger.

**Existing journals must be upgraded first:** back up and stop old writers, explicitly run [upgrade_confirmation.sql](../exact/postgres/upgrade_confirmation.sql), also available as `UpgradeConfirmationSQL`, in the same trusted `search_path`, then start the new version. The transactional script adds confirmation columns/constraints, is repeatable and preserves historical transactions/states; roll back any open transaction after failure. Reapplying `SchemaSQL` alone does not upgrade an existing table. Runtime accounts need no DDL permissions. Do not mix old binaries unaware of success semantics or roll back by deleting columns, clearing confirmations or downgrading states.

- `Reserve` verifies authorization offline, then reserves network + Permit2 + payer + canonical nonce. Identical authorizations return the original record; different digests conflict. Different proxies share the same Permit2 nonce namespace.
- `SaveTransaction` atomically persists a single transaction ID and signed payload. Only one distinct result wins concurrent writes; identical writes are idempotent. Saved data cannot be replaced or the same network/txid assigned to another authorization.
- `BeginBroadcast` commits `broadcast_unknown` **before** returning the sole broadcast permission and persisted transaction. Never broadcast unsaved local data or proceed without this call succeeding.
- `RecordAccepted` records node acceptance as `pending`, never success merely from acceptance. If already `succeeded`, a matching txid/digest callback preserves success and confirmation timestamps. Timeout, process exit, missing transaction or an ambiguous database commit requires `Get` and tracking the original txid, never re-signing, replacement or automatic new payment. A crash before actual submission may also leave unknown; automatic rebroadcast or reservation release is not implemented.
- Prefer `Submit(ctx, id, prepared, executionClient, now)` to load original bytes, validate, CAS to unknown, broadcast only that durable snapshot and persist acceptance. Concurrent/repeated submissions get only one permission; unknown/pending/succeeded never sends again. An accepted request whose ACK cannot be persisted remains unknown with the original txid. If the SQL commit itself is ambiguous, use `Get`; never assume permission remains unused.
- `Track(ctx, id, executionClient)` performs read-only recovery using the journal txid without recreating expired authorization or a Prepare object. It never marks success or rebroadcasts based on a lookup. Bound the complete database/network operation with a caller context. No background executor, polling or queue is started.
- `store.CheckSettlement(ctx, id, executorAddress, executionClient, now)` recovers the prepared binding from an unknown/pending/succeeded original record, checks trusted contracts/executor and the original authorization identity, then verifies the solidified transaction and exact payment. No in-memory prepared object is needed, even after expiry. It never changes state/timestamps or broadcasts; invalid/mismatched legacy bytes fail before node reads. Missing evidence or errors in a later audit do not automatically revoke durable success.
- `store.ConfirmSettlement(ctx, id, executorAddress, executionClient, now)` internally performs that check, then atomically CASes the original authorization/txid/signed bytes from unknown/pending to `succeeded`. `Record.Confirmation` stores the block height, block ID and database confirmation time. There is no public setter accepting a success flag or caller-provided receipt. Concurrent matching results are idempotent; conflicting blocks never overwrite. Unconfirmed evidence returns `ErrSettlementUnconfirmed`. Any error, including a lost commit response, requires `Get` before claiming success; it never permits a new payment.
- For an already successful record, `ConfirmSettlement` first restores and checks the original binding/trusted configuration, then returns the persisted result without node reads or timestamp changes. Use `CheckSettlement` explicitly for a fresh node audit. Re-saving identical bytes and delayed acceptance callbacks preserve success; `BeginBroadcast`/`Submit` never grant another permission. Bound SQL, recovery, RPC and commit with one caller context.

These guarantees apply only to cooperating instances sharing the same trusted database. Reservation does not lock on-chain nonce/funds. `SaveTransaction` itself still treats bytes as opaque: use the trusted builder to bind ID, signatures, authorization and expiry; never accept arbitrary client transaction bytes. `succeeded` persists a trusted-node verification result, not an independent chain proof; database write access remains a trust boundary. **A successful record does not bind a URL/business request or authorize repeated resource delivery.** HTTP authentication, associating business requests with quotes and fulfillment idempotency remain application responsibilities; neither exactly-once on-chain payment nor exactly-once delivery is guaranteed. Contract preflight, account-permission, fee-resource and risk gates remain necessary.

Signed payloads can be broadcast: use least-privilege database access, encryption and backups. `Transaction.Payload` is excluded from JSON but must never be logged manually. Runtime accounts should lack DELETE/TRUNCATE privileges; do not automatically purge expired reservations or delete history when rolling back code. CI's temporary localhost database uses trust authentication and is **not a deployment configuration**.

## Official facilitator mechanism

Construct with `postgres.NewFacilitator(store, config)`, then register through `x402.Newx402Facilitator().Register([]x402.Network{x402.Network(mechanism.Network())}, mechanism)`. No custom TRON verification, transaction encoding or settlement state machine is needed. Construction does not connect, migrate, sign, broadcast or start HTTP/background services.

Supply trusted `FacilitatorConfig` values explicitly:

| Configuration | Meaning |
| --- | --- |
| `Preflight` | Authorization domain, JSON-RPC endpoint and independently verified Permit2/proxy runtime-code hashes |
| `Execution` | Same-network native-node root and pinned full genesis ID; wallet and walletsolidity APIs required |
| `Policy` / `MaxAmount` | Fixed network, single asset/recipient and per-payment atomic-unit cap; no request overrides |
| `ExecutorAddress` / `Signer` | Default-owner EOA gas payer and external digest signer; must honor context and never broadcast itself |
| `FeeLimitSUN` / `Lifetime` | Positive per-transaction fee limit and whole-millisecond lifetime greater than 15 seconds, at most 10 minutes |
| `Timeout` / `PollInterval` | Default 90-second total budget, at most 3 minutes; default 3-second confirmation polling, range 1–10 seconds |

- V2/exact/Permit2/upfront only, quote timeouts 6–600 seconds; new construction still requires at least 15 seconds remaining. Declares `supportedAssetTransferMethods: ["permit2"]` and the configured executor signer. All deployment/network settings must agree with Store. No dynamic multi-merchant routing, other schemes, extensions, multisig or contract wallets.
- `Verify` is a read-only fresh-payment check: no journal reservation, signing or broadcasting and no promise of later success.
- `Settle` does not depend on a preceding `Verify`. New payments pass signature/policy/time/on-chain checks, then reserve the journal. A row-locked transaction rechecks state, constructs, signs and commits the unique original transaction before broadcasting. Preflight, broadcast and finality waiting run outside that transaction; competing instances reconcile the winner rather than create another payment.
- Existing unknown/pending/succeeded authorizations match trusted requirements, signatures and durable identity, then recover the original transaction. They do not rerun current-validity admission or query already-consumed nonces. `VerifyAuthorizationBinding` is historical identification only, never a replacement for fresh `VerifyAuthorization`/`Preflight.Check` admission.
- Only successful `ConfirmSettlement` persistence permits the official `success=true` response with original txid/network/payer/amount. Missing solidified evidence is polled within the budget. Timeout, unknown broadcast or unavailable verification nodes return official `settlement_pending` with the original txid. Ambiguous persistence returns `settlement_recovery_required` and requires journal readback; neither outcome permits reservation release, re-signing saved transactions or another payment.
- The official resource server retries pending with an original txid **once using identical bytes**, not indefinitely. The mechanism owns bounded waiting. The paired HTTP examples align remote/middleware/write timeouts; proxies and callers must match the budget too. After interruption, recover with the same payment payload rather than authorizing a new payment.

External signing holds one database connection and a row lock: use short signing deadlines and size pools appropriately. A crash after signing but before save commit may cause a later signer invocation; uncommitted bytes were never broadcast by this library. **Exactly-once HSM invocation is not guaranteed.** Cooperating instances must share the journal and follow this workflow. Per-payment/fee caps are not aggregate sponsorship budgets. Public endpoints still require authentication, rate limits and expense budgets; independently verify deployments, token semantics, gas resources and account permissions.
