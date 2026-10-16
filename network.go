package tron

import (
	"errors"
	"strconv"
	"strings"
)

const (
	// NetworkMainnet 是 TRON 主网的规范 CAIP-2 标识，不代表已支持主网结算。
	NetworkMainnet = "tron:728126428"
	// NetworkNile 是 TRON Nile 测试网的规范 CAIP-2 标识。
	NetworkNile = "tron:3448148188"
	// NetworkShasta 是 TRON Shasta 测试网的规范 CAIP-2 标识。
	NetworkShasta = "tron:2494104990"
)

// ErrUnsupportedNetwork 表示网络不是已识别的 TRON 网络或历史别名。
var ErrUnsupportedNetwork = errors.New("不支持的 TRON 网络")

// NormalizeNetwork 将已知网络或历史十六进制别名规范化为十进制 CAIP-2 标识。
// 输入不接受首尾空白、通配符或未知网络。
func NormalizeNetwork(network string) (string, error) {
	chainID, err := ChainID(network)
	if err != nil {
		return "", err
	}
	return "tron:" + strconv.FormatUint(uint64(chainID), 10), nil
}

// ChainID 返回已知网络的链 ID，仅十六进制引用不区分大小写。
// 网络标识用于隔离签名域，不能根据地址格式推断。
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
