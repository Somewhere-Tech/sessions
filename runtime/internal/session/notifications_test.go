package session

import (
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/providerfault"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestSessionNotificationCooldownKeepsLatestPendingPayload(t *testing.T) {
	root := t.TempDir()
	notifications := make(chan PushPayload, 3)
	manager := NewManager(testConfig(root), prototest.NewLauncher(), ManagerOptions{
		DisableWatchers:  true,
		ActivityInterval: time.Hour,
		NotifyCooldown:   40 * time.Millisecond,
		Notify:           func(payload PushPayload) { notifications <- payload },
	})
	t.Cleanup(manager.Close)

	manager.queueSessionNotification("same-session", state.NotifyDone, PushPayload{Body: "first"})
	manager.queueSessionNotification("same-session", state.NotifyWaiting, PushPayload{Body: "second"})
	manager.queueSessionNotification("same-session", state.NotifyLost, PushPayload{Body: "latest"})
	if first := <-notifications; first.Body != "first" {
		t.Fatalf("first notification = %#v", first)
	}
	select {
	case latest := <-notifications:
		if latest.Body != "latest" {
			t.Fatalf("coalesced notification = %#v", latest)
		}
	case <-time.After(time.Second):
		t.Fatal("latest pending notification was not delivered at the next cooldown window")
	}
	select {
	case unexpected := <-notifications:
		t.Fatalf("intermediate notification escaped coalescing: %#v", unexpected)
	case <-time.After(30 * time.Millisecond):
	}
}

func TestStructuredTurnCompletionRecognizesBothProviders(t *testing.T) {
	tests := []struct {
		name string
		kind string
		raw  string
	}{
		{name: "codex", kind: state.KindCodexAppServer, raw: `{"type":"codex","subtype":"turn_completed","source":"codex-app-server"}`},
		{name: "claude", kind: state.KindClaudeStructured, raw: `{"type":"result","subtype":"success","source":"claude-p-stream-json"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !structuredTurnCompleted(test.kind, []byte(test.raw)) {
				t.Fatalf("completion was not recognized: kind=%q raw=%s", test.kind, test.raw)
			}
		})
	}
	if structuredTurnCompleted(state.KindCodexAppServer, []byte(`{"type":"assistant","source":"codex-app-server"}`)) {
		t.Fatal("assistant content was mistaken for a turn completion")
	}
}

func TestProviderFaultNotificationIsOncePerEpisode(t *testing.T) {
	root := t.TempDir()
	notifications := make(chan PushPayload, 4)
	manager := NewManager(testConfig(root), prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour, NotifyCooldown: time.Millisecond,
		Notify: func(payload PushPayload) { notifications <- payload },
	})
	t.Cleanup(manager.Close)
	created, err := manager.Create(t.Context(), state.CreateSessionRequest{Cmd: "codex", Cwd: root, Name: "review"})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := manager.Get(created.ID)
	fault := providerfault.Fault{Kind: providerfault.KindUnavailable, Detail: "Codex API unavailable (503, overloaded)", Status: 503}
	current.SetProviderFault("codex", fault, time.Now().UnixMilli())
	manager.mu.Lock()
	runtime := manager.runtimes[created.ID]
	manager.mu.Unlock()
	runtime.notifyDone()
	runtime.notifyDone()
	first := <-notifications
	if first.Title != "🟠 review — Codex is unavailable" || first.Body != fault.Detail {
		t.Fatalf("fault notification = %#v", first)
	}
	select {
	case duplicate := <-notifications:
		t.Fatalf("same fault episode notified twice: %#v", duplicate)
	case <-time.After(20 * time.Millisecond):
	}
	current.ClearProviderFault()
	runtime.notifyDone()
	select {
	case <-notifications:
	case <-time.After(time.Second):
		t.Fatal("successful turn did not reset the notification episode")
	}
	current.SetProviderFault("codex", fault, time.Now().UnixMilli())
	runtime.notifyDone()
	select {
	case next := <-notifications:
		if next.Title != first.Title {
			t.Fatalf("new episode notification = %#v", next)
		}
	case <-time.After(time.Second):
		t.Fatal("new fault episode was not notified")
	}
}

func TestProviderRetryNotifiesOnlyAfterExhaustion(t *testing.T) {
	root := t.TempDir()
	notifications := make(chan PushPayload, 2)
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour, NotifyCooldown: time.Millisecond,
		Notify: func(payload PushPayload) { notifications <- payload },
	})
	t.Cleanup(manager.Close)
	created, err := manager.Create(t.Context(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: root, Name: "review", Kind: state.KindCodexAppServer,
	})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := manager.Get(created.ID)
	current.SetProviderFault("codex", providerfault.Fault{
		Kind: providerfault.KindUnavailable, Detail: "Codex API unavailable (503, overloaded)", Status: 503,
	}, time.Now().UnixMilli())
	runner := launcher.Runner(created.ID)
	runner.SetRetry(&proto.ProviderRetry{Attempt: 1, Max: 5, NextAt: time.Now().Add(30 * time.Second).UnixMilli(), Kind: providerfault.KindUnavailable})
	awaitCondition(t, func() bool { return current.Info().Retry != nil })
	manager.mu.Lock()
	runtime := manager.runtimes[created.ID]
	manager.mu.Unlock()
	runtime.notifyDone()
	select {
	case payload := <-notifications:
		t.Fatalf("scheduled retry notified early: %#v", payload)
	case <-time.After(20 * time.Millisecond):
	}
	runner.AddCodexEvent(map[string]any{
		"type": "system", "subtype": "provider_retry", "attempt": 5, "max": 5,
	})
	runner.SetRetry(nil)
	awaitCondition(t, func() bool { return current.Info().Retry == nil })
	runtime.notifyDone()
	select {
	case payload := <-notifications:
		if payload.Title != "🔴 review — Codex stayed unavailable for 13 minutes" {
			t.Fatalf("exhaustion notification = %#v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("retry exhaustion did not notify")
	}
}
