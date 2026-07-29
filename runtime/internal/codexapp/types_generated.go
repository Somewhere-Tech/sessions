// Code generated from the Codex 0.144.5 app-server protoschema subset used by
// this package. DO NOT EDIT.

package codexapp

import "encoding/json"

// ClientInfo identifies this JSON-RPC client during initialize.
type ClientInfo struct {
	Name    string  `json:"name"`
	Title   *string `json:"title,omitempty"`
	Version string  `json:"version"`
}

type InitializeCapabilities struct {
	ExperimentalAPI bool `json:"experimentalApi,omitempty"`
}

type InitializeParams struct {
	ClientInfo   ClientInfo             `json:"clientInfo"`
	Capabilities InitializeCapabilities `json:"capabilities"`
}

type InitializeResponse struct {
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
	UserAgent      string `json:"userAgent"`
}

// ThreadStartParams is the current-protocol equivalent of the legacy
// newConversation parameters.
type ThreadStartParams struct {
	ApprovalPolicy string         `json:"approvalPolicy,omitempty"`
	Config         map[string]any `json:"config,omitempty"`
	CWD            string         `json:"cwd,omitempty"`
	Ephemeral      *bool          `json:"ephemeral,omitempty"`
	Model          string         `json:"model,omitempty"`
	Sandbox        string         `json:"sandbox,omitempty"`
	ServiceTier    string         `json:"serviceTier,omitempty"`
}

type ThreadStartResponse struct {
	ApprovalPolicy json.RawMessage `json:"approvalPolicy"`
	CWD            string          `json:"cwd"`
	Model          string          `json:"model"`
	ModelProvider  string          `json:"modelProvider"`
	Thread         Thread          `json:"thread"`
}

type Thread struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
}

// TurnStartParams is the current-protocol equivalent of the legacy
// sendUserTurn/sendUserMessage parameters.
type TurnStartParams struct {
	ApprovalPolicy string      `json:"approvalPolicy,omitempty"`
	Effort         string      `json:"effort,omitempty"`
	Input          []UserInput `json:"input"`
	Model          string      `json:"model,omitempty"`
	ServiceTier    string      `json:"serviceTier,omitempty"`
	ThreadID       string      `json:"threadId"`
}

type UserInput struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type TurnStartResponse struct {
	Turn Turn `json:"turn"`
}

type Turn struct {
	ID     string       `json:"id"`
	Items  []ThreadItem `json:"items"`
	Status string       `json:"status"`
	Error  *TurnError   `json:"error,omitempty"`
}

type TurnError struct {
	Message string `json:"message,omitempty"`
}

// ThreadItem contains the common and agent-message fields from the
// protoschema. Raw preserves provider additions and item-specific fields.
type ThreadItem struct {
	ID    string          `json:"id"`
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	Phase *string         `json:"phase,omitempty"`
	Raw   json.RawMessage `json:"-"`
}

func (i *ThreadItem) UnmarshalJSON(data []byte) error {
	type itemAlias ThreadItem
	var decoded itemAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*i = ThreadItem(decoded)
	i.Raw = append(i.Raw[:0], data...)
	return nil
}

type TokenUsageBreakdown struct {
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	InputTokens           int64 `json:"inputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

type TokenUsage struct {
	Last               TokenUsageBreakdown `json:"last"`
	ModelContextWindow *int64              `json:"modelContextWindow,omitempty"`
	Total              TokenUsageBreakdown `json:"total"`
}

type AgentMessageDeltaNotification struct {
	Delta    string `json:"delta"`
	ItemID   string `json:"itemId"`
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type ItemStartedNotification struct {
	Item        ThreadItem `json:"item"`
	StartedAtMS int64      `json:"startedAtMs"`
	ThreadID    string     `json:"threadId"`
	TurnID      string     `json:"turnId"`
}

type ItemCompletedNotification struct {
	CompletedAtMS int64      `json:"completedAtMs"`
	Item          ThreadItem `json:"item"`
	ThreadID      string     `json:"threadId"`
	TurnID        string     `json:"turnId"`
}

type ThreadTokenUsageUpdatedNotification struct {
	ThreadID   string     `json:"threadId"`
	TokenUsage TokenUsage `json:"tokenUsage"`
	TurnID     string     `json:"turnId"`
}

type TurnCompletedNotification struct {
	ThreadID string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}
