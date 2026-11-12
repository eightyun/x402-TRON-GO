# Resource server integration

[简体中文](resource-server.zh-CN.md) | English

How an API resource server quotes TRON payments and gates handlers on trusted settlement.

## Integration and example server

Register `exact.NewServer(exact.ServerConfig{Network: network, Asset: asset, PayTo: payTo})` with the official resource server. One trusted network/asset/recipient; no database or wallet key. `ParsePrice` accepts canonical positive atomic-unit strings (`"1000"`), official `x402.AssetAmount` values or strict equivalent maps. **No `$0.01`, floating-point prices or implicit exchange rates.**

Only the official `upfront` flow is enabled. Quotes include `assetTransferMethod: permit2` and `paymentFlow: upfront`; handlers run only after a trusted facilitator reports successful settlement. Pending, unknown, rejected, failed or timed-out results must not be reported as successful. Upfront does not first call `/verify`: **`/settle` must perform complete authorization/preflight, durable submission and solidified execution/received-token verification itself**. Broadcast acceptance is not success.

Startup requires `/supported` to advertise matching v2/exact/canonical network and `permit2` in `extra.supportedAssetTransferMethods`. This existing TRON ecosystem capability field is not copied to payment quotes and is not contract or settlement proof. Independently verify matching client/facilitator contract deployments.

The runnable [server example](../examples/server/main.go) uses official net/http middleware. Set the following environment variables, then run `go run ./examples/server`:

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

The [self-hosted facilitator](facilitator.md) requires independently provisioned/verified contracts and nodes; no public facilitator address is assumed. Automated HTTP integration tests use synthetic chain state/receipts. Separate live-program and packaged-example exercises are recorded in [Nile live payments](validation.md#nile-live-payments); **neither is production acceptance**.
