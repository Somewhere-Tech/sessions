package watch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The originator, source and thread_source values below are the complete
// measured vocabulary of the 176 Codex rollouts and 27 Claude transcripts on
// the development machine, not invented examples. If a provider adds a value
// this table does not know, the default branches are what has to hold.
func TestCodexSurfaceNormalizesEveryMeasuredOriginator(t *testing.T) {
	for _, test := range []struct {
		name       string
		payload    map[string]any
		kind       string
		label      string
		originator string
		source     string
		actor      string
	}{
		{
			name:    "codex TUI in a terminal",
			payload: map[string]any{"originator": "codex-tui", "source": "cli", "thread_source": "user"},
			kind:    SurfaceCodexCLI, label: "Codex CLI", originator: "codex-tui",
			source: "cli", actor: ActorUser,
		},
		{
			name:    "codex desktop app",
			payload: map[string]any{"originator": "Codex Desktop", "source": "vscode", "thread_source": "user"},
			kind:    SurfaceCodexDesktop, label: "Codex Desktop", originator: "Codex Desktop",
			source: "vscode", actor: ActorUser,
		},
		{
			name:    "codex desktop running an automation",
			payload: map[string]any{"originator": "Codex Desktop", "source": "vscode", "thread_source": "automation"},
			kind:    SurfaceCodexDesktop, label: "Codex Desktop", originator: "Codex Desktop",
			source: "vscode", actor: ActorAutomation,
		},
		{
			name:    "work-account desktop keeps its own label",
			payload: map[string]any{"originator": "codex_work_desktop", "source": "vscode", "thread_source": "user"},
			kind:    SurfaceCodexDesktop, label: "Codex Desktop (work)", originator: "codex_work_desktop",
			source: "vscode", actor: ActorUser,
		},
		{
			name:    "headless exec run",
			payload: map[string]any{"originator": "codex_exec", "source": "exec", "thread_source": "user"},
			kind:    SurfaceCodexExec, label: "Codex exec", originator: "codex_exec",
			source: "exec", actor: ActorUser,
		},
		{
			name:    "sessions own runner under its old name",
			payload: map[string]any{"originator": "pretty-pty", "source": "vscode"},
			kind:    SurfaceSessions, label: "Codex via Sessions", originator: "pretty-pty",
			source: "vscode", actor: "",
		},
		{
			name: "spawned child agent",
			payload: map[string]any{
				"originator": "Codex Desktop", "thread_source": "subagent",
				"source": map[string]any{"subagent": map[string]any{
					"thread_spawn": map[string]any{"parent_thread_id": "019f7ca8", "depth": float64(1)},
				}},
			},
			kind: SurfaceCodexDesktop, label: "Codex Desktop", originator: "Codex Desktop",
			source: "subagent", actor: ActorAgent,
		},
		{
			name:    "an originator nobody has seen shows itself rather than a guess",
			payload: map[string]any{"originator": "codex_future_surface", "source": "cli", "thread_source": "user"},
			kind:    "codex-future-surface", label: "codex_future_surface",
			originator: "codex_future_surface", source: "cli", actor: ActorUser,
		},
		{
			name:    "an unrecognised thread_source is kept raw and claims nothing",
			payload: map[string]any{"originator": "codex-tui", "source": "cli", "thread_source": "cron"},
			kind:    SurfaceCodexCLI, label: "Codex CLI", originator: "codex-tui",
			source: "cli", actor: "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := CodexSurface(test.payload)
			if got.Kind != test.kind || got.Label != test.label ||
				got.Originator != test.originator || got.Source != test.source || got.Actor != test.actor {
				t.Fatalf("CodexSurface = %#v", got)
			}
			if !got.Known() {
				t.Fatalf("surface reports nothing recorded: %#v", got)
			}
		})
	}
}

// An absent thread_source is 47 of the 176 rollouts here. It must stay absent:
// reporting it as "user" would assert the opposite of what the column exists to
// separate, on a quarter of the history.
func TestCodexSurfaceLeavesAnUnrecordedActorUnrecorded(t *testing.T) {
	got := CodexSurface(map[string]any{"originator": "Codex Desktop", "source": "vscode"})
	if got.Actor != "" || got.ActorRaw != "" {
		t.Fatalf("actor invented from nothing: %#v", got)
	}
	if got.Kind != SurfaceCodexDesktop {
		t.Fatalf("surface = %#v", got)
	}
}

func TestCodexSurfaceOfNothingIsNotKnown(t *testing.T) {
	if CodexSurface(nil).Known() {
		t.Fatal("a nil payload reported a known surface")
	}
	if CodexSurface(map[string]any{"cwd": "/tmp"}).Known() {
		t.Fatal("a payload with no provenance reported a known surface")
	}
}

func TestClaudeSurfaceNormalizesMeasuredEntrypoints(t *testing.T) {
	for _, test := range []struct {
		name         string
		entrypoint   string
		promptSource string
		sidechain    bool
		kind         string
		label        string
		actor        string
	}{
		{
			name: "interactive CLI", entrypoint: "cli", promptSource: "typed",
			kind: SurfaceClaudeCLI, label: "Claude Code", actor: ActorUser,
		},
		{
			name: "desktop app", entrypoint: "claude-desktop",
			kind: SurfaceClaudeDesktop, label: "Claude Desktop", actor: ActorUser,
		},
		{
			name: "agent SDK", entrypoint: "sdk-cli", promptSource: "sdk",
			kind: SurfaceClaudeSDK, label: "Claude Agent SDK", actor: ActorAutomation,
		},
		{
			name:       "SDK entrypoint with no promptSource still reads as programmatic",
			entrypoint: "sdk-cli",
			kind:       SurfaceClaudeSDK, label: "Claude Agent SDK", actor: ActorAutomation,
		},
		{
			name:       "a sidechain is a sub-agent whatever surface it ran on",
			entrypoint: "cli", promptSource: "typed", sidechain: true,
			kind: SurfaceClaudeCLI, label: "Claude Code", actor: ActorAgent,
		},
		{
			name:       "an SDK-sourced prompt on the CLI is automation, not the user",
			entrypoint: "cli", promptSource: "sdk",
			kind: SurfaceClaudeCLI, label: "Claude Code", actor: ActorAutomation,
		},
		{
			name: "an unknown entrypoint shows itself", entrypoint: "vscode-extension",
			kind: "vscode-extension", label: "vscode-extension", actor: "",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := ClaudeSurface(test.entrypoint, test.promptSource, "2.1.220", test.sidechain)
			if got.Kind != test.kind || got.Label != test.label || got.Actor != test.actor {
				t.Fatalf("ClaudeSurface = %#v", got)
			}
			if got.Originator != test.entrypoint || got.Version != "2.1.220" {
				t.Fatalf("raw values not preserved: %#v", got)
			}
		})
	}
}

func TestSurfaceFiltersAcceptWhatAPersonWouldType(t *testing.T) {
	for _, typed := range []string{"codex-desktop", "Codex Desktop", "codex_desktop", "CODEX-DESKTOP"} {
		if got := NormalizeSurfaceKind(typed); got != SurfaceCodexDesktop {
			t.Fatalf("NormalizeSurfaceKind(%q) = %q", typed, got)
		}
	}
	if got := NormalizeSurfaceKind("pretty-pty"); got != SurfaceSessions {
		t.Fatalf("Sessions' own old originator = %q", got)
	}
	if got := NormalizeSurfaceKind("sdk-cli"); got != SurfaceClaudeSDK {
		t.Fatalf("claude sdk = %q", got)
	}
	for _, typed := range []string{"me", "mine", "user", "human"} {
		if got := NormalizeActor(typed); got != ActorUser {
			t.Fatalf("NormalizeActor(%q) = %q", typed, got)
		}
	}
	if NormalizeActor("nonsense") != "" {
		t.Fatal("an unreadable actor must be refused, not guessed")
	}
}

// The surface is optional in the wire contract in both directions: a record
// written before the field existed decodes without it, and a decoder that knows
// the field must see nil rather than an empty struct.
func TestSurfaceIsOmittedWhenNothingWasRecorded(t *testing.T) {
	encoded, err := json.Marshal(ResumableSession{SessionID: "one", Tool: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	var decoded ResumableSession
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Surface != nil {
		t.Fatalf("absent surface decoded as %#v", decoded.Surface)
	}
	var old ResumableSession
	if err := json.Unmarshal([]byte(`{"sessionId":"one","tool":"codex","cwd":"/tmp"}`), &old); err != nil {
		t.Fatalf("a record predating the field must still decode: %v", err)
	}
	if old.Surface != nil {
		t.Fatalf("old record grew a surface: %#v", old.Surface)
	}
}

func TestReadCodexConversationSurfaceReadsTheSessionMetaLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-2026-08-05T13-33-15-019fd3a1.jsonl")
	meta, err := json.Marshal(map[string]any{
		"timestamp": "2026-08-05T13:33:15Z", "type": "session_meta",
		"payload": map[string]any{
			"id": "019fd3a1", "cwd": dir, "originator": "Codex Desktop",
			"source": "vscode", "thread_source": "automation", "cli_version": "0.104.0",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(meta, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	surface, ok := ReadCodexConversationSurface(path)
	if !ok {
		t.Fatal("session_meta carried provenance but none was read")
	}
	if surface.Kind != SurfaceCodexDesktop || surface.Actor != ActorAutomation ||
		surface.Originator != "Codex Desktop" || surface.Version != "0.104.0" {
		t.Fatalf("surface = %#v", surface)
	}

	// A rollout whose first line is not a session_meta reports not-known rather
	// than a blank surface, so a caller can tell silence from an answer.
	bare := filepath.Join(dir, "rollout-2026-08-05T13-33-16-019fd3a2.jsonl")
	if err := os.WriteFile(bare, []byte("{\"type\":\"response_item\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadCodexConversationSurface(bare); ok {
		t.Fatal("a rollout with no session_meta reported a surface")
	}
}

func TestReadClaudeConversationSurfaceReadsTheTranscriptPrefix(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "22222222-2222-4222-8222-222222222222.jsonl")
	contents := "{\"type\":\"summary\",\"summary\":\"no launch context here\"}\n" +
		"{\"type\":\"user\",\"cwd\":\"/Users/uzair/pretty-PTY\",\"entrypoint\":\"claude-desktop\"," +
		"\"version\":\"2.1.220\",\"promptSource\":\"typed\",\"isSidechain\":false," +
		"\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	surface, ok := ReadClaudeConversationSurface(path)
	if !ok {
		t.Fatal("transcript carried provenance but none was read")
	}
	if surface.Kind != SurfaceClaudeDesktop || surface.Actor != ActorUser ||
		surface.Originator != "claude-desktop" || surface.Version != "2.1.220" {
		t.Fatalf("surface = %#v", surface)
	}
	facts, ok := ReadClaudeTranscriptFacts(path)
	if !ok || facts.CWD != "/Users/uzair/pretty-PTY" {
		t.Fatalf("facts = %#v ok=%v", facts, ok)
	}

	empty := filepath.Join(dir, "33333333-3333-4333-8333-333333333333.jsonl")
	if err := os.WriteFile(empty, []byte("{\"type\":\"summary\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadClaudeConversationSurface(empty); ok {
		t.Fatal("a transcript with no launch context reported a surface")
	}
}

// Claude's live per-process registry and its transcripts must describe the same
// surface with the same words, or a conversation would change its name the
// moment it stopped running.
func TestClaudeLiveRegistryEntryReportsTheSameVocabulary(t *testing.T) {
	live := ClaudeLiveSession{Entrypoint: "claude-desktop", Version: "2.1.220"}
	if got := live.Surface(); got.Kind != SurfaceClaudeDesktop || got.Label != "Claude Desktop" {
		t.Fatalf("live surface = %#v", got)
	}
	if got := (ClaudeLiveSession{}).Surface(); got.Known() {
		t.Fatalf("an entry with no entrypoint reported a surface: %#v", got)
	}
}
