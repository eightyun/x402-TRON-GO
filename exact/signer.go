package exact

import (
	"context"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	tron "github.com/eightyun/x402-tron-go"
)

var ErrEOASigner = errors.New("exact: EOA signer unavailable or signing key invalid")

// EOASigner is an optional in-memory signer for a single EOA. It does not load keys from files
// or environment variables and never exports the key. The key resides in the Go process, which
// is not equivalent to an HSM or MPC; production deployments may use an external
// AuthorizationSigner instead.
type EOASigner struct {
	mu      sync.RWMutex
	key     *secp256k1.PrivateKey
	address string
}

// NewEOASigner copies a private key that must be exactly 32 bytes and within the curve order.
// Callers remain responsible for the lifetime of the original input.
func NewEOASigner(privateKey []byte) (*EOASigner, error) {
	var scalar secp256k1.ModNScalar
	defer scalar.Zero()
	if len(privateKey) != 32 || scalar.SetByteSlice(privateKey) || scalar.IsZero() {
		return nil, ErrEOASigner
	}
	key := secp256k1.NewPrivateKey(&scalar)
	hash := keccak(key.PubKey().SerializeUncompressed()[1:])
	address, _ := tron.NormalizeAddressBase58("41" + hex.EncodeToString(hash[12:]))
	return &EOASigner{key: key, address: address}, nil
}

// Address returns the Base58Check payer address that can be configured for a Client.
func (signer *EOASigner) Address() string {
	if signer == nil {
		return ""
	}
	return signer.address
}

// SignAuthorization signs an already computed TIP-712 digest without adding a prefix or hashing
// it again.
func (signer *EOASigner) SignAuthorization(ctx context.Context, digest [32]byte) ([]byte, error) {
	return signer.signDigest(ctx, digest)
}

// SignTransaction signs the native transaction SHA-256 digest computed by a trusted builder,
// without adding a prefix or hashing it again. It must only be used as the signer of a
// SettlementBuilder or Facilitator and must never be exposed to untrusted callers as an
// arbitrary-digest signing service.
func (signer *EOASigner) SignTransaction(ctx context.Context, digest [32]byte) ([]byte, error) {
	return signer.signDigest(ctx, digest)
}

func (signer *EOASigner) signDigest(ctx context.Context, digest [32]byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if signer == nil {
		return nil, ErrEOASigner
	}
	signer.mu.RLock()
	defer signer.mu.RUnlock()
	if signer.key == nil {
		return nil, ErrEOASigner
	}
	compact := ecdsa.SignCompact(signer.key, digest[:], false)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if compact[0] != 27 && compact[0] != 28 {
		return nil, ErrEOASigner
	}
	signature := make([]byte, 65)
	copy(signature, compact[1:])
	signature[64] = compact[0]
	return signature, nil
}

// Close idempotently stops signing and makes a best effort to clear the internal scalar. The Go
// runtime provides no guarantee that memory is fully erased.
func (signer *EOASigner) Close() error {
	if signer == nil {
		return nil
	}
	signer.mu.Lock()
	defer signer.mu.Unlock()
	if signer.key != nil {
		signer.key.Zero()
		signer.key = nil
	}
	return nil
}

// String prevents common formatted output from exposing the internal scalar.
func (*EOASigner) String() string { return "EOASigner(<redacted>)" }

// GoString prevents %#v from printing the internal scalar.
func (*EOASigner) GoString() string { return "EOASigner(<redacted>)" }
