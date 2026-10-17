# x402-tron-go

[简体中文](README.md) | English

An independent TRON adapter SDK for x402: clients/agents, API resource servers and facilitators should be able to integrate without implementing TRON protocol details or depending on our payment gateway. Currently provides official client/resource-server mechanisms, HTTP examples, authorization/preflight, constrained transaction construction, broadcast/lookup, read-only solidified payment-event checks, verification recovery after restart and an optional durable journal. **Durable success states, a complete facilitator and a real testnet round trip are not complete. Do not use with real funds.** Not affiliated with the x402 Foundation or TRON.

## Integration status

| Role | Status |
| --- | --- |
| Client / agent | Implements official `SchemeNetworkClient`; configure signing/payment policy and use the official HTTP client for 402 retries |
| API resource server | Implements official `SchemeNetworkServer`, Permit2 quotes and an `upfront` middleware example; requires a trusted facilitator, not just offline verification |
| Self-hosted facilitator | Construction/broadcast/journal, read-only solidified payment checks and restart recovery exist; success states and official mechanism integration pending |

Ordinary clients do not need PostgreSQL or gateway merchant/ledger modules. PostgreSQL is an optional durable storage implementation for self-hosted facilitators. Trusted contracts, asset/recipient policy, wallet signing, gas and necessary Permit2 token approval remain prerequisites; no implicit approval or on-chain action.

## Client quick start

Requires Go 1.26+. No stable release yet. From a clone:

```bash
go run ./examples/client
```

Uses a random ephemeral in-process key and a loopback fixture with the real official SDK: `402 → TRON authorization → PAYMENT-SIGNATURE retry → 202 authorization verified, not settled`. No blockchain, existing wallet, transaction or successful-settlement response. Public fixture addresses are not trusted deployment configurations. This is not a complete on-chain round trip.

Register the mechanism in an application without building authorization fields, nonces or TIP-712 digests yourself:

```go
import (
    "net/http"
    "time"

    "github.com/eightyun/x402-tron-go/exact"
    x402 "github.com/x402-foundation/x402/go/v2"
    x402http "github.com/x402-foundation/x402/go/v2/http"
)

func paymentHTTPClient(contracts exact.Config, signer *exact.EOASigner, asset, payTo, maxAmount string) (*http.Client, error) {
    mechanism, err := exact.NewClient(exact.ClientConfig{
        Authorization: contracts, PayerAddress: signer.Address(),
        Asset: asset, PayTo: payTo, MaxAmount: maxAmount,
        Signer: signer.SignAuthorization,
    })
    if err != nil {
        return nil, err
    }
    network := x402.Network(mechanism.Network())
    core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{
        AllowedAssets: []x402.SpendControlAsset{{Network: network, Asset: asset, MaxAmountPerPayment: maxAmount}},
    })).Register(network, mechanism)
    return x402http.WrapHTTPClientWithPayment(&http.Client{
        Timeout: 15 * time.Second,
        CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
    }, x402http.Newx402HTTPClient(core)), nil
}
```

Optional `exact.NewEOASigner(keyBytes)` copies a strictly valid nonzero 32-byte private key into memory. Manage the original input and call `Close()` when finished. No file/env loading or key export; common formatting and JSON do not expose internal scalars. Best-effort memory clearing is not secure hardware. Production applications can supply an external `ClientConfig.Signer`: sign the **already computed TIP-712 digest**, without rehashing, personal-sign prefixes or confusing it with transaction SHA-256 signing.

Before signing, the client checks configured network, asset, recipient, per-payment `MaxAmount` and the official SDK's supplied atomic cap. Unknown extra fields/nonempty extensions are rejected. `Lifetime` defaults to 60 seconds, accepts whole seconds from 6 seconds to 10 minutes, and is bounded by the quote timeout. Signature and remaining validity are rechecked after signing. External signers must support concurrency and bounded contexts; their raw errors are redacted. No balance/allowance reads. Never derive trusted policy from an untrusted 402 quote.

**Per-payment caps are not aggregate budgets.** Each payload creation issues a new spendable authorization; do not automatically repay unknown outcomes. Authorizations do not bind URLs/request bodies. Restrict trusted origins and budgets, reject unsafe redirects and avoid recovery hooks issuing additional authorizations. Neither HTTP 2xx nor offline verification proves on-chain payment.

## API resource-server integration

Register `exact.NewServer(exact.ServerConfig{Network: network, Asset: asset, PayTo: payTo})` with the official resource server. One trusted network/asset/recipient; no database or wallet key. `ParsePrice` accepts canonical positive atomic-unit strings (`"1000"`), official `x402.AssetAmount` values or strict equivalent maps. **No `$0.01`, floating-point prices or implicit exchange rates.**

Only the official `upfront` flow is enabled. Quotes include `assetTransferMethod: permit2` and `paymentFlow: upfront`; handlers run only after a trusted facilitator reports successful settlement. Pending, unknown, rejected, failed or timed-out results must not be reported as successful. Upfront does not first call `/verify`: **`/settle` must perform complete authorization/preflight, durable submission and solidified execution/received-token verification itself**. Broadcast acceptance is not success.

Startup requires `/supported` to advertise matching v2/exact/canonical network and `permit2` in `extra.supportedAssetTransferMethods`. This existing TRON ecosystem capability field is not copied to payment quotes and is not contract or settlement proof. Independently verify matching client/facilitator contract deployments.

The runnable [server example](examples/server/main.go) uses official net/http middleware. Set the following environment variables, then run `go run ./examples/server`:

| Variable | Meaning |
| --- | --- |
| `X402_NETWORK` | Defaults to Nile: `tron:3448148188` |
| `X402_ASSET` | Required trusted token address |
| `X402_PAY_TO` | Required merchant recipient |
| `X402_PRICE_ATOMIC` | Required positive atomic-unit price |
| `X402_FACILITATOR_URL` | Required trusted HTTPS base URL; no userinfo/query/fragment |
| `X402_LISTEN_ADDR` | Defaults to `127.0.0.1:8080` |

Explicit initialization and quote validation happen before listening; failures stop startup. `GET /resource` returns 402 without payment. HEAD is rejected to prevent Go GET-route HEAD matching from bypassing payment. Includes bounded startup/settlement/HTTP timeouts, no upstream redirects, bounded headers and graceful shutdown. External deployment needs TLS ingress and appropriate authentication. Use official `AuthProvider` for facilitator credentials, never URL tokens.

**This repository does not yet provide a complete facilitator for real payments.** Configure a separately verified service; no public facilitator is assumed and no real remote deployment was accepted. Successful facilitators used in automated tests exist only in test code and demonstrate control flow, not blockchain settlement.

Upfront payment is not automatically refunded if the handler later fails; do not settle again. Applications need fulfillment idempotency, lookup and compensation. New upfront quotes require the updated client; existing no-paymentFlow quotes/signatures remain compatible. The digest does not bind paymentFlow; the verifier and official server compare accepted terms with the trusted quote to prevent stripping/downgrading it.

## Available utilities

- `NormalizeNetwork` and `ChainID`: Mainnet, Nile, Shasta; known legacy hexadecimal aliases normalize to decimal CAIP-2 identifiers.
- `NormalizeAddressHex` and `NormalizeAddressBase58`: validate and convert TRON addresses.
- `AddressForSigning`: explicitly convert a valid TRON address to a 20-byte `0x` address for TIP-712.

The root package uses only the standard library. The `exact` package uses official x402 Go SDK v2.28.0 types and established secp256k1/Keccak libraries. Offline verification makes no RPC calls; optional preflight performs read-only JSON-RPC. Neither stores keys, approves tokens, consumes nonces or broadcasts transactions. Recognizing a network does not mean settlement is supported. Address format cannot distinguish TRON networks or establish account activation, safety, or balance.

## Offline exact Permit2 verification

`exact.NewOfflineVerifier` requires a trusted network, Permit2 contract and exact proxy. No default mainnet configuration or client-selected contracts. Deployment owners must independently validate contract code and domain/proxy bindings.

`Digest` computes the TIP-712 digest for external signing. `VerifyAuthorization` consumes official SDK `types.PaymentPayload`, trusted server-side `types.PaymentRequirements` and server time. It binds the quote, network, token, amount, recipient, proxy, nonce and validity, then recovers the EOA signer.

Current subset: required `extra.assetTransferMethod: "permit2"`, optional `paymentFlow: "upfront"`, no other fields/flows or nonempty extensions. Accepted and trusted quote flows must match. Two-field witness (`to`, `validAfter`); strict TRON addresses in configuration/requirements and 20-byte `0x` addresses in authorizations; canonical decimal uint256 amounts/times, decimal or 32-byte hex nonces. Deadline must have at least six seconds remaining, bounded by the quote timeout. Only 65-byte low-s EOA signatures with v=0/1 or 27/28; no contract-wallet signatures.

**Valid authorization is not proof of funds or settlement.** No balance, allowance, nonce bitmap or chain-state checks; no nonce consumption or concurrency/replay protection. Repeated verification can succeed. **Do not deliver paid resources from this result.** Chain preflight, durable idempotency, settlement and final outcome verification remain necessary.

The authorization does not bind a specific HTTP URL or request body. Resource servers must also prevent duplicate fulfillment using the same payment.

The public TronWeb 6.3.0 vector verifies digest/signature compatibility offline. Its ephemeral random test key was discarded; no live wallet or blockchain was used. This is not full client/facilitator interoperability acceptance.

## Read-only chain preflight

`exact.NewPreflightVerifier` accepts `PreflightConfig` with trusted `Authorization` settings, `NodeURL`, `Permit2CodeHash` and `ProxyCodeHash`. Hashes must be independently verified runtime-bytecode Keccak-256 values (`0x` + 32 bytes), not learned from the node on first use. Token addresses come from trusted server-side requirements.

`Check(ctx, payload, requirements, now)` verifies authorization, chain ID and a fresh latest head; pins Permit2/proxy code; checks proxy `PERMIT2()`/`WITNESS_TYPEHASH()` and Permit2 `DOMAIN_SEPARATOR()`; requires token code; reads balance, Permit2 allowance and the `nonceBitmap(payer, nonce >> 8)` bit; then rechecks chain/head and authorization validity including elapsed RPC time.

Read failures, insufficient funds/allowance, used nonce or mismatched contracts/network/heads fail closed. A successful `PreflightResult` is **not a settlement proof or a reservation of balance/nonce**. The configured RPC and independently verified code hashes remain trust boundaries; no malicious-node state proofs are provided.

A normal round uses 13 sequential reads. Total timeout defaults to 15 seconds, at most one minute; no automatic retries. Heads may be at most 60 seconds old or five seconds ahead of server time. Slow nodes may conservatively fail due to head changes; callers can retry preflight within their budget, never infer permission to start another transaction.

TRON [eth_call](https://developers.tron.network/reference/eth_call) and [eth_getCode](https://developers.tron.network/reference/eth_getcode) read latest state, not historical state selected by block references. Matching before/after heads is a drift check, **not an atomic snapshot or finality guarantee**. Revalidate before settlement and verify actual execution outcomes.

HTTPS is required by default; redirects are blocked. API keys use only the `TRON-PRO-API-KEY` header. URLs cannot contain credentials, query or fragment. An optional `HTTPClient` supports trusted CA/proxy settings without in-place mutation. Explicit `AllowInsecureLocalHTTP` permits only loopback/localhost development nodes. Never derive node configuration from payment requests.

## Constrained settlement construction and external signing

`exact.NewSettlementBuilder` requires trusted `Authorization`, an explicit gas-paying `ExecutorAddress`, positive `FeeLimitSUN`, and a whole-millisecond `Lifetime` greater than 15 seconds and at most 10 minutes. No default mainnet/contracts, private-key custody or arbitrary node-built transaction JSON.

`Build` validates payment authorization and uses a trusted `ReferenceBlock` plus server time to locally construct one exact-proxy `settle` call, TAPOS and TRON raw Protobuf. The transaction ID is `SHA-256(raw_data)`. Reference network, height and ID must match; block time may be at most 60 seconds old or five seconds ahead. This does not prove chain membership, finality or honest RPC responses: deployment and chain preflight remain required. Expiry is capped by the payment deadline; construction and both sides of signing require at least 15 seconds remaining.

The returned `PreparedSettlement` is immutable. `RawData()` returns a copy containing the payment authorization signature: keep it confidential. `Sign(ctx, DigestSigner, now)` sends only the fixed SHA-256 digest to the caller's external signer, then recovers the gas payer's public key. Elapsed signing time counts against validity; original signer error messages are not exposed. Signatures are low-s, 65-byte `r||s||v`; accept v=0/1 or 27/28 and emit canonical v=0/1. The payer's TIP-712 authorization and the executor's transaction signature are distinct; their digests are not interchangeable.

`SettlementTransaction.Payload` is **complete binary Transaction Protobuf, not JSON**, suitable for a later hex-encoded [broadcasthex](https://developers.tron.network/reference/broadcasthex) request. `ValidateSigned` accepts only this prepared object's canonical encoding and checks all raw bytes, ID and signature. Unknown/duplicate fields, attached TRX/TRC10, extra contracts/signatures or changed fee policy are rejected. This is not a generic transaction importer, current-expiry check or receipt proof.

Only a single EOA using default owner permission is supported; no multisig, custom permissions or contract wallets. On-chain permissions, fee resources and execution outcomes still require verification. No key loader or successful settlement state yet. Persist the unique signed transaction and obtain journal broadcast permission before submission; never extend or rebuild a payment during recovery. The public [TronWeb settlement vector](exact/testdata/tronweb-settlement.json) uses a synthetic block and a discarded random executor key: encoding/signature interoperability only, not live testnet acceptance.

## Single-attempt broadcast and original-transaction lookup

`exact.NewExecutionClient` requires trusted `ExecutionConfig`: fixed `Network`, native FullNode root `NodeURL`, and an independently verified `GenesisBlockID` (64 hex digits without `0x`, zero first eight bytes, last four bytes matching the TRON chain ID). Never select nodes from payment requests or learn genesis identity on first use. Synthetic test IDs are not deployment configuration. HTTPS is required except explicitly enabled loopback HTTP. No path, query, fragment or userinfo in the URL. Total timeout defaults to 15 seconds, at most one minute; responses are capped at 1 MiB. Redirects are blocked; errors exclude URLs, API keys and original node messages. Custom HTTP transports are trusted configuration and must not retry writes internally.

- Low-level `Broadcast(ctx, prepared, transaction, now)` requires prior durable journal permission. It validates bytes/signature/network/remaining lifetime, pins block zero from **the same native node**, makes one `/wallet/broadcasthex` request, then rechecks network identity. Only explicit acceptance with a successful (or omitted) code and matching original txid acknowledges this attempt, not settlement. Network-check time counts against pre-send validity.
- `Lookup(ctx, txid)` pins genesis before/after `/wallet/gettransactionbyid` and validates txID against `SHA-256(raw_data_hex)`. `TransactionObservation.Found` is **not execution success, finality or receipt of funds**. Only an empty object means currently not found. Timeouts, ambiguous/duplicate top-level JSON keys, bad hashes and network mismatches return errors. Neither absence nor rejection authorizes reservation release, re-signing or replacement.

Only simulated-node tests have exercised these HTTP requests: no real Nile broadcast yet. Node acceptance, a FullNode body, execution and solidified payment events are distinct evidence layers. Neither method declares success; use the read-only checks below. No malicious-node state proofs are provided.

## Solidified original-transaction and payment-event checks

`executionClient.CheckSettlement(ctx, prepared, transaction, time.Now())` uses the **original prepared object and signed transaction**. No rebuilding expired authorization, broadcasting or journal/ledger writes. Expired transactions/authorizations can be checked historically; `now` must be trusted server time.

The configured endpoint must expose `/walletsolidity`. Within one bounded budget, the checker:

1. Pins the solidity-view genesis ID and reads a fresh solidified head.
2. Reads the solidified body/receipt; matches original raw bytes, txid/SHA-256, single-contract execution result and receipt identity.
3. Checks receipt block height/time, the initial solidified-head boundary and exactly one original transaction in the block. Same-height head/block IDs must agree.
4. Requires exactly one standard-token Transfer from payer to recipient for the exact amount. Rejects partial/excess/duplicate payments, fees or other same-token transfers involving either party; no summing or treating gross flow as net receipt.
5. Rechecks non-regressing heads, unchanged original block ID/time/transaction membership and network identity, then returns `SettlementObservation.Confirmed=true` with block ID/height.

Missing solidified bodies/receipts, or a receipt just beyond the initial head, produce `Confirmed=false`, not terminal failure. Non-successful execution reports, inconsistent evidence, network failures, timeout or invalid critical fields return errors, **never Confirmed=true or permission to release reservations, re-sign or rebroadcast**.

A successful check normally makes nine sequential read-only requests, without retries. Existing 15-second default total timeout (at most one minute), 1 MiB response limit, TLS/redirect restrictions and error redaction apply. Critical nested JSON rejects duplicate keys. Native integer fields reject strings/floats/overflow. Heads may lag trusted time by two minutes or lead by five seconds; total elapsed time also counts against initial-snapshot freshness. Slow nodes, long chain stalls or oversized blocks may conservatively fail; only retry read-only checks within a caller budget.

**Limits:** trusted standard TRC20 tokens only; no self-transfers, fee-on-transfer, rebasing or custom event semantics. Trusted node and independently verified contract/token deployments remain assumptions. No complete chain, Merkle or malicious-node state verification; event semantics depend on the trusted token implementation. See [TRON confirmation semantics](https://developers.tron.network/docs/confirmation-semantics) and [event encoding](https://developers.tron.network/docs/event).

After restart, `exact.RestoreSettlement(config, executorAddress, originalTransaction)` recovers a `PreparedSettlement` solely from the persisted original signed transaction. Configuration and fee-paying executor must be trusted deployment inputs. It strictly decodes this package's fixed Protobuf/ABI, checks byte-for-byte canonical re-encoding, verifies the payer's TIP-712 signature, executor's transaction signature and original txid. Authorization fields come from the original call, not an auxiliary snapshot or current quote. No `Build`, forged current time for expired authorization, validity extension, signing or broadcasting. TAPOS bytes are not a complete reference-block proof; recovery does not prove the original quote, preflight or successful payment.

Compare `prepared.Authorization()` with the original trusted journal identity before calling the read-only solidified checker. Only the package's fixed single-contract, single-EOA-signature canonical encoding is accepted; other valid TRON transactions may be rejected. This is not a generic transaction importer. Durable success states and a complete facilitator remain pending; recovery and simulated tests are not deployment acceptance.

## Durable settlement journal (optional PostgreSQL)

`exact/postgres.New` accepts a caller-managed `pgxpool.Pool` and trusted `exact.Config`. Explicitly apply [schema.sql](exact/postgres/schema.sql), also available as `SchemaSQL`, under a dedicated trusted `search_path`; construction never runs DDL. Other packages do not require a database. No dependency on the payment gateway or merchant ledger.

- `Reserve` verifies authorization offline, then reserves network + Permit2 + payer + canonical nonce. Identical authorizations return the original record; different digests conflict. Different proxies share the same Permit2 nonce namespace.
- `SaveTransaction` atomically persists a single transaction ID and signed payload. Only one distinct result wins concurrent writes; identical writes are idempotent. Saved data cannot be replaced or the same network/txid assigned to another authorization.
- `BeginBroadcast` commits `broadcast_unknown` **before** returning the sole broadcast permission and persisted transaction. Never broadcast unsaved local data or proceed without this call succeeding.
- `RecordAccepted` records node acceptance as `pending`, not success. Timeout, process exit, missing transaction or an ambiguous database commit requires `Get` and tracking the original txid, never re-signing, replacement or automatic new payment. A crash before actual submission may also leave unknown; automatic rebroadcast or reservation release is not implemented.
- Prefer `Submit(ctx, id, prepared, executionClient, now)` to load original bytes, validate, CAS to unknown, broadcast only that durable snapshot and persist acceptance. Concurrent/repeated submissions get only one permission; unknown/pending never sends again. An accepted request whose ACK cannot be persisted remains unknown with the original txid. If the SQL commit itself is ambiguous, use `Get`; never assume permission remains unused.
- `Track(ctx, id, executionClient)` performs read-only recovery using the journal txid without recreating expired authorization or a Prepare object. It never marks success or rebroadcasts based on a lookup. Bound the complete database/network operation with a caller context. No background executor, polling or queue is started.
- `store.CheckSettlement(ctx, id, executorAddress, executionClient, now)` recovers the prepared binding from an unknown/pending original record, checks trusted contracts/executor and the original authorization identity, then verifies the solidified transaction and exact payment. No in-memory prepared object is needed, even after expiry. It never changes state/timestamps or broadcasts; invalid/mismatched legacy bytes fail before node reads. Bound all SQL, parsing and RPC work with a caller context.

These guarantees apply only to cooperating instances sharing the same journal. Reservation does not lock on-chain nonce/funds. `SaveTransaction` itself still treats bytes as opaque: use the trusted builder to bind ID, signatures, authorization and expiry; never accept arbitrary client transaction bytes. Recovery revalidates these bindings, but success states are not yet persisted. **No current journal state permits resource delivery; this is not an exactly-once on-chain payment guarantee.** Contract preflight, account-permission, fee-resource and risk gates remain necessary.

Signed payloads can be broadcast: use least-privilege database access, encryption and backups. `Transaction.Payload` is excluded from JSON but must never be logged manually. Runtime accounts should lack DELETE/TRUNCATE privileges; do not automatically purge expired reservations or delete history when rolling back code. CI's temporary localhost database uses trust authentication and is **not a deployment configuration**.

## Development

Requires Go 1.26+. CI uses 1.26.x. No release exists; public APIs may change.

```bash
go mod verify
go vet . ./exact ./exact/postgres ./examples/client ./examples/server
go test -race -count=1 -cover . ./exact ./exact/postgres ./examples/client ./examples/server
go build . ./exact ./exact/postgres ./examples/client ./examples/server
```

Inputs are strict: no whitespace trimming, unknown networks, wildcards, or decimal leading zeros. Namespace must be `tron`; legacy hexadecimal references are case-insensitive. Addresses must be 34-character Base58Check or 42-character `41`-prefixed hex. Arbitrary `0x` addresses are not accepted as TRON input. Formatting is not authorization verification.

Database tests require `X402_TEST_DATABASE_URL` pointing to a dedicated PostgreSQL database with permission to create temporary schemas. Each test creates and cleans only its own random schema. Without this variable integration tests explicitly skip; this is not a passing database acceptance run. CI provisions PostgreSQL 16 and runs them.

Tests cover public TronWeb vectors, tampering, amount/fee/time boundaries, simulated nodes and PostgreSQL concurrency/recovery. Solidified checks add historical expiry, original-byte/block membership, execution states, changed heads/blocks, exact/duplicate/reversed events, nested duplicate JSON, network/timeouts/concurrency. Recovery covers restricted decoding, both signatures, tampering and original-transaction verification through reopened database connections. Receipts/blocks are synthetic; no live-chain acceptance is claimed. Next: success states, a complete facilitator and a Nile round trip before gateway integration.

## References and status

- [TRON address specification](https://developers.tron.network/docs/account)
- [Official x402 Go SDK](https://github.com/x402-foundation/x402/tree/main/go)
- [TRON ecosystem reference](https://github.com/BofAI/x402)

[Apache-2.0 licensed](LICENSE). See [third-party notices](THIRD_PARTY_NOTICES.md). No stable release or independent security audit yet.
