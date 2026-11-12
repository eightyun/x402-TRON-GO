# Client integration

[简体中文](client.zh-CN.md) | English

This guide shows how to register the TRON `exact` client mechanism with the official x402 Go client. It covers supplying a signer and limiting which payments the client will authorize.

## Run the example

The example requires Go 1.26.10 or later. There is no stable release yet, so run it from a clone of the repository:

```bash
go run ./examples/client
```

The example runs the real official x402 SDK against a loopback HTTP fixture. It signs with a random, ephemeral, in-process key. The flow is:

`402 → TRON authorization → PAYMENT-SIGNATURE retry → 202 authorization verified, not settled`

The example does not:

- connect to a blockchain node;
- use an existing wallet;
- send a transaction;
- return a successful settlement response.

It is not a complete on-chain round trip. The fixture addresses are public protocol fixtures, not trusted deployment configurations.

## Register the mechanism

The mechanism builds the authorization fields, the nonce and the TIP-712 digest for you. Register it with the official client as follows:

```go
import (
    "net/http"
    "time"

    "github.com/eightyun/x402-tron-go/exact"
    x402 "github.com/x402-foundation/x402/go/v2"
    x402http "github.com/x402-foundation/x402/go/v2/http"
)

// contracts must come from an independently verified, trusted deployment configuration.
// All amounts are in the token's smallest unit.
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

`ClientConfig` fields:

| Field | Meaning |
| --- | --- |
| `Authorization` | Trusted contract configuration (network, Permit2 address and proxy address). |
| `PayerAddress` | The payer's TRON address, for example `signer.Address()`. |
| `Asset` | The TRC-20 token the client is willing to pay with. |
| `PayTo` | The trusted recipient. |
| `MaxAmount` | Per-payment cap in the token's smallest unit. It must be nonzero. |
| `Lifetime` | Validity window of each authorization. Defaults to `60` seconds. Must be a whole number of seconds from `6` seconds to `10` minutes. |
| `Signer` | Function that signs the TIP-712 digest. See [Signers](#signers). |

`NewClient` returns `ErrClientConfig` if any field is invalid.

## Signers

### Built-in EOA signer

`exact.NewEOASigner(keyBytes)` is an optional in-memory signer for a single EOA.

- It accepts only a strictly valid, nonzero, 32-byte private key, and copies it into memory.
- You still manage the original key bytes. Call `Close()` when you are finished.
- It does not load keys from files or environment variables, and it cannot export the key.
- Common formatting (`String`, `GoString`) and JSON output do not expose the internal scalar.
- `Close()` clears memory on a best-effort basis only. It is not a substitute for secure hardware.

### External signers

Production applications can supply their own `ClientConfig.Signer`. The signer receives the **already computed TIP-712 digest** and must sign it as is:

- do not hash it again;
- do not add a `personal_sign` prefix;
- do not confuse it with the SHA-256 transaction-digest signing used for settlement.

External signers must be safe for concurrent use and must respect bounded contexts. The client does not pass the signer's raw error messages through; they are redacted to `ErrClientSign`.

## What the client checks

Before signing, the client checks the quote against its configuration:

- the network, asset and recipient must match the configured values;
- the amount must not exceed the per-payment `MaxAmount`;
- the amount must not exceed the cap supplied by the official SDK (`MaxAmountPerPayment` from `SpendControls`);
- unknown `extra` fields and nonempty extensions are rejected.

A quote that breaks the payment policy fails with `ErrClientPolicy`.

The authorization's validity is the smaller of `Lifetime` and the quote's timeout. If that leaves less than 6 seconds, payload creation fails with `ErrInvalidValidity`. After signing, the client checks the signature and the remaining validity again.

The client does not read balances or allowances. The payer must have granted the Permit2 contract an allowance beforehand.

## Limits and non-guarantees

> [!WARNING]
> **Per-payment caps are not aggregate budgets.** Every payload creation issues a new, spendable authorization. Do not automatically pay again when the outcome of a payment is unknown.

- Never derive trusted policy (assets, recipients or contracts) from an untrusted 402 quote.
- Authorizations are not bound to the URL or the request body.
- Your application must restrict trusted origins and budgets itself.
- Reject unsafe redirects. The example above stops at the first redirect with `http.ErrUseLastResponse`.
- Avoid recovery hooks that issue additional authorizations.
- Neither an HTTP 2xx response nor offline verification proves that the payment landed on-chain.
