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

	"golang.org/x/sync/semaphore"
)

const (
	maxRPCResponseBytes       = 1 << 20
	maxConcurrentPreflightRPC = 6
)

type readRPC struct {
	endpoint string
	apiKey   string
	client   *http.Client
	slots    *semaphore.Weighted
	batch    bool
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
	return &readRPC{endpoint: endpoint.String(), apiKey: config.APIKey, client: &client, slots: semaphore.NewWeighted(maxConcurrentPreflightRPC), batch: config.BatchRPC}, nil
}

type rpcQuery struct {
	method   string
	params   []interface{}
	result   interface{}
	validate func() error
}

type rpcRequest struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      int           `json:"id"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

func (rpc *readRPC) call(ctx context.Context, method string, params []interface{}, result interface{}) error {
	body, err := json.Marshal(rpcRequest{"2.0", 1, method, params})
	if err != nil {
		return ErrNodeRead
	}
	data, err := rpc.exchange(ctx, body, 1)
	if err != nil {
		return fmt.Errorf("调用只读 RPC %s: %w", method, err)
	}
	value, err := rpcResult(data, 1)
	if err != nil || json.Unmarshal(value, result) != nil {
		return ErrNodeRead
	}
	return nil
}

// group 只接受本包固定检查组，不提供外部任意方法或自动单请求降级。
func (rpc *readRPC) group(ctx context.Context, queries ...rpcQuery) error {
	if len(queries) == 0 || len(queries) > maxConcurrentPreflightRPC {
		return ErrNodeRead
	}
	if !rpc.batch {
		checks := make([]func(context.Context) error, len(queries))
		for index, query := range queries {
			checks[index] = func(ctx context.Context) error {
				if err := rpc.call(ctx, query.method, query.params, query.result); err != nil {
					return err
				}
				if query.validate != nil {
					return query.validate()
				}
				return nil
			}
		}
		return parallelChecks(ctx, checks...)
	}
	requests := make([]rpcRequest, len(queries))
	for index, query := range queries {
		requests[index] = rpcRequest{"2.0", index + 1, query.method, query.params}
	}
	body, err := json.Marshal(requests)
	if err != nil {
		return ErrNodeRead
	}
	data, err := rpc.exchange(ctx, body, int64(len(queries)))
	if err != nil {
		return err
	}
	var responses []json.RawMessage
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '[' || json.Unmarshal(data, &responses) != nil || len(responses) != len(queries) {
		return ErrNodeRead
	}
	values := make([]json.RawMessage, len(queries))
	for _, response := range responses {
		fields, err := nativeObject(response)
		var id int
		if err != nil || json.Unmarshal(fields["id"], &id) != nil || id < 1 || id > len(queries) || values[id-1] != nil {
			return ErrNodeRead
		}
		value, err := rpcResult(response, id)
		if err != nil {
			return err
		}
		values[id-1] = value
	}
	// 完整验证对应关系后才解码；任何一项失败均不能进入后续预检阶段。
	for index, query := range queries {
		if values[index] == nil || json.Unmarshal(values[index], query.result) != nil {
			return ErrNodeRead
		}
		if query.validate != nil {
			if err := query.validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func rpcResult(data []byte, id int) (json.RawMessage, error) {
	fields, err := nativeObject(data)
	var version string
	_, hasError := fields["error"]
	if err != nil || hasError || json.Unmarshal(fields["jsonrpc"], &version) != nil || version != "2.0" || string(fields["id"]) != strconv.Itoa(id) || len(fields["result"]) == 0 || string(fields["result"]) == "null" {
		return nil, ErrNodeRead
	}
	return fields["result"], nil
}

func (rpc *readRPC) exchange(ctx context.Context, body []byte, weight int64) ([]byte, error) {
	// 同一客户端的所有预检共用额度，排队及响应读取均计入调用方原有总预算。
	if err := rpc.slots.Acquire(ctx, weight); err != nil {
		return nil, err
	}
	defer rpc.slots.Release(weight)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, rpc.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrNodeRead
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if rpc.apiKey != "" {
		request.Header.Set("TRON-PRO-API-KEY", rpc.apiKey)
	}
	response, err := rpc.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrNodeRead
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrNodeRead
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRPCResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrNodeRead
	}
	if len(data) > maxRPCResponseBytes {
		return nil, ErrNodeRead
	}
	return data, nil
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

func codeQuery(address [20]byte, result *string) rpcQuery {
	return rpcQuery{method: "eth_getCode", params: []interface{}{"0x" + hex.EncodeToString(address[:]), "latest"}, result: result}
}

func rpcCode(result string) ([]byte, error) {
	if len(result) < 2 || len(result) > maxRPCResponseBytes || !strings.HasPrefix(result, "0x") || len(result)%2 != 0 {
		return nil, ErrNodeRead
	}
	code, err := hex.DecodeString(result[2:])
	if err != nil {
		return nil, ErrNodeRead
	}
	return code, nil
}

func wordQuery(result *string, payer, contract [20]byte, signature string, arguments ...[32]byte) rpcQuery {
	selector := keccak([]byte(signature))
	data := "0x" + hex.EncodeToString(selector[:4])
	for _, argument := range arguments {
		data += hex.EncodeToString(argument[:])
	}
	return rpcQuery{method: "eth_call", params: []interface{}{map[string]string{
		"from": "0x" + hex.EncodeToString(payer[:]), "to": "0x" + hex.EncodeToString(contract[:]), "data": data,
	}, "latest"}, result: result}
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

type rpcBlock struct {
	Number    string `json:"number"`
	Hash      string `json:"hash"`
	Timestamp string `json:"timestamp"`
}

func (block *rpcBlock) UnmarshalJSON(data []byte) error {
	fields, err := nativeObject(data)
	if err != nil || json.Unmarshal(fields["number"], &block.Number) != nil || json.Unmarshal(fields["hash"], &block.Hash) != nil || json.Unmarshal(fields["timestamp"], &block.Timestamp) != nil {
		return ErrNodeRead
	}
	return nil
}

func parseHead(block rpcBlock) (observedHead, error) {
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
