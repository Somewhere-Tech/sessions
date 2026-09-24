package codexapp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// A usage read peeks at the identity without a token refresh and then reads
// every metered bucket, keeping nullable fields absent rather than zero.
func TestRateLimitReadKeepsBucketsAndNullableFields(t *testing.T) {
	transport := &captureTransport{writes: make(chan []byte, 4)}
	client := &Client{transport: transport, pending: make(map[string]chan callResponse)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	accounts := make(chan *Account, 1)
	go func() {
		account, err := client.PeekAccount(ctx)
		if err != nil {
			t.Error(err)
		}
		accounts <- account
	}()
	request := nextWrite(t, transport)
	if request["method"] != "account/read" || request["params"].(map[string]any)["refreshToken"] != false {
		t.Fatalf("usage identity read asked for a token refresh: %v", request)
	}
	client.handleResponse(wireMessage{ID: json.RawMessage(`1`), Result: json.RawMessage(`{"account":{"type":"chatgpt","email":"a@example.test","planType":"team","chatgptAccountId":"ws-1"}}`)})
	if account := <-accounts; account == nil || account.ChatgptAccountID != "ws-1" {
		t.Fatalf("account = %#v, want the reported account id", account)
	}

	limits := make(chan RateLimits, 1)
	go func() {
		result, err := client.ReadRateLimits(ctx)
		if err != nil {
			t.Error(err)
		}
		limits <- result
	}()
	if request = nextWrite(t, transport); request["method"] != "account/rateLimits/read" {
		t.Fatalf("request = %v", request)
	}
	client.handleResponse(wireMessage{ID: json.RawMessage(`2`), Result: json.RawMessage(`{
		"rateLimits":{"limitId":"codex","primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1790000000},"secondary":null},
		"rateLimitsByLimitId":{
			"codex":{"limitId":"codex","primary":{"usedPercent":12,"windowDurationMins":300,"resetsAt":1790000000},"secondary":{"usedPercent":40,"windowDurationMins":10080,"resetsAt":null}},
			"codex_other":{"limitId":"codex_other","limitName":"Other","primary":{"usedPercent":3}}
		}}`)})
	result := <-limits
	if len(result.ByLimitID) != 2 || result.Legacy == nil {
		t.Fatalf("result = %#v, want both buckets and the legacy view", result)
	}
	other := result.ByLimitID["codex_other"]
	if other.Primary == nil || other.Primary.UsedPercent != 3 || other.Primary.ResetsAt != nil || other.Primary.WindowDurationMins != nil || other.Secondary != nil {
		t.Fatalf("other bucket = %#v, want absent fields left absent", other)
	}
	if secondary := result.ByLimitID["codex"].Secondary; secondary == nil || secondary.ResetsAt != nil || *secondary.WindowDurationMins != 10080 {
		t.Fatalf("secondary = %#v", secondary)
	}
}

func TestMethodUnsupportedRecognisesAnOlderAppServer(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{rpcError{Code: -32601, Message: "Method not found"}, true},
		{rpcError{Code: -32600, Message: "Invalid request: unknown variant `account/rateLimits/read`"}, true},
		{rpcError{Code: -32603, Message: "codex account authentication required"}, false},
		{errors.New("unknown variant"), false},
		{context.DeadlineExceeded, false},
	}
	for _, c := range cases {
		if got := MethodUnsupported(c.err); got != c.want {
			t.Errorf("MethodUnsupported(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}
