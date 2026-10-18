# x402-tron-go

[简体中文](README.md) | English

An independent TRON adapter SDK for x402: clients/agents, API resource servers and facilitators should be able to integrate without implementing TRON protocol details or depending on our payment gateway. Provides official mechanisms for all three roles, a runnable facilitator HTTP example, authorization/preflight, transaction construction/broadcast, solidified exact-payment checks and durable recovery. **Real Nile HTTP payments, terminal-success recovery and cross-process recovery after a pending-process crash have passed; this remains an unaudited testnet development version. Do not use with real funds.** Not affiliated with the x402 Foundation or TRON.

## Integration status

| Role | Status |
| --- | --- |
| Client / agent | Implements official `SchemeNetworkClient`; configure signing/payment policy and use the official HTTP client for 402 retries |
| API resource server | Implements official `SchemeNetworkServer`, Permit2 quotes and an `upfront` middleware example; requires a trusted facilitator, not just offline verification |
| Self-hosted facilitator | Official mechanism and authenticated, concurrency-bounded HTTP example: preflight, controlled signing/broadcast, bounded confirmation and original-transaction recovery; one Nile payment and succeeded-record restart verified, full deployment/interruption acceptance pending |

Ordinary clients do not need PostgreSQL or gateway merchant/ledger modules. PostgreSQL is an optional durable storage implementation for self-hosted facilitators. Trusted contracts, asset/recipient policy, wallet signing, gas and necessary Permit2 token approval remain prerequisites; no implicit approval or on-chain action.

**Interoperability boundary:** offline bidirectional authorization tests use the actual published `@bankofai/x402-tron@1.2.0` / `@bankofai/x402-core@1.1.1`. That release configures only legacy hexadecimal network IDs (for example Nile `tron:0xcd8690dc`), not this project's default decimal quotes. The Go mechanism accepts known aliases, but official-core registration keys must still match the quote. This does not establish complete HTTP interoperability between default examples and that release. Tests do not patch upstream constants or weaken signature checks to hide the difference.

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
| `X402_FACILITATOR_TOKEN` | Required for this project's service; official `AuthProvider` sends Bearer to all three endpoints; optional for unauthenticated third-party services |
| `X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP` | Off by default; explicit `true` allows HTTP only to literal loopback IPs, not `localhost` or public addresses |
| `X402_LISTEN_ADDR` | Defaults to `127.0.0.1:8080` |

Explicit initialization and quote validation happen before listening; failures stop startup. `GET /resource` returns 402 without payment. HEAD is rejected to prevent Go GET-route HEAD matching from bypassing payment. Includes bounded startup/settlement/HTTP timeouts, no upstream redirects, bounded headers and graceful shutdown. External deployment needs TLS ingress and appropriate authentication. Use official `AuthProvider` for facilitator credentials, never URL tokens.

Remote requests have a 200-second budget covering the mechanism's maximum 180 seconds. Middleware allows 410 seconds and HTTP writes 420 seconds, covering one official identical-payload pending retry. Startup discovery remains bounded to 30 seconds. Match proxy/caller budgets too; timeout does not authorize another payment.

The self-hosted example below requires independently provisioned/verified contracts and nodes; no public facilitator address is assumed. Automated HTTP integration tests use synthetic chain state/receipts. The separate single-payment Nile exercise below **does not establish clean-room deployment of the executable examples or production acceptance**.

### Nile live-chain validation (2026-10-18)

Core revision `39b83a63fab27715306cf6fb273a9dd7184195e5`, Go 1.26.5 and official Go SDK v2.28.0 used the source-reproduced Permit2/proxy below, Nile test USDT `TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf`, separate payer/executor/recipient addresses and dedicated PostgreSQL. A one-off local integration program used public SDK APIs and official HTTP middleware with two loopback TLS services and real nodes/receipts; core code and admission checks were unchanged.

- After a limited 0.003 test-USDT approval, a real 402 quote produced one 0.001 test-USDT authorization. Full simulation, durable unique transaction, single broadcast, solidified checks and persisted success preceded HTTP 200 resource delivery.
- Settlement [135dfb80…23efe5](https://nile.tronscan.org/#/transaction/135dfb80322290839444d93864f8190ae16cb0aadb1b9d9621110f96b823efe5), solidified block `71933724`, with one exact Transfer and matching balance changes. Approval cost 0 and settlement cost 7.237 test TRX; these are observed fees, not future estimates.
- After normal process exit, a new process replayed the identical authorization and returned the same txid from the existing `succeeded` record. Additional authorization signatures, transaction signatures and broadcasts were all zero; original signed bytes, balances and fees were unchanged. **This is terminal-success recovery across processes, not a forced crash while pending.**

A second real payment verified pending-interruption recovery: settlement [3a339895…18873a](https://nile.tronscan.org/#/transaction/3a339895a1c7f3e66370e33e8bc71c729ff287c826dcf82129fa6ffd7318873a) was journaled as `pending`, absent from both solidified body/receipt endpoints, and not delivered when its executor process was SIGSTOP/SIGKILL-terminated and reaped. A new process loaded no private keys and reused the original header, database and 603 signed bytes, delivering only after solidification in block `71934525`. Additional authorization signatures, transaction signatures and broadcasts were zero. The first record stayed unchanged; payer/recipient changed by exactly 0.001 test USDT. The second fee was 5.737 test TRX, totaling 12.974 for both payments.

These exercises used separate integration programs, not clean-room deployment of the complete executable examples. Shasta/Mainnet, live end-to-end upstream-SDK payments, long-running stability and independent security remain unvalidated. Private keys, original spendable authorizations and local operational materials are not published; no copy-ready production-funds configuration is supplied. Clean-room integration and prerelease packaging remain pending.

## Self-hosted facilitator HTTP example

### Real payment client and identical-request recovery

The [payer example](examples/payer/main.go) is an explicitly enabled Linux/macOS testnet client, separate from the offline-handshake-only `examples/client`. Use [.env.example](.env.example) to configure trusted network/Permit2/proxy, token/recipient, `X402_MAX_AMOUNT_ATOMIC`, `X402_PAYER_ADDRESS`, `X402_PAYER_KEY_FILE` and resource URL. The template is not loaded automatically. Never commit actual credentials, keys or authorizations.

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

The [runnable example](examples/facilitator/main.go) targets Linux/macOS, allows only Nile/Shasta and literal loopback listeners, and **rejects mainnet/public binding**. Configure these variables and run `go run ./examples/facilitator`:

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

Administrators explicitly apply [schema.sql](exact/postgres/schema.sql) using a separate migration role; existing tables need the upgrade described below. Do not put database URLs/passwords in command-line arguments. Runtime roles should lack DDL, DELETE and TRUNCATE privileges. Startup checks configuration, database connectivity and readability of current-version columns. It **does not create/migrate tables, exhaustively inspect constraints, check live deployments, sign, approve or submit payments**. This is not full deployment acceptance: verify the complete schema, executor default-owner permissions/TRX/energy, token semantics and payer Permit2 allowance independently.

For local resource-server integration set `X402_FACILITATOR_URL=http://127.0.0.1:8081`, the matching token and `X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP=true`, then run `go run ./examples/server`. Cross-machine deployments require trusted TLS ingress forwarding to loopback, ingress rate limits and aggregate expense budgets. The key stays in process memory and is cleared best-effort on shutdown, not HSM/MPC security. SDK integrations can supply an external `FacilitatorConfig.Signer`. The example loads only the explicitly configured key file, without discovering or modifying existing wallets.

Responses follow official formats. Unknown/unconfirmed results remain unsuccessful; HTTP failures preserve `settlement_pending` and the original txid for identical-payload recovery. Disconnect/503 is not proof of payment failure: never generate a replacement authorization. SIGTERM drains accepted requests; interrupted work recovers from the same journal, without automatic background executors or rebroadcast.

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

Single-request parallel reads remain the default. Set `PreflightConfig.BatchRPC: true` (`X402_RPC_BATCH=true` for the deployment/facilitator examples) to reduce HTTP round trips. This is JSON-RPC batch, not on-chain Multicall or an atomic snapshot: all logical reads, full settlement simulation, unchanged-head and freshness checks remain mandatory. Groups contain at most six queries with a 1MiB total response limit; missing/duplicate/unknown IDs and error responses fail closed, without fallback or automatic retry. Slow public nodes can still return `chain_state_changed`; do not disable checks. Other fixed, sanitized reasons are `chain_read_failed`, `chain_head_stale`, `chain_network_mismatch`, and `chain_contract_mismatch`.

`exact.NewPreflightVerifier` accepts `PreflightConfig` with trusted `Authorization` settings, `NodeURL`, `Permit2CodeHash` and `ProxyCodeHash`. Hashes must be independently verified runtime-bytecode Keccak-256 values (`0x` + 32 bytes), not learned from the node on first use. Token addresses come from trusted server-side requirements.

`Check(ctx, payload, requirements, now)` verifies authorization, chain ID and a fresh latest head; pins Permit2/proxy code; checks proxy `PERMIT2()`/`WITNESS_TYPEHASH()` and Permit2 `DOMAIN_SEPARATOR()`; requires token code; reads balance, Permit2 allowance and the `nonceBitmap(payer, nonce >> 8)` bit; then rechecks chain/head and authorization validity including elapsed RPC time.

Read failures, insufficient funds/allowance, used nonce or mismatched contracts/network/heads fail closed. A successful `PreflightResult` is **not a settlement proof or a reservation of balance/nonce**. The configured RPC and independently verified code hashes remain trust boundaries; no malicious-node state proofs are provided.

`CheckWithSimulation(ctx, payload, requirements, executorAddress, now)` additionally simulates the complete `settle` via `eth_call` after the nonce check and before final head/validity checks. The fee-paying caller must be trusted deployment configuration, never payment input. The proxy, authorization, recipient, signature and ABI encoding match the native transaction builder, with zero attached TRX. The fixed ABI returns nothing: only exact `"0x"` is accepted. Unsupported methods, reverts, read failures and malformed results fail closed without fallback. Responses reject duplicate keys, error envelopes and trailing JSON; errors do not expose raw node details.

Existing `Check` keeps its 13 reads; simulation uses 14 within the same total timeout, defaulting to 15 seconds and capped at one minute, without automatic retries. Independent reads run in groups: chain ID/head, six contract checks, then balance/allowance/nonce. Full simulation remains separate and requires all preceding checks to pass. Each verifier shares a limit of six in-flight JSON-RPC requests across callers; queueing counts against the budget and all group tasks finish before return. Cancellation/deadlines retain context semantics; other errors follow the original logical priority, not response arrival order. Other read-only requests in a failed group may already have been sent, but no later simulation/signing/broadcast stage starts.

Custom `HTTPClient`/Transport implementations must support concurrency and honor context cancellation. This is not a global limit across verifier instances or an aggregate expense budget; bursts may still trigger node 429 responses and fail closed. No JSON-RPC batching, Multicall contract dependency, cache, or serial fallback is introduced. Heads must still be at most 60 seconds old or five seconds ahead, and before/after heads must match. Slow nodes can still fail conservatively; callers may retry preflight within their own budget, never infer permission to start another transaction.

The self-hosted facilitator **requires full simulation** for `/verify` and fresh-payment `/settle`, using its configured `ExecutorAddress`. Simulation failure returns `settlement_simulation_failed` without creating a reservation, signing or broadcasting. This strengthens admission: nodes unable to simulate the full call cannot admit fresh payments. Previously submitted/pending/succeeded records still recover only the original transaction, without re-simulation or another payment. Simulation success does not establish account permissions, sufficient Energy/Bandwidth or fees, reserve funds, or prove broadcast or solidified receipt.

TRON [eth_call](https://developers.tron.network/reference/eth_call) and [eth_getCode](https://developers.tron.network/reference/eth_getcode) read latest state, not historical state selected by block references. Matching before/after heads is a drift check, **not an atomic snapshot or finality guarantee**. Revalidate before settlement and verify actual execution outcomes.

HTTPS is required by default; redirects are blocked. API keys use only the `TRON-PRO-API-KEY` header. URLs cannot contain credentials, query or fragment. An optional `HTTPClient` supports trusted CA/proxy settings without in-place mutation. Explicit `AllowInsecureLocalHTTP` permits only loopback/localhost development nodes. Never derive node configuration from payment requests.

## Wallet-free deployment checks

The same `PreflightVerifier` offers `CheckDeployment(ctx, assetAddress, now)`. Without a payer, signature or valid quote, it checks the configured chain ID, fresh head, pinned Permit2/proxy runtime hashes, nonempty token code, `PERMIT2()`, two-field `WITNESS_TYPEHASH()` and `DOMAIN_SEPARATOR()`, then rechecks chain/head/freshness. A normal round still uses 10 read-only requests in concurrent groups: boundary pair → six contract reads → boundary pair. The existing total budget and shared six-request limit apply, without automatic retries. It returns a `DeploymentResult`; existing payment verification and settlement remain unchanged.

Set the following environment variables, then run the [deployment-check example](examples/deployment/main.go) with `go run ./examples/deployment`:

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

### Reproduced Nile contract builds

On 2026-10-18, local builds from the following pinned sources and compilers, linked only through compiler-declared `immutableReferences` using known constructor values, matched Nile `eth_getCode` **byte for byte, including metadata**. This is not merely an ABI/getter/length comparison, metadata-stripped comparison, or trust in a node-reported hash.

| Item | Permit2 | exact proxy |
| --- | --- | --- |
| Address | `TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h` | `TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F` |
| Pinned source | [SUN Permit2 67b1f91](https://github.com/sun-protocol/sunswap-permit2/tree/67b1f91662424416e50d05d734f9fd3856039cc5), including vendored solmate | [BofAI contracts 82aa2b2](https://github.com/BofAI/x402-contracts/tree/82aa2b2b761ebe61f08b4e6ae2558846c67ded47), OpenZeppelin 5.6.1 |
| Compiler | solc `0.8.17+commit.8df45f5f` | solc `0.8.25+commit.b61c2a91` |
| Settings | optimizer 999999, viaIR, London | optimizer 999999, Cancun |
| Runtime bytes | 9193 | 3593 |
| Runtime Keccak-256 | `0xfdd5ad3c59a61e0d232cc5ec5fe684bbe3083990c0873441a6c928bb452a4705` | `0x45b702594297f3ce7ea10f4ff622608653dd3ce289445269686607ac1d5f626d` |

Both use `metadata.useLiteralContent=true`, matching the [deployment plugin setting](https://github.com/sun-protocol/sunhat/blob/2efcfb61092024c69fb86f0568edc4be5c7798b7/src/index.ts#L326). Permit2 immutables use the Nile chain ID and its address to derive the domain; the proxy links the Permit2 address above. **These are reproducible standard solc builds, not the same-numbered TRON fork compiler**: that fork produced a different proxy runtime and must not be substituted.

Compiler SHA-256: 0.8.17 is `617828e63be485c7cc2dbcbdd5a22b582b40fafaa41016ad595637b83c90656c`; 0.8.25 is `f8c9554471ff2db3843167dffb7a503293b5dc728c8305b044ef9fd37d626ca7`, from the [official Solidity compiler index](https://github.com/ethereum/solc-bin/blob/gh-pages/wasm/list.json). These are explicitly checked reference settings, **not automatically selected SDK/example defaults**; code reproduction alone is not Mainnet configuration, a security audit, or proof of payment. Head drift can still reject a preflight round and must not be bypassed. See the separate single-payment evidence and unverified scope above.

## Constrained settlement construction and external signing

`exact.NewSettlementBuilder` requires trusted `Authorization`, an explicit gas-paying `ExecutorAddress`, positive `FeeLimitSUN`, and a whole-millisecond `Lifetime` greater than 15 seconds and at most 10 minutes. No default mainnet/contracts, private-key custody or arbitrary node-built transaction JSON.

`Build` validates payment authorization and uses a trusted `ReferenceBlock` plus server time to locally construct one exact-proxy `settle` call, TAPOS and TRON raw Protobuf. The transaction ID is `SHA-256(raw_data)`. Reference network, height and ID must match; block time may be at most 60 seconds old or five seconds ahead. This does not prove chain membership, finality or honest RPC responses: deployment and chain preflight remain required. Expiry is capped by the payment deadline; construction and both sides of signing require at least 15 seconds remaining.

The returned `PreparedSettlement` is immutable. `RawData()` returns a copy containing the payment authorization signature: keep it confidential. `Sign(ctx, DigestSigner, now)` sends only the fixed SHA-256 digest to the caller's external signer, then recovers the gas payer's public key. Elapsed signing time counts against validity; original signer error messages are not exposed. Signatures are low-s, 65-byte `r||s||v`; accept v=0/1 or 27/28 and emit canonical v=0/1. The payer's TIP-712 authorization and the executor's transaction signature are distinct; their digests are not interchangeable.

`SettlementTransaction.Payload` is **complete binary Transaction Protobuf, not JSON**, suitable for a later hex-encoded [broadcasthex](https://developers.tron.network/reference/broadcasthex) request. `ValidateSigned` accepts only this prepared object's canonical encoding and checks all raw bytes, ID and signature. Unknown/duplicate fields, attached TRX/TRC10, extra contracts/signatures or changed fee policy are rejected. This is not a generic transaction importer, current-expiry check or receipt proof.

Only a single EOA using default owner permission is supported; no multisig, custom permissions or contract wallets. On-chain permissions, fee resources and execution outcomes still require verification. The builder neither loads keys nor determines settlement success. Persist the unique signed transaction and obtain journal broadcast permission before submission; never extend or rebuild a payment during recovery. The public [TronWeb settlement vector](exact/testdata/tronweb-settlement.json) uses a synthetic block and a discarded random executor key: encoding/signature interoperability only, not live testnet acceptance.

## Single-attempt broadcast and original-transaction lookup

`exact.NewExecutionClient` requires trusted `ExecutionConfig`: fixed `Network`, native FullNode root `NodeURL`, and an independently verified `GenesisBlockID` (64 hex digits without `0x`, zero first eight bytes, last four bytes matching the TRON chain ID). Never select nodes from payment requests or learn genesis identity on first use. Synthetic test IDs are not deployment configuration. HTTPS is required except explicitly enabled loopback HTTP. No path, query, fragment or userinfo in the URL. Total timeout defaults to 15 seconds, at most one minute; responses are capped at 1 MiB. Redirects are blocked; errors exclude URLs, API keys and original node messages. Custom HTTP transports are trusted configuration and must not retry writes internally.

- Low-level `Broadcast(ctx, prepared, transaction, now)` requires prior durable journal permission. It validates bytes/signature/network/remaining lifetime, pins block zero from **the same native node**, makes one `/wallet/broadcasthex` request, then rechecks network identity. Only explicit acceptance with a successful (or omitted) code and matching original txid acknowledges this attempt, not settlement. Network-check time counts against pre-send validity.
- `Lookup(ctx, txid)` pins genesis before/after `/wallet/gettransactionbyid` and validates txID against `SHA-256(raw_data_hex)`. `TransactionObservation.Found` is **not execution success, finality or receipt of funds**. Only an empty object means currently not found. Timeouts, ambiguous/duplicate top-level JSON keys, bad hashes and network mismatches return errors. Neither absence nor rejection authorizes reservation release, re-signing or replacement.

These requests were exercised by the single real Nile settlement above; automated tests still use simulated nodes. Node acceptance, a FullNode body, execution and solidified payment events are distinct evidence layers. Neither method declares success; use the read-only checks below. No malicious-node state proofs are provided.

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

Compare `prepared.Authorization()` with the original trusted journal identity before calling the read-only solidified checker. Only the package's fixed single-contract, single-EOA-signature canonical encoding is accepted; other valid TRON transactions may be rejected. This is not a generic transaction importer. `ConfirmSettlement` below combines recovery, checking and durable success and is used by the facilitator and its HTTP example. One real payment does not cover every interruption window; simulated tests are not deployment acceptance.

## Durable settlement journal (optional PostgreSQL)

`exact/postgres.New` accepts a caller-managed `pgxpool.Pool` and trusted `exact.Config`. Explicitly apply [schema.sql](exact/postgres/schema.sql), also available as `SchemaSQL`, under a dedicated trusted `search_path`; construction never runs DDL. Other packages do not require a database. No dependency on the payment gateway or merchant ledger.

**Existing journals must be upgraded first:** back up and stop old writers, explicitly run [upgrade_confirmation.sql](exact/postgres/upgrade_confirmation.sql), also available as `UpgradeConfirmationSQL`, in the same trusted `search_path`, then start the new version. The transactional script adds confirmation columns/constraints, is repeatable and preserves historical transactions/states; roll back any open transaction after failure. Reapplying `SchemaSQL` alone does not upgrade an existing table. Runtime accounts need no DDL permissions. Do not mix old binaries unaware of success semantics or roll back by deleting columns, clearing confirmations or downgrading states.

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

## Official facilitator mechanism (optional PostgreSQL)

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

## Development

Requires Go 1.26+. CI uses 1.26.x. No release exists; public APIs may change.

```bash
go mod verify
go vet . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
go test -race -count=1 -cover . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
go build . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
```

Inputs are strict: no whitespace trimming, unknown networks, wildcards, or decimal leading zeros. Namespace must be `tron`; legacy hexadecimal references are case-insensitive. Addresses must be 34-character Base58Check or 42-character `41`-prefixed hex. Arbitrary `0x` addresses are not accepted as TRON input. Formatting is not authorization verification.

Database tests require `X402_TEST_DATABASE_URL` pointing to a dedicated PostgreSQL database with permission to create temporary schemas. Each test creates and cleans only its own random schema. Without this variable integration tests explicitly skip; this is not a passing database acceptance run. CI provisions PostgreSQL 16 and runs them.

### Full HTTP payment and original-transaction recovery

After configuring a dedicated test database, run the [three-role integration test](exact/postgres/http_roundtrip_integration_test.go) independently:

```bash
go test -race -count=1 ./exact/postgres -run '^TestHTTPAutomaticPaymentRoundTripAndOriginalRecovery$'
```

The official HTTP client calls `exact.NewClient` from an actual 402 challenge to generate a random nonce and real signature. Test TLS resource and authenticated facilitator services then perform preflight, full simulation, journal persistence, synthetic-node broadcast/solidified checks, and delivery only after durable success. Cases cover simulation rejection without reservation/payment/delivery, over-budget quotes rejected before signing, pending without delivery, identical-authorization recovery retaining one transaction, and cached success with the node unavailable. Failures may also include `PAYMENT-RESPONSE`: inspect `success`, the error reason and original transaction ID rather than treating header presence as success.

This automated test generates its own ephemeral payer key, loads no existing wallet and sends no live-chain transaction; it is separate from the manually executed Nile validation above. Recovery reuses the original authorization. **Payment deduplication is not fulfillment deduplication**: replaying a resource request may execute its handler again; business delivery idempotency remains the integrator's responsibility. This does not replace clean-room example deployment, live payment or process-interruption acceptance.

### Bidirectional upstream TRON SDK interoperability

The [interop suite](interop/bankofai/interop.test.mjs) calls public core/client/facilitator APIs from actual published upstream packages rather than copying their algorithms or merely comparing JSON. Go uses this project's public client and the official Go core; the reverse direction uses upstream core/facilitator with real cryptographic signature verification. Nile/Shasta cover legacy extra and upfront, signature/amount/recipient/proxy/nonce/time/network tampering, and Go's trusted-domain/accepted bindings.

Only this optional suite needs Node.js 22+ (CI uses 24.x) and npm; Go runtime dependencies are unchanged. Explicitly prepare the Go module cache first:

```bash
go mod download
npm --prefix interop/bankofai ci --ignore-scripts --no-audit --no-fund
npm --prefix interop/bankofai test
```

Versions and complete tarball integrity are locked. During tests Node network access is blocked and the Go helper cannot download modules. Payers are random ephemeral keys, with no existing-wallet/funds input; signatures travel through in-memory pipes, not logs. The upstream client retains explicit asset/per-payment spend controls, without automatic approval. Facilitator balance/allowance reads are explicitly simulated; no transaction-writing capability is provided. **This proves authorization interoperability, not on-chain settlement.**

Upstream 1.2.0 actually uses `tron:0xcd8690dc` / `tron:0x94a9059e`. Tests register the same keys as its real quotes, without silently rewriting them to decimal or patching SDK constants. This project's default remains canonical decimal. Newer source-level alias support does not imply support in that published package. A separate test records rejection of decimal quotes; no all-version/default-HTTP compatibility is claimed. Align quote/discovery/registration network profiles and verify the exact counterpart version before integration.

Tests cover public TronWeb vectors, tampering, amount/fee/time boundaries, simulated nodes and PostgreSQL concurrency/recovery. Solidified checks cover original bytes/block membership, execution, changed heads/blocks, exact/duplicate/reversed events, malformed JSON and network/timeouts. Recovery/persistence cover upgrades/rollback, constraints, conflicting evidence, delayed callbacks, SQL failure and lost responses after actual commit. Facilitator tests cover official SDK/HTTP authentication, input/capacity/error redaction, dynamic ephemeral signatures, 16 concurrent calls with single signing/broadcast, expired/consumed-nonce recovery, pending/node failures and never broadcasting uncommitted bytes. Automated receipts/blocks are synthetic; live exercises are recorded separately above. Linux CI also builds and launches three executable examples with a random PostgreSQL schema and self-signed TLS synthetic node. macOS system roots do not read SSL_CERT_FILE, so that synthetic-process case explicitly skips there without bypassing TLS. Next: standalone live-example integration and prerelease packaging before gateway integration.

## Build testnet candidate packages

Run from a clean, committed checkout with a trusted Go toolchain. The output parent must be an absolute directory outside the repository:

```bash
go run ./tools/package -output /absolute/path/outside-repository
```

Defaults: macOS/Linux, amd64/arm64; use e.g. `-targets linux/amd64` for a smaller matrix. No Git tag or GitHub release is created. Each bundle includes `deployment`, `facilitator`, `server`, `payer`, configuration template, both READMEs, database schema/upgrade SQL, project and compiled-dependency license texts, and source/toolchain metadata. The source archive uses only `git archive`, never ignored local directories. `manifest.json` records the revision and archive hashes; check `SHA256SUMS` with `shasum -a 256 -c SHA256SUMS`. Hashes provide integrity, not source authentication/signatures.

Fixed build flags, archive ordering/timestamps/modes permit comparison of repeated builds with the same commit, Go version and target matrix; different Go patch versions need not produce identical bytes. Dirty trees, existing output directories, missing dependency licenses and module replacements are rejected. Run extracted `bin/<program>` with the configuration described above; `.env.example` is not loaded automatically. Do not use Mainnet or unlimited approval. These are **unaudited testnet examples**, not a custody service, production deployment guarantee or stable release.

## References and status

- [TRON address specification](https://developers.tron.network/docs/account)
- [Official x402 Go SDK](https://github.com/x402-foundation/x402/tree/main/go)
- [TRON ecosystem reference](https://github.com/BofAI/x402)

[Apache-2.0 licensed](LICENSE). See [third-party notices](THIRD_PARTY_NOTICES.md). No stable release or independent security audit yet.
