package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestClientExample(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var output bytes.Buffer
	if err := run(ctx, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "202 已验签、未结算") || !strings.Contains(output.String(), "无资金转移") {
		t.Fatal("示例未清楚标记未结算")
	}
}
