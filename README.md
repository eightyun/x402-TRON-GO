# x402-tron-go

[![CI](https://github.com/eightyun/x402-tron-go/actions/workflows/ci.yml/badge.svg)](https://github.com/eightyun/x402-tron-go/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/eightyun/x402-tron-go.svg)](https://pkg.go.dev/github.com/eightyun/x402-tron-go)
[![Release](https://img.shields.io/github/v/release/eightyun/x402-tron-go?include_prereleases&sort=semver)](https://github.com/eightyun/x402-tron-go/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

English | [简体中文](README.zh-CN.md)

**TRON support for [x402](https://x402.org) in Go.** x402-tron-go plugs the TRON network into the official [x402 Go SDK](https://github.com/x402-foundation/x402/tree/main/go), so clients, API servers and facilitators can accept HTTP 402 payments in TRC-20 tokens without writing any TRON-specific protocol code.

> [!WARNING]
> **Testnet release candidate.** This project has not been independently audited. Do not use it with real funds. It is not affiliated with the x402 Foundation or TRON.

## Features

- **Official SDK mechanisms** for all three x402 roles: `SchemeNetworkClient`, `SchemeNetworkServer` and `SchemeNetworkFacilitator`, using the `exact` scheme with Permit2 and the `upfront` payment flow.
- **A library, not a platform.** Import the packages into your own application. No hosted gateway, Docker image or separate service is required.
- **Durable facilitator (optional).** A PostgreSQL settlement journal guarantees that each authorization produces at most one broadcast, and that recovery after a crash, timeout or restart reuses the original signed transaction instead of paying twice.
- **Settlement is final before delivery.** Before a transaction is built or broadcast, the facilitator verifies the signature, contract code, balance, allowance and nonce, and simulates the full settlement call. Success is reported only after the transaction is solidified and its exact token transfer is confirmed.
- **Fail-closed inputs.** Strict decoding rejects unknown, duplicate and ambiguous fields. Responses from nodes are size-limited and checked against a pinned genesis block, and errors are redacted.
- **Interoperable** with the published TRON TypeScript SDK [`@bankofai/x402-tron`](https://www.npmjs.com/package/@bankofai/x402-tron), verified in both directions on the Nile testnet.

## Status

| Role | Package | Status |
| --- | --- | --- |
| Client / agent | `exact` | Ready for testnet |
| Resource server | `exact` | Ready for testnet; requires a trusted facilitator |
| Facilitator | `exact/postgres` | Ready for testnet; live Nile payments and crash recovery verified |

| Network | CAIP-2 ID | Status |
| --- | --- | --- |
| Nile testnet | `tron:3448148188` | Live payments verified |
| Shasta testnet | `tron:2494104990` | Offline tests only |
| Mainnet | `tron:728126428` | Not validated; the examples refuse it |

Releases are currently `v0.1.0-rc.*`. The public API may still change before `v0.1.0`.

## Installation

Requires **Go 1.26.10 or later** (the minimum required by the official x402 Go SDK). Use the latest Go 1.26 patch release to get standard-library security fixes.

```bash
go get github.com/eightyun/x402-tron-go/exact@v0.1.0-rc.3

# Only needed if you run your own durable facilitator
go get github.com/eightyun/x402-tron-go/exact/postgres@v0.1.0-rc.3
```

| Package | Purpose | Dependencies |
| --- | --- | --- |
| `github.com/eightyun/x402-tron-go` | TRON address and network utilities | Standard library only |
| `.../exact` | Client and resource-server mechanisms, offline verification, chain preflight, settlement building and verification | No database |
| `.../exact/postgres` | Durable facilitator, in-process client and HTTP handler | PostgreSQL via `pgx` |

## Quick start

All contract addresses below are Nile examples. In production, **verify every deployment address and runtime code hash yourself**; the SDK never learns them from a node or from a payment request.

### Client

Pay for x402-protected HTTP resources with the official HTTP client:

```go
mechanism, err := exact.NewClient(exact.ClientConfig{
    Authorization: exact.Config{
        Network:        tron.NetworkNile,
        Permit2Address: "TYQuuhGbEMxF7nZxUHV3uHJxAVVAegNU9h",
        ProxyAddress:   "TFGoaq2KjizijgjtkVxT7yjffW1A5T1j6F",
    },
    PayerAddress: signer.Address(),
    Signer:       signer.SignAuthorization, // or your own TIP-712 digest signer
    Asset:        asset,                    // TRC-20 token you are willing to pay with
    PayTo:        payTo,                    // recipient you trust
    MaxAmount:    "1000",                   // per-payment cap in atomic units
})
if err != nil {
    return err
}

network := x402.Network(mechanism.Network())
core := x402.Newx402Client(x402.WithSpendControls(x402.SpendControls{
    AllowedAssets: []x402.SpendControlAsset{{Network: network, Asset: asset, MaxAmountPerPayment: "1000"}},
})).Register(network, mechanism)

client := x402http.WrapHTTPClientWithPayment(&http.Client{Timeout: 15 * time.Second}, x402http.Newx402HTTPClient(core))
```

The payer must have granted the Permit2 contract a TRC-20 allowance beforehand. The SDK never approves tokens on its own. See the [client guide](guide/client.md).

### Resource server

Quote TRON prices and run handlers only after settlement succeeds:

```go
mechanism, err := exact.NewServer(exact.ServerConfig{Network: tron.NetworkNile, Asset: asset, PayTo: payTo})
if err != nil {
    return err
}

network := x402.Network(mechanism.Network())
routes := x402http.RoutesConfig{"GET /resource": {Accepts: []x402http.PaymentOption{{
    Scheme: "exact", Network: network, PayTo: payTo, Price: "1000", MaxTimeoutSeconds: 60,
}}}}
core := x402.Newx402ResourceServer(x402.WithFacilitatorClient(facilitator)).Register(network, mechanism)
server := x402http.Wrappedx402HTTPResourceServer(routes, core)
if err := server.Initialize(ctx); err != nil {
    return err
}
handler := nethttp.PaymentMiddlewareFromHTTPServer(server)(mux)
```

Prices are atomic-unit strings such as `"1000"`. Dollar amounts and floating-point prices are rejected. See the [resource server guide](guide/resource-server.md).

### Facilitator

Embed the durable facilitator directly in your resource server, with no extra service:

```go
store, err := postgres.New(pool, contracts) // apply postgres.SchemaSQL beforehand
if err != nil {
    return err
}
mechanism, err := postgres.NewFacilitator(store, postgres.FacilitatorConfig{
    Preflight:       exact.PreflightConfig{Authorization: contracts, NodeURL: jsonRPCURL, Permit2CodeHash: permit2Hash, ProxyCodeHash: proxyHash},
    Execution:       exact.ExecutionConfig{Network: tron.NetworkNile, NodeURL: fullNodeURL, GenesisBlockID: genesisID},
    Policy:          exact.ServerConfig{Network: tron.NetworkNile, Asset: asset, PayTo: payTo},
    ExecutorAddress: executor,       // pays the TRX fees
    Signer:          signTransaction, // signs the transaction digest
    MaxAmount:       "1000",
    FeeLimitSUN:     20_000_000,
    Lifetime:        time.Minute,
})
if err != nil {
    return err
}
facilitator, err := postgres.NewLocalClient(mechanism, 8) // pass to x402.WithFacilitatorClient
```

To run the facilitator as a separate service instead, use `postgres.NewHTTPHandler`. See the [facilitator guide](guide/facilitator.md) for configuration, the database schema and recovery semantics.

## How it works

```
Client                     Resource server                Facilitator                      TRON
  │  GET /resource              │                              │                              │
  │ ──────────────────────────▶ │                              │                              │
  │  402 + TRON exact quote     │                              │                              │
  │ ◀────────────────────────── │                              │                              │
  │  sign Permit2 witness       │                              │                              │
  │  GET + PAYMENT-SIGNATURE    │                              │                              │
  │ ──────────────────────────▶ │  settle                      │                              │
  │                             │ ───────────────────────────▶ │  verify, preflight, simulate │
  │                             │                              │  journal → sign → broadcast  │
  │                             │                              │ ───────────────────────────▶ │
  │                             │                              │  solidified + exact transfer │
  │                             │  success + txid              │ ◀─────────────────────────── │
  │  200 + PAYMENT-RESPONSE     │ ◀─────────────────────────── │                              │
  │ ◀────────────────────────── │                              │                              │
```

The payer signs an off-chain Permit2 authorization. The facilitator's executor account submits it through the x402 exact Permit2 proxy contract and pays the TRX fees. The resource is delivered only after the transfer is final.

## Examples

| Example | What it does |
| --- | --- |
| [`examples/client`](examples/client) | Offline 402 handshake with an ephemeral key. Touches no blockchain. Run it with `go run ./examples/client`. |
| [`examples/server`](examples/server) | Resource server using the official `net/http` middleware |
| [`examples/facilitator`](examples/facilitator) | Authenticated facilitator HTTP service (testnet and loopback only) |
| [`examples/payer`](examples/payer) | Testnet payer with crash-safe `pay` / `recover` |
| [`examples/deployment`](examples/deployment) | Read-only check of contract deployments and code hashes |

The configuration for each example is described in the guides, and [`.env.example`](.env.example) lists every variable. Runnable API examples also live in [`exact/example_test.go`](exact/example_test.go) and [`exact/postgres/example_test.go`](exact/postgres/example_test.go).

## Documentation

| Guide | Contents |
| --- | --- |
| [Client](guide/client.md) | Registration, signers, spend limits and what a valid payment does and does not prove |
| [Resource server](guide/resource-server.md) | Quotes, the `upfront` flow, facilitator discovery and example server configuration |
| [Facilitator](guide/facilitator.md) | Embedding or serving the facilitator, the payer example, the settlement journal and recovery |
| [Building blocks](guide/components.md) | Offline verification, preflight, deployment checks, transaction building, broadcast and finality checks |
| [Testing and validation](guide/validation.md) | Test suites, interoperability tests, Nile live payments, reproducible contract builds and packaging |
| [Threat model](THREAT_MODEL.md) | Assets, trust boundaries, threats, mitigations and residual risks |

## Interoperability

Offline tests run against the published `@bankofai/x402-tron@2.1.2` and `@x402/core@2.29.0` in both directions. Both projects use decimal CAIP-2 identifiers (for example `tron:3448148188`) and accept legacy hexadecimal IDs as aliases.

Live Nile payments have been completed in both directions: from the TypeScript payer to the Go facilitator, and from the Go payer to the TypeScript facilitator. Differences in receipt format and replay behavior are documented in [Testing and validation](guide/validation.md#nile-live-payments). One of them has been reported upstream as [BofAI/x402#122](https://github.com/BofAI/x402/issues/122).

## Development

```bash
go mod verify
go vet ./...
go test -race ./exact/... .
```

PostgreSQL integration tests run when `X402_TEST_DATABASE_URL` points to a dedicated test database, and are skipped otherwise. The interoperability suite needs Node.js 24.22.1 or later. CI also runs `govulncheck` and builds release bundles. See [Testing and validation](guide/validation.md).

Contributions are welcome. Please open an issue to discuss significant changes. Before sending a pull request, run `gofmt`, `go vet` and the tests for the packages you changed.

## Security

Please report vulnerabilities privately through [GitHub Security Advisories](https://github.com/eightyun/x402-tron-go/security/advisories/new) and not through public issues. See [SECURITY.md](SECURITY.md) and the [threat model](THREAT_MODEL.md).

Keep in mind:

- A per-payment cap is not a cumulative budget, and an authorization is not bound to a specific URL. Limit trusted origins and total spending in your application.
- A timeout or a pending result does not mean the payment failed. Recover with the same authorization, and never create a replacement.
- The facilitator trusts its configured nodes and database. It does not verify Merkle proofs and cannot defend against malicious nodes.

## License

Licensed under the [Apache License 2.0](LICENSE). Third-party components are listed in [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
