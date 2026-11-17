package tron

import (
	"errors"
	"strconv"
	"strings"
)

const (
	// NetworkMainnet is the canonical CAIP-2 identifier of TRON mainnet. Its presence does not
	// mean mainnet settlement is supported.
	NetworkMainnet = "tron:728126428"
	// NetworkNile is the canonical CAIP-2 identifier of the TRON Nile testnet.
	NetworkNile = "tron:3448148188"
	// NetworkShasta is the canonical CAIP-2 identifier of the TRON Shasta testnet.
	NetworkShasta = "tron:2494104990"
)

// ErrUnsupportedNetwork reports that the network is not a recognized TRON network or legacy
// alias.
var ErrUnsupportedNetwork = errors.New("tron: unsupported TRON network")

// NormalizeNetwork normalizes a known network or legacy hex alias to its decimal CAIP-2
// identifier. Leading or trailing whitespace, wildcards and unknown networks are rejected.
func NormalizeNetwork(network string) (string, error) {
	chainID, err := ChainID(network)
	if err != nil {
		return "", err
	}
	return "tron:" + strconv.FormatUint(uint64(chainID), 10), nil
}

// ChainID returns the chain ID of a known network. Only hex references are matched
// case-insensitively. The network identifier isolates signing domains and must not be inferred
// from the address format.
func ChainID(network string) (uint32, error) {
	if len(network) > len(NetworkNile) || !strings.HasPrefix(network, "tron:") {
		return 0, ErrUnsupportedNetwork
	}
	switch strings.ToLower(network[len("tron:"):]) {
	case "728126428", "0x2b6653dc":
		return 728126428, nil
	case "3448148188", "0xcd8690dc":
		return 3448148188, nil
	case "2494104990", "0x94a9059e":
		return 2494104990, nil
	default:
		return 0, ErrUnsupportedNetwork
	}
}
