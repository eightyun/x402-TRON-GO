package main

import (
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	tron "github.com/eightyun/x402-tron-go"
	"github.com/eightyun/x402-tron-go/exact"
	"github.com/eightyun/x402-tron-go/exact/postgres"
)

var errConfiguration = errors.New("facilitator 示例配置或部署前置条件无效")

type configuration struct {
	address     string
	databaseURL string
	keyFile     string
	http        postgres.HTTPConfig
	mechanism   postgres.FacilitatorConfig
}

func loadConfig() (configuration, error) {
	network := os.Getenv("X402_NETWORK")
	if network == "" {
		network = tron.NetworkNile
	}
	network, err := tron.NormalizeNetwork(network)
	if err != nil || network == tron.NetworkMainnet {
		return configuration{}, errConfiguration
	}
	address := os.Getenv("X402_FACILITATOR_LISTEN_ADDR")
	if address == "" {
		address = "127.0.0.1:8081"
	}
	host, port, err := net.SplitHostPort(address)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 1 || number > 65535 || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return configuration{}, errConfiguration
	}
	fee, err := strconv.ParseInt(os.Getenv("X402_FEE_LIMIT_SUN"), 10, 64)
	if err != nil || fee <= 0 {
		return configuration{}, errConfiguration
	}
	parallel := 8
	if value := os.Getenv("X402_MAX_CONCURRENT"); value != "" {
		parallel, err = strconv.Atoi(value)
		if err != nil || parallel < 1 || parallel > 64 {
			return configuration{}, errConfiguration
		}
	}
	timeout := 90 * time.Second
	if value := os.Getenv("X402_SETTLEMENT_TIMEOUT"); value != "" {
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout < time.Second || timeout > 3*time.Minute {
			return configuration{}, errConfiguration
		}
	}
	authorization := exact.Config{Network: network, Permit2Address: os.Getenv("X402_PERMIT2_ADDRESS"), ProxyAddress: os.Getenv("X402_PROXY_ADDRESS")}
	config := configuration{
		address: address, databaseURL: os.Getenv("X402_DATABASE_URL"), keyFile: os.Getenv("X402_EXECUTOR_KEY_FILE"),
		http: postgres.HTTPConfig{BearerToken: os.Getenv("X402_FACILITATOR_TOKEN"), MaxConcurrent: parallel},
		mechanism: postgres.FacilitatorConfig{
			Preflight: exact.PreflightConfig{Authorization: authorization, NodeURL: os.Getenv("X402_JSON_RPC_URL"),
				APIKey: os.Getenv("X402_NODE_API_KEY"), Permit2CodeHash: os.Getenv("X402_PERMIT2_CODE_HASH"), ProxyCodeHash: os.Getenv("X402_PROXY_CODE_HASH")},
			Execution: exact.ExecutionConfig{Network: network, NodeURL: os.Getenv("X402_NATIVE_NODE_URL"),
				APIKey: os.Getenv("X402_NODE_API_KEY"), GenesisBlockID: os.Getenv("X402_GENESIS_BLOCK_ID")},
			Policy:    exact.ServerConfig{Network: network, Asset: os.Getenv("X402_ASSET"), PayTo: os.Getenv("X402_PAY_TO")},
			MaxAmount: os.Getenv("X402_MAX_AMOUNT_ATOMIC"), FeeLimitSUN: fee, Lifetime: 60 * time.Second, Timeout: timeout,
		},
	}
	if config.databaseURL == "" || !filepath.IsAbs(config.keyFile) || strings.TrimSpace(config.keyFile) != config.keyFile {
		return configuration{}, errConfiguration
	}
	return config, nil
}

func loadSigner(path string) (*exact.EOASigner, error) {
	if !filepath.IsAbs(path) {
		return nil, errConfiguration
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errConfiguration
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || (info.Mode().Perm() != 0400 && info.Mode().Perm() != 0600) || (info.Size() != 64 && info.Size() != 65) {
		return nil, errConfiguration
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return nil, errConfiguration
	}
	data, err := io.ReadAll(io.LimitReader(file, 66))
	defer clear(data)
	if err != nil {
		return nil, errConfiguration
	}
	if len(data) == 65 && data[64] == '\n' {
		data = data[:64]
	}
	var key [32]byte
	defer clear(key[:])
	if len(data) != 64 {
		return nil, errConfiguration
	}
	if _, err := hex.Decode(key[:], data); err != nil {
		return nil, errConfiguration
	}
	signer, err := exact.NewEOASigner(key[:])
	if err != nil {
		return nil, errConfiguration
	}
	return signer, nil
}
