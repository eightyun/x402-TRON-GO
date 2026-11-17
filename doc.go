// Package tron provides embeddable x402 TRON network identifiers and address validation.
//
// The exact subpackage implements the official x402 SDK client and resource server mechanisms
// together with settlement components. The exact/postgres subpackage provides an optional
// durable implementation for deployments that run their own facilitator. Clients and resource
// servers do not require PostgreSQL, a standalone example service or a payment gateway.
//
// Importing these packages does not load wallets, start servers or contact nodes. Real payments
// still require explicitly configured trusted deployments, signers, assets and limits, plus a
// settlement side that supports TRON.
//
// Live payments and recovery of the original transaction have been validated on the Nile
// testnet. This does not imply mainnet support or any guarantee of production fund safety, and
// the code has not been audited.
package tron
