# Testing and validation

[简体中文](validation.zh-CN.md) | English

This page explains how to run the test suites and what each suite covers. It also records the live payments on the Nile testnet, the reproduced contract builds and how to build testnet packages.

## Running tests

Tests require Go 1.26.10 or later. CI uses the latest 1.26.x patch release. Only a testnet release candidate exists, so public APIs may still change.

The commands below match the package list used in CI:

```bash
go mod verify
go vet . ./exact ./exact/postgres ./examples/client ./examples/payer ./internal/examplekeys ./examples/server ./examples/facilitator ./examples/deployment ./interop/bankofai/bridge ./tools/package
go test -race -count=1 -cover . ./exact ./exact/postgres ./examples/client ./examples/payer ./internal/examplekeys ./examples/server ./examples/facilitator ./examples/deployment ./interop/bankofai/bridge ./tools/package
go build . ./exact ./exact/postgres ./examples/client ./examples/payer ./internal/examplekeys ./examples/server ./examples/facilitator ./examples/deployment ./interop/bankofai/bridge
```

### Database tests

PostgreSQL integration tests need `X402_TEST_DATABASE_URL`. It must point to a dedicated PostgreSQL test database with permission to create temporary schemas.

- Each test creates its own random schema and cleans up only that schema.
- Without the variable, the integration tests skip explicitly. A skipped run is not a passing database acceptance run.
- CI provisions PostgreSQL 16 and runs these tests.

### Node.js

Only the optional [upstream SDK interoperability suite](#upstream-tron-sdk-interoperability) needs Node.js and npm. All other tests use Go only.

### Input rules

Inputs are strict. The tests rely on the following rules:

- Whitespace is not trimmed.
- Unknown networks, wildcards and decimal leading zeros are rejected.
- The namespace must be `tron`. Legacy hexadecimal references are case-insensitive.
- Addresses must be 34-character Base58Check or 42-character hex with a `41` prefix. Arbitrary `0x` addresses are not accepted as TRON input.
- Formatting an address is not authorization verification.

### Test coverage

| Area | What is covered |
| --- | --- |
| General | Addresses, networks, public TronWeb encoding and signature vectors, tampering, authorization, amount, fee and time boundaries, simulated nodes, PostgreSQL concurrency and recovery |
| Solidified checks | Historical expired transactions, original bytes and block membership, execution status, changed heads and blocks, exact, duplicate and reversed events, malformed and nested duplicate JSON, network failures, timeouts and concurrency |
| Recovery and persistence | Upgrades and rollback, constraints, conflicting evidence, delayed callbacks, SQL failure, lost responses after an actual commit |
| Facilitator | Official SDK and HTTP authentication, input and capacity limits, error redaction, dynamic ephemeral signatures, 16 concurrent calls with a single signing and broadcast, recovery of expired and consumed nonces, pending and node failures, never broadcasting uncommitted bytes |

Automated receipts and blocks are synthetic fixtures. Live exercises are recorded separately in [Nile live payments](#nile-live-payments).

Linux CI also builds and launches three executable examples. They use a random PostgreSQL schema and a synthetic node with a self-signed TLS certificate. macOS system roots do not read `SSL_CERT_FILE`, so this synthetic-process case skips explicitly on macOS. It does not bypass TLS.

The testnet-candidate scope is complete. Gateway integration and version publication are separate steps. Nothing here operates on Mainnet automatically.

## HTTP payment round trip and original-transaction recovery

The [three-role integration test](../exact/postgres/http_roundtrip_integration_test.go) runs a full HTTP payment. Configure a dedicated test database first, then run it on its own:

```bash
go test -race -count=1 ./exact/postgres -run '^TestHTTPAutomaticPaymentRoundTripAndOriginalRecovery$'
```

How the test works:

1. The official HTTP client receives a real 402 challenge.
2. It calls `exact.NewClient` to generate a random nonce and a real signature.
3. A test TLS resource server and an authenticated facilitator service run preflight and full simulation, and persist the journal entry.
4. A synthetic node handles the broadcast and the solidified checks.
5. The resource is delivered only after durable success.

The test cases cover:

- Simulation rejection, with no reservation, payment or delivery.
- Over-budget quotes, rejected before signing.
- Pending results, with no delivery.
- Recovery with the identical authorization, which keeps one transaction.
- Cached success while the node is unavailable.

A failure response may also include `PAYMENT-RESPONSE`. Inspect `success`, the error reason and the original transaction ID. Do not treat the presence of the header as success.

What the test does and does not prove:

- It generates its own ephemeral payer key. It loads no existing wallet and sends no live-chain transaction. It is separate from the manual [Nile live payments](#nile-live-payments).
- Recovery reuses the original authorization and does not create a new one.
- **Payment deduplication is not fulfillment deduplication.** Replaying a resource request may run its handler again. Idempotent business delivery remains the integrator's responsibility.
- It does not replace clean-room example deployment, live payment or process-interruption acceptance.

## Upstream TRON SDK interoperability

The [interop suite](../interop/bankofai/interop.test.mjs) tests both directions against the actual published upstream packages. It calls their public core, client and facilitator APIs. It does not copy their algorithms or merely compare JSON.

- **Go side.** Go uses this project's public client and the official Go core.
- **Reverse direction.** The upstream core and facilitator verify the authorizations.
- Signature verification is real cryptography in both directions.

Coverage on Nile and Shasta:

- Legacy `extra` and the `upfront` flow.
- Tampering with the signature, amount, recipient, proxy, nonce, time and network.
- Go's trusted-domain and `accepted` bindings.

### Running the suite

The suite needs Node.js 24.22.1 or later, which the upstream package requires, and npm. CI uses Node 24.x. Go runtime dependencies are unchanged.

Prepare the Go module cache first, then install and run:

```bash
go mod download
npm --prefix interop/bankofai ci --ignore-scripts --no-audit --no-fund
npm --prefix interop/bankofai test
```

### Isolation

- Package versions and complete tarball integrity are locked.
- During the tests, Node network access is blocked and the Go helper cannot download modules.
- Payers are random ephemeral keys. No existing wallet or funds are used.
- Signatures travel through in-memory pipes. They are not logged.
- The upstream client keeps explicit asset and per-payment spend controls. There is no automatic approval.
- Facilitator balance and allowance reads are explicitly simulated. No transaction-writing capability is provided.

**This proves authorization interoperability, not on-chain settlement.**

### Network identifiers

Upstream 2.1.2 uses decimal `tron:3448148188` and `tron:2494104990`. These match the Go root package defaults. Upstream also normalizes `tron:0xcd8690dc` and `tron:0x94a9059e` to decimal.

A dedicated test asserts this behavior. If upstream changes, the test fails and the integration must be reviewed again.

- The tests do not patch SDK constants.
- Compatibility with all upstream versions, or with the default HTTP flow, is not claimed.
- Older `1.x` releases recognize only hexadecimal identifiers.

Before integrating, verify the counterpart's exact version and the network profile it uses for quotes, discovery and registration.

## Nile live payments

All runs used Nile test USDT `TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf` and the source-reproduced Permit2 and proxy contracts described in [Reproduced Nile contract builds](#reproduced-nile-contract-builds). Fees are observed values, not estimates of future fees.

| Run | Date | Direction | Transactions | Result | Fee (test TRX) |
| --- | --- | --- | --- | --- | --- |
| 1 | 2026-10-18 | Go client to Go facilitator | [135dfb80…23efe5](https://nile.tronscan.org/#/transaction/135dfb80322290839444d93864f8190ae16cb0aadb1b9d9621110f96b823efe5) | Delivered; terminal-success recovery across processes | 7.237 |
| 2 | 2026-10-18 | Go client to Go facilitator | [3a339895…18873a](https://nile.tronscan.org/#/transaction/3a339895a1c7f3e66370e33e8bc71c729ff287c826dcf82129fa6ffd7318873a) | Delivered after pending-interruption recovery | 5.737 (12.974 for runs 1 and 2) |
| 3 | 2026-10-18 | Packaged Go payer, server and facilitator | [ea6149a0…1f9249d](https://nile.tronscan.org/#/transaction/ea6149a03be449d61d31770edfba94c2d2690299d6c688a004b5a51431f9249d) | Delivered; recovery after server and facilitator restart | 5.737 (18.711 for runs 1 to 3) |
| 4 | 2026-11-11 | Upstream JS payer to Go resource server and Go facilitator | [da6c5611…71d4b82](https://nile.tronscan.org/#/transaction/da6c5611d3e7741e930f63ac775bd31e45443a2f8d6cf0f3ed35d6c0571d4b82), [2bb10acb…7d83090](https://nile.tronscan.org/#/transaction/2bb10acb83ed2d53a023f55fa6b2cee74ce1dfa23c7f896fe164a38747d83090) | Both delivered | 5.737 per settlement |
| 5 | 2026-11-11 | Go payer to Go resource server to upstream facilitator | [e6f7d422…f2379bfe](https://nile.tronscan.org/#/transaction/e6f7d422c8e20796d487f3e21eb903e5301714b83599cc2259403abdf2379bfe), [5a63e5e7…f079e1e1d](https://nile.tronscan.org/#/transaction/5a63e5e73e20a90787bcda3c0ea724d1a03ef3477d464a59db8627df079e1e1d) | Both solidified; three interoperability differences found | 5.737 per settlement, plus 1.71 for a reverted replay |

### Setup for runs 1 to 3

- Go 1.26.5 and official Go SDK v2.28.0. Run 1 used core revision `39b83a63fab27715306cf6fb273a9dd7184195e5`, run 2 used `d04f500`, and run 3 used packaged builds from `3f1beac` (see below).
- Separate payer, executor and recipient addresses, and a dedicated PostgreSQL database.
- A one-off local integration program used the public SDK APIs and the official HTTP middleware.
- Two loopback TLS services talked to real nodes and produced real receipts.
- Core code and admission checks were not changed.

### Run 1: first payment and terminal-success recovery

- **Approval:** a limited approval of 0.003 test USDT. Fee 0.
- **Payment:** a real 402 quote produced one authorization for 0.001 test USDT.
- **Order of operations:** full simulation, a durable unique transaction, a single broadcast, solidified checks and persisted success all happened before the HTTP 200 resource delivery.
- **Transaction:** [135dfb80…23efe5](https://nile.tronscan.org/#/transaction/135dfb80322290839444d93864f8190ae16cb0aadb1b9d9621110f96b823efe5), solidified in block `71933724`. It contains one exact Transfer, and the balance changes match.
- **Fee:** 7.237 test TRX for the settlement.
- **Recovery:** after the process exited normally, a new process replayed the identical authorization. It returned the same txid from the existing `succeeded` record.
  - Additional authorization signatures, transaction signatures and broadcasts were all zero.
  - The original signed bytes, balances and fees were unchanged.
- **What it proved:** terminal-success recovery across processes. **It is not a forced crash while pending.**

### Run 2: pending-interruption recovery

- **Interruption:** settlement [3a339895…18873a](https://nile.tronscan.org/#/transaction/3a339895a1c7f3e66370e33e8bc71c729ff287c826dcf82129fa6ffd7318873a) was journaled as `pending`. At that point it was absent from both the solidified transaction and receipt endpoints, and nothing had been delivered. The executor process was then stopped with SIGSTOP, killed with SIGKILL and reaped.
- **Recovery:** a new process loaded no private keys. It reused the original header, the same database and the original 603 signed bytes.
- **Delivery:** only after the transaction solidified in block `71934525`.
- **Counters:** additional authorization signatures, transaction signatures and broadcasts were zero.
- **Checks:** the run 1 records were unchanged. Payer and recipient balances changed by exactly 0.001 test USDT.
- **Fee:** 5.737 test TRX. The total for runs 1 and 2 is 12.974 test TRX.
- **What it proved:** recovery of a real pending settlement after the executor process was killed.

### Run 3: packaged three-process integration

- **Build:** three independent executables extracted from real archives built from clean commit `3f1beac5d027598a30e1a03b2b2503880c7d06c5`.
- **Setup:** the same dedicated journal, the remaining limited allowance and `X402_RPC_BATCH=true`.
- **Transaction:** [ea6149a0…1f9249d](https://nile.tronscan.org/#/transaction/ea6149a03be449d61d31770edfba94c2d2690299d6c688a004b5a51431f9249d), solidified in block `71935578` before the HTTP 200 delivery.
- **Recovery:** the resource server and the facilitator were both stopped and restarted as new processes. Payer recovery loaded no key and returned the same transaction from the original header. The 603 signed bytes, the hash and both earlier records were unchanged.
- **Database role:** the runtime role had no DDL, DELETE or TRUNCATE permission. Login was disabled and the services were stopped afterward.
- **Audit:** an independent read-only audit verified the exact Transfer, a total of 0.003 test USDT across the three payments, total fees of 18.711 test TRX and zero remaining allowance. No further approval was made.
- **What it proved:** the packaged executables work end to end, and recovery survives a restart of both services.

### Run 4: upstream JS payer to Go (2026-11-11)

- **Payer:** the published upstream `@bankofai/x402-tron@2.1.2` with the official `@x402/core@2.29.0`. Only public APIs were used. The signer had no chain read or write capability.
- **Server side:** a Go resource server and a Go facilitator built from clean commit `1bf1019` with Go 1.26.11 and official Go SDK v2.30.0.
- **Database:** a new dedicated journal database and a least-privilege runtime role.
- **Approval:** a limited approval of 0.002 test USDT. Fee 0.
- **Transactions:** two payments of 0.001 test USDT, each generated from a real 402 quote.
  - [da6c5611…71d4b82](https://nile.tronscan.org/#/transaction/da6c5611d3e7741e930f63ac775bd31e45443a2f8d6cf0f3ed35d6c0571d4b82), block `72617132`.
  - [2bb10acb…7d83090](https://nile.tronscan.org/#/transaction/2bb10acb83ed2d53a023f55fa6b2cee74ce1dfa23c7f896fe164a38747d83090), block `72617154`.
- **Delivery:** HTTP 200 was returned only after solidified verification and persistence.
- **Audit:** an independent read-only audit found payer −0.002 and recipient +0.002 test USDT, and the allowance back at zero.
- **Fee:** 5.737 test TRX per settlement.
- **What it proved:** full HTTP payment interoperability from the upstream JS payer to the Go resource server and facilitator.

### Run 5: Go payer to upstream facilitator (2026-11-11)

- **Path:** Go payer (`examples/payer`) to a Go resource server to the upstream `@bankofai/x402-tron@2.1.2` facilitator.
- **Facilitator wrapper:** the upstream facilitator ran behind a local HTTP wrapper with an 11 TRX fee limit and a bounded number of writes.
- **Transactions:** two settlements of 0.001 test USDT. Both solidified with exact balance changes.
  - [e6f7d422…f2379bfe](https://nile.tronscan.org/#/transaction/e6f7d422c8e20796d487f3e21eb903e5301714b83599cc2259403abdf2379bfe), block `72617474`.
  - [5a63e5e7…f079e1e1d](https://nile.tronscan.org/#/transaction/5a63e5e73e20a90787bcda3c0ea724d1a03ef3477d464a59db8627df079e1e1d), block `72617628`.
- **Fees:** a limited approval of 0.002 test USDT cost 0.345 test TRX, and each settlement cost 5.737 test TRX. Including the reverted replay below, the run cost 13.529 test TRX.
- **What it proved:** payments work in the reverse direction. The run also found three interoperability differences:

1. **Receipt format.** The upstream receipt reports `payer` in `0x` signing form and omits the optional `amount`. The example payer compared these fields literally and reported the payment as unconfirmed. Fixed in `64f8e75`: the payer is now compared against the configured payer, and `amount` is checked only when present.
2. **Replay behavior.** The upstream facilitator has no durable journal and keeps no record after success. Resubmitting the same authorization broadcasts again, and the chain reverts because the nonce is already spent ([4a491674…](https://nile.tronscan.org/#/transaction/4a491674e88d0015d53f0feae1f3d666ffd4a660d3ba851c7de6b93bc97dcc1d), 1.71 test TRX, no transfer). Payers that use an upstream facilitator should not automatically replay a request that already returned a txid. Reported upstream as [BofAI/x402#122](https://github.com/BofAI/x402/issues/122).
3. **Success timing.** Upstream returns `success=true` as soon as the transaction is packed, before the solidity node has it. This project's facilitator returns success only after solidified verification of the exact transfer.

### Scope

These runs complete the v0.1 testnet-candidate scope for Nile payments, interruption recovery and packaged three-process integration.

What they used:

- An existing schema, independently checked contracts and local loopback services.

What they are not:

- Not a blank-machine installation.
- Not independent-server TLS operations.
- Not production acceptance.

Still not validated:

- Shasta and Mainnet.
- Long-running stability.
- Independent security review.

Other limits:

- Public-RPC head drift can still reject new payments.
- Private keys, original authorizations and local operational materials are not published.
- No copy-ready configuration for production funds is supplied.
- Candidate packages can be built locally. Testnet release candidate `v0.1.0-rc.3` is published as a GitHub prerelease. There is no stable release yet.

## Reproduced Nile contract builds

On 2026-10-18, local builds of the deployed Nile contracts matched Nile `eth_getCode` **byte for byte, including metadata**.

- The builds used the pinned sources and compilers in the table below.
- Linking used only the compiler-declared `immutableReferences`, filled with known constructor values.
- This is not an ABI, getter or length comparison. It is not a metadata-stripped comparison. It does not trust a hash reported by a node.

| Item | Permit2 | exact proxy |
| --- | --- | --- |
| Address | `TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h` | `TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F` |
| Pinned source | [SUN Permit2 67b1f91](https://github.com/sun-protocol/sunswap-permit2/tree/67b1f91662424416e50d05d734f9fd3856039cc5), including vendored solmate | [BofAI contracts 82aa2b2](https://github.com/BofAI/x402-contracts/tree/82aa2b2b761ebe61f08b4e6ae2558846c67ded47), OpenZeppelin 5.6.1 |
| Compiler | solc `0.8.17+commit.8df45f5f` | solc `0.8.25+commit.b61c2a91` |
| Compiler SHA-256 | `617828e63be485c7cc2dbcbdd5a22b582b40fafaa41016ad595637b83c90656c` | `f8c9554471ff2db3843167dffb7a503293b5dc728c8305b044ef9fd37d626ca7` |
| Settings | optimizer 999999, viaIR, London | optimizer 999999, Cancun |
| Runtime bytes | 9193 | 3593 |
| Runtime Keccak-256 | `0xfdd5ad3c59a61e0d232cc5ec5fe684bbe3083990c0873441a6c928bb452a4705` | `0x45b702594297f3ce7ea10f4ff622608653dd3ce289445269686607ac1d5f626d` |

Build details:

- Both builds use `metadata.useLiteralContent=true`, which matches the [deployment plugin setting](https://github.com/sun-protocol/sunhat/blob/2efcfb61092024c69fb86f0568edc4be5c7798b7/src/index.ts#L326).
- Permit2 immutables use the Nile chain ID and the Permit2 address to derive the domain.
- The proxy links the Permit2 address above.
- The compiler SHA-256 values come from the [official Solidity compiler index](https://github.com/ethereum/solc-bin/blob/gh-pages/wasm/list.json).

**These are reproducible standard solc builds, not the same-numbered TRON fork compiler.** The fork produced a different proxy runtime and must not be substituted.

Limits:

- These are explicitly checked reference settings. **They are not automatically selected SDK or example defaults.**
- Code reproduction alone is not Mainnet configuration, a security audit or proof of payment.
- Head drift can still reject a preflight round, and the check must not be bypassed.
- For payment evidence and the unvalidated scope, see [Nile live payments](#nile-live-payments).

## Building testnet candidate packages

Run the packaging tool from a clean, committed checkout with a trusted Go toolchain. The output parent must be an absolute directory outside the repository:

```bash
go run ./tools/package -output /absolute/path/outside-repository
```

### Targets

- The default targets are macOS and Linux, on amd64 and arm64.
- Use a flag such as `-targets linux/amd64` for a smaller matrix.
- The tool creates no Git tag and no GitHub release.

### Bundle contents

Each bundle contains:

- The `deployment`, `facilitator`, `server` and `payer` executables.
- The configuration template.
- Both READMEs and the guides.
- The security policy and the threat model.
- The database schema and upgrade SQL.
- License texts for the project and for every compiled dependency.
- Source and toolchain metadata.

The source archive is produced only by `git archive`. Ignored local directories are never included.

### Integrity

- `manifest.json` records the source revision and the archive hashes.
- Check the bundle with `shasum -a 256 -c SHA256SUMS`.
- Hashes provide integrity only. They are not source authentication or signatures.

### Reproducibility

Build flags, archive ordering, timestamps and file modes are fixed. Repeated builds with the same commit, Go version and target matrix can be compared. Different Go patch versions need not produce identical bytes.

The tool rejects dirty trees, existing output directories, missing dependency licenses and module replacements. It never overwrites an earlier package.

### Running the bundle

Run the extracted `bin/<program>` with the configuration described in the [resource server](resource-server.md) and [facilitator](facilitator.md) guides.

- The bundled `.env.example` is not loaded automatically.
- Do not use Mainnet or an unlimited approval.

These are **unaudited testnet examples**. They are not a custody service, a production deployment guarantee or a stable release.
