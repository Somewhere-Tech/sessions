package watch

import (
	"strings"
)

// Surface answers the question neither provider's own picker will: where did
// this conversation come from, and did a person start it.
//
// Codex writes the answer into the session_meta line of every rollout, and
// Claude writes it into every transcript record. Both are recorded fact, not
// inference — the value is whatever the provider stamped at launch. Measured
// across the 176 rollouts and 27 transcripts on the development machine, the
// vocabulary is small and stable:
//
//	Codex  originator    codex-tui | Codex Desktop | codex_exec | pretty-pty |
//	                     codex_work_desktop
//	Codex  source        cli | vscode | exec | {"subagent":{"thread_spawn":…}}
//	Codex  thread_source user | automation | subagent | (absent)
//	Claude entrypoint    cli | sdk-cli | claude-desktop
//	Claude promptSource  typed | sdk
//
// Raw values are never discarded. Label is what a person reads, Kind is what a
// filter matches, and Originator/Source/ActorRaw are exactly what the provider
// wrote — so a Codex release that invents a new originator shows up as itself
// rather than being folded into a neighbouring bucket or dropped.
//
// Every field is optional and every zero value means "not recorded", which is
// deliberately distinct from any known value: a rollout written before Codex
// added thread_source has no answer to "who started this", and reporting that
// as "user" would be a fabrication in exactly the column a person is trying to
// use to separate their own work from an agent's.
type ConversationSurface struct {
	// Kind is the stable machine token: codex-cli, codex-desktop, codex-exec,
	// claude-cli, claude-desktop, claude-sdk, sessions, or a slug of an
	// unrecognised originator. This is what --surface matches.
	Kind string `json:"kind,omitempty"`

	// Label is the human name for Kind, always naming the provider so it can
	// stand in for the tool column without losing it.
	Label string `json:"label,omitempty"`

	// Originator is the provider's raw value: Codex `originator`, Claude
	// `entrypoint`. Kept so an unrecognised surface is still reportable.
	Originator string `json:"originator,omitempty"`

	// Source is Codex's raw `source`. A nested source object is reduced to its
	// single key ("subagent"), because the nesting carries the spawn details
	// rather than the surface.
	Source string `json:"source,omitempty"`

	// Actor is who drove the conversation: ActorUser, ActorAutomation, or
	// ActorAgent. Empty means the provider did not record it.
	Actor string `json:"actor,omitempty"`

	// ActorRaw is the provider's raw value: Codex `thread_source`, Claude
	// `promptSource`.
	ActorRaw string `json:"actor_raw,omitempty"`

	// Version is the client that wrote the conversation: Codex `cli_version`,
	// Claude `version`. Not shown in a compact row; reported by `source`.
	Version string `json:"version,omitempty"`
}

// Surface kinds. These are the tokens `sessions history --surface` accepts.
const (
	SurfaceCodexCLI      = "codex-cli"
	SurfaceCodexDesktop  = "codex-desktop"
	SurfaceCodexExec     = "codex-exec"
	SurfaceClaudeCLI     = "claude-cli"
	SurfaceClaudeDesktop = "claude-desktop"
	SurfaceClaudeSDK     = "claude-sdk"

	// SurfaceSessions is a conversation Sessions itself started. Codex records
	// it as the originator `pretty-pty`, which is this runner under the name it
	// shipped with before the rename; a conversation Sessions started has to be
	// recognisable as such even though the marker still says the old name.
	SurfaceSessions = "sessions"
)

// Actors. Kept to three because a longer vocabulary is not one a person can
// hold: either you did it, a schedule or a script did it, or another agent did.
const (
	ActorUser       = "user"
	ActorAutomation = "automation"
	ActorAgent      = "agent"
)

// KnownSurfaceKinds lists the tokens with a curated label, for help text and
// for telling a caller what they could have typed. An unrecognised originator
// still produces a usable Kind; it is just not in this list.
func KnownSurfaceKinds() []string {
	return []string{
		SurfaceCodexCLI, SurfaceCodexDesktop, SurfaceCodexExec,
		SurfaceClaudeCLI, SurfaceClaudeDesktop, SurfaceClaudeSDK, SurfaceSessions,
	}
}

// Known reports whether anything at all was recorded. A surface with no
// originator and no actor is indistinguishable from an absent one, and callers
// must not attach it to a row: an empty badge reads as a claim about the
// conversation rather than as silence about it.
func (s ConversationSurface) Known() bool {
	return s.Kind != "" || s.Originator != "" || s.Actor != "" || s.Source != ""
}

// Display is the label if there is one, falling back to the raw originator.
func (s ConversationSurface) Display() string {
	if s.Label != "" {
		return s.Label
	}
	return s.Originator
}

// CodexSurface reads provenance out of a Codex rollout's session_meta payload.
// The payload is passed in rather than re-read: session_meta is already parsed
// by the resolver and by the resumable scanner, and a second reader of the same
// line is a second thing to keep in agreement with the provider.
func CodexSurface(payload map[string]any) ConversationSurface {
	if payload == nil {
		return ConversationSurface{}
	}
	surface := ConversationSurface{
		Originator: strings.TrimSpace(stringField(payload, "originator")),
		Source:     codexSourceName(payload["source"]),
		ActorRaw:   strings.TrimSpace(stringField(payload, "thread_source")),
		Version:    strings.TrimSpace(stringField(payload, "cli_version")),
	}
	surface.Kind, surface.Label = codexSurfaceKind(surface.Originator)
	if surface.Kind == "" && surface.Originator != "" {
		surface.Kind, surface.Label = normalizeSurfaceToken(surface.Originator), surface.Originator
	}
	surface.Actor = codexActor(surface.ActorRaw, surface.Source)
	return surface
}

// codexSurfaceKind maps a raw Codex originator to a token and a human label.
//
// The three interactive originators stay separate because they are genuinely
// different places: the TUI in a terminal, the desktop app, and a headless
// `codex exec` run that nobody was watching. `codex_work_desktop` is the
// desktop app on a work account — the same surface, so it shares a token, but
// the label keeps the distinction because losing it would make two differently
// authenticated apps read identically.
//
// An originator this does not recognise gets no curated answer -- callers slug
// it and show it verbatim rather than folding it into "Codex". Guessing which
// existing bucket a new originator belongs to is how a surface column stops
// being trustworthy.
func codexSurfaceKind(originator string) (kind, label string) {
	switch normalizeSurfaceToken(originator) {
	case "codex-tui":
		return SurfaceCodexCLI, "Codex CLI"
	case "codex-desktop":
		return SurfaceCodexDesktop, "Codex Desktop"
	case "codex-work-desktop":
		return SurfaceCodexDesktop, "Codex Desktop (work)"
	case "codex-exec":
		return SurfaceCodexExec, "Codex exec"
	case "pretty-pty", "sessions", "sessions-pty":
		return SurfaceSessions, "Codex via Sessions"
	default:
		return "", ""
	}
}

// codexActor reads thread_source, which is the field that separates work the
// user did from work something else did. An absent thread_source stays absent:
// 47 of the 176 rollouts on the development machine predate the field, and
// calling those "user" would quietly assert the opposite of what this column
// exists to show.
func codexActor(threadSource, source string) string {
	if source == "subagent" {
		// A spawned child thread, whatever thread_source says about the
		// conversation that spawned it.
		return ActorAgent
	}
	switch strings.ToLower(strings.TrimSpace(threadSource)) {
	case "user":
		return ActorUser
	case "automation":
		return ActorAutomation
	case "subagent", "agent":
		return ActorAgent
	default:
		return ""
	}
}

// codexSourceName reduces `source` to a name. It is a plain string in every
// rollout but one, where it is {"subagent":{"thread_spawn":{…}}} — the spawn
// details belong to the parent link rather than to the surface, so only the
// key survives here.
func codexSourceName(value any) string {
	switch source := value.(type) {
	case string:
		return strings.TrimSpace(source)
	case map[string]any:
		if len(source) != 1 {
			return ""
		}
		for key := range source {
			return strings.TrimSpace(key)
		}
	}
	return ""
}

// ClaudeSurface maps Claude's recorded launch context. entrypoint names the
// surface; promptSource and isSidechain name the actor.
//
// The actor rules are ordered by how direct the evidence is. isSidechain is
// Claude saying this branch belongs to a sub-agent. promptSource is Claude
// saying whether the prompt was typed or came from the SDK — the most direct
// answer available, and the reason it outranks the entrypoint. Only when
// neither is present does the entrypoint decide, and there it is a reading of
// what the entrypoint means rather than a guess about the conversation:
// `sdk-cli` is programmatic by construction, `cli` and `claude-desktop` are
// surfaces a person types into.
func ClaudeSurface(entrypoint, promptSource, version string, sidechain bool) ConversationSurface {
	surface := ConversationSurface{
		Originator: strings.TrimSpace(entrypoint),
		ActorRaw:   strings.TrimSpace(promptSource),
		Version:    strings.TrimSpace(version),
	}
	surface.Kind, surface.Label = claudeSurfaceKind(surface.Originator)
	if surface.Kind == "" && surface.Originator != "" {
		surface.Kind, surface.Label = normalizeSurfaceToken(surface.Originator), surface.Originator
	}
	switch {
	case sidechain:
		surface.Actor = ActorAgent
	case strings.EqualFold(surface.ActorRaw, "typed"):
		surface.Actor = ActorUser
	case strings.EqualFold(surface.ActorRaw, "sdk"):
		surface.Actor = ActorAutomation
	case surface.Kind == SurfaceClaudeSDK:
		surface.Actor = ActorAutomation
	case surface.Kind == SurfaceClaudeCLI || surface.Kind == SurfaceClaudeDesktop:
		surface.Actor = ActorUser
	}
	return surface
}

// claudeSurfaceKind maps a raw Claude entrypoint.
//
// A Claude conversation Sessions started reports `cli`, because Sessions spawns
// the real `claude` binary in a pty and Claude has no way to know who its
// parent is. That is left alone rather than overwritten from the Sessions
// record: the transcript says CLI, and a surface column that sometimes reports
// the launcher and sometimes the provider would be worse than one that always
// reports what the provider wrote. Sessions-owned rows are already separable —
// they are the ones that are not `external`.
func claudeSurfaceKind(entrypoint string) (kind, label string) {
	switch normalizeSurfaceToken(entrypoint) {
	case "cli":
		return SurfaceClaudeCLI, "Claude Code"
	case "claude-desktop", "desktop":
		return SurfaceClaudeDesktop, "Claude Desktop"
	case "sdk-cli", "sdk":
		return SurfaceClaudeSDK, "Claude Agent SDK"
	default:
		return "", ""
	}
}

// Surface reports where a live Claude process was launched from, using the
// entrypoint Claude records in its own per-process registry. Same vocabulary as
// the transcript reader, so a live conversation and a recorded one describe
// their origin with the same words.
func (s ClaudeLiveSession) Surface() ConversationSurface {
	return ClaudeSurface(s.Entrypoint, "", s.Version, false)
}

// normalizeSurfaceToken folds a provider identifier into a filterable token.
// Codex spells its originators inconsistently — codex-tui with a dash,
// codex_exec with an underscore, "Codex Desktop" with a space and a capital —
// and a person typing --surface codex-desktop should not have to know which.
func normalizeSurfaceToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	folded := make([]byte, 0, len(value))
	previousDash := false
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9'):
			folded = append(folded, character)
			previousDash = false
		default:
			if !previousDash && len(folded) > 0 {
				folded = append(folded, '-')
				previousDash = true
			}
		}
	}
	return strings.TrimRight(string(folded), "-")
}

// NormalizeSurfaceKind folds what a caller typed into the token a surface
// carries, so --surface "Codex Desktop", codex_desktop and codex-desktop all
// select the same rows.
func NormalizeSurfaceKind(value string) string {
	kind, _ := codexSurfaceKind(value)
	if kind != "" {
		return kind
	}
	if kind, _ = claudeSurfaceKind(value); kind != "" {
		return kind
	}
	return normalizeSurfaceToken(value)
}

// NormalizeActor folds what a caller typed into an actor value. "me", "mine"
// and "human" all mean the same thing to the person typing them.
func NormalizeActor(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "user", "me", "mine", "human", "typed":
		return ActorUser
	case "automation", "auto", "scheduled", "sdk":
		return ActorAutomation
	case "agent", "subagent", "sidechain", "child":
		return ActorAgent
	default:
		return ""
	}
}

func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}
