package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A steer whose outcome Codex never confirmed is part of the conversation a
// reader must see: its exact text appears once, as the user's, labelled
// unconfirmed. A provider echo of the same text is not a second message, and a
// refusal proved before anything was written is not a message at all.
func TestTranscriptShowsAnUnconfirmedSteerOnceAndLabelled(t *testing.T) {
	const id = "cccccccc-dddd-4eee-8fff-000000000001"
	const steered = "Ändere den Plan:\n\tzweite Zeile — 日本語 ✓"
	user := func(at, subtype, text string) map[string]any {
		return map[string]any{"type": "user", "subtype": subtype, "source": "codex-app-server", "timestamp": at,
			"message": map[string]any{"role": "user", "content": text}}
	}
	events := []any{
		user("2026-10-06T12:00:00Z", "user_message", "long task"),
		map[string]any{"type": "system", "subtype": "input_rejected", "source": "codex-app-server",
			"timestamp": "2026-10-06T12:00:01Z", "unconfirmed": true, "unconfirmedInput": steered,
			"operationId": "op-1", "error": "Codex did not confirm the message sent to its active turn"},
		map[string]any{"type": "system", "subtype": "input_rejected", "source": "codex-app-server",
			"timestamp": "2026-10-06T12:00:02Z", "input": "refused before writing", "error": "Codex finished its turn"},
		map[string]any{"type": "codex", "subtype": "item_completed", "source": "codex-app-server",
			"timestamp": "2026-10-06T12:00:03Z", "item": map[string]any{"id": "steer-1", "type": "userMessage",
				"content": []any{map[string]any{"type": "text", "text": steered}}}},
		user("2026-10-06T12:00:04Z", "user_steer", "accepted steer"),
		map[string]any{"type": "assistant", "source": "codex-app-server", "timestamp": "2026-10-06T12:00:05Z",
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "done"}}}},
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/api/sessions":
			_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
				"id": id, "cmd": "codex", "args": []any{}, "cwd": "/tmp", "createdAt": 1, "tool": "codex",
				"kind": "codex-app-server",
			}}})
		case "/api/sessions/" + id + "/events":
			_ = json.NewEncoder(response).Encode(map[string]any{"events": events, "nextIndex": len(events)})
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	runCLI := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if code := run(append([]string{"--host", server.URL}, args...), strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("%v exit=%d stderr=%q", args, code, stderr.String())
		}
		return stdout.String()
	}
	unconfirmed := `{"role":"user","text":` + jsonString(t, steered) + `,"timestamp":"2026-10-06T12:00:01Z","delivery":"unconfirmed","operation_id":"op-1"}`
	assertJSONEqual(t, runCLI("--json", "transcript", id[:8]), `[`+
		`{"role":"user","text":"long task","timestamp":"2026-10-06T12:00:00Z"},`+unconfirmed+`,`+
		`{"role":"user","text":"accepted steer","timestamp":"2026-10-06T12:00:04Z"},`+
		`{"role":"assistant","text":"done","timestamp":"2026-10-06T12:00:05Z"}]`)
	assertJSONEqual(t, runCLI("--json", "last", id[:8], "--role", "user", "-n", "5"), `[`+
		`{"role":"user","text":"long task","timestamp":"2026-10-06T12:00:00Z"},`+unconfirmed+`,`+
		`{"role":"user","text":"accepted steer","timestamp":"2026-10-06T12:00:04Z"}]`)
	text := runCLI("transcript", id[:8])
	if strings.Count(text, steered) != 1 || !strings.Contains(text, "[user · delivery unconfirmed]\n"+steered+"\n") ||
		strings.Contains(text, "refused before writing") {
		t.Fatalf("transcript text = %q", text)
	}
}

func jsonString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// The runner appends an unconfirmed steer's record once Codex's answer fails
// to come, possibly after the turn's later output and completion, stamped with
// its submission time. The CLI lists it where it was sent. A completed agent message is listed whole at
// its completion, so one that spans the steer is listed after it: the CLI does
// not invent where inside that text the steer landed.
func TestTranscriptListsALateSteerRecordAtItsSubmission(t *testing.T) {
	const id = "cccccccc-dddd-4eee-8fff-000000000002"
	const steered = "Ändere den Plan:\n\tzweite Zeile — 日本語 ✓"
	at := func(second int) string { return "2026-10-06T12:00:0" + string(rune('0'+second)) + "Z" }
	agent := func(second int, text string) map[string]any {
		return map[string]any{"type": "assistant", "subtype": "item_completed", "source": "codex-app-server", "timestamp": at(second),
			"message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	user := map[string]any{"type": "user", "subtype": "user_message", "source": "codex-app-server", "timestamp": at(0),
		"message": map[string]any{"role": "user", "content": "long task"}}
	done := map[string]any{"type": "codex", "subtype": "turn_completed", "source": "codex-app-server", "timestamp": at(5)}
	unknown := map[string]any{"type": "system", "subtype": "input_rejected", "source": "codex-app-server", "timestamp": at(2),
		"unconfirmed": true, "unconfirmedInput": steered, "operationId": "op-late", "error": "no answer"}
	for _, test := range []struct {
		name   string
		events []any
		want   []string
	}{
		{"late unknown", []any{user, agent(1, "Before."), agent(3, "After."), done, unknown}, []string{"long task", "Before.", steered, "After."}},
		{"spanning item", []any{user, agent(4, "Before.After."), done, unknown}, []string{"long task", steered, "Before.After."}},
	} {
		events := test.events
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/sessions":
				_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
					"id": id, "cmd": "codex", "args": []any{}, "cwd": "/tmp", "createdAt": 1, "tool": "codex", "kind": "codex-app-server",
				}}})
			case "/api/sessions/" + id + "/events":
				_ = json.NewEncoder(response).Encode(map[string]any{"events": events, "nextIndex": len(events)})
			default:
				http.NotFound(response, request)
			}
		}))
		t.Setenv("HOME", t.TempDir())
		for _, command := range [][]string{{"transcript", id[:8]}, {"last", id[:8], "-n", "5"}} {
			var stdout, stderr bytes.Buffer
			if code := run(append([]string{"--host", server.URL, "--json"}, command...), strings.NewReader(""), &stdout, &stderr); code != 0 {
				t.Fatalf("%s %v exit=%d stderr=%q", test.name, command, code, stderr.String())
			}
			var turns []messageTurn
			if err := json.Unmarshal(stdout.Bytes(), &turns); err != nil {
				t.Fatal(err)
			}
			got := make([]string, 0, len(turns))
			for _, turn := range turns {
				got = append(got, turn.Text)
			}
			// `last -n 5` holds every message here, so it reads in the same order.
			if strings.Join(got, "|") != strings.Join(test.want, "|") {
				t.Fatalf("%s %v = %q, want %q", test.name, command, got, test.want)
			}
		}
		server.Close()
	}
}

// An accepted steer is recorded when Codex answers, stamped with when it was
// sent. The CLI lists a uniquely identified one in its own turn where it was
// sent, whether the answer came before or after the turn completed. What the
// record cannot establish keeps its recorded position: identical repeated
// records, no turn of its own, or an unreadable time.
func TestTranscriptListsALateAcceptedSteerAtItsSubmission(t *testing.T) {
	const id = "cccccccc-dddd-4eee-8fff-000000000003"
	const steered = "Ändere den Plan:\n\tzweite Zeile — 日本語 ✓"
	at := func(second int) string { return "2026-10-06T12:00:0" + string(rune('0'+second)) + "Z" }
	agent := func(second int, text string) map[string]any {
		return map[string]any{"type": "assistant", "subtype": "item_completed", "source": "codex-app-server", "turnId": "turn-1",
			"timestamp": at(second), "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}}
	}
	user := map[string]any{"type": "user", "subtype": "user_message", "source": "codex-app-server", "timestamp": at(0),
		"message": map[string]any{"role": "user", "content": "long task"}}
	done := map[string]any{"type": "codex", "subtype": "turn_completed", "source": "codex-app-server", "turnId": "turn-1", "timestamp": at(5)}
	steer := func(fields map[string]any) map[string]any {
		value := map[string]any{"type": "user", "subtype": "user_steer", "source": "codex-app-server", "turnId": "turn-1",
			"timestamp": at(2), "message": map[string]any{"role": "user", "content": steered}}
		for key, field := range fields {
			value[key] = field
		}
		return value
	}
	placed := []string{"long task", "Before.", steered, "After."}
	recorded := []string{"long task", "Before.", "After.", steered}
	for _, test := range []struct {
		name   string
		events []any
		want   []string
	}{
		{"after completion", []any{user, agent(1, "Before."), agent(3, "After."), done, steer(nil)}, placed},
		{"before completion", []any{user, agent(1, "Before."), agent(3, "After."), steer(nil), done}, placed},
		{"spanning item", []any{user, agent(4, "Before.After."), done, steer(nil)}, []string{"long task", steered, "Before.After."}},
		{"identical repeats", []any{user, agent(1, "Before."), agent(3, "After."), done, steer(nil), steer(nil)}, append(recorded, steered)},
		{"no turn", []any{user, agent(1, "Before."), agent(3, "After."), done, steer(map[string]any{"turnId": nil})}, recorded},
		{"unreadable time", []any{user, agent(1, "Before."), agent(3, "After."), done, steer(map[string]any{"timestamp": "later"})}, recorded},
	} {
		events := test.events
		server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
			response.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/api/sessions":
				_ = json.NewEncoder(response).Encode(map[string]any{"sessions": []any{map[string]any{
					"id": id, "cmd": "codex", "args": []any{}, "cwd": "/tmp", "createdAt": 1, "tool": "codex", "kind": "codex-app-server",
				}}})
			case "/api/sessions/" + id + "/events":
				_ = json.NewEncoder(response).Encode(map[string]any{"events": events, "nextIndex": len(events)})
			default:
				http.NotFound(response, request)
			}
		}))
		t.Setenv("HOME", t.TempDir())
		var stdout, stderr bytes.Buffer
		if code := run([]string{"--host", server.URL, "--json", "transcript", id[:8]}, strings.NewReader(""), &stdout, &stderr); code != 0 {
			t.Fatalf("%s exit=%d stderr=%q", test.name, code, stderr.String())
		}
		var turns []messageTurn
		if err := json.Unmarshal(stdout.Bytes(), &turns); err != nil {
			t.Fatal(err)
		}
		got := make([]string, 0, len(turns))
		for _, turn := range turns {
			got = append(got, turn.Text)
		}
		if strings.Join(got, "|") != strings.Join(test.want, "|") {
			t.Fatalf("%s = %q, want %q", test.name, got, test.want)
		}
		server.Close()
	}
}
