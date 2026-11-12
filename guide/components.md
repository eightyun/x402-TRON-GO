# Building blocks

[简体中文](components.zh-CN.md) | English

Lower-level components used by the mechanisms. Most integrators do not call these directly; they document the exact checks performed and their limits.

## Address and network utilities

- `NormalizeNetwork` and `ChainID`: Mainnet, Nile, Shasta; known legacy hexadecimal aliases normalize to decimal CAIP-2 identifiers.
- `NormalizeAddressHex` and `NormalizeAddressBase58`: validate and convert TRON addresses.
- `AddressForSigning`: explicitly convert a valid TRON address to a 20-byte `0x` address for TIP-712.

The root package uses only the standard library. The `exact` package uses official x402 Go SDK v2.30.0 types and established secp256k1/Keccak libraries. Offline verification makes no RPC calls; optional preflight performs read-only JSON-RPC. Neither stores keys, approves tokens, consumes nonces or broadcasts transactions. Recognizing a network does not mean settlement is supported. Address format cannot distinguish TRON networks or establish account activation, safety, or balance.

## Offline exact Permit2 verification

`exact.NewOfflineVerifier` requires a trusted network, Permit2 contract and exact proxy. No default mainnet configuration or client-selected contracts. Deployment owners must independently validate contract code and domain/proxy bindings.

`Digest` computes the TIP-712 digest for external signing. `VerifyAuthorization` consumes official SDK `types.PaymentPayload`, trusted server-side `types.PaymentRequirements` and server time. It binds the quote, network, token, amount, recipient, proxy, nonce and validity, then recovers the EOA signer.

Current subset: required `extra.assetTransferMethod: "permit2"`, optional `paymentFlow: "upfront"`, no other fields/flows or nonempty extensions. Accepted and trusted quote flows must match. Two-field witness (`to`, `validAfter`); strict TRON addresses in configuration/requirements and 20-byte `0x` addresses in authorizations; canonical decimal uint256 amounts/times, decimal or 32-byte hex nonces. Deadline must have at least six seconds remaining, bounded by the quote timeout. Only 65-byte low-s EOA signatures with v=0/1 or 27/28; no contract-wallet signatures.

**Valid authorization is not proof of funds or settlement.** No balance, allowance, nonce bitmap or chain-state checks; no nonce consumption or concurrency/replay protection. Repeated verification can succeed. **Do not deliver paid resources from this result.** Chain preflight, durable idempotency, settlement and final outcome verification remain necessary.

The authorization does not bind a specific HTTP URL or request body. Resource servers must also prevent duplicate fulfillment using the same payment.

The public TronWeb 6.3.0 vector verifies digest/signature compatibility offline. Its ephemeral random test key was discarded; no live wallet or blockchain was used. This is not full client/facilitator interoperability acceptance.

## Read-only chain preflight

Batch payment preflight combines the chain ID/initial head with some contract reads, then the remaining contracts and payment state, saving one round trip. Other read-only queries in a group may already have run when one check rejects; every check must pass before the separate full simulation and final-head comparison. Default-mode stage gates remain unchanged. Neither mode is an atomic snapshot or guarantees public-RPC availability.

Single-request parallel reads remain the default. Set `PreflightConfig.BatchRPC: true` (`X402_RPC_BATCH=true` for the deployment/facilitator examples) to reduce HTTP round trips. This is JSON-RPC batch, not on-chain Multicall or an atomic snapshot: all logical reads, full settlement simulation, unchanged-head and freshness checks remain mandatory. Groups contain at most six queries with a 1MiB total response limit; missing/duplicate/unknown IDs and error responses fail closed, without fallback or automatic retry. Slow public nodes can still return `chain_state_changed`; do not disable checks. Other fixed, sanitized reasons are `chain_read_failed`, `chain_head_stale`, `chain_network_mismatch`, and `chain_contract_mismatch`.

`exact.NewPreflightVerifier` accepts `PreflightConfig` with trusted `Authorization` settings, `NodeURL`, `Permit2CodeHash` and `ProxyCodeHash`. Hashes must be independently verified runtime-bytecode Keccak-256 values (`0x` + 32 bytes), not learned from the node on first use. Token addresses come from trusted server-side requirements.

`Check(ctx, payload, requirements, now)` verifies authorization, chain ID and a fresh latest head; pins Permit2/proxy code; checks proxy `PERMIT2()`/`WITNESS_TYPEHASH()` and Permit2 `DOMAIN_SEPARATOR()`; requires token code; reads balance, Permit2 allowance and the `nonceBitmap(payer, nonce >> 8)` bit; then rechecks chain/head and authorization validity including elapsed RPC time.

Read failures, insufficient funds/allowance, used nonce or mismatched contracts/network/heads fail closed. A successful `PreflightResult` is **not a settlement proof or a reservation of balance/nonce**. The configured RPC and independently verified code hashes remain trust boundaries; no malicious-node state proofs are provided.

`CheckWithSimulation(ctx, payload, requirements, executorAddress, now)` additionally simulates the complete `settle` via `eth_call` after the nonce check and before final head/validity checks. The fee-paying caller must be trusted deployment configuration, never payment input. The proxy, authorization, recipient, signature and ABI encoding match the native transaction builder, with zero attached TRX. The fixed ABI returns nothing: only exact `"0x"` is accepted. Unsupported methods, reverts, read failures and malformed results fail closed without fallback. Responses reject duplicate keys, error envelopes and trailing JSON; errors do not expose raw node details.

Existing `Check` keeps its 13 reads; simulation uses 14 within the same total timeout, defaulting to 15 seconds and capped at one minute, without automatic retries. Independent reads run in groups: chain ID/head, six contract checks, then balance/allowance/nonce. Full simulation remains separate and requires all preceding checks to pass. Each verifier shares a limit of six in-flight JSON-RPC requests across callers; queueing counts against the budget and all group tasks finish before return. Cancellation/deadlines retain context semantics; other errors follow the original logical priority, not response arrival order. Other read-only requests in a failed group may already have been sent, but no later simulation/signing/broadcast stage starts.

Custom `HTTPClient`/Transport implementations must support concurrency and honor context cancellation. Single calls and opt-in batches share six logical-read slots, so batching does not multiply the allowance to 36 reads. This is not a global limit across verifier instances or an aggregate expense budget; bursts may still trigger node 429 responses and fail closed. No Multicall contract dependency, cache or automatic fallback is introduced. Heads must still be at most 60 seconds old or five seconds ahead, and before/after heads must match. Slow nodes can still fail conservatively; callers may retry preflight within their own budget, never infer permission to start another transaction.

The self-hosted facilitator **requires full simulation** for `/verify` and fresh-payment `/settle`, using its configured `ExecutorAddress`. Simulation failure returns `settlement_simulation_failed` without creating a reservation, signing or broadcasting. This strengthens admission: nodes unable to simulate the full call cannot admit fresh payments. Previously submitted/pending/succeeded records still recover only the original transaction, without re-simulation or another payment. Simulation success does not establish account permissions, sufficient Energy/Bandwidth or fees, reserve funds, or prove broadcast or solidified receipt.

TRON [eth_call](https://developers.tron.network/reference/eth_call) and [eth_getCode](https://developers.tron.network/reference/eth_getcode) read latest state, not historical state selected by block references. Matching before/after heads is a drift check, **not an atomic snapshot or finality guarantee**. Revalidate before settlement and verify actual execution outcomes.

HTTPS is required by default; redirects are blocked. API keys use only the `TRON-PRO-API-KEY` header. URLs cannot contain credentials, query or fragment. An optional `HTTPClient` supports trusted CA/proxy settings without in-place mutation. Explicit `AllowInsecureLocalHTTP` permits only loopback/localhost development nodes. Never derive node configuration from payment requests.

## Wallet-free deployment checks

The same `PreflightVerifier` offers `CheckDeployment(ctx, assetAddress, now)`. Without a payer, signature or valid quote, it checks the configured chain ID, fresh head, pinned Permit2/proxy runtime hashes, nonempty token code, `PERMIT2()`, two-field `WITNESS_TYPEHASH()` and `DOMAIN_SEPARATOR()`, then rechecks chain/head/freshness. A normal round still uses 10 read-only requests in concurrent groups: boundary pair → six contract reads → boundary pair. The existing total budget and shared six-request limit apply, without automatic retries. It returns a `DeploymentResult`; existing payment verification and settlement remain unchanged.

Set the following environment variables, then run the [deployment-check example](../examples/deployment/main.go) with `go run ./examples/deployment`:

| Variable | Meaning |
| --- | --- |
| `X402_NETWORK` | Defaults to Nile; only Nile/Shasta, rejecting Mainnet and its aliases |
| `X402_JSON_RPC_URL` | Required trusted HTTPS JSON-RPC; no URL credentials, query or redirects |
| `X402_PERMIT2_ADDRESS` / `X402_PROXY_ADDRESS` | Required independently verified deployment addresses; no defaults |
| `X402_PERMIT2_CODE_HASH` / `X402_PROXY_CODE_HASH` | Required independently trusted runtime-bytecode Keccak-256 hashes; never learned automatically |
| `X402_ASSET` | Required nonzero TRON token address, distinct from both configured contracts |
| `X402_NODE_API_KEY` | Optional request-header credential, never printed |

The command has a 15-second total budget. Success emits structured network/asset/checked contracts (`41`-prefixed hex), configured code hashes and head observations with `readOnly: true` and `paymentValidated: false`; failures exit nonzero with sanitized categories. No wallet loading, database connection, user-fund queries, authorization creation, `settle` simulation, token approval or broadcast.

**Not complete deployment or payment acceptance.** Verify expected hashes from independent trusted material first, not the node's current response. Token checks require only nonempty code, not decimals/transfer semantics. Native genesis/solidified endpoints, full `settle` simulation capability, account permissions and fee resources are not checked. This result cannot establish payment success or replace per-payment `CheckWithSimulation` and solidified receipt verification.

## Constrained settlement construction and external signing

`exact.NewSettlementBuilder` requires trusted `Authorization`, an explicit gas-paying `ExecutorAddress`, positive `FeeLimitSUN`, and a whole-millisecond `Lifetime` greater than 15 seconds and at most 10 minutes. No default mainnet/contracts, private-key custody or arbitrary node-built transaction JSON.

`Build` validates payment authorization and uses a trusted `ReferenceBlock` plus server time to locally construct one exact-proxy `settle` call, TAPOS and TRON raw Protobuf. The transaction ID is `SHA-256(raw_data)`. Reference network, height and ID must match; block time may be at most 60 seconds old or five seconds ahead. This does not prove chain membership, finality or honest RPC responses: deployment and chain preflight remain required. Expiry is capped by the payment deadline; construction and both sides of signing require at least 15 seconds remaining.

The returned `PreparedSettlement` is immutable. `RawData()` returns a copy containing the payment authorization signature: keep it confidential. `Sign(ctx, DigestSigner, now)` sends only the fixed SHA-256 digest to the caller's external signer, then recovers the gas payer's public key. Elapsed signing time counts against validity; original signer error messages are not exposed. Signatures are low-s, 65-byte `r||s||v`; accept v=0/1 or 27/28 and emit canonical v=0/1. The payer's TIP-712 authorization and the executor's transaction signature are distinct; their digests are not interchangeable.

`SettlementTransaction.Payload` is **complete binary Transaction Protobuf, not JSON**, suitable for a later hex-encoded [broadcasthex](https://developers.tron.network/reference/broadcasthex) request. `ValidateSigned` accepts only this prepared object's canonical encoding and checks all raw bytes, ID and signature. Unknown/duplicate fields, attached TRX/TRC10, extra contracts/signatures or changed fee policy are rejected. This is not a generic transaction importer, current-expiry check or receipt proof.

Only a single EOA using default owner permission is supported; no multisig, custom permissions or contract wallets. On-chain permissions, fee resources and execution outcomes still require verification. The builder neither loads keys nor determines settlement success. Persist the unique signed transaction and obtain journal broadcast permission before submission; never extend or rebuild a payment during recovery. The public [TronWeb settlement vector](../exact/testdata/tronweb-settlement.json) uses a synthetic block and a discarded random executor key: encoding/signature interoperability only, not live testnet acceptance.

## Single-attempt broadcast and original-transaction lookup

`exact.NewExecutionClient` requires trusted `ExecutionConfig`: fixed `Network`, native FullNode root `NodeURL`, and an independently verified `GenesisBlockID` (64 hex digits without `0x`, zero first eight bytes, last four bytes matching the TRON chain ID). Never select nodes from payment requests or learn genesis identity on first use. Synthetic test IDs are not deployment configuration. HTTPS is required except explicitly enabled loopback HTTP. No path, query, fragment or userinfo in the URL. Total timeout defaults to 15 seconds, at most one minute; responses are capped at 1 MiB. Redirects are blocked; errors exclude URLs, API keys and original node messages. Custom HTTP transports are trusted configuration and must not retry writes internally.

- Low-level `Broadcast(ctx, prepared, transaction, now)` requires prior durable journal permission. It validates bytes/signature/network/remaining lifetime, pins block zero from **the same native node**, makes one `/wallet/broadcasthex` request, then rechecks network identity. Only explicit acceptance with a successful (or omitted) code and matching original txid acknowledges this attempt, not settlement. Network-check time counts against pre-send validity.
- `Lookup(ctx, txid)` pins genesis before/after `/wallet/gettransactionbyid` and validates txID against `SHA-256(raw_data_hex)`. `TransactionObservation.Found` is **not execution success, finality or receipt of funds**. Only an empty object means currently not found. Timeouts, ambiguous/duplicate top-level JSON keys, bad hashes and network mismatches return errors. Neither absence nor rejection authorizes reservation release, re-signing or replacement.

These requests were exercised by the real settlements in [Nile live payments](validation.md#nile-live-payments); automated tests still use simulated nodes. Node acceptance, a FullNode body, execution and solidified payment events are distinct evidence layers. Neither method declares success; use the read-only checks below. No malicious-node state proofs are provided.

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

Compare `prepared.Authorization()` with the original trusted journal identity before calling the read-only solidified checker. Only the package's fixed single-contract, single-EOA-signature canonical encoding is accepted; other valid TRON transactions may be rejected. This is not a generic transaction importer. `ConfirmSettlement` in the [durable settlement journal](facilitator.md#durable-settlement-journal) combines recovery, checking and durable success and is used by the facilitator and its HTTP example. One real payment does not cover every interruption window; simulated tests are not deployment acceptance.
