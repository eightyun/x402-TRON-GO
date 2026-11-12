# Client integration

[简体中文](client.zh-CN.md) | English

How to register the TRON `exact` client mechanism with the official x402 Go client, supply a signer and bound what the client will authorize.

## Quick start and registration

Requires Go 1.26.10+. No stable release yet. From a clone:

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
