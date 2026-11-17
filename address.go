package tron

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// ErrInvalidAddress reports that the input is not a valid TRON Base58Check or 41-prefixed
// hex address.
var ErrInvalidAddress = errors.New("tron: invalid TRON address")

// NormalizeAddressHex returns the address as 42 lowercase hex characters with the 41 prefix.
// It accepts Base58Check or 41-prefixed hex and rejects whitespace and 0x-prefixed input.
func NormalizeAddressHex(value string) (string, error) {
	address, err := decodeAddress(value)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(address[:]), nil
}

// NormalizeAddressBase58 returns the validated canonical Base58Check address.
// A valid format does not mean the account is activated, the address is safe or the asset is
// supported, and it cannot be used to distinguish networks.
func NormalizeAddressBase58(value string) (string, error) {
	address, err := decodeAddress(value)
	if err != nil {
		return "", err
	}
	return encodeAddress(address), nil
}

// AddressForSigning explicitly converts a valid TRON address into the 20-byte 0x address
// required by TIP-712. It only converts the format: it does not verify any signature or
// authorization, and it does not accept arbitrary EVM addresses as input.
func AddressForSigning(value string) (string, error) {
	address, err := decodeAddress(value)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(address[1:]), nil
}

func decodeAddress(value string) ([21]byte, error) {
	var address [21]byte
	switch len(value) {
	case 42:
		if _, err := hex.Decode(address[:], []byte(value)); err != nil || address[0] != 0x41 {
			return [21]byte{}, ErrInvalidAddress
		}
		return address, nil
	case 34:
		if value[0] != 'T' {
			return address, ErrInvalidAddress
		}
	default:
		return address, ErrInvalidAddress
	}

	// 固定 25 字节承载 21 字节地址与 4 字节校验和，避免外部输入驱动大整数分配。
	var checked [25]byte
	for index := range len(value) {
		carry := strings.IndexByte(base58Alphabet, value[index])
		if carry < 0 {
			return address, ErrInvalidAddress
		}
		for offset := len(checked) - 1; offset >= 0; offset-- {
			carry += int(checked[offset]) * 58
			checked[offset] = byte(carry)
			carry >>= 8
		}
		if carry != 0 {
			return address, ErrInvalidAddress
		}
	}
	if checked[0] != 0x41 {
		return address, ErrInvalidAddress
	}
	first := sha256.Sum256(checked[:21])
	second := sha256.Sum256(first[:])
	if !bytes.Equal(checked[21:], second[:4]) {
		return address, ErrInvalidAddress
	}
	copy(address[:], checked[:21])
	return address, nil
}

func encodeAddress(address [21]byte) string {
	var checked [25]byte
	copy(checked[:21], address[:])
	first := sha256.Sum256(address[:])
	second := sha256.Sum256(first[:])
	copy(checked[21:], second[:4])

	// 0x41 前缀的 25 字节 Base58Check 编码固定为 34 位，无需处理其他网络的前导零。
	var encoded [34]byte
	for index := len(encoded) - 1; index >= 0; index-- {
		remainder := 0
		for offset := range checked {
			current := remainder*256 + int(checked[offset])
			checked[offset] = byte(current / 58)
			remainder = current % 58
		}
		encoded[index] = base58Alphabet[remainder]
	}
	return string(encoded[:])
}
