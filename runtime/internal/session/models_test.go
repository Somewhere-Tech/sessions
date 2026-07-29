package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestCodexAppServerCreateResolvesDeclaredChoiceBeforeLaunch(t *testing.T) {
	root := t.TempDir()
	launcher := prototest.NewLauncher()
	listCalls := 0
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		ActivityInterval: time.Hour,
		Notify:           func(PushPayload) {},
		ListCodexModels: func(_ context.Context, codexPath string) ([]codexapp.Model, error) {
			listCalls++
			if codexPath != "codex" {
				t.Fatalf("codex path = %q", codexPath)
			}
			return testCodexCatalog(), nil
		},
	})
	t.Cleanup(manager.Close)

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: root, Kind: state.KindCodexAppServer,
		Args: []string{"-c", `model_reasoning_effort="high"`, "-c", `service_tier="priority"`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if listCalls != 1 {
		t.Fatalf("catalog calls = %d, want 1", listCalls)
	}
	if created.Model != "wire-alpha" || created.Effort != "high" || !created.Fast {
		t.Fatalf("resolved session controls = model %q effort %q fast %v", created.Model, created.Effort, created.Fast)
	}
	if len(launcher.Launches) != 1 || !strings.Contains(strings.Join(launcher.Launches[0].Info.Args, " "), "--model wire-alpha") {
		t.Fatalf("launches = %#v", launcher.Launches)
	}
}

func TestCodexAppServerCreateRejectsInvalidChoiceWithoutLaunching(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantError string
	}{
		{
			name: "model", args: []string{"--model", "missing"},
			wantError: `model "missing" not available; valid: [alpha, beta]`,
		},
		{
			name: "effort", args: []string{"--model", "beta", "-c", `model_reasoning_effort="high"`},
			wantError: `effort "high" not supported by model "beta"; valid: [medium]`,
		},
		{
			name: "fast service tier", args: []string{"--model", "beta", "-c", `service_tier="priority"`},
			wantError: `service tier "priority" not supported by model "beta"; valid: []`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			launcher := prototest.NewLauncher()
			manager := NewManager(testConfig(root), launcher, ManagerOptions{
				ActivityInterval: time.Hour,
				Notify:           func(PushPayload) {},
				ListCodexModels: func(context.Context, string) ([]codexapp.Model, error) {
					return testCodexCatalog(), nil
				},
			})
			t.Cleanup(manager.Close)
			_, err := manager.Create(context.Background(), state.CreateSessionRequest{
				Cmd: "codex", Cwd: root, Kind: state.KindCodexAppServer, Args: test.args,
			})
			if err == nil || err.Error() != test.wantError {
				t.Fatalf("Create() error = %v, want %q", err, test.wantError)
			}
			if len(launcher.Launches) != 0 {
				t.Fatalf("invalid choice launched %d runners", len(launcher.Launches))
			}
		})
	}
}

func testCodexCatalog() []codexapp.Model {
	return []codexapp.Model{
		{
			ID: "alpha", Model: "wire-alpha", IsDefault: true,
			SupportedReasoningEfforts: []codexapp.ReasoningEffortOption{{ReasoningEffort: "low"}, {ReasoningEffort: "high"}},
			ServiceTiers:              []codexapp.ModelServiceTier{{ID: "priority"}},
		},
		{
			ID: "beta", Model: "wire-beta",
			SupportedReasoningEfforts: []codexapp.ReasoningEffortOption{{ReasoningEffort: "medium"}},
		},
	}
}
