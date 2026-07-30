package migrate

import (
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestSessionRequestCreatesProviderDefaultContinuations(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "workspace")
	cases := []struct {
		name           string
		tool           string
		id             string
		argv           []string
		kind           string
		conversationID string
	}{
		{
			name: "Claude", tool: "claude-code",
			id:   "11111111-1111-4111-8111-111111111111",
			argv: []string{"claude", "--resume", "11111111-1111-4111-8111-111111111111"},
			kind: "", conversationID: "",
		},
		{
			name: "Codex", tool: "codex",
			id:   "22222222-2222-4222-8222-222222222222",
			argv: []string{"codex", "resume", "22222222-2222-4222-8222-222222222222"},
			kind: state.KindCodexAppServer, conversationID: "22222222-2222-4222-8222-222222222222",
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request, err := SessionRequest(CreateRequest{
				Tool: test.tool, UUID: test.id, Cwd: cwd,
				ResumeRecipe: test.argv, Name: "Continue BOLO",
				SourceID: "source-lane", SourceEndpoint: "this-machine",
			})
			if err != nil {
				t.Fatal(err)
			}
			if request.Kind != test.kind || request.ConversationID != test.conversationID ||
				request.Cmd != test.argv[0] || request.Name != "Continue BOLO" {
				t.Fatalf("create request = %#v", request)
			}
		})
	}
}

func TestSessionRequestCreatesExplicitRichClaudeContinuation(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	request, err := SessionRequest(CreateRequest{
		Tool: "claude-code", UUID: id, Cwd: filepath.Join(t.TempDir(), "workspace"),
		ResumeRecipe: []string{"claude", "--resume", id}, RuntimeMode: RuntimeRich,
		SourceID: "source-lane", SourceEndpoint: "this-machine",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Kind != state.KindClaudeStructured || request.ConversationID != id {
		t.Fatalf("Rich Claude continuation = %#v", request)
	}
}

func TestSessionRequestCreatesExplicitTerminalContinuation(t *testing.T) {
	id := "11111111-1111-4111-8111-111111111111"
	request, err := SessionRequest(CreateRequest{
		Tool: "claude-code", UUID: id, Cwd: filepath.Join(t.TempDir(), "workspace"),
		ResumeRecipe: []string{"claude", "--resume", id}, RuntimeMode: RuntimeTerminal,
		SourceID: "source-lane", SourceEndpoint: "this-machine",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Kind != "" || request.ConversationID != "" || request.Args[0] != "--resume" {
		t.Fatalf("terminal continuation = %#v", request)
	}
}

func TestSessionRequestRejectsUnsafeOrUnsupportedContinuation(t *testing.T) {
	base := CreateRequest{
		Tool: "claude-code", UUID: "11111111-1111-4111-8111-111111111111",
		Cwd:          filepath.Join(t.TempDir(), "workspace"),
		ResumeRecipe: []string{"claude", "--resume", "11111111-1111-4111-8111-111111111111"},
		SourceID:     "source-lane", SourceEndpoint: "this-machine",
	}
	cases := []struct {
		name   string
		mutate func(*CreateRequest)
	}{
		{name: "non-minimal recipe", mutate: func(value *CreateRequest) {
			value.ResumeRecipe = append(value.ResumeRecipe, "--dangerously-skip-permissions")
		}},
		{name: "shell", mutate: func(value *CreateRequest) {
			value.Tool = "terminal"
			value.ResumeRecipe = []string{"bash"}
		}},
		{name: "missing source", mutate: func(value *CreateRequest) {
			value.SourceEndpoint = ""
		}},
		{name: "relative workspace", mutate: func(value *CreateRequest) {
			value.Cwd = "workspace"
		}},
		{name: "unknown runtime", mutate: func(value *CreateRequest) {
			value.RuntimeMode = "magic"
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			value := base
			value.ResumeRecipe = append([]string(nil), base.ResumeRecipe...)
			test.mutate(&value)
			if _, err := SessionRequest(value); err == nil {
				t.Fatalf("unsafe request was accepted: %#v", value)
			}
		})
	}
}
