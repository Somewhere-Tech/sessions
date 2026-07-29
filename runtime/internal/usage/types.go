// Package usage builds a local, incremental token and cost ledger from the
// Claude Code and Codex JSONL files that already exist on this Mac.
package usage

import "time"

const (
	ModeAuto      = "auto"
	ModeCalculate = "calculate"
	ModeDisplay   = "display"
)

type Options struct {
	Path           string
	ClaudeRoots    []string
	CodexRoots     []string
	RunnerStateDir string
	Machine        string
	Now            func() time.Time
}

type Tokens struct {
	Input         int64 `json:"inputTokens"`
	Output        int64 `json:"outputTokens"`
	CacheCreation int64 `json:"cacheCreationTokens"`
	CacheRead     int64 `json:"cacheReadTokens"`
	Reasoning     int64 `json:"reasoningTokens"`
}

// Reasoning is a reported subset of output tokens, not an additional billable
// bucket, so it is deliberately excluded from Total and pricing arithmetic.
func (t Tokens) Total() int64 { return t.Input + t.Output + t.CacheCreation + t.CacheRead }

type ScanStats struct {
	FilesSeen   int `json:"filesSeen"`
	FilesRead   int `json:"filesRead"`
	LinesRead   int `json:"linesRead"`
	EntriesSeen int `json:"entriesSeen"`
}

type PricingProvenance struct {
	Source   string `json:"source"`
	Revision string `json:"revision"`
	URL      string `json:"url"`
	Note     string `json:"note"`
}

type ReportOptions struct {
	Group     string
	Mode      string
	Since     time.Time
	Until     time.Time
	Provider  string
	Dimension string
}

type ReportRow struct {
	Key               string            `json:"key"`
	Start             string            `json:"start,omitempty"`
	Provider          string            `json:"provider,omitempty"`
	SessionID         string            `json:"sessionId,omitempty"`
	ProviderSessionID string            `json:"providerSessionId,omitempty"`
	Tags              map[string]string `json:"tags,omitempty"`
	Models            []string          `json:"models"`
	Tokens            Tokens            `json:"tokens"`
	CostUSD           float64           `json:"costUSD"`
	RecordedCostUSD   float64           `json:"recordedCostUSD"`
	CalculatedCostUSD float64           `json:"calculatedCostUSD"`
	Entries           int64             `json:"entries"`
	MissingPricing    int64             `json:"missingPricingEntries"`
}

type Report struct {
	SchemaVersion int               `json:"schemaVersion"`
	Machine       string            `json:"machine"`
	GeneratedAt   string            `json:"generatedAt"`
	Group         string            `json:"group"`
	Mode          string            `json:"mode"`
	Dimension     string            `json:"dimension,omitempty"`
	Pricing       PricingProvenance `json:"pricing"`
	Scan          ScanStats         `json:"scan"`
	Rows          []ReportRow       `json:"rows"`
	Totals        ReportRow         `json:"totals"`
}

// ObservedSession ties a provider conversation to the physical local logs
// which contributed usage during a time window. It is intentionally local-only
// metadata used to build Today without uploading or copying transcripts.
type ObservedSession struct {
	Provider          string
	ProviderSessionID string
	FirstActivityAt   int64
	LastActivityAt    int64
	Entries           int64
	SourcePaths       []string
	TurnIDs           []string
}
