package session

import (
	"context"
	"crypto/sha256"
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

type failingAttributionWriter struct{ err error }

func (w failingAttributionWriter) RecordMessageRelayed(context.Context, ledger.MessageRelayed) error {
	return w.err
}

func TestAttributedInputDeliversOnceAndStoresNoContent(t *testing.T) {
	root := t.TempDir()
	store, err := ledger.Open(context.Background(), ledger.Options{
		Path: filepath.Join(root, "ledger", "lanes.sqlite3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		Attributions: store.Attributions(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)

	source, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "PM Claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "Reviewer",
	})
	if err != nil {
		t.Fatal(err)
	}
	const text = "Review café 🚀\nwithout leaking this body"
	if err := manager.InputAttributed(context.Background(), target.ID, text, state.InputAttribution{
		SourceSessionID: source.ID, Client: "sessions-cli",
	}); err != nil {
		t.Fatal(err)
	}
	if got := launcher.Runner(target.ID).Inputs(); len(got) != 1 || got[0] != text {
		t.Fatalf("runner inputs = %#v", got)
	}
	relays, err := manager.MessageRelays(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(relays) != 1 {
		t.Fatalf("relays = %#v", relays)
	}
	exact := sha256.Sum256([]byte(text))
	if relays[0].Author.Name != "PM Claude" || relays[0].Author.ID != source.ID ||
		relays[0].ContentSHA256 != fmt.Sprintf("%x", exact[:]) ||
		relays[0].ContentBytes != len([]byte(text)) {
		t.Fatalf("relay = %#v", relays[0])
	}
	events, err := store.Events(context.Background(), target.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), text) ||
			strings.Contains(string(event.Payload), "without leaking") {
			t.Fatalf("ledger leaked message content: %s", event.Payload)
		}
	}

	before := len(launcher.Runner(target.ID).Inputs())
	for _, sourceID := range []string{
		"not-a-uuid",
		"00000000-0000-4000-8000-000000000099",
		target.ID,
	} {
		err := manager.InputAttributed(context.Background(), target.ID, "must not land", state.InputAttribution{
			SourceSessionID: sourceID, Client: "sessions-cli",
		})
		if err == nil {
			t.Fatalf("source %q unexpectedly succeeded", sourceID)
		}
	}
	if got := len(launcher.Runner(target.ID).Inputs()); got != before {
		t.Fatalf("invalid source delivered input: before=%d after=%d", before, got)
	}
	err = manager.InputAttributed(context.Background(), target.ID, " \n\t", state.InputAttribution{
		SourceSessionID: source.ID, Client: "sessions-cli",
	})
	var invalidInput *InvalidAttributedInputError
	if !errors.As(err, &invalidInput) || len(launcher.Runner(target.ID).Inputs()) != before {
		t.Fatalf("whitespace input error=%v inputs=%#v", err, launcher.Runner(target.ID).Inputs())
	}

	failedTarget, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "Ended target",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Runner(failedTarget.ID).Kill(context.Background()); err != nil {
		t.Fatal(err)
	}
	err = manager.InputAttributed(context.Background(), failedTarget.ID, "must not land", state.InputAttribution{
		SourceSessionID: source.ID, Client: "sessions-cli",
	})
	var unavailable *MessageInputUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("ended target error = %v", err)
	}
	failedRelays, readErr := manager.MessageRelays(context.Background(), failedTarget.ID)
	if readErr != nil || len(failedRelays) != 0 {
		t.Fatalf("failed input relays = %#v, err=%v", failedRelays, readErr)
	}

	manager.attributions = failingAttributionWriter{err: errors.New("disk full")}
	commitTarget, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "Commit target",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.InputAttributed(context.Background(), commitTarget.ID, "lands once", state.InputAttribution{
		SourceSessionID: source.ID, Client: "sessions-cli",
	})
	var commitErr *MessageAttributionCommitError
	if !errors.As(err, &commitErr) || !strings.Contains(err.Error(), "do not retry") {
		t.Fatalf("commit failure = %v", err)
	}
	if got := launcher.Runner(commitTarget.ID).Inputs(); len(got) != 1 || got[0] != "lands once" {
		t.Fatalf("commit failure delivery = %#v", got)
	}
}
