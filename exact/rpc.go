package exact

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxRPCResponseBytes = 1 << 20

type readRPC struct {
	endpoint string
	apiKey   string
	client   *http.Client
}

func newReadRPC(config PreflightConfig) (*readRPC, error) {
	endpoint, err := url.Parse(config.NodeURL)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery || strings.Contains(config.NodeURL, "#") || endpoint.Opaque != "" || strings.ContainsAny(config.APIKey, "\r\n") || len(config.APIKey) > 4096 {
		return nil, ErrPreflightConfig
	}
	if endpoint.Scheme != "https" {
		ip := net.ParseIP(endpoint.Hostname())
		local := endpoint.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())
		if endpoint.Scheme != "http" || !config.AllowInsecureLocalHTTP || !local {
			return nil, ErrPreflightConfig
		}
	}
	client := http.Client{Timeout: config.Timeout}
	if config.HTTPClient != nil {
		client = *config.HTTPClient
		if client.Timeout == 0 || client.Timeout > config.Timeout {
			client.Timeout = config.Timeout
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &readRPC{endpoint: endpoint.String(), apiKey: config.APIKey, client: &client}, nil
}

func (rpc *readRPC) call(ctx context.Context, method string, params []interface{}, result interface{}) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("调用只读 RPC %s: %w", method, err)
		}
	}()
	// 方法仅由本包的固定只读调用点指定，不导出任意 RPC 转发入口。
	body, err := json.Marshal(struct {
		JSONRPC string        `json:"jsonrpc"`
		ID      int           `json:"id"`
		Method  string        `json:"method"`
		Params  []interface{} `json:"params"`
	}{"2.0", 1, method, params})
	if err != nil {
		return ErrNodeRead
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, rpc.endpoint, bytes.NewReader(body))
	if err != nil {
		return ErrNodeRead
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if rpc.apiKey != "" {
		request.Header.Set("TRON-PRO-API-KEY", rpc.apiKey)
	}
	response, err := rpc.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrNodeRead
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ErrNodeRead
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRPCResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrNodeRead
	}
	if len(data) > maxRPCResponseBytes {
		return ErrNodeRead
	}
	// 复用已有严格对象解析；按精确键读取，避免重复字段或大小写覆盖把错误变为成功。
	fields, err := nativeObject(data)
	var version string
	if err != nil || json.Unmarshal(fields["jsonrpc"], &version) != nil || version != "2.0" || string(fields["id"]) != "1" || len(fields["result"]) == 0 || string(fields["result"]) == "null" {
		return ErrNodeRead
	}
	if json.Unmarshal(fields["result"], result) != nil {
		return ErrNodeRead
	}
	return nil
}

// simulateSettlement 只调用eth_call，不提供任意合约/方法转发入口；调用数据来自本包固定编码器。
func (rpc *readRPC) simulateSettlement(ctx context.Context, executor, proxy [20]byte, callData []byte) error {
	var result string
	if err := rpc.call(ctx, "eth_call", []interface{}{map[string]string{
		"from": "0x" + hex.EncodeToString(executor[:]), "to": "0x" + hex.EncodeToString(proxy[:]),
		"data": "0x" + hex.EncodeToString(callData), "value": "0x0",
	}, "latest"}, &result); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.Join(ErrSettlementSimulation, err)
	}
	// 固定settle ABI没有返回值，非空数据不能被当成成功或布尔true。
	if result != "0x" {
		return ErrSettlementSimulation
	}
	return nil
}

func (rpc *readRPC) code(ctx context.Context, address [20]byte) ([]byte, error) {
	var result string
	if err := rpc.call(ctx, "eth_getCode", []interface{}{"0x" + hex.EncodeToString(address[:]), "latest"}, &result); err != nil {
		return nil, err
	}
	if len(result) < 2 || len(result) > maxRPCResponseBytes || !strings.HasPrefix(result, "0x") || len(result)%2 != 0 {
		return nil, ErrNodeRead
	}
	code, err := hex.DecodeString(result[2:])
	if err != nil {
		return nil, ErrNodeRead
	}
	return code, nil
}

func (rpc *readRPC) contractWord(ctx context.Context, payer, contract [20]byte, signature string, arguments ...[32]byte) ([32]byte, error) {
	selector := keccak([]byte(signature))
	data := "0x" + hex.EncodeToString(selector[:4])
	for _, argument := range arguments {
		data += hex.EncodeToString(argument[:])
	}
	var result string
	if err := rpc.call(ctx, "eth_call", []interface{}{map[string]string{
		"from": "0x" + hex.EncodeToString(payer[:]), "to": "0x" + hex.EncodeToString(contract[:]), "data": data,
	}, "latest"}, &result); err != nil {
		return [32]byte{}, err
	}
	return rpcWord(result)
}

func rpcWord(value string) ([32]byte, error) {
	var word [32]byte
	if len(value) != 66 || !strings.HasPrefix(value, "0x") {
		return word, ErrNodeRead
	}
	if _, err := hex.Decode(word[:], []byte(value[2:])); err != nil {
		return [32]byte{}, ErrNodeRead
	}
	return word, nil
}

func rpcQuantity(value string) (uint64, error) {
	if len(value) < 3 || len(value) > 18 || !strings.HasPrefix(value, "0x") || (len(value) > 3 && value[2] == '0') {
		return 0, ErrNodeRead
	}
	for _, digit := range value[2:] {
		if !(digit >= '0' && digit <= '9' || digit >= 'a' && digit <= 'f' || digit >= 'A' && digit <= 'F') {
			return 0, ErrNodeRead
		}
	}
	number, err := strconv.ParseUint(value[2:], 16, 64)
	if err != nil {
		return 0, ErrNodeRead
	}
	return number, nil
}

type observedHead struct {
	Number    uint64
	Hash      [32]byte
	Timestamp int64
}

func (rpc *readRPC) head(ctx context.Context) (observedHead, error) {
	var block struct {
		Number    string `json:"number"`
		Hash      string `json:"hash"`
		Timestamp string `json:"timestamp"`
	}
	if err := rpc.call(ctx, "eth_getBlockByNumber", []interface{}{"latest", false}, &block); err != nil {
		return observedHead{}, err
	}
	height, heightErr := rpcQuantity(block.Number)
	hash, hashErr := rpcWord(block.Hash)
	timestamp, timestampErr := rpcQuantity(block.Timestamp)
	if heightErr != nil || hashErr != nil || timestampErr != nil || hash == [32]byte{} || binary.BigEndian.Uint64(hash[:8]) != height || timestamp > math.MaxInt64 {
		return observedHead{}, ErrNodeRead
	}
	return observedHead{Number: height, Hash: hash, Timestamp: int64(timestamp)}, nil
}

func (head observedHead) fresh(now time.Time) error {
	// 直接比较秒数，避免恶意超大时间戳在 time.Unix/Duration 运算中溢出。
	age := new(big.Int).Sub(big.NewInt(now.Unix()), big.NewInt(head.Timestamp))
	if age.Cmp(big.NewInt(60)) > 0 || age.Cmp(big.NewInt(-5)) < 0 || (age.Cmp(big.NewInt(60)) == 0 && now.Nanosecond() > 0) {
		return ErrStaleHead
	}
	return nil
}
