package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestConversationIdentityCollisionRefuseForceFreshAndAfterKill(t *testing.T) {
	root := t.TempDir()
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	ctx := context.Background()
	createResume := func(provider, name string, force bool) (state.SessionInfo, error) {
		t.Helper()
		return manager.Create(ctx, state.CreateSessionRequest{
			Cmd: "claude", Args: []string{"--resume", provider}, Cwd: root, Name: name, Force: force,
		})
	}

	refuseProvider := "11111111-1111-4111-8111-111111111111"
	primary, err := createResume(refuseProvider, "primary", false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = createResume(refuseProvider, "duplicate", false)
	var liveErr *ConversationLiveError
	if !errors.As(err, &liveErr) {
		t.Fatalf("duplicate resume error = %T %v", err, err)
	}
	want := fmt.Sprintf("conversation %s is already live as \"primary\" (session %s) — attach with `sessions attach %s`, or re-run with --force to take over.", refuseProvider, primary.ID, primary.ID)
	if err.Error() != want || len(launcher.Launches) != 1 {
		t.Fatalf("refuse err=%q launches=%d", err, len(launcher.Launches))
	}

	forceProvider := "22222222-2222-4222-8222-222222222222"
	old, err := createResume(forceProvider, "force source", false)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := createResume(forceProvider, "force target", true)
	if err != nil {
		t.Fatal(err)
	}
	oldSession, present := manager.Get(old.ID)
	if !present || oldSession.Info().Exited || !manager.Input(ctx, old.ID, "still alive") {
		t.Fatal("forced takeover killed or detached the existing session")
	}
	events, err := store.Events(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	rebound := false
	for _, event := range events {
		rebound = rebound || event.Type == ledger.EventProviderRebound
	}
	if !rebound {
		t.Fatalf("old lane events have no provider_rebound: %#v", events)
	}
	binding, err := store.LiveBindingFor(ctx, forceProvider)
	if err != nil || binding == nil || binding.SessionID != replacement.ID {
		t.Fatalf("forced live binding = %#v err=%v", binding, err)
	}

	freshProvider := "33333333-3333-4333-8333-333333333333"
	fresh, err := manager.Create(ctx, state.CreateSessionRequest{
		Cmd: "claude", Args: []string{"--session-id", freshProvider}, Cwd: root, Name: "fresh",
	})
	if err != nil || fresh.ID == "" {
		t.Fatalf("fresh session = %#v err=%v", fresh, err)
	}

	afterKillProvider := "44444444-4444-4444-8444-444444444444"
	doomed, err := createResume(afterKillProvider, "doomed", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RequestKill(ctx, doomed.ID, false); err != nil {
		t.Fatal(err)
	}
	afterKill, err := createResume(afterKillProvider, "after kill", false)
	if err != nil || afterKill.ID == "" {
		t.Fatalf("resume after tombstone = %#v err=%v", afterKill, err)
	}

	t.Logf("refuse=true force=%s rebound=true old_still_live=true fresh=%s after_kill=%s launches=%d",
		replacement.ID, fresh.ID, afterKill.ID, len(launcher.Launches))
}

func TestMovedConversationRefusesUnlessForced(t *testing.T) {
	root := t.TempDir()
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	ctx := context.Background()
	provider := "55555555-5555-4555-8555-555555555555"
	created, err := manager.Create(ctx, state.CreateSessionRequest{
		Cmd: "claude", Args: []string{"--resume", provider}, Cwd: root, Name: "moved source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrations().RecordMovedTo(ctx, ledger.MovedTo{
		Meta: ledger.Meta{LaneID: created.ID}, TargetEndpoint: "studio.example", NewLaneID: "remote-session",
	}); err != nil {
		t.Fatal(err)
	}
	request := state.CreateSessionRequest{Cmd: "claude", Args: []string{"--resume", provider}, Cwd: root}
	_, err = manager.Create(ctx, request)
	var movedErr *ConversationMovedError
	if !errors.As(err, &movedErr) || err.Error() != "conversation moved to studio.example; reopening here forks it. --force to fork." {
		t.Fatalf("moved refusal = %T %v", err, err)
	}
	request.Force = true
	forced, err := manager.Create(ctx, request)
	if err != nil || forced.ID == "" {
		t.Fatalf("forced moved fork = %#v err=%v", forced, err)
	}
	if got := strings.TrimSpace(movedErr.Machine); got != "studio.example" {
		t.Fatalf("moved machine = %q", got)
	}
	if source, present := manager.Get(created.ID); !present || source.Info().Exited {
		t.Fatal("forced moved fork killed the source session")
	}
	events, err := store.Events(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	rebound := false
	for _, event := range events {
		rebound = rebound || event.Type == ledger.EventProviderRebound
	}
	if !rebound {
		t.Fatalf("forced moved fork did not record provider_rebound: %#v", events)
	}
}
