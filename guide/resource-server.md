# Resource server integration

[简体中文](resource-server.zh-CN.md) | English

This guide shows how an API resource server quotes TRON payments and runs a handler only after a trusted facilitator confirms settlement. It also describes the configuration of the example server.

## Registering the mechanism

Register `exact.NewServer` with the official x402 resource server:

```go
exact.NewServer(exact.ServerConfig{Network: network, Asset: asset, PayTo: payTo})
```

- Each mechanism serves one trusted network, one asset and one recipient.
- The resource server needs no database and no wallet key.

## Prices

`ParsePrice` accepts these formats:

- A canonical, positive string in atomic units, such as `"1000"`.
- An official `x402.AssetAmount` value.
- A map strictly equivalent to `x402.AssetAmount`.

It does **not** accept dollar prices such as `$0.01`, floating-point prices or implicit exchange rates.

## Payment flow

Only the official `upfront` flow is enabled. Every quote includes `assetTransferMethod: permit2` and `paymentFlow: upfront`.

Your handler runs only after a trusted facilitator reports successful settlement. A result that is pending, unknown, rejected, failed or timed out must never be reported as successful.

The `upfront` flow does not call `/verify` first. The facilitator's `/settle` endpoint must therefore do all of the following itself:

- Verify the full authorization and run preflight checks.
- Record the submission durably.
- Confirm that the transaction is solidified and that the token transfer was actually received.

> [!WARNING]
> Acceptance of a broadcast is not success. The facilitator must not report success after broadcasting alone.

## Facilitator discovery

At startup, the facilitator's `/supported` response must advertise:

- `x402Version: 2`
- `scheme: exact`
- The canonical network identifier that matches your configuration.
- `permit2` in the `extra.supportedAssetTransferMethods` array.

`extra.supportedAssetTransferMethods` is an existing capability field from the TRON ecosystem. It is not copied into payment quotes. The advertisement is not proof of trusted contracts or of settlement. Verify independently that the client and the facilitator use matching contract deployments.

## Example server

The runnable [server example](../examples/server/main.go) uses the official `net/http` middleware. Set the environment variables below, then run `go run ./examples/server`.

| Variable | Required | Meaning |
| --- | --- | --- |
| `X402_NETWORK` | No | Network identifier. Defaults to Nile, `tron:3448148188`. |
| `X402_ASSET` | Yes | Address of the trusted token. |
| `X402_PAY_TO` | Yes | Merchant recipient address. |
| `X402_PRICE_ATOMIC` | Yes | Positive price in atomic units. |
| `X402_FACILITATOR_URL` | Yes | Base HTTPS URL of the trusted facilitator. It must not contain user info, a query or a fragment. |
| `X402_FACILITATOR_TOKEN` | For this project's facilitator | Sent as a Bearer token to all three facilitator endpoints through the official `AuthProvider`. Optional for third-party facilitators that do not require authentication. |
| `X402_ALLOW_INSECURE_LOCAL_FACILITATOR_HTTP` | No | Off by default. Only the explicit value `true` allows plain HTTP, and only to literal loopback IP addresses. `localhost` and public addresses are still rejected. |
| `X402_LISTEN_ADDR` | No | Listen address. Defaults to `127.0.0.1:8080`. |

### Behavior

- The server initializes the mechanism and validates the quote before it starts listening. Any configuration or capability failure stops startup.
- `GET /resource` returns `402` when no payment is attached.
- `HEAD` requests are rejected. In Go, a `GET` route also matches `HEAD`, which would otherwise bypass the payment gate.
- Startup, settlement and HTTP timeouts are all bounded.
- Redirects from the facilitator are not followed.
- Request header size is bounded.
- The server shuts down gracefully.

For external deployment, put the server behind TLS ingress and add appropriate authentication. Pass facilitator credentials through the official `AuthProvider`. Never put tokens in the URL.

### Timeouts

| Budget | Value | Purpose |
| --- | --- | --- |
| Remote facilitator request | `200` seconds | Covers the mechanism's maximum of `180` seconds. |
| Payment middleware | `410` seconds | Covers one official retry of a pending result with the identical payload and the original txid. |
| HTTP write | `420` seconds | Covers the same retry, with headroom over the middleware budget. |
| Startup discovery | `30` seconds | Bounds the `/supported` check at startup. |

Reverse proxies and callers must use matching budgets.

> [!WARNING]
> A timeout does not mean the payment failed, and it does not authorize another payment. Do not create a new authorization after a timeout.

## Limits

- The [self-hosted facilitator](facilitator.md) needs contracts and nodes that you provision and verify yourself. No public facilitator address is assumed.
- Automated HTTP integration tests use synthetic chain state and receipts.
- Separate live-program and packaged-example runs are recorded in [Nile live payments](validation.md#nile-live-payments). **Neither counts as production acceptance.**
