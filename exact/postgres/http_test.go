package postgres

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/x402-foundation/x402/go/v2/types"
)

func TestUniqueHTTPJSON(t *testing.T) {
	for _, test := range []struct {
		input string
		valid bool
	}{
		{`{"a":{"b":[{"c":1},null,"value"]}}`, true},
		{`{"a":{},"b":[]}`, true},
		{`{"a":1,"a":2}`, false},
		{`{"a":{"amount":"1","Amount":"2"}}`, false},
		{`{"a":1,"a":2}`, false},
		{`{"a":[{"b":1,"b":2}]}`, false},
		{`{"a":1} null`, false},
		{`{"a":]}`, false},
		{`{"a"`, false},
		{`[1`, false},
		{`[1,]`, false},
		{``, false},
	} {
		if got := uniqueHTTPJSON([]byte(test.input)); got != test.valid {
			t.Fatalf("JSON 歧义检测不符: %q", test.input)
		}
	}
}

// FuzzStrictPaymentDecoding 检查外部请求解码：任意输入不 panic；歧义检测不宽于标准 JSON；
// 任何被接受的载荷/报价，在顶层或 accepted 内注入未知字段后都必须被拒绝。
func FuzzStrictPaymentDecoding(f *testing.F) {
	const quote = `{"scheme":"exact","network":"tron:3448148188","asset":"TXYZopYRdj2D9XRtbG411XZZ3kM5VkAeBf","amount":"1000","payTo":"TNPeeaaFB7K9cmo4uQpcU32zGK8G1NYqeL","maxTimeoutSeconds":60,"extra":{"assetTransferMethod":"permit2","paymentFlow":"upfront"}}`
	f.Add([]byte(`{"x402Version":2,"payload":{"signature":"0x00"},"accepted":`+quote+`}`), []byte(quote))
	f.Add([]byte(`{"x402Version":2,"payload":{},"accepted":{"network":"tron:0xcd8690dc"},"resource":{"url":"https://example.invalid"}}`), []byte(`{"network":"eip155:1"}`))
	f.Add([]byte(`{"x402Version":2,"accepted":null}`), []byte(`null`))
	f.Add([]byte(`{"X402VERSION":2,"ACCEPTED":{"NETWORK":"tron:3448148188"}}`), []byte(`{"Network":"tron:3448148188"}`))
	f.Add([]byte(`{"x402Version":2,"accepted":{"network":"tron:3448148188","other":1}}`), []byte(`{"network":"tron:3448148188","other":1}`))
	f.Fuzz(func(t *testing.T, payload, requirements []byte) {
		for _, data := range [][]byte{payload, requirements} {
			if uniqueHTTPJSON(data) && !json.Valid(data) {
				t.Fatalf("歧义检测接受了非法 JSON: %q", data)
			}
		}
		// 两个真实入口都先做歧义检测；大小写重复键会让注入位置不确定，这里同样前置。
		var payment *types.PaymentPayload
		if uniqueHTTPJSON(payload) && strictPaymentPayload(payload, &payment) && payment != nil {
			for _, injected := range injectedUnknown(payload, true) {
				var again *types.PaymentPayload
				if strictPaymentPayload(injected, &again) {
					t.Fatalf("载荷未知字段被接受: %q", injected)
				}
			}
		}
		var accepted *types.PaymentRequirements
		if uniqueHTTPJSON(requirements) && strictPaymentRequirements(requirements, &accepted) && accepted != nil {
			for _, injected := range injectedUnknown(requirements, false) {
				var again *types.PaymentRequirements
				if strictPaymentRequirements(injected, &again) {
					t.Fatalf("报价未知字段被接受: %q", injected)
				}
			}
		}
	})
}

// injectedUnknown 返回在顶层（及可选的 accepted 对象内）加入未知字段后的输入；无法解析为对象时返回空。
func injectedUnknown(data []byte, nested bool) [][]byte {
	const unknown = "x402-fuzz-unknown"
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil || top == nil {
		return nil
	}
	var variants [][]byte
	withTop := maps.Clone(top)
	withTop[unknown] = json.RawMessage(`0`)
	if encoded, err := json.Marshal(withTop); err == nil {
		variants = append(variants, encoded)
	}
	if !nested {
		return variants
	}
	for key, value := range top {
		var inner map[string]json.RawMessage
		// 上游按字段名大小写不敏感匹配 accepted，这里同样只对能映射到 accepted 的对象注入。
		if !strings.EqualFold(key, "accepted") || json.Unmarshal(value, &inner) != nil || inner == nil {
			continue
		}
		inner[unknown] = json.RawMessage(`0`)
		innerJSON, err := json.Marshal(inner)
		if err != nil {
			continue
		}
		withInner := maps.Clone(top)
		withInner[key] = innerJSON
		if encoded, err := json.Marshal(withInner); err == nil {
			variants = append(variants, encoded)
		}
	}
	return variants
}
