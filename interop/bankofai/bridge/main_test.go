package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/eightyun/x402-tron-go/exact"
	"github.com/x402-foundation/x402/go/v2/types"
)

func testRequest(t *testing.T) request {
	t.Helper()
	data, err := os.ReadFile("../../../exact/testdata/tronweb-permit2.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Config       exact.Config              `json:"config"`
		Requirements types.PaymentRequirements `json:"requirements"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	fixture.Requirements.Extra = map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"}
	return request{Operation: "create", Config: fixture.Config, Requirements: fixture.Requirements, MaxAmount: fixture.Requirements.Amount}
}

// encodeRequest 与 JS 调用方一致：尚无授权时不发送 payload 字段，避免零值 accepted 被上游网络校验提前拒绝。
func encodeRequest(t *testing.T, input request) []byte {
	t.Helper()
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if input.Payload.X402Version != 0 {
		return data
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "payload")
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestBridgeCreatesAndVerifiesPublicSDKPayload(t *testing.T) {
	input := testRequest(t)
	data := encodeRequest(t, input)
	var output bytes.Buffer
	if err := run(context.Background(), bytes.NewReader(data), &output); err != nil {
		t.Fatal(err)
	}
	var result response
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.Valid || result.Payload == nil || result.Payer == "" {
		t.Fatal("未生成有效临时授权")
	}
	input.Operation, input.Payload = "verify", *result.Payload
	data, _ = json.Marshal(input)
	output.Reset()
	if err := run(context.Background(), bytes.NewReader(data), &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || !result.Valid {
		t.Fatal("未通过公开SDK验证")
	}
	input.Payload.Payload["signature"] = "0x" + strings.Repeat("0", 130)
	data, _ = json.Marshal(input)
	output.Reset()
	if err := run(context.Background(), bytes.NewReader(data), &output); err != nil {
		t.Fatal(err)
	}
	var rejected response
	if err := json.Unmarshal(output.Bytes(), &rejected); err != nil || rejected.Valid || rejected.Payload != nil || rejected.Payer != "" {
		t.Fatal("无效授权未被拒绝")
	}
}

func TestBridgeRejectsInvalidInputAndPolicy(t *testing.T) {
	for _, data := range []string{"", "{}", `{"unknown":1}`, `{} {}`, strings.Repeat(" ", 65537)} {
		if err := run(context.Background(), strings.NewReader(data), io.Discard); !errors.Is(err, errBridge) {
			t.Fatal("无效请求未拒绝")
		}
	}
	for _, mutation := range []func(*request){
		func(input *request) { input.Config.Network = "tron:728126428" },
		func(input *request) { input.Operation = "broadcast" },
		func(input *request) { input.MaxAmount = "1" },
		func(input *request) { input.Config.ProxyAddress = input.Config.Permit2Address },
	} {
		input := testRequest(t)
		mutation(&input)
		data := encodeRequest(t, input)
		if err := run(context.Background(), bytes.NewReader(data), io.Discard); !errors.Is(err, errBridge) {
			t.Fatal("越权/不支持请求未拒绝")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, strings.NewReader("{}"), io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal("未遵守取消")
	}
}
