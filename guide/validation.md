# Testing and validation

[简体中文](validation.zh-CN.md) | English

How to run the test suites, the upstream SDK interoperability tests, the recorded Nile live payments, reproduced contract builds and testnet packages.

## Running tests

Requires Go 1.26.10+. CI uses the latest 1.26.x patch. Only a testnet release candidate exists; public APIs may change.

```bash
go mod verify
go vet . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
go test -race -count=1 -cover . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
go build . ./exact ./exact/postgres ./examples/client ./examples/server ./examples/facilitator
```

Inputs are strict: no whitespace trimming, unknown networks, wildcards, or decimal leading zeros. Namespace must be `tron`; legacy hexadecimal references are case-insensitive. Addresses must be 34-character Base58Check or 42-character `41`-prefixed hex. Arbitrary `0x` addresses are not accepted as TRON input. Formatting is not authorization verification.

Database tests require `X402_TEST_DATABASE_URL` pointing to a dedicated PostgreSQL database with permission to create temporary schemas. Each test creates and cleans only its own random schema. Without this variable integration tests explicitly skip; this is not a passing database acceptance run. CI provisions PostgreSQL 16 and runs them.

### Full HTTP payment and original-transaction recovery

After configuring a dedicated test database, run the [three-role integration test](../exact/postgres/http_roundtrip_integration_test.go) independently:

```bash
go test -race -count=1 ./exact/postgres -run '^TestHTTPAutomaticPaymentRoundTripAndOriginalRecovery$'
```

The official HTTP client calls `exact.NewClient` from an actual 402 challenge to generate a random nonce and real signature. Test TLS resource and authenticated facilitator services then perform preflight, full simulation, journal persistence, synthetic-node broadcast/solidified checks, and delivery only after durable success. Cases cover simulation rejection without reservation/payment/delivery, over-budget quotes rejected before signing, pending without delivery, identical-authorization recovery retaining one transaction, and cached success with the node unavailable. Failures may also include `PAYMENT-RESPONSE`: inspect `success`, the error reason and original transaction ID rather than treating header presence as success.

This automated test generates its own ephemeral payer key, loads no existing wallet and sends no live-chain transaction; it is separate from the manually executed Nile validation above. Recovery reuses the original authorization. **Payment deduplication is not fulfillment deduplication**: replaying a resource request may execute its handler again; business delivery idempotency remains the integrator's responsibility. This does not replace clean-room example deployment, live payment or process-interruption acceptance.

### Bidirectional upstream TRON SDK interoperability

The [interop suite](../interop/bankofai/interop.test.mjs) calls public core/client/facilitator APIs from actual published upstream packages rather than copying their algorithms or merely comparing JSON. Go uses this project's public client and the official Go core; the reverse direction uses upstream core/facilitator with real cryptographic signature verification. Nile/Shasta cover legacy extra and upfront, signature/amount/recipient/proxy/nonce/time/network tampering, and Go's trusted-domain/accepted bindings.

Only this optional suite needs Node.js 24.22.1+ (required by the upstream package; CI uses 24.x) and npm; Go runtime dependencies are unchanged. Explicitly prepare the Go module cache first:

```bash
go mod download
npm --prefix interop/bankofai ci --ignore-scripts --no-audit --no-fund
npm --prefix interop/bankofai test
```

Versions and complete tarball integrity are locked. During tests Node network access is blocked and the Go helper cannot download modules. Payers are random ephemeral keys, with no existing-wallet/funds input; signatures travel through in-memory pipes, not logs. The upstream client retains explicit asset/per-payment spend controls, without automatic approval. Facilitator balance/allowance reads are explicitly simulated; no transaction-writing capability is provided. **This proves authorization interoperability, not on-chain settlement.**

Upstream 2.1.2 uses decimal `tron:3448148188` / `tron:2494104990`, matching the Go root package defaults, and normalizes `tron:0xcd8690dc` / `tron:0x94a9059e` to decimal; a dedicated test asserts this and fails if upstream changes, requiring re-review. Tests do not patch SDK constants, and no all-version/default-HTTP compatibility is claimed; older `1.x` releases recognize only hexadecimal. Verify the counterpart's exact version and quote/discovery/registration network profile before integration.

Tests cover public TronWeb vectors, tampering, amount/fee/time boundaries, simulated nodes and PostgreSQL concurrency/recovery. Solidified checks cover original bytes/block membership, execution, changed heads/blocks, exact/duplicate/reversed events, malformed JSON and network/timeouts. Recovery/persistence cover upgrades/rollback, constraints, conflicting evidence, delayed callbacks, SQL failure and lost responses after actual commit. Facilitator tests cover official SDK/HTTP authentication, input/capacity/error redaction, dynamic ephemeral signatures, 16 concurrent calls with single signing/broadcast, expired/consumed-nonce recovery, pending/node failures and never broadcasting uncommitted bytes. Automated receipts/blocks are synthetic; live exercises are recorded separately in [Nile live payments](#nile-live-payments). Linux CI also builds and launches three executable examples with a random PostgreSQL schema and self-signed TLS synthetic node. macOS system roots do not read SSL_CERT_FILE, so that synthetic-process case explicitly skips there without bypassing TLS. Testnet-candidate scope is complete; gateway integration or version publication is a separate step, never automatic Mainnet operation.

## Nile live payments

Core revision `39b83a63fab27715306cf6fb273a9dd7184195e5`, Go 1.26.5 and official Go SDK v2.28.0 used the source-reproduced Permit2/proxy below, Nile test USDT `TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf`, separate payer/executor/recipient addresses and dedicated PostgreSQL. A one-off local integration program used public SDK APIs and official HTTP middleware with two loopback TLS services and real nodes/receipts; core code and admission checks were unchanged.

- After a limited 0.003 test-USDT approval, a real 402 quote produced one 0.001 test-USDT authorization. Full simulation, durable unique transaction, single broadcast, solidified checks and persisted success preceded HTTP 200 resource delivery.
- Settlement [135dfb80…23efe5](https://nile.tronscan.org/#/transaction/135dfb80322290839444d93864f8190ae16cb0aadb1b9d9621110f96b823efe5), solidified block `71933724`, with one exact Transfer and matching balance changes. Approval cost 0 and settlement cost 7.237 test TRX; these are observed fees, not future estimates.
- After normal process exit, a new process replayed the identical authorization and returned the same txid from the existing `succeeded` record. Additional authorization signatures, transaction signatures and broadcasts were all zero; original signed bytes, balances and fees were unchanged. **This is terminal-success recovery across processes, not a forced crash while pending.**

A second real payment verified pending-interruption recovery: settlement [3a339895…18873a](https://nile.tronscan.org/#/transaction/3a339895a1c7f3e66370e33e8bc71c729ff287c826dcf82129fa6ffd7318873a) was journaled as `pending`, absent from both solidified body/receipt endpoints, and not delivered when its executor process was SIGSTOP/SIGKILL-terminated and reaped. A new process loaded no private keys and reused the original header, database and 603 signed bytes, delivering only after solidification in block `71934525`. Additional authorization signatures, transaction signatures and broadcasts were zero. The first record stayed unchanged; payer/recipient changed by exactly 0.001 test USDT. The second fee was 5.737 test TRX, totaling 12.974 for both payments.

The third payment ran three independent executables extracted from actual archives built from clean commit `3f1beac5d027598a30e1a03b2b2503880c7d06c5`, using the same dedicated journal, remaining limited allowance and `X402_RPC_BATCH=true`. Settlement [ea6149a0…1f9249d](https://nile.tronscan.org/#/transaction/ea6149a03be449d61d31770edfba94c2d2690299d6c688a004b5a51431f9249d) solidified in block `71935578` before HTTP200 delivery. The resource server and facilitator both stopped and restarted as new processes; payer recovery loaded no key and returned the same transaction from the original header, retaining the 603 signed bytes/hash and both earlier records. The database runtime role lacked DDL/DELETE/TRUNCATE permissions; login was disabled and services stopped afterward. Independent read-only audit verified the exact Transfer, 0.003 test USDT total across three payments, 18.711 test TRX total fees, and zero remaining allowance, without another approval.

On 2026-11-11 a **cross-SDK live payment** was completed: the payer was the actual published upstream `@bankofai/x402-tron@2.1.2` with the official `@x402/core@2.29.0` (public APIs only; the signer had no chain read/write capability), calling a Go resource server and Go facilitator built from clean commit `1bf1019` with Go 1.26.11 and official Go SDK v2.30.0, using a new dedicated journal database and a least-privilege runtime role. After a limited 0.002 test-USDT approval (fee 0), two 0.001 test-USDT payments were generated from real 402 quotes: [da6c5611…71d4b82](https://nile.tronscan.org/#/transaction/da6c5611d3e7741e930f63ac775bd31e45443a2f8d6cf0f3ed35d6c0571d4b82) (block `72617132`) and [2bb10acb…7d83090](https://nile.tronscan.org/#/transaction/2bb10acb83ed2d53a023f55fa6b2cee74ce1dfa23c7f896fe164a38747d83090) (block `72617154`); HTTP 200 was returned only after solidified verification and persistence. Independent read-only audit: payer −0.002 and recipient +0.002 test USDT, allowance back to zero, 5.737 test TRX per settlement. This proves complete HTTP payment interoperability for upstream JS payer → Go resource server/facilitator.

The same day the **reverse direction** was completed: Go payer (`examples/payer`) → Go resource server → upstream `@bankofai/x402-tron@2.1.2` facilitator (behind a local HTTP wrapper with an 11 TRX fee limit and a bounded number of writes). Two 0.001 test-USDT settlements, [e6f7d422…f2379bfe](https://nile.tronscan.org/#/transaction/e6f7d422c8e20796d487f3e21eb903e5301714b83599cc2259403abdf2379bfe) (block `72617474`) and [5a63e5e7…f079e1e1d](https://nile.tronscan.org/#/transaction/5a63e5e73e20a90787bcda3c0ea724d1a03ef3477d464a59db8627df079e1e1d) (block `72617628`), solidified successfully with exact balance changes. Three interoperability differences were observed:

- The upstream receipt reports `payer` in 0x signing form and omits the optional `amount`; the example payer compared them literally and reported unconfirmed. Fixed in `64f8e75`: compare against the configured payer and check `amount` only when present.
- The upstream facilitator has no durable journal and keeps no record after success: resubmitting the same authorization broadcasts again and the chain reverts because the nonce is spent ([4a491674…](https://nile.tronscan.org/#/transaction/4a491674e88d0015d53f0feae1f3d666ffd4a660d3ba851c7de6b93bc97dcc1d), 1.71 test TRX, no transfer). Payers using an upstream facilitator should not automatically replay a request that already returned a txid; reported upstream as [BofAI/x402#122](https://github.com/BofAI/x402/issues/122).
- Upstream returns `success=true` once the transaction is packed, before the solidity node has it; this project's facilitator returns success only after solidified verification of the exact transfer.

This completes the v0.1 testnet-candidate scope for Nile payments, interruption recovery and packaged three-process integration. It uses existing schema/independently checked contracts and local loopback services, not blank-machine installation, independent-server TLS operations or production acceptance. Public-RPC head drift can still reject new payments. Shasta/Mainnet, long-running stability and independent security remain unvalidated. Private keys, original authorizations and local operational materials are not published; no copy-ready production-funds configuration is supplied. Candidate packages can be built locally; testnet release candidate `v0.1.0-rc.3` is published as a GitHub prerelease, with no stable release yet.

## Reproduced Nile contract builds

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

## Build testnet candidate packages

Run from a clean, committed checkout with a trusted Go toolchain. The output parent must be an absolute directory outside the repository:

```bash
go run ./tools/package -output /absolute/path/outside-repository
```

Defaults: macOS/Linux, amd64/arm64; use e.g. `-targets linux/amd64` for a smaller matrix. No Git tag or GitHub release is created. Each bundle includes `deployment`, `facilitator`, `server`, `payer`, configuration template, both READMEs, the guides, security policy and threat model, database schema/upgrade SQL, project and compiled-dependency license texts, and source/toolchain metadata. The source archive uses only `git archive`, never ignored local directories. `manifest.json` records the revision and archive hashes; check `SHA256SUMS` with `shasum -a 256 -c SHA256SUMS`. Hashes provide integrity, not source authentication/signatures.

Fixed build flags, archive ordering/timestamps/modes permit comparison of repeated builds with the same commit, Go version and target matrix; different Go patch versions need not produce identical bytes. Dirty trees, existing output directories, missing dependency licenses and module replacements are rejected. Run extracted `bin/<program>` with the configuration described in the [resource server](resource-server.md) and [facilitator](facilitator.md) guides; `.env.example` is not loaded automatically. Do not use Mainnet or unlimited approval. These are **unaudited testnet examples**, not a custody service, production deployment guarantee or stable release.
