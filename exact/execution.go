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
	ErrExecutionConfig   = errors.New("exact: invalid execution node configuration")
	ErrExecutionInput    = errors.New("exact: invalid transaction lookup input")
	ErrExecutionNode     = errors.New("exact: native node request failed or returned an invalid response")
	ErrBroadcastUnknown  = errors.New("exact: broadcast outcome unknown; keep and track the original transaction")
	ErrBroadcastRejected = errors.New("exact: node rejected this broadcast attempt; the original transaction has not necessarily failed")
)

// ExecutionConfig pins a single native node root URL and an independently verified full genesis
// block ID. Broadcast and lookup use the wallet API; solidified verification additionally
// requires the same configured endpoint to serve walletsolidity. GenesisBlockID is never learned
// automatically and the configuration is never taken from a payment request. A custom Transport
// must not retry write requests.
type ExecutionConfig struct {
	Network                string
	NodeURL                string
	GenesisBlockID         string
	APIKey                 string `json:"-"`
	Timeout                time.Duration
	HTTPClient             *http.Client `json:"-"`
	AllowInsecureLocalHTTP bool
}

// ExecutionClient provides single broadcasts of the original transaction, read-only lookups and
// solidified payment verification. It does not build, sign or automatically retry. Cross-process
// idempotency is provided by the persistent journal; Broadcast must not be used as a complete
// settlement by bypassing the journal.
type ExecutionClient struct {
	network   string
	genesis   [32]byte
	transport *readRPC
	timeout   time.Duration
}

// TransactionObservation only reports whether the node currently returns the original transaction
// body. It does not indicate execution success or solidification.
type TransactionObservation struct {
	Network       string
	TransactionID string
	Found         bool
}

// NewExecutionClient reuses the existing HTTP security configuration and does not contact the node
// during construction. GenesisBlockID is a 32-byte hex string without a 0x prefix and must be
// independently verified by the deployer.
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

// Network returns the pinned network. A nil or zero-value client returns an empty string.
func (client *ExecutionClient) Network() string {
	if client == nil {
		return ""
	}
	return client.network
}

// Broadcast sends the prepared and already persisted raw bytes exactly once. The caller must obtain
// broadcast permission from the journal before calling it. A nil error only means the node
// explicitly accepted the transaction and the network checks before and after passed; it must
// never be treated as proof of payment success. On an error, rejection, timeout or interruption
// after submission, the caller must not release the reservation, re-sign or build another payment.
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

// Lookup only queries the FullNode for the original transaction body. It does not read the
// execution receipt or determine solidified success. An empty object means the transaction was not
// found this time; it must not be used to conclude failure, release the reservation or re-sign.
// Errors still keep the original txid.
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
