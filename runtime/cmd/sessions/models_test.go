package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
)

func TestModelsCommandPrintsHumanAndJSONCatalog(t *testing.T) {
	catalog := []codexapp.Model{{
		ID: "alpha", DisplayName: "Alpha", IsDefault: true, DefaultReasoningEffort: "medium",
		SupportedReasoningEfforts: []codexapp.ReasoningEffortOption{{ReasoningEffort: "low"}, {ReasoningEffort: "high"}},
		ServiceTiers:              []codexapp.ModelServiceTier{{ID: "priority", Name: "Fast"}},
	}}
	for _, test := range []struct {
		name     string
		json     bool
		validate func(*testing.T, string)
	}{
		{
			name: "human",
			validate: func(t *testing.T, output string) {
				for _, want := range []string{"MODEL", "DEFAULT EFFORT", "alpha", "Alpha", "yes", "medium", "low,high", "priority"} {
					if !strings.Contains(output, want) {
						t.Fatalf("human output %q missing %q", output, want)
					}
				}
			},
		},
		{
			name: "json", json: true,
			validate: func(t *testing.T, output string) {
				var decoded []codexapp.Model
				if err := json.Unmarshal([]byte(output), &decoded); err != nil {
					t.Fatal(err)
				}
				if len(decoded) != 1 || decoded[0].ID != "alpha" || len(decoded[0].ServiceTiers) != 1 {
					t.Fatalf("JSON catalog = %#v", decoded)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			application := &app{
				stdout: &output, wantJSON: test.json,
				listModels: func(context.Context) ([]codexapp.Model, error) { return catalog, nil },
			}
			if err := application.cmdModels(nil); err != nil {
				t.Fatal(err)
			}
			test.validate(t, output.String())
		})
	}
}

func TestModelCommandUsesDurableNextTurnControl(t *testing.T) {
	const id = "25000000-0000-4000-8000-000000000001"
	t.Setenv("HOME", t.TempDir())
	var received struct {
		Model  string  `json:"model"`
		Effort *string `json:"effort"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/sessions":
			_, _ = response.Write([]byte(`{"sessions":[{"id":"` + id + `","description":"","cmd":"codex","args":[],"cwd":"/tmp","createdAt":1,"pid":12,"tool":"codex","working":false,"lastDataAt":1,"lastUserMessageAt":null,"exited":false}]}`))
		case request.Method == http.MethodPut && request.URL.Path == "/api/sessions/"+id+"/model":
			if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
				t.Errorf("decode model body: %v", err)
			}
			_, _ = response.Write([]byte(`{"id":"` + id + `","description":"","cmd":"codex","args":[],"cwd":"/tmp","createdAt":1,"pid":12,"tool":"codex","model":"gpt-next","effort":"high","working":false,"lastDataAt":1,"lastUserMessageAt":null,"exited":false}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "model", id[:8], "gpt-next", "--effort", "high")
	if code != 0 || stderr != "" || stdout != "Next turn: gpt-next · high\n" {
		t.Fatalf("model exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if received.Model != "gpt-next" || received.Effort == nil || *received.Effort != "high" {
		t.Fatalf("model body = %#v", received)
	}
}
