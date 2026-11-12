# Building blocks

[简体中文](components.zh-CN.md) | English

This page documents the lower-level components that the `exact` mechanisms are built from. Most integrators never call them directly. Read it if you need these building blocks yourself, or if you want to know exactly what each step checks and what it does not guarantee.

## Dependencies and side effects

- The root package `github.com/eightyun/x402-tron-go` uses only the Go standard library.
- The `exact` package uses the protocol types of the official x402 Go SDK v2.30.0. It verifies signatures with established secp256k1 and Keccak libraries.
- Offline verification makes no RPC calls.
- The optional `PreflightVerifier` performs read-only JSON-RPC calls only.
- None of these components store keys, approve tokens, consume nonces or broadcast transactions. Broadcasting is available only through the separate `ExecutionClient.Broadcast` described below.

## Address and network utilities

| Function | Purpose |
| --- | --- |
| `NormalizeNetwork` | Normalizes Mainnet, Nile and Shasta identifiers to decimal CAIP-2 form. Known legacy hexadecimal aliases are accepted. |
| `ChainID` | Returns the chain ID of a known network. |
| `NormalizeAddressHex` | Validates an address and returns the 42-character `41`-prefixed lowercase hexadecimal form. |
| `NormalizeAddressBase58` | Validates an address and returns the canonical Base58Check form. |
| `AddressForSigning` | Explicitly converts a valid TRON address to the 20-byte `0x` address that TIP-712 needs. |

Recognizing a network does not mean that settlement is supported on it.

Mainnet and the testnets use the same address format. **You cannot tell the network from an address.** A valid address format also does not show that the account is activated, safe or funded.

### Usage

Requires Go 1.26.10 or later. CI uses the latest 1.26.x patch release. Only testnet release candidates exist so far, and the public API may still change.

```go
import tron "github.com/eightyun/x402-tron-go"

network, err := tron.NormalizeNetwork("tron:0xcd8690dc")
if err != nil {
    return err
}
// network is tron:3448148188

address, err := tron.NormalizeAddressHex("TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL")
if err != nil {
    return err
}
// address is 418840e6c55b9ada326d211d818c34a994aeced808
```

### Input strictness

- Whitespace is never trimmed implicitly.
- Unknown networks, wildcards and decimal network references with leading zeros are rejected.
- The network namespace must be lowercase `tron`. Legacy hexadecimal references are case-insensitive.
- The address functions accept only 34-character Base58Check or 42-character `41`-prefixed hexadecimal input.
- `AddressForSigning` outputs a 20-byte `0x` address. It does not accept arbitrary EVM addresses as input.
- Format conversion is not signature verification.

## Offline exact Permit2 verification

`exact.NewOfflineVerifier` takes an explicit trusted configuration: one TRON network, the Permit2 contract and the exact proxy address.

- There is no default Mainnet configuration.
- Contract addresses are never taken from the payer-supplied `extra` field.
- Deployment owners must independently verify the contract bytecode and the domain and proxy bindings.

### Methods

- `Digest` computes the TIP-712 digest of the two-field `Witness(to, validAfter)` for an external signer.
- `VerifyAuthorization` takes the official SDK `types.PaymentPayload`, the trusted server-side `types.PaymentRequirements` and the server's current time. It binds the quote, network, token, amount, recipient, proxy, nonce and validity window. It then recovers the EOA public key and checks that it matches the payer.

### Accepted subset

This is a limited interoperability subset, not full support for every x402 or TRON extension.

- `extra` must contain `assetTransferMethod: "permit2"`.
- `extra` may additionally contain `paymentFlow: "upfront"`. No other fields, flows or nonempty `extensions` are accepted.
- The `paymentFlow` in the accepted payment and in the trusted quote must match.
- Addresses in the configuration and the requirements use strict TRON format. Addresses inside the authorization use strict 20-byte `0x` format.
- Amounts and times are canonical decimal uint256 values. Nonces may be decimal or 32-byte `0x` hexadecimal.
- The deadline must have at least 6 seconds remaining and must not exceed the quote timeout.
- Only 65-byte low-s EOA signatures are accepted, with `v` equal to 0/1 or 27/28. Contract-wallet signatures are not supported.

> [!WARNING]
> **A valid offline authorization is not proof that the payer can pay, and not proof that the payment arrived.** Verification reads no balance, allowance, nonce bitmap or other chain state. It does not consume the nonce and gives no concurrency or replay protection, so verifying the same authorization again can succeed. **Do not deliver paid resources based on this result.** Chain preflight, durable idempotency, settlement and verification of the final outcome are still required.

The authorization does not bind a specific HTTP URL or request body. Resource servers must also prevent the same payment from fulfilling more than one request.

### Test vector

[`exact/testdata/tronweb-permit2.json`](../exact/testdata/tronweb-permit2.json) is a public vector generated and verified independently with TronWeb 6.3.0. It used an ephemeral random test key that was then discarded. It checks digest and signature compatibility offline only. It is not full client or facilitator interoperability acceptance, and no live wallet or blockchain was involved.

## Read-only chain preflight

`exact.NewPreflightVerifier` combines offline verification with read-only checks of chain state. It does not hold keys, consume nonces or broadcast. A verifier can be used concurrently.

### Configuration

`PreflightConfig` is trusted deployment configuration. It must never come from a payment request.

| Field | Meaning |
| --- | --- |
| `Authorization` | The trusted contract configuration, the same as for offline verification. |
| `NodeURL` | Trusted JSON-RPC endpoint. |
| `APIKey` | Optional. Sent only in the `TRON-PRO-API-KEY` header. |
| `Permit2CodeHash`, `ProxyCodeHash` | Keccak-256 of the runtime bytecode, as `0x` plus 32 bytes. Take them from independently verified deployment material. They are never learned from the node on first use. |
| `Timeout` | Total budget per check. Defaults to 15 s, maximum 1 min. |
| `HTTPClient` | Optional. Use it for trusted CA or proxy settings. It is never modified in place. |
| `AllowInsecureLocalHTTP` | Explicitly allows plain HTTP, only for loopback or localhost development nodes. |
| `BatchRPC` | Opt-in JSON-RPC batch mode. See below. |

Connection rules:

- HTTPS is required by default.
- Redirects are blocked.
- The URL must not contain credentials, a query or a fragment.

The token address always comes from the trusted server-side requirements.

### `Check`

`Check(ctx, payload, requirements, now)` runs these steps:

1. Verifies the authorization offline, then reads the chain ID and the latest head.
2. Checks that the head is fresh: at most 60 s behind server time or 5 s ahead.
3. Pins the Permit2 and proxy runtime code hashes. Checks the proxy's `PERMIT2()` and two-field `WITNESS_TYPEHASH()`, and Permit2's `DOMAIN_SEPARATOR()`. Requires the token address to have code.
4. Reads the payer's token balance, the Permit2 allowance and the nonce bit in `nonceBitmap(payer, nonce >> 8)`.
5. Reads the chain ID and head again. The head must equal the first one and still be fresh.
6. Rechecks the authorization's validity, counting the time spent on RPC calls.

The check fails closed on read failures, insufficient balance or allowance, an already used nonce, mismatched contracts or bindings, a wrong network or a changed head.

A successful `PreflightResult` contains only the verified authorization and the observed head. **It is not a settlement proof, and it does not reserve balance or the nonce.** The configured RPC node and the independently verified code hashes remain trust boundaries. No state proofs against a malicious node are provided.

### `CheckWithSimulation`

`CheckWithSimulation(ctx, payload, requirements, executorAddress, now)` runs the same checks. After the nonce check, and before the final head and validity checks, it also simulates the complete `settle` call with `eth_call`.

- `executorAddress` is the fee-paying caller. It must be fixed deployment configuration, never payment input.
- The proxy, authorization, recipient, signature and ABI encoding are the same as in the native transaction builder. No TRX is attached.
- The fixed ABI returns nothing, so only the exact result `"0x"` is accepted.
- Unsupported methods, reverts, read failures and malformed results fail closed. There is no fallback.
- Responses with duplicate keys, error envelopes or trailing JSON are rejected. Errors never expose raw node details.

A successful simulation does not establish account permissions or sufficient Energy, Bandwidth or fees. It does not reserve funds and does not prove that a transaction was broadcast or a payment solidified.

### Request counts, concurrency and retries

- `Check` makes 13 reads. `CheckWithSimulation` makes 14. Both share the same total `Timeout` and never retry automatically.
- Independent reads run in concurrent groups: chain ID and head, then six contract checks, then balance, allowance and nonce. The full simulation runs separately, and only after all preceding checks pass.
- Each verifier allows at most 6 in-flight JSON-RPC requests, shared across all its callers. Time spent queueing counts against the budget.
- A group returns only after all of its tasks have finished.
- Cancellation and deadlines keep their context semantics. Other errors are reported in the original logical order of the checks, not in the order responses arrive.
- When one check in a group fails, other read-only requests in that group may already have been sent. No later stage starts: no simulation, signing or broadcast.

The limit of 6 is per verifier instance. It is not a global limit across verifiers and not an aggregate cost budget. Bursts can still trigger HTTP 429 responses from the node, which fail closed. There is no Multicall contract dependency, no cache and no automatic fallback.

A custom `HTTPClient` or Transport must support concurrent use and honor context cancellation.

Slow nodes can still make the check fail conservatively. Callers may retry preflight within their own budget. A failed or retried preflight never grants permission to start another transaction.

### JSON-RPC batch mode

Single-request parallel reads are the default. Set `PreflightConfig.BatchRPC: true` to reduce HTTP round trips. The deployment and facilitator examples enable it with `X402_RPC_BATCH=true`.

In batch mode, payment preflight sends two batches, saving one round trip:

1. The chain ID, the initial head and four of the contract reads.
2. The remaining two contract reads, plus balance, allowance and nonce.

The default mode keeps its stage-by-stage gates. Batch mode changes only how requests are sent:

- It is a JSON-RPC batch, not an on-chain Multicall and not an atomic snapshot.
- Every logical read, the full settlement simulation and the unchanged-head and freshness checks remain mandatory.
- A batch contains at most 6 queries, with a total response limit of 1 MiB.
- Missing, duplicate or unknown IDs and error responses fail closed. There is no fallback and no automatic retry.
- Single requests and batches share the same 6 logical-read slots. Batching does not multiply the allowance to 36 reads.
- When one check rejects, other read-only queries in the same batch may already have run. Every check must still pass before the separate full simulation and the final head comparison.

Neither mode guarantees that a public RPC endpoint will be available on every attempt.

### Failure reasons

Slow public nodes can still produce `chain_state_changed`. Do not work around this by disabling checks. The other fixed, sanitized reasons are:

- `chain_read_failed`
- `chain_head_stale`
- `chain_network_mismatch`
- `chain_contract_mismatch`

### Use in the self-hosted facilitator

The self-hosted facilitator **always requires the full simulation** for `/verify` and for `/settle` of a fresh payment. It uses its configured `ExecutorAddress`.

- A failed simulation returns `settlement_simulation_failed`. No reservation is created, and nothing is signed or broadcast.
- This is stricter than state preflight alone. A node that cannot simulate the full call cannot admit fresh payments.
- Records that were already submitted, pending or succeeded only recover their original transaction. They are not simulated again, and a consumed nonce or an unavailable simulation never leads to a second payment.

### Chain-state semantics

TRON [eth_call](https://developers.tron.network/reference/eth_call) and [eth_getCode](https://developers.tron.network/reference/eth_getcode) read the latest state. You cannot select historical state by passing a block reference.

> [!WARNING]
> Matching heads before and after the reads is only a drift check. **It is not an atomic snapshot and not a finality guarantee.** Revalidate before settlement, and verify the actual on-chain execution outcome.

## Wallet-free deployment checks

The same `PreflightVerifier` offers `CheckDeployment(ctx, assetAddress, now)`. It needs no payer, signature or valid quote. It runs these steps:

1. Reads the chain ID and a fresh head.
2. Checks the pinned Permit2 and proxy runtime code hashes and requires nonempty token code. Checks `PERMIT2()`, the two-field `WITNESS_TYPEHASH()` and `DOMAIN_SEPARATOR()`.
3. Reads the chain ID and head again, and checks that the head is unchanged and still fresh.

A normal run makes 10 read-only requests in concurrent groups: 2 boundary reads, then 6 contract reads, then 2 boundary reads. It uses the same total budget and the shared limit of 6 in-flight requests, without automatic retries. The asset address must differ from both configured contracts.

It returns a `DeploymentResult`. Payment verification and settlement are unchanged.

### Running the example

Set the following environment variables, then run the [deployment-check example](../examples/deployment/main.go) with `go run ./examples/deployment`.

| Variable | Meaning |
| --- | --- |
| `X402_NETWORK` | Defaults to Nile. Only Nile and Shasta are allowed. Mainnet and its aliases are rejected. |
| `X402_JSON_RPC_URL` | Required. Trusted HTTPS JSON-RPC endpoint. URL credentials, queries and redirects are not allowed. |
| `X402_PERMIT2_ADDRESS` / `X402_PROXY_ADDRESS` | Required. Independently verified deployment addresses. There are no defaults. |
| `X402_PERMIT2_CODE_HASH` / `X402_PROXY_CODE_HASH` | Required. Independently trusted Keccak-256 hashes of the runtime bytecode. They are never learned automatically. |
| `X402_ASSET` | Required. Nonzero TRON token address to check. It must differ from both configured contracts. |
| `X402_NODE_API_KEY` | Optional. Sent only as a request header and never printed. |
| `X402_RPC_BATCH` | Optional. `true` enables JSON-RPC batch mode. Only `true`, `false` or an empty value are accepted. |

The command has a total budget of 15 s.

- On success it prints structured output: the network, the asset and the checked contracts (as `41`-prefixed hex), the configured code hashes and the head observations, with `readOnly: true` and `paymentValidated: false`.
- On failure it exits with a nonzero status and prints only a sanitized category.

It never loads a wallet, connects to a database, queries user funds, creates an authorization, simulates `settle`, approves tokens or broadcasts.

> [!WARNING]
> **This is not complete deployment or payment acceptance.** Verify the expected hashes from independent trusted material first. Never use the node's current response as the expected value. The token check requires only nonempty code and does not verify decimals or transfer semantics. Native genesis and solidified endpoints, the ability to simulate a full `settle`, account permissions and fee resources are not checked. The result cannot show that a payment succeeded. It does not replace the per-payment `CheckWithSimulation` or the solidified receipt check.

## Constrained settlement construction and external signing

`exact.NewSettlementBuilder` builds the one transaction shape this package settles with. It takes a `SettlementBuilderConfig`:

| Field | Meaning |
| --- | --- |
| `Authorization` | Trusted contract configuration. |
| `ExecutorAddress` | The account that pays the gas. Must be set explicitly. |
| `FeeLimitSUN` | Fee limit in SUN. Must be positive. |
| `Lifetime` | Transaction lifetime. Must be a whole number of milliseconds, greater than 15 s and at most 10 min. |

There is no default Mainnet or contract configuration. The builder holds no private keys and never accepts arbitrary transaction JSON built by a node.

### `Build`

`Build` first verifies the payment authorization. It then uses a trusted `ReferenceBlock` and server time to build locally one exact-proxy `settle` call, the TAPOS fields and the TRON raw Protobuf. The transaction ID is `SHA-256(raw_data)`.

- The reference block's network, height and ID must be consistent.
- The reference block time may be at most 60 s old or 5 s ahead.
- Expiry is capped by the payment authorization deadline.
- Construction requires at least 15 s of remaining validity, and so does each side of signing.

These offline checks do not prove that the block belongs to the chain, that it is final, or that the RPC responses were honest. Deployment checks and chain preflight are still required.

### `PreparedSettlement` and `Sign`

The returned `PreparedSettlement` is immutable.

- `RawData()` returns a copy of the raw data. It contains the payer's authorization signature, so keep it confidential.
- `Sign(ctx, DigestSigner, now)` sends only the fixed SHA-256 digest to the caller's external signer. It then recovers the public key and checks that it belongs to the gas payer.
- Time spent signing counts against the validity window. The signer's original error messages are not exposed.
- Signatures are low-s, 65-byte `r||s||v`. The signer may return `v` as 0/1 or 27/28. The output is always canonical 0/1.

The payer's TIP-712 authorization and the executor's transaction signature are two different signatures. Their digests are not interchangeable.

### `SettlementTransaction` and `ValidateSigned`

`SettlementTransaction.Payload` is a **complete binary Transaction Protobuf, not JSON**. It is suitable for a later hex-encoded [broadcasthex](https://developers.tron.network/reference/broadcasthex) request.

`ValidateSigned` accepts only the canonical encoding of the matching prepared object. It checks every raw byte, the ID and the signature. It rejects:

- unknown or duplicate fields
- attached TRX or TRC10
- extra contracts or extra signatures
- a changed fee policy

It is not a generic transaction importer. It does not check the current expiry and does not prove receipt of funds.

### Limits

- Only a single EOA using the default owner permission is supported. Multisig, custom permissions and contract wallets are not.
- On-chain account permissions, fee resources and execution outcomes still need to be verified.
- The builder never loads keys and never decides whether settlement succeeded.
- After signing, persist the unique signed transaction and obtain broadcast permission from the journal before submitting it. Never extend the lifetime or build a new payment during recovery.

The public [TronWeb settlement vector](../exact/testdata/tronweb-settlement.json) uses a synthetic block and a discarded random executor key. It verifies encoding and signature interoperability only, not live testnet acceptance.

## Single-attempt broadcast and original-transaction lookup

`exact.NewExecutionClient` takes a trusted `ExecutionConfig`:

| Field | Meaning |
| --- | --- |
| `Network` | Fixed TRON network. |
| `NodeURL` | Root URL of a native FullNode. HTTPS is required unless plain loopback HTTP is explicitly enabled. No path, query, fragment or userinfo. |
| `GenesisBlockID` | Independently verified genesis block ID: 64 hex digits without `0x`, the first 8 bytes zero, and the last 4 bytes matching the TRON chain ID. |
| `APIKey` | Optional. Sent only in the `TRON-PRO-API-KEY` header. |
| `Timeout` | Total budget. Defaults to 15 s, maximum 1 min. |
| `HTTPClient` | Optional custom client. |
| `AllowInsecureLocalHTTP` | Allows plain HTTP for loopback nodes only. |

- Never select a node from a payment request, and never learn the genesis identity on first use.
- Synthetic IDs from the tests are not deployment configuration.
- Responses are capped at 1 MiB. Redirects are blocked.
- Errors never include URLs, API keys or original node messages.
- A custom HTTP transport is trusted configuration. It must not retry write requests internally.

### `Broadcast`

`Broadcast(ctx, prepared, transaction, now)` is a low-level, single-attempt write. Call it only after the durable journal has granted permission.

1. Validates the complete bytes, the signature, the network and the remaining lifetime.
2. Reads block 0 from **the same native node** and compares the full genesis ID.
3. Sends exactly one `/wallet/broadcasthex` request.
4. Checks the network identity again.

Only an explicit acceptance, with a successful (or omitted) code and the original txid, acknowledges the attempt. That means the node accepted this broadcast, not that settlement succeeded. Time spent on the network checks counts against the validity window before sending.

### `Lookup`

`Lookup(ctx, txid)` checks the genesis ID before and after calling `/wallet/gettransactionbyid`. It validates the returned txID and recomputes `SHA-256(raw_data_hex)`.

- `TransactionObservation.Found` **does not mean successful execution, finality or receipt of funds**.
- Only an empty object means the transaction is currently not found.
- Timeouts, ambiguous or duplicate top-level JSON keys, wrong hashes and network mismatches return errors.
- Neither "not found" nor a rejected request authorizes releasing a reservation, signing again or creating a replacement.

### Evidence layers

These requests were exercised by the real settlements in [Nile live payments](validation.md#nile-live-payments). The automated tests still use simulated nodes.

Node acceptance, a FullNode transaction body, the execution receipt and solidified payment events are separate layers of evidence. Neither `Broadcast` nor `Lookup` declares success. Use the read-only check below. No state proofs against a malicious node are provided.

## Solidified original-transaction and payment-event checks

`executionClient.CheckSettlement(ctx, prepared, transaction, time.Now())` uses the **original prepared object and the original signed transaction**. It never rebuilds an expired authorization, broadcasts or writes to the journal or ledger. Expired transactions and authorizations can still be checked historically. `now` must be trusted server time.

The configured endpoint must expose the `/walletsolidity` API. Within one bounded budget, the checker:

1. Pins the genesis ID of the solidity view and reads a fresh solidified head.
2. Reads the solidified transaction body and receipt. Matches the original raw bytes byte for byte, the txid and its SHA-256, the single-contract execution result and the receipt identity.
3. Checks the receipt's block height and time, the initial solidified-head boundary, and that the block contains exactly one copy of the original transaction. At the same height, the head ID and the block ID must agree.
4. Requires exactly one standard token `Transfer` from the payer to the recipient for the exact amount. Partial, excess or duplicate payments, fee deductions and any other same-token transfer in or out of either party are rejected. Amounts are never summed, and gross flow is never treated as net receipt.
5. Rechecks that the heads have not regressed, that the original block ID, time and transaction membership are unchanged, and the network identity. It then returns `SettlementObservation.Confirmed=true` with the block ID and height.

### Results

- A missing solidified body or receipt, or a receipt just beyond the initial head, returns `Confirmed=false`. This is not a terminal failure.
- A non-successful execution report, inconsistent evidence, network failures, a timeout or an invalid critical field return an error. They **never return `Confirmed=true` and never grant permission to release a reservation, sign again or rebroadcast**.

### Budget and freshness

- A successful check normally makes 9 sequential read-only requests, without retries.
- The same limits apply: 15 s default total timeout (maximum 1 min), 1 MiB response limit, TLS and redirect restrictions, and error redaction.
- Critical nested JSON objects reject duplicate keys. Native integer fields reject strings, floats and overflow.
- Solidified heads may lag trusted time by at most 2 min or lead it by 5 s. The total elapsed time also counts against the freshness of the initial snapshot.
- Slow nodes, long chain stalls or oversized blocks may make the check fail conservatively. Only retry the read-only check, within the caller's budget.

### Limits

- Only trusted standard TRC-20 tokens are supported. Self-transfers, fee-on-transfer, rebasing and custom event semantics are not.
- A trusted node and independently verified contract and token deployments remain assumptions.
- There is no verification of the complete chain, no Merkle proofs and no protection against malicious node state.
- The meaning of the events depends on the trusted token implementation.

See [TRON confirmation semantics](https://developers.tron.network/docs/confirmation-semantics) and [event encoding](https://developers.tron.network/docs/event) for how the solidified view and logs are meant to be used.

### Recovery after restart

`exact.RestoreSettlement(config, executorAddress, originalTransaction)` recovers a `PreparedSettlement` solely from the persisted original signed transaction. `config` and the fee-paying executor must be trusted deployment inputs.

It performs these checks:

1. Strictly decodes this package's fixed Protobuf and ABI.
2. Re-encodes the result and compares it byte for byte with the original.
3. Verifies the payer's TIP-712 signature, the executor's transaction signature and the original txid.

The authorization fields come from the original call. They never come from an auxiliary snapshot or the current quote.

Recovery never calls `Build`, never fakes the current time to accept an expired authorization, and never extends validity, signs or broadcasts. The TAPOS bytes are not a complete proof of the reference block. Recovery alone does not prove the original quote, the preflight or a successful payment.

Before calling the read-only solidified check, compare `prepared.Authorization()` with the trusted authorization identity in the original journal record.

Only this package's fixed canonical encoding is accepted: a single contract call with a single EOA signature. Other valid TRON transactions may be rejected. This is not a generic importer of historical transactions.

`ConfirmSettlement` in the [durable settlement journal](facilitator.md#durable-settlement-journal) combines recovery, the check and durable recording of success. The facilitator and its HTTP example use it. One real payment does not cover every interruption window, and simulated tests are not deployment acceptance.
