package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubmitReceiptRecoveryNeverAcknowledgesAConflictingPayload(t *testing.T) {
	for _, test := range []struct {
		name         string
		status       int
		body         string
		wantRecovery bool
	}{
		{"broken-success-body", http.StatusOK, "{", true},
		{"server-lost-response", http.StatusInternalServerError, `{"error":"receipt write interrupted"}`, true},
		{"conflicting-message", http.StatusConflict, `{"error":"operation reused for different content"}`, false},
		{"broken-conflict-body", http.StatusConflict, "{", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			lookups, sends := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					sends++
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, test.body)
					return
				}
				lookups++
				_, _ = io.WriteString(w, `{"operation_id":"fixture-operation","status":"accepted","delivered":true,"acceptance":"transcript"}`)
			}))
			defer server.Close()
			t.Setenv("HOME", t.TempDir())
			app, err := newApp([]string{"--host", server.URL}, strings.NewReader(""), io.Discard, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer app.close()
			receipt, err := app.submitComposer("/api/sessions/fixture/input", "new message", "", "fixture-operation", time.Second)
			if test.wantRecovery {
				if err != nil || lookups != 1 || receipt.Acceptance != "transcript" {
					t.Fatalf("receipt=%+v lookups=%d err=%v", receipt, lookups, err)
				}
			} else if err == nil || lookups != 0 || receipt.Delivered {
				t.Fatalf("conflicting payload recovered unrelated receipt: %+v lookups=%d err=%v", receipt, lookups, err)
			}
			if sends != 1 {
				t.Fatalf("sends=%d", sends)
			}
		})
	}
}

func TestLegacyDuplicateReceiptDoesNotBecomeProviderConfirmation(t *testing.T) {
	result, final := acknowledgedSendResult(deliveryReceipt{Status: "accepted", Delivered: true, Duplicate: true}, "claude-code")
	if !final || result.Confirmed == nil || *result.Confirmed || result.Confidence != "unknown" {
		t.Fatalf("old duplicate=%+v final=%v", result, final)
	}
	for _, boundary := range []string{"runner", "provider", "transcript"} {
		result, final = acknowledgedSendResult(deliveryReceipt{Status: "accepted", Delivered: true, Duplicate: true, Acceptance: boundary}, "claude-code")
		if !final || result.Confirmed == nil || !*result.Confirmed {
			t.Fatalf("typed/transcript receipt lost evidence: %+v", result)
		}
	}
}

func TestLegacySendNeverConfirmsSuffixOrUnrelatedUserEvent(t *testing.T) {
	intended := "BEGIN\n" + strings.Repeat("é🙂fixture ", 600) + "\nEND"
	for _, echo := range []string{intended[len(intended)-310:], "unrelated new message", ""} {
		t.Run(string([]rune(echo)[:min(8, len([]rune(echo)))]), func(t *testing.T) {
			const id = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			submitted, inputs := false, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/sessions":
					at := 1
					if submitted {
						at = 2
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"sessions": []any{map[string]any{
						"id": id, "tool": "claude-code", "cmd": "claude", "working": true, "lastUserMessageAt": at,
					}}})
				case strings.HasSuffix(r.URL.Path, "/events"):
					events := []any{}
					if submitted && echo != "" {
						events = append(events, map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": echo}})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"events": events, "nextIndex": len(events)})
				case strings.HasSuffix(r.URL.Path, "/submit"):
					submitted = true
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
				case strings.HasSuffix(r.URL.Path, "/snapshot"):
					_, _ = io.WriteString(w, "❯ \n· Working")
				case strings.HasSuffix(r.URL.Path, "/input"):
					inputs++
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			t.Setenv("HOME", t.TempDir())
			app, err := newApp([]string{"--host", server.URL}, strings.NewReader(""), io.Discard, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			defer app.close()
			clock := time.Unix(1, 0)
			app.now = func() time.Time { return clock }
			app.sleep = func(d time.Duration) { clock = clock.Add(d) }
			result, err := app.sendAndConfirm(id, intended, 600*time.Millisecond, false)
			if err != nil {
				t.Fatal(err)
			}
			if result.Confirmed == nil || *result.Confirmed || result.ExitCode == 0 {
				t.Fatalf("partial/unrelated event confirmed complete message: %+v", result)
			}
			if inputs != 0 {
				t.Fatalf("ambiguous send injected %d extra Enter keys", inputs)
			}
		})
	}
}
