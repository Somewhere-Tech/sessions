package proto

import (
	"context"
	"encoding/json"
)

// RunnerInfo is the decoded HELLO payload plus the socket path used by the
// daemon. Sequence numbers deliberately stay uint32 because that is the
// canonical wire width.
type RunnerInfo struct {
	ID              string         `json:"id"`
	Cmd             string         `json:"cmd"`
	Args            []string       `json:"args"`
	Cwd             string         `json:"cwd"`
	Cols            int            `json:"cols"`
	Rows            int            `json:"rows"`
	CreatedAt       int64          `json:"createdAt"`
	PID             int            `json:"pid"`
	SocketPath      string         `json:"sockPath"`
	CurrentSeq      uint32         `json:"currentSeq,omitempty"`
	ProtocolVersion int            `json:"protocolVersion,omitempty"`
	RuntimeVersion  string         `json:"runtimeVersion,omitempty"`
	ConversationID  string         `json:"conversationId,omitempty"`
	RemoteEndpoint  string         `json:"remoteEndpoint,omitempty"`
	ClaudeSessionID string         `json:"claudeSessionId,omitempty"`
	Retry           *ProviderRetry `json:"retry,omitempty"`
	Turn            *TurnState     `json:"turn,omitempty"`
}

// TurnState is the Rich runner's exact current provider-turn state. It lives
// in HELLO so a replacement daemon does not have to infer live work from a
// bounded history replay.
type TurnState struct {
	Working bool `json:"working"`
}

// ProviderRetry is the live runner-owned schedule for one retained failed
// Rich turn. Attempt is the next retry that will run, not the original turn.
type ProviderRetry struct {
	Attempt int    `json:"attempt"`
	Max     int    `json:"max"`
	NextAt  int64  `json:"nextAt"`
	Kind    string `json:"kind"`
}

type ProviderRetryState struct {
	Retry *ProviderRetry `json:"retry"`
}

type LaunchRequest struct {
	Info RunnerInfo
	Env  map[string]string
}

type OutputEvent struct {
	Seq  uint32 `json:"seq"`
	Data string `json:"data"`
	At   int64  `json:"-"`
}

type ExitEvent struct {
	Code   *int    `json:"code"`
	Signal *string `json:"signal"`
	Seq    uint32  `json:"seq"`
	Reason string  `json:"reason,omitempty"`
}

type ReplayWindow struct {
	Events     []OutputEvent
	Structured []json.RawMessage
	Gap        bool
	Oldest     uint32
	Current    uint32
}

type EventKind uint8

const (
	EventOutput EventKind = iota + 1
	EventExit
	EventClaude
	EventCodex
	EventRunnerLost
	EventRetry
)

type Event struct {
	Kind        EventKind
	Output      OutputEvent
	Exit        ExitEvent
	ClaudeEvent json.RawMessage
	CodexEvent  json.RawMessage
	ClaudeIndex int64
	// ClaudeActivityAt is derived by state.recordClaudeLocked and carried to
	// the session manager so provider activity is ledgered without reparsing.
	ClaudeActivityAt int64
	Retry            *ProviderRetry
}

// Runner is a daemon-side connection to one canonical runner socket. The
// concrete implementation below speaks the frame protocol in proto.go; test
// implementations use the same semantic surface without redefining the wire.
type Runner interface {
	Info() RunnerInfo
	Replay(context.Context, uint32) ReplayWindow
	Input(context.Context, string) error
	ConfigureModel(context.Context, ModelControl) error
	Approve(context.Context, ApprovalControl) error
	Resize(context.Context, int, int) error
	Kill(context.Context) error
	Subscribe() (<-chan Event, func())
}

// RunnerLauncher creates or attaches socket-backed runners. ProgramArguments
// is persisted in the launchd plist before Launch is invoked.
type RunnerLauncher interface {
	ProgramArguments(LaunchRequest) []string
	Launch(context.Context, LaunchRequest) (Runner, error)
	Attach(context.Context, RunnerInfo) (Runner, error)
}

// RunnerWaker is implemented by launchers that can restart a runner which
// deliberately stayed paused after a reboot, so a person's first message or
// first look wakes it without a manual resume.
type RunnerWaker interface {
	Wake(context.Context, string) (Runner, error)
}

// RunnerLaunchPreflight is implemented by launchers that can validate a
// launch without creating persistent state. Registry invokes it before the
// lifecycle boundary, metadata, plist, and launch-started event.
type RunnerLaunchPreflight interface {
	Preflight(LaunchRequest) error
}
