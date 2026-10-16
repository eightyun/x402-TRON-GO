package exact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	tron "github.com/eightyun/x402-tron-go"
)

var (
	ErrExecutionConfig   = errors.New("原生结算节点配置无效")
	ErrExecutionInput    = errors.New("原交易查询参数无效")
	ErrExecutionNode     = errors.New("原生节点访问失败或响应无效")
	ErrBroadcastUnknown  = errors.New("原交易广播结果未知，必须保留并追踪原交易")
	ErrBroadcastRejected = errors.New("节点拒绝了本次广播请求，不代表原交易最终失败")
)

// ExecutionConfig 固定一个原生 FullNode 根 URL 与独立核验的完整创世区块 ID。
// 不自动学习 GenesisBlockID，不从付款请求获取配置；自定义 Transport 不得重试写请求。
type ExecutionConfig struct {
	Network                string
	NodeURL                string
	GenesisBlockID         string
	APIKey                 string `json:"-"`
	Timeout                time.Duration
	HTTPClient             *http.Client `json:"-"`
	AllowInsecureLocalHTTP bool
}

// ExecutionClient 仅提供单次原交易广播和只读查找，不构建、签名或自动重试。
// 跨进程幂等由持久化日志提供，不能绕过日志直接把 Broadcast 当完整结算。
type ExecutionClient struct {
	network   string
	genesis   [32]byte
	transport *readRPC
	timeout   time.Duration
}

// TransactionObservation 仅表示节点当前是否返回原交易主体，不代表执行成功或固化。
type TransactionObservation struct {
	Network       string
	TransactionID string
	Found         bool
}

// NewExecutionClient 复用现有 HTTP 安全配置，构造时不访问节点。
// GenesisBlockID 为无 0x 的 32 字节 hex，必须由部署方独立核验。
func NewExecutionClient(config ExecutionConfig) (*ExecutionClient, error) {
	network, err := tron.NormalizeNetwork(config.Network)
	chainID, chainErr := tron.ChainID(config.Network)
	var genesis [32]byte
	if err != nil || chainErr != nil || len(config.GenesisBlockID) != 64 {
		return nil, ErrExecutionConfig
	}
	if _, err := hex.Decode(genesis[:], []byte(config.GenesisBlockID)); err != nil || binary.BigEndian.Uint64(genesis[:8]) != 0 || binary.BigEndian.Uint32(genesis[28:]) != chainID {
		return nil, ErrExecutionConfig
	}
	if config.Timeout == 0 {
		config.Timeout = 15 * time.Second
	}
	if config.Timeout < 0 || config.Timeout > time.Minute {
		return nil, ErrExecutionConfig
	}
	// 复用 URL、TLS、API key、超时和禁止重定向策略，不调用只读 JSON-RPC 方法。
	transport, err := newReadRPC(PreflightConfig{
		NodeURL: config.NodeURL, APIKey: config.APIKey, Timeout: config.Timeout,
		HTTPClient: config.HTTPClient, AllowInsecureLocalHTTP: config.AllowInsecureLocalHTTP,
	})
	if err != nil {
		return nil, ErrExecutionConfig
	}
	endpoint, err := url.Parse(transport.endpoint)
	if err != nil || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawPath != "" {
		return nil, ErrExecutionConfig
	}
	transport.endpoint = strings.TrimSuffix(transport.endpoint, "/")
	return &ExecutionClient{network: network, genesis: genesis, transport: transport, timeout: config.Timeout}, nil
}

// Network 返回固定网络；零值客户端返回空字符串。
func (client *ExecutionClient) Network() string {
	if client == nil {
		return ""
	}
	return client.network
}

// Broadcast 只发送一次已准备且已持久化的原始字节；调用前必须取得日志广播许可。
// nil 仅表示节点明确受理且前后网络核对通过，绝不能作为付款成功凭据。
// 错误、拒绝、超时或提交后中断均不得释放占用、重签或另行构建付款。
func (client *ExecutionClient) Broadcast(ctx context.Context, prepared *PreparedSettlement, transaction SettlementTransaction, now time.Time) error {
	if !client.valid() {
		return ErrExecutionConfig
	}
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Now()
	if err := prepared.ValidateBroadcast(transaction, now); err != nil {
		return err
	}
	if prepared.Authorization().Network != client.network {
		return ErrChainMismatch
	}
	if err := client.checkNetwork(ctx); err != nil {
		return err
	}
	if err := prepared.ValidateBroadcast(transaction, now.Add(time.Since(started))); err != nil {
		return err
	}
	result, err := client.post(ctx, "/wallet/broadcasthex", map[string]string{"transaction": hex.EncodeToString(transaction.Payload)})
	if err != nil {
		return errors.Join(ErrBroadcastUnknown, err)
	}
	var accepted bool
	var id, code string
	if json.Unmarshal(result["result"], &accepted) != nil || string(result["result"]) == "null" || json.Unmarshal(result["txid"], &id) != nil || id != transaction.ID {
		return errors.Join(ErrBroadcastUnknown, ErrExecutionNode)
	}
	if raw, exists := result["code"]; exists && (string(raw) == "null" || json.Unmarshal(raw, &code) != nil) {
		return errors.Join(ErrBroadcastUnknown, ErrExecutionNode)
	}
	if err := client.checkNetwork(ctx); err != nil {
		return errors.Join(ErrBroadcastUnknown, err)
	}
	if !accepted {
		if code == "SUCCESS" {
			return errors.Join(ErrBroadcastUnknown, ErrExecutionNode)
		}
		return ErrBroadcastRejected
	}
	if code != "" && code != "SUCCESS" {
		return errors.Join(ErrBroadcastUnknown, ErrExecutionNode)
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(ErrBroadcastUnknown, err)
	}
	return nil
}

// Lookup 只查询 FullNode 的原交易主体，不读取执行回执或提供固化成功判断。
// 空对象表示本次未找到，不能据此判失败、释放占用或重签；错误仍保留原 txid。
func (client *ExecutionClient) Lookup(ctx context.Context, transactionID string) (TransactionObservation, error) {
	if !client.valid() {
		return TransactionObservation{}, ErrExecutionConfig
	}
	if !validExecutionID(transactionID) {
		return TransactionObservation{}, ErrExecutionInput
	}
	observation := TransactionObservation{Network: client.network, TransactionID: transactionID}
	ctx, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()
	if err := client.checkNetwork(ctx); err != nil {
		return observation, err
	}
	result, err := client.post(ctx, "/wallet/gettransactionbyid", map[string]string{"value": transactionID})
	if err != nil {
		return observation, err
	}
	if len(result) != 0 {
		var id, rawHex string
		if json.Unmarshal(result["txID"], &id) != nil || id != transactionID || json.Unmarshal(result["raw_data_hex"], &rawHex) != nil || len(rawHex) == 0 || len(rawHex) > maxRPCResponseBytes || len(rawHex)%2 != 0 {
			return observation, ErrExecutionNode
		}
		raw, err := hex.DecodeString(rawHex)
		if err != nil {
			return observation, ErrExecutionNode
		}
		digest := sha256.Sum256(raw)
		if hex.EncodeToString(digest[:]) != transactionID {
			return observation, ErrExecutionNode
		}
	}
	if err := client.checkNetwork(ctx); err != nil {
		return observation, err
	}
	if err := ctx.Err(); err != nil {
		return observation, err
	}
	observation.Found = len(result) != 0
	return observation, nil
}

func (client *ExecutionClient) valid() bool {
	return client != nil && client.transport != nil && client.network != ""
}

func validExecutionID(id string) bool {
	if len(id) != 64 || id == strings.Repeat("0", 64) || strings.ToLower(id) != id {
		return false
	}
	var decoded [32]byte
	_, err := hex.Decode(decoded[:], []byte(id))
	return err == nil
}

func (client *ExecutionClient) checkNetwork(ctx context.Context) error {
	result, err := client.post(ctx, "/wallet/getblockbynum", map[string]int{"num": 0})
	if err != nil {
		return err
	}
	var id string
	if json.Unmarshal(result["blockID"], &id) != nil || len(id) != 64 {
		return ErrExecutionNode
	}
	var decoded [32]byte
	if _, err := hex.Decode(decoded[:], []byte(id)); err != nil {
		return ErrExecutionNode
	}
	if decoded != client.genesis {
		return ErrChainMismatch
	}
	return nil
}

func (client *ExecutionClient) post(ctx context.Context, path string, params interface{}) (map[string]json.RawMessage, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, ErrExecutionNode
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.transport.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, ErrExecutionNode
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if client.transport.apiKey != "" {
		request.Header.Set("TRON-PRO-API-KEY", client.transport.apiKey)
	}
	response, err := client.transport.client.Do(request)
	if err != nil {
		return nil, executionError(ctx, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrExecutionNode
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxRPCResponseBytes+1))
	if err != nil {
		return nil, executionError(ctx, err)
	}
	if len(data) > maxRPCResponseBytes {
		return nil, ErrExecutionNode
	}
	fields, err := nativeObject(data)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return fields, err
}

func executionError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrExecutionNode
}

func nativeObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, ErrExecutionNode
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, ErrExecutionNode
		}
		key, ok := token.(string)
		if !ok {
			return nil, ErrExecutionNode
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, ErrExecutionNode
		}
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, ErrExecutionNode
		}
		if (key == "Error" || key == "error") && string(value) != "null" {
			return nil, ErrExecutionNode
		}
		fields[key] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, ErrExecutionNode
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrExecutionNode
	}
	return fields, nil
}
