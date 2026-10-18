package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemoteFacilitatorURLAndTimeout(t *testing.T) {
	t.Parallel()
	for _, address := range []string{"", "http://example.invalid", "http://localhost:8081", "https://user:secret@example.invalid", "https://example.invalid?token=x", "https://example.invalid#x", "file:///tmp/facilitator"} {
		if _, err := remoteFacilitator(address, "", true); err == nil {
			t.Fatal("不安全地址未拒绝")
		}
	}
	if _, err := remoteFacilitator("http://127.0.0.1:8081", "", false); err == nil {
		t.Fatal("默认允许明文")
	}
	for _, address := range []string{"https://example.invalid", "http://127.0.0.1:8081/", "http://[::1]:8081"} {
		client, err := remoteFacilitator(address, strings.Repeat("t", 32), true)
		if err != nil || client.HTTPClient().Timeout != 200*time.Second || client.GetAuthProvider() == nil || !errors.Is(client.HTTPClient().CheckRedirect(nil, nil), http.ErrUseLastResponse) {
			t.Fatal("可信本机/HTTPS 配置、鉴权或超时错误")
		}
	}
	for _, token := range []string{"short", strings.Repeat("t", 257), strings.Repeat("t", 32) + "\r\n"} {
		if _, err := remoteFacilitator("https://example.invalid", token, false); err == nil {
			t.Fatal("令牌注入未拒绝")
		}
	}
}

func TestRemoteFacilitatorOfficialAuthHeaders(t *testing.T) {
	t.Parallel()
	token := strings.Repeat("a", 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("凭据未通过官方 AuthProvider 传递")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kinds":[],"extensions":[],"signers":{}}`))
	}))
	defer server.Close()
	client, err := remoteFacilitator(server.URL, token, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetSupported(context.Background()); err != nil {
		t.Fatal(err)
	}
	headers, err := client.GetAuthProvider().GetAuthHeaders(context.Background())
	if err != nil || headers.Verify["Authorization"] != "Bearer "+token || headers.Settle["Authorization"] != "Bearer "+token {
		t.Fatal("部分端点缺少鉴权")
	}
	headers.Verify["Authorization"] = "changed"
	if headers.Settle["Authorization"] != "Bearer "+token {
		t.Fatal("端点凭据共享可变 map")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.GetAuthProvider().GetAuthHeaders(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("鉴权未遵守取消")
	}
}
