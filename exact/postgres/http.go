package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	x402 "github.com/x402-foundation/x402/go/v2"
	"github.com/x402-foundation/x402/go/v2/types"
)

var ErrHTTPConfig = errors.New("facilitator HTTP 配置无效")

// HTTPConfig 固定服务间令牌及每进程同时执行的预检/结算上限；不替代 TLS、累计费用预算和入口限流。
type HTTPConfig struct {
	BearerToken   string `json:"-"`
	MaxConcurrent int
}

// NewHTTPHandler 通过官方 core 暴露 /supported、/verify、/settle；不启动监听或访问节点/数据库。
// 调用方须配置 HTTP 读写超时、TLS 入口；所有路径都要求 32~256 字节无空白 ASCII Bearer 令牌。
func NewHTTPHandler(mechanism *Facilitator, config HTTPConfig) (http.Handler, error) {
	if !mechanism.valid() || len(config.BearerToken) < 32 || len(config.BearerToken) > 256 {
		return nil, ErrHTTPConfig
	}
	for _, char := range []byte(config.BearerToken) {
		if char < 33 || char > 126 {
			return nil, ErrHTTPConfig
		}
	}
	if config.MaxConcurrent == 0 {
		config.MaxConcurrent = 8
	}
	if config.MaxConcurrent < 1 || config.MaxConcurrent > 64 {
		return nil, ErrHTTPConfig
	}
	credential := sha256.Sum256([]byte("Bearer " + config.BearerToken))
	active := make(chan struct{}, config.MaxConcurrent)
	core := x402.Newx402Facilitator().Register([]x402.Network{x402.Network(mechanism.Network())}, mechanism)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		provided := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare(credential[:], provided[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeHTTPJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		method := http.MethodPost
		switch r.URL.Path {
		case "/supported":
			method = http.MethodGet
		case "/verify", "/settle":
		default:
			writeHTTPJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeHTTPJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		if r.URL.RawQuery != "" || r.URL.ForceQuery {
			writeHTTPJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		if r.URL.Path == "/supported" {
			writeHTTPJSON(w, http.StatusOK, core.GetSupported())
			return
		}
		select {
		case active <- struct{}{}:
			defer func() { <-active }()
		default:
			writeHTTPJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "capacity_exceeded"})
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" || r.Header.Get("Content-Encoding") != "" {
			writeHTTPJSON(w, http.StatusUnsupportedMediaType, map[string]string{"error": "json_required"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			status := http.StatusBadRequest
			var size *http.MaxBytesError
			if errors.As(err, &size) {
				status = http.StatusRequestEntityTooLarge
			}
			writeHTTPJSON(w, status, map[string]string{"error": "invalid_request"})
			return
		}
		var request struct {
			X402Version         int                       `json:"x402Version"`
			PaymentPayload      types.PaymentPayload      `json:"paymentPayload"`
			PaymentRequirements types.PaymentRequirements `json:"paymentRequirements"`
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		if !uniqueHTTPJSON(body) || decoder.Decode(&request) != nil || request.X402Version != 2 || request.PaymentPayload.X402Version != 2 {
			writeHTTPJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		payload, _ := json.Marshal(request.PaymentPayload)
		requirements, _ := json.Marshal(request.PaymentRequirements)
		ctx, cancel := context.WithTimeout(r.Context(), mechanism.timeout+5*time.Second)
		defer cancel()
		if r.URL.Path == "/verify" {
			result, err := core.Verify(ctx, payload, requirements)
			if err == nil && result != nil {
				writeHTTPJSON(w, http.StatusOK, result)
				return
			}
			response := x402.VerifyResponse{InvalidReason: "verification_unavailable"}
			var failure *x402.VerifyError
			if errors.As(err, &failure) {
				response.InvalidReason, response.Payer = failure.InvalidReason, failure.Payer
			}
			writeHTTPJSON(w, http.StatusBadRequest, response)
			return
		}
		result, err := core.Settle(ctx, payload, requirements)
		if err == nil && result != nil {
			writeHTTPJSON(w, http.StatusOK, result)
			return
		}
		response := x402.SettleResponse{ErrorReason: "settlement_unavailable", Network: x402.Network(mechanism.Network())}
		var failure *x402.SettleError
		if errors.As(err, &failure) {
			response.ErrorReason, response.Payer, response.Transaction = failure.ErrorReason, failure.Payer, failure.Transaction
		}
		// pending 保留原 txid；官方客户端可原样恢复，不输出任意错误原文。
		writeHTTPJSON(w, http.StatusBadRequest, response)
	}), nil
}

func writeHTTPJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// uniqueHTTPJSON 拒绝任何层级的歧义键及尾随值；深度限制防止恶意嵌套消耗栈。
func uniqueHTTPJSON(body []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		switch token {
		case json.Delim('{'):
			keys := make(map[string]bool)
			for decoder.More() {
				token, err := decoder.Token()
				key, ok := token.(string)
				key = strings.ToLower(key)
				if err != nil || !ok || keys[key] || !value(depth+1) {
					return false
				}
				keys[key] = true
			}
			token, err = decoder.Token()
			return err == nil && token == json.Delim('}')
		case json.Delim('['):
			for decoder.More() {
				if !value(depth + 1) {
					return false
				}
			}
			token, err = decoder.Token()
			return err == nil && token == json.Delim(']')
		default:
			_, delim := token.(json.Delim)
			return !delim
		}
	}
	if !value(0) {
		return false
	}
	_, err := decoder.Token()
	return errors.Is(err, io.EOF)
}
