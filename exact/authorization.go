// Package exact provides TRON exact Permit2 client, resource server and settlement components.
// Nodes and trusted deployments remain part of the trust boundary.
package exact

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	tron "github.com/eightyun/x402-tron-go"
	"github.com/x402-foundation/x402/go/v2/types"
	"golang.org/x/crypto/sha3"
)

var (
	ErrInvalidConfig        = errors.New("exact: invalid offline verifier configuration")
	ErrInvalidRequirements  = errors.New("exact: unsupported or inconsistent payment requirements")
	ErrInvalidAuthorization = errors.New("exact: invalid Permit2 authorization fields")
	ErrInvalidSignature     = errors.New("exact: invalid Permit2 authorization signature")
	ErrInvalidValidity      = errors.New("exact: Permit2 authorization outside the allowed validity window")
)

// Config explicitly configures the trusted network and the contracts already verified by the
// deployer. Contract addresses are never taken from the client-supplied extra.
type Config struct {
	Network        string `json:"network"`
	Permit2Address string `json:"permit2Address"`
	ProxyAddress   string `json:"proxyAddress"`
}

// TokenPermissions is the exact token and atomic-unit amount the payer authorizes for transfer.
type TokenPermissions struct {
	Token  string `json:"token"`
	Amount string `json:"amount"`
}

// Witness is the two-field signed witness used by the exact proxy. It does not include the
// facilitator field used by upto.
type Witness struct {
	To         string `json:"to"`
	ValidAfter string `json:"validAfter"`
}

// Permit2Authorization matches the wire fields of exact Permit2 in the TRON ecosystem.
// Amounts and times are decimal uint256 values; the nonce may also be a client-generated
// 32-byte 0x-encoded value.
type Permit2Authorization struct {
	From      string           `json:"from"`
	Permitted TokenPermissions `json:"permitted"`
	Spender   string           `json:"spender"`
	Nonce     string           `json:"nonce"`
	Deadline  string           `json:"deadline"`
	Witness   Witness          `json:"witness"`
}

// Permit2Payload is the TRON exact Permit2 part of an x402 payload, not the full protocol envelope.
type Permit2Payload struct {
	Signature     string               `json:"signature"`
	Authorization Permit2Authorization `json:"permit2Authorization"`
}

// VerifiedAuthorization is only the result of a single offline verification. It must never be
// used as proof of settlement or as a credential for delivering resources.
type VerifiedAuthorization struct {
	Network string
	Payer   string
	Nonce   string
	Digest  [32]byte
}

// OfflineVerifier is an immutable offline verifier for a single network and is safe for
// concurrent use. It does not query balances, allowances, nonce bitmaps or contract code, and
// provides no replay protection or settlement guarantee.
type OfflineVerifier struct {
	network string
	proxy   [20]byte
	domain  [32]byte
}

// NewOfflineVerifier validates the configuration and fixes the signing domain. It does not
// verify that the configured contracts are actually deployed.
func NewOfflineVerifier(config Config) (*OfflineVerifier, error) {
	network, networkErr := tron.NormalizeNetwork(config.Network)
	chainID, chainErr := tron.ChainID(config.Network)
	permit2, permitErr := configuredAddress(config.Permit2Address)
	proxy, proxyErr := configuredAddress(config.ProxyAddress)
	if networkErr != nil || chainErr != nil || permitErr != nil || proxyErr != nil || permit2 == proxy {
		return nil, ErrInvalidConfig
	}
	var chainWord [32]byte
	binary.BigEndian.PutUint32(chainWord[28:], chainID)
	domain := hashWords(
		keccak([]byte("EIP712Domain(string name,uint256 chainId,address verifyingContract)")),
		keccak([]byte("Permit2")), chainWord, addressWord(permit2),
	)
	return &OfflineVerifier{network: network, proxy: proxy, domain: domain}, nil
}

// Digest computes the exact Permit2 TIP-712 digest for use with external signers.
// It only checks that the fields are encodable; it does not verify payment requirements, the
// validity window, the signature, or whether the nonce has been used.
func (verifier *OfflineVerifier) Digest(authorization Permit2Authorization) ([32]byte, error) {
	if verifier == nil || verifier.network == "" {
		return [32]byte{}, ErrInvalidConfig
	}
	parsed, err := parseAuthorization(authorization)
	if err != nil {
		return [32]byte{}, err
	}
	return verifier.digest(parsed), nil
}

// VerifyAuthorization verifies an EOA authorization against the server's trusted quote; now
// should come from the server clock. Callers must still verify on-chain state, persistent
// idempotency, the nonce and final settlement before delivering resources.
// Only permit2 with an optional paymentFlow=upfront is supported; any other extra or extensions
// are rejected.
func (verifier *OfflineVerifier) VerifyAuthorization(payload types.PaymentPayload, requirements types.PaymentRequirements, now time.Time) (VerifiedAuthorization, error) {
	raw, authorization, err := verifier.boundAuthorization(payload, requirements)
	if err != nil {
		return VerifiedAuthorization{}, err
	}
	nowSeconds := now.Unix()
	remaining := new(big.Int).Sub(new(big.Int).SetBytes(authorization.deadline[:]), big.NewInt(nowSeconds))
	validAfter := new(big.Int).SetBytes(authorization.validAfter[:])
	if nowSeconds < 0 || remaining.Cmp(big.NewInt(6)) < 0 || remaining.Cmp(big.NewInt(int64(requirements.MaxTimeoutSeconds))) > 0 || validAfter.Cmp(big.NewInt(nowSeconds)) > 0 {
		return VerifiedAuthorization{}, ErrInvalidValidity
	}
	return verifier.verifyBoundSignature(raw, authorization)
}

// VerifyAuthorizationBinding only checks the historical binding between a signed authorization
// and a trusted quote; it does not check the current validity window. It may only be used for
// recovery against existing persisted records, never for admitting new payments, signing,
// broadcasting or delivering resources. New requests must use VerifyAuthorization or
// Preflight.Check with the current server time instead.
func (verifier *OfflineVerifier) VerifyAuthorizationBinding(payload types.PaymentPayload, requirements types.PaymentRequirements) (VerifiedAuthorization, error) {
	raw, authorization, err := verifier.boundAuthorization(payload, requirements)
	if err != nil {
		return VerifiedAuthorization{}, err
	}
	return verifier.verifyBoundSignature(raw, authorization)
}

func (verifier *OfflineVerifier) boundAuthorization(payload types.PaymentPayload, requirements types.PaymentRequirements) (Permit2Payload, parsedAuthorization, error) {
	if verifier == nil || verifier.network == "" {
		return Permit2Payload{}, parsedAuthorization{}, ErrInvalidConfig
	}
	if payload.X402Version != 2 || len(payload.Extensions) != 0 || !validExtra(requirements.Extra) || !validExtra(payload.Accepted.Extra) || requirements.Extra["paymentFlow"] != payload.Accepted.Extra["paymentFlow"] {
		return Permit2Payload{}, parsedAuthorization{}, ErrInvalidRequirements
	}
	asset, recipient, amount, err := verifier.requirements(requirements)
	if err != nil {
		return Permit2Payload{}, parsedAuthorization{}, err
	}
	acceptedAsset, acceptedRecipient, acceptedAmount, err := verifier.requirements(payload.Accepted)
	if err != nil || acceptedAsset != asset || acceptedRecipient != recipient || acceptedAmount != amount || payload.Accepted.MaxTimeoutSeconds != requirements.MaxTimeoutSeconds {
		return Permit2Payload{}, parsedAuthorization{}, ErrInvalidRequirements
	}
	raw, err := parsePayload(payload.Payload)
	if err != nil {
		return Permit2Payload{}, parsedAuthorization{}, err
	}
	authorization, err := parseAuthorization(raw.Authorization)
	if err != nil {
		return Permit2Payload{}, parsedAuthorization{}, err
	}
	if authorization.token != asset || authorization.to != recipient || authorization.amount != amount || authorization.spender != verifier.proxy {
		return Permit2Payload{}, parsedAuthorization{}, ErrInvalidAuthorization
	}
	return raw, authorization, nil
}

func (verifier *OfflineVerifier) verifyBoundSignature(raw Permit2Payload, authorization parsedAuthorization) (VerifiedAuthorization, error) {
	digest := verifier.digest(authorization)
	if !verifySignature(digest, raw.Signature, authorization.from) {
		return VerifiedAuthorization{}, ErrInvalidSignature
	}
	payer, err := tron.NormalizeAddressBase58("41" + hex.EncodeToString(authorization.from[:]))
	if err != nil {
		return VerifiedAuthorization{}, ErrInvalidAuthorization
	}
	return VerifiedAuthorization{
		Network: verifier.network, Payer: payer,
		Nonce: new(big.Int).SetBytes(authorization.nonce[:]).String(), Digest: digest,
	}, nil
}

func (verifier *OfflineVerifier) requirements(requirements types.PaymentRequirements) ([20]byte, [20]byte, [32]byte, error) {
	network, networkErr := tron.NormalizeNetwork(requirements.Network)
	asset, assetErr := configuredAddress(requirements.Asset)
	recipient, recipientErr := configuredAddress(requirements.PayTo)
	amount, amountErr := uint256Word(requirements.Amount, false)
	if requirements.Scheme != "exact" || networkErr != nil || network != verifier.network || assetErr != nil || recipientErr != nil || amountErr != nil || amount == [32]byte{} || requirements.MaxTimeoutSeconds <= 0 {
		return [20]byte{}, [20]byte{}, [32]byte{}, ErrInvalidRequirements
	}
	return asset, recipient, amount, nil
}

func validExtra(extra map[string]interface{}) bool {
	return extra["assetTransferMethod"] == "permit2" && (len(extra) == 1 || len(extra) == 2 && extra["paymentFlow"] == "upfront")
}

type parsedAuthorization struct {
	from, token, spender, to            [20]byte
	amount, nonce, deadline, validAfter [32]byte
}

func parseAuthorization(authorization Permit2Authorization) (parsedAuthorization, error) {
	var parsed parsedAuthorization
	for _, field := range []struct {
		value string
		out   *[20]byte
	}{
		{authorization.From, &parsed.from}, {authorization.Permitted.Token, &parsed.token},
		{authorization.Spender, &parsed.spender}, {authorization.Witness.To, &parsed.to},
	} {
		value, err := signingAddress(field.value)
		if err != nil {
			return parsedAuthorization{}, ErrInvalidAuthorization
		}
		*field.out = value
	}
	for _, field := range []struct {
		value string
		hex   bool
		out   *[32]byte
	}{
		{authorization.Permitted.Amount, false, &parsed.amount}, {authorization.Nonce, true, &parsed.nonce},
		{authorization.Deadline, false, &parsed.deadline}, {authorization.Witness.ValidAfter, false, &parsed.validAfter},
	} {
		value, err := uint256Word(field.value, field.hex)
		if err != nil {
			return parsedAuthorization{}, ErrInvalidAuthorization
		}
		*field.out = value
	}
	if parsed.amount == [32]byte{} || bytes.Compare(parsed.validAfter[:], parsed.deadline[:]) >= 0 {
		return parsedAuthorization{}, ErrInvalidAuthorization
	}
	return parsed, nil
}

func parsePayload(payload map[string]interface{}) (Permit2Payload, error) {
	var result Permit2Payload
	if len(payload) != 2 {
		return result, ErrInvalidAuthorization
	}
	auth, ok := payload["permit2Authorization"].(map[string]interface{})
	if !ok || len(auth) != 6 {
		return result, ErrInvalidAuthorization
	}
	permitted, permittedOK := auth["permitted"].(map[string]interface{})
	witness, witnessOK := auth["witness"].(map[string]interface{})
	if !permittedOK || !witnessOK || len(permitted) != 2 || len(witness) != 2 {
		return result, ErrInvalidAuthorization
	}
	for _, field := range []struct {
		values map[string]interface{}
		key    string
		out    *string
	}{
		{payload, "signature", &result.Signature}, {auth, "from", &result.Authorization.From},
		{auth, "spender", &result.Authorization.Spender}, {auth, "nonce", &result.Authorization.Nonce},
		{auth, "deadline", &result.Authorization.Deadline},
		{permitted, "token", &result.Authorization.Permitted.Token}, {permitted, "amount", &result.Authorization.Permitted.Amount},
		{witness, "to", &result.Authorization.Witness.To}, {witness, "validAfter", &result.Authorization.Witness.ValidAfter},
	} {
		value, ok := field.values[field.key].(string)
		if !ok {
			return Permit2Payload{}, ErrInvalidAuthorization
		}
		*field.out = value
	}
	return result, nil
}

func configuredAddress(value string) ([20]byte, error) {
	normalized, err := tron.AddressForSigning(value)
	if err != nil {
		return [20]byte{}, ErrInvalidAuthorization
	}
	return signingAddress(normalized)
}

func signingAddress(value string) ([20]byte, error) {
	var address [20]byte
	if len(value) != 42 || !strings.HasPrefix(value, "0x") {
		return address, ErrInvalidAuthorization
	}
	if _, err := hex.Decode(address[:], []byte(value[2:])); err != nil || address == [20]byte{} {
		return [20]byte{}, ErrInvalidAuthorization
	}
	return address, nil
}

func uint256Word(value string, allowHex bool) ([32]byte, error) {
	var word [32]byte
	if allowHex && len(value) == 66 && strings.HasPrefix(value, "0x") {
		if _, err := hex.Decode(word[:], []byte(value[2:])); err == nil {
			return word, nil
		}
		return word, ErrInvalidAuthorization
	}
	if len(value) == 0 || len(value) > 78 || (len(value) > 1 && value[0] == '0') {
		return word, ErrInvalidAuthorization
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return word, ErrInvalidAuthorization
		}
	}
	number, ok := new(big.Int).SetString(value, 10)
	if !ok || number.BitLen() > 256 {
		return word, ErrInvalidAuthorization
	}
	number.FillBytes(word[:])
	return word, nil
}

func (verifier *OfflineVerifier) digest(authorization parsedAuthorization) [32]byte {
	permissions := hashWords(keccak([]byte("TokenPermissions(address token,uint256 amount)")), addressWord(authorization.token), authorization.amount)
	witness := hashWords(keccak([]byte("Witness(address to,uint256 validAfter)")), addressWord(authorization.to), authorization.validAfter)
	structHash := hashWords(
		keccak([]byte("PermitWitnessTransferFrom(TokenPermissions permitted,address spender,uint256 nonce,uint256 deadline,Witness witness)TokenPermissions(address token,uint256 amount)Witness(address to,uint256 validAfter)")),
		permissions, addressWord(authorization.spender), authorization.nonce, authorization.deadline, witness,
	)
	var message [66]byte
	message[0], message[1] = 0x19, 0x01
	copy(message[2:34], verifier.domain[:])
	copy(message[34:], structHash[:])
	return keccak(message[:])
}

func addressWord(address [20]byte) [32]byte {
	var word [32]byte
	copy(word[12:], address[:])
	return word
}

func hashWords(words ...[32]byte) [32]byte {
	hasher := sha3.NewLegacyKeccak256()
	for _, word := range words {
		_, _ = hasher.Write(word[:])
	}
	var result [32]byte
	hasher.Sum(result[:0])
	return result
}

func keccak(value []byte) [32]byte {
	hasher := sha3.NewLegacyKeccak256()
	_, _ = hasher.Write(value)
	var result [32]byte
	hasher.Sum(result[:0])
	return result
}

func verifySignature(digest [32]byte, signature string, payer [20]byte) bool {
	if len(signature) != 132 || !strings.HasPrefix(signature, "0x") {
		return false
	}
	var decoded [65]byte
	if _, err := hex.Decode(decoded[:], []byte(signature[2:])); err != nil {
		return false
	}
	v := decoded[64]
	if v == 27 || v == 28 {
		v -= 27
	}
	if v > 1 {
		return false
	}
	var r, s secp256k1.ModNScalar
	if r.SetByteSlice(decoded[:32]) || r.IsZero() || s.SetByteSlice(decoded[32:64]) || s.IsZero() || s.IsOverHalfOrder() {
		return false
	}
	var compact [65]byte
	compact[0] = 27 + v
	copy(compact[1:], decoded[:64])
	publicKey, _, err := ecdsa.RecoverCompact(compact[:], digest[:])
	if err != nil {
		return false
	}
	publicHash := keccak(publicKey.SerializeUncompressed()[1:])
	return bytes.Equal(publicHash[12:], payer[:])
}
