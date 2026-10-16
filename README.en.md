# x402-tron-go

[简体中文](README.md) | English

An independent Go project for TRON support in x402, providing network/address utilities, offline `exact` Permit2 authorization verification, read-only chain preflight and an optional durable settlement journal. **No complete client, facilitator, or on-chain settlement is available. Do not use with real funds.** Not affiliated with the x402 Foundation or TRON.

## Available utilities

- `NormalizeNetwork` and `ChainID`: Mainnet, Nile, Shasta; known legacy hexadecimal aliases normalize to decimal CAIP-2 identifiers.
- `NormalizeAddressHex` and `NormalizeAddressBase58`: validate and convert TRON addresses.
- `AddressForSigning`: explicitly convert a valid TRON address to a 20-byte `0x` address for TIP-712.

The root package uses only the standard library. The `exact` package uses official x402 Go SDK v2.28.0 types and established secp256k1/Keccak libraries. Offline verification makes no RPC calls; optional preflight performs read-only JSON-RPC. Neither stores keys, approves tokens, consumes nonces or broadcasts transactions. Recognizing a network does not mean settlement is supported. Address format cannot distinguish TRON networks or establish account activation, safety, or balance.

## Offline exact Permit2 verification

`exact.NewOfflineVerifier` requires a trusted network, Permit2 contract and exact proxy. No default mainnet configuration or client-selected contracts. Deployment owners must independently validate contract code and domain/proxy bindings.

`Digest` computes the TIP-712 digest for external signing. `VerifyAuthorization` consumes official SDK `types.PaymentPayload`, trusted server-side `types.PaymentRequirements` and server time. It binds the quote, network, token, amount, recipient, proxy, nonce and validity, then recovers the EOA signer.

Current subset: only `extra: {assetTransferMethod: "permit2"}`, no other extra fields or nonempty extensions; two-field witness (`to`, `validAfter`); strict TRON addresses in configuration/requirements and 20-byte `0x` addresses in authorizations; canonical decimal uint256 amounts/times, decimal or 32-byte hex nonces. Deadline must have at least six seconds remaining, bounded by the quote timeout. Only 65-byte low-s EOA signatures with v=0/1 or 27/28; no contract-wallet signatures.

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

## Durable settlement journal (optional PostgreSQL)

`exact/postgres.New` accepts a caller-managed `pgxpool.Pool` and trusted `exact.Config`. Explicitly apply [schema.sql](exact/postgres/schema.sql), also available as `SchemaSQL`, under a dedicated trusted `search_path`; construction never runs DDL. Other packages do not require a database. No dependency on the payment gateway or merchant ledger.

- `Reserve` verifies authorization offline, then reserves network + Permit2 + payer + canonical nonce. Identical authorizations return the original record; different digests conflict. Different proxies share the same Permit2 nonce namespace.
- `SaveTransaction` atomically persists a single transaction ID and signed payload. Only one distinct result wins concurrent writes; identical writes are idempotent. Saved data cannot be replaced or the same network/txid assigned to another authorization.
- `BeginBroadcast` commits `broadcast_unknown` **before** returning the sole broadcast permission and persisted transaction. Never broadcast unsaved local data or proceed without this call succeeding.
- `RecordAccepted` records node acceptance as `pending`, not success. Timeout, process exit, missing transaction or an ambiguous database commit requires `Get` and tracking the original txid, never re-signing, replacement or automatic new payment. A crash before actual submission may also leave unknown; automatic rebroadcast or reservation release is not implemented.

These guarantees apply only to cooperating instances sharing the same journal. Reservation does not lock on-chain nonce or funds. Transaction bytes are opaque: a trusted builder must validate ID, signatures, authorization binding and expiry. The journal does not establish these properties. Actual transaction construction, signing, broadcasting and finalized execution/receipt validation remain unimplemented. **No journal state permits resource delivery; this is not an exactly-once on-chain payment guarantee.**

Signed payloads can be broadcast: use least-privilege database access, encryption and backups. `Transaction.Payload` is excluded from JSON but must never be logged manually. Runtime accounts should lack DELETE/TRUNCATE privileges; do not automatically purge expired reservations or delete history when rolling back code. CI's temporary localhost database uses trust authentication and is **not a deployment configuration**.

## Development

Requires Go 1.26+. CI uses 1.26.x. No release exists; public APIs may change.

```bash
go mod verify
go vet . ./exact ./exact/postgres
go test -race -count=1 -cover . ./exact ./exact/postgres
go build . ./exact ./exact/postgres
```

Inputs are strict: no whitespace trimming, unknown networks, wildcards, or decimal leading zeros. Namespace must be `tron`; legacy hexadecimal references are case-insensitive. Addresses must be 34-character Base58Check or 42-character `41`-prefixed hex. Arbitrary `0x` addresses are not accepted as TRON input. Formatting is not authorization verification.

Database tests require `X402_TEST_DATABASE_URL` pointing to a dedicated PostgreSQL database with permission to create temporary schemas. Each test creates and cleans only its own random schema. Without this variable integration tests explicitly skip; this is not a passing database acceptance run. CI provisions PostgreSQL 16 and runs them.

Tests cover public vectors, independent encoding checks, authorization tampering, uint256/time boundaries, signature canonicality, fuzz seeds, simulated-node preflight failures and actual PostgreSQL concurrency/recovery. Journal test payloads are storage fixtures, not valid on-chain transactions. No live-chain acceptance is claimed. Next: trusted transaction construction/signing, broadcast and finality checks, official SDK mechanism interfaces and Nile interoperability before gateway integration.

## References and status

- [TRON address specification](https://developers.tron.network/docs/account)
- [Official x402 Go SDK](https://github.com/x402-foundation/x402/tree/main/go)
- [TRON ecosystem reference](https://github.com/BofAI/x402)

[Apache-2.0 licensed](LICENSE). See [third-party notices](THIRD_PARTY_NOTICES.md). No stable release or independent security audit yet.
