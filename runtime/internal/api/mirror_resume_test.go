package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/recovery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// This is the case the recovery plan already labels transcript-recovery and
// the resume route used to refuse: the provider handle is perfectly well
// known, the provider deleted its own transcript on its retention timer, and
// Sessions still holds the copy it teed while the session ran. `sessions
// recover` prints `sessions resume <id>` for exactly these, so the route it
// names has to work.
func TestResumeRestoresAConversationThePrunedProviderStillHasAHandleFor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)

	created, err := daemon.registry.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: daemon.root, Name: "pruned-but-kept",
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner := daemon.launcher.Runner(created.ID); runner != nil {
		if err := runner.Kill(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	awaitSessionExited(t, daemon.registry, created.ID)

	// Sessions' own copy of the conversation. The provider transcript that
	// would normally sit in ~/.claude/projects is deliberately absent: that is
	// what the retention timer does, and what makes a native resume refuse.
	mirrorPath := watch.TranscriptMirrorPath(daemon.config.RunnerStateDir, created.ID)
	conversation := strings.Join([]string{
		`{"type":"user","uuid":"m1","sessionId":"` + created.ID +
			`","message":{"role":"user","content":"where did the auth fix land"}}`,
		`{"type":"assistant","uuid":"m2","sessionId":"` + created.ID +
			`","message":{"role":"assistant","content":"in files.go, behind the state root change"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(mirrorPath, []byte(conversation), 0o600); err != nil {
		t.Fatal(err)
	}
	if !watch.TranscriptMirrorUsable(mirrorPath) {
		t.Fatal("the fixture mirror is not usable; the rest of this test would prove nothing")
	}

	body := strings.NewReader(`{"target":"` + created.ID + `","historyId":"` + created.ID + `"}`)
	response := serve(
		t, daemon.handler, http.MethodPost, "/api/recovery/adopt", body, "127.0.0.1:1", nil,
	)
	if response.Code != http.StatusCreated {
		t.Fatalf("resume status=%d body=%s — the command `sessions recover` prints for this case must work",
			response.Code, response.Body.String())
	}
	var result recovery.AdoptResult
	decodeBody(t, response, &result)
	if !result.OK {
		t.Fatalf("result = %+v, want a successful restore", result)
	}
	if !result.TranscriptRecovery {
		t.Fatal("the restore did not report that it came from Sessions' own transcript")
	}
	if result.ImportedMessages != 2 {
		t.Fatalf("imported %d messages, want the whole conversation", result.ImportedMessages)
	}
	if result.SourceHistoryID != created.ID {
		t.Fatalf("sourceHistoryId = %q, want %q", result.SourceHistoryID, created.ID)
	}
	_ = filepath.Base(mirrorPath)
}
