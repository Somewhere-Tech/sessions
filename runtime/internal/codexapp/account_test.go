package codexapp

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestAccountLoginCompletionRejectsOnlyTheMatchingAttempt(t *testing.T) {
	client := &Client{}
	if client.AccountLoginFailed("current") {
		t.Fatal("pending login was treated as failed")
	}
	client.handleNotification("account/login/completed", json.RawMessage(`{"loginId":"previous","success":false,"error":"sensitive provider details"}`))
	if client.AccountLoginFailed("current") {
		t.Fatal("previous login failure affected the current attempt")
	}
	client.handleNotification("account/login/completed", json.RawMessage(`{"loginId":"current","success":false}`))
	if !client.AccountLoginFailed("current") {
		t.Fatal("provider rejection was ignored")
	}
	client.handleNotification("account/login/completed", json.RawMessage(`{"loginId":"current","success":true}`))
	if client.AccountLoginFailed("current") {
		t.Fatal("successful login was treated as failed")
	}
}

func TestAccountRPCUsesDeviceFlowAndProviderReportedIdentity(t *testing.T) {
	transport := &captureTransport{writes: make(chan []byte, 4)}
	client := &Client{transport: transport, pending: make(map[string]chan callResponse)}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		login, err := client.StartAccountLogin(ctx)
		if err == nil && (login.UserCode != "CODE" || login.VerificationURL != "https://auth.openai.com/codex/device") {
			t.Error("lost device authorization data")
		}
		done <- err
	}()
	request := nextWrite(t, transport)
	if request["method"] != "account/login/start" || request["params"].(map[string]any)["type"] != "chatgptDeviceCode" {
		t.Fatalf("request=%v", request)
	}
	client.handleResponse(wireMessage{ID: json.RawMessage(`1`), Result: json.RawMessage(`{"loginId":"login","verificationUrl":"https://auth.openai.com/codex/device","userCode":"CODE"}`)})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	go func() {
		account, err := client.ReadAccount(ctx)
		if err == nil && (account == nil || account.Email != "second@example.test") {
			t.Error("lost account identity")
		}
		done <- err
	}()
	request = nextWrite(t, transport)
	if request["method"] != "account/read" || request["params"].(map[string]any)["refreshToken"] != true {
		t.Fatalf("request=%v", request)
	}
	client.handleResponse(wireMessage{ID: json.RawMessage(`2`), Result: json.RawMessage(`{"account":{"type":"chatgpt","email":"second@example.test","planType":"pro"}}`)})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
