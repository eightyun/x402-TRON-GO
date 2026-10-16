# x402-tron-go

[简体中文](README.md) | English

An independent Go project for TRON support in x402, providing network/address utilities and offline `exact` Permit2 authorization verification. **No complete client, facilitator, or on-chain settlement is available. Do not use with real funds.** Not affiliated with the x402 Foundation or TRON.

## Available utilities

- `NormalizeNetwork` and `ChainID`: Mainnet, Nile, Shasta; known legacy hexadecimal aliases normalize to decimal CAIP-2 identifiers.
- `NormalizeAddressHex` and `NormalizeAddressBase58`: validate and convert TRON addresses.
- `AddressForSigning`: explicitly convert a valid TRON address to a 20-byte `0x` address for TIP-712.

The root package uses only the standard library. The `exact` package uses official x402 Go SDK v2.28.0 types and established secp256k1/Keccak libraries. No RPC calls, key storage, token approvals, or transactions. Recognizing a network does not mean settlement is supported. Address format cannot distinguish TRON networks or establish account activation, safety, or balance.

## Offline exact Permit2 verification

`exact.NewOfflineVerifier` requires a trusted network, Permit2 contract and exact proxy. No default mainnet configuration or client-selected contracts. Deployment owners must independently validate contract code and domain/proxy bindings.

`Digest` computes the TIP-712 digest for external signing. `VerifyAuthorization` consumes official SDK `types.PaymentPayload`, trusted server-side `types.PaymentRequirements` and server time. It binds the quote, network, token, amount, recipient, proxy, nonce and validity, then recovers the EOA signer.

Current subset: only `extra: {assetTransferMethod: "permit2"}`, no other extra fields or nonempty extensions; two-field witness (`to`, `validAfter`); strict TRON addresses in configuration/requirements and 20-byte `0x` addresses in authorizations; canonical decimal uint256 amounts/times, decimal or 32-byte hex nonces. Deadline must have at least six seconds remaining, bounded by the quote timeout. Only 65-byte low-s EOA signatures with v=0/1 or 27/28; no contract-wallet signatures.

**Valid authorization is not proof of funds or settlement.** No balance, allowance, nonce bitmap or chain-state checks; no nonce consumption or concurrency/replay protection. Repeated verification can succeed. **Do not deliver paid resources from this result.** Chain preflight, durable idempotency, settlement and final outcome verification remain necessary.

The authorization does not bind a specific HTTP URL or request body. Resource servers must also prevent duplicate fulfillment using the same payment.

The public TronWeb 6.3.0 vector verifies digest/signature compatibility offline. Its ephemeral random test key was discarded; no live wallet or blockchain was used. This is not full client/facilitator interoperability acceptance.

## Development

Requires Go 1.26+. CI uses 1.26.x. No release exists; public APIs may change.

```bash
go mod verify
go vet . ./exact
go test -race -count=1 -cover . ./exact
go build . ./exact
```

Inputs are strict: no whitespace trimming, unknown networks, wildcards, or decimal leading zeros. Namespace must be `tron`; legacy hexadecimal references are case-insensitive. Addresses must be 34-character Base58Check or 42-character `41`-prefixed hex. Arbitrary `0x` addresses are not accepted as TRON input. Formatting is not authorization verification.

Tests cover public vectors, independent encoding checks, authorization tampering, uint256/time boundaries, signature canonicality and fuzz seeds. No live-chain acceptance is claimed. Next: chain-state checks and settlement before gateway integration.

## References and status

- [TRON address specification](https://developers.tron.network/docs/account)
- [Official x402 Go SDK](https://github.com/x402-foundation/x402/tree/main/go)
- [TRON ecosystem reference](https://github.com/BofAI/x402)

[Apache-2.0 licensed](LICENSE). See [third-party notices](THIRD_PARTY_NOTICES.md). No stable release or independent security audit yet.
