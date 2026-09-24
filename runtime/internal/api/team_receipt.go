package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/somewhere-tech/sessions/runtime/internal/verdict"
)

// A receipt attributes claims to their producer. A finished turn is not proof
// of a commit, a push, passing tests, or completion of the delegated task.
type handoffReceipt struct {
	Source    string   `json:"source"`
	Seq       uint64   `json:"seq,omitempty"`
	At        string   `json:"at,omitempty"`
	Outcome   string   `json:"outcome,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Workspace string   `json:"workspace,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	Commits   []string `json:"commits,omitempty"`
	Push      string   `json:"push"`
	Tests     []string `json:"tests,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
	Remaining []string `json:"remaining,omitempty"`
	Detail    string   `json:"detail,omitempty"`
}

func (s *Server) attachTeamReceipts(listing *teamListing) {
	store, err := verdict.NewStore(verdict.Options{StateDir: s.config.RunnerStateDir})
	for i := range listing.Members {
		member := &listing.Members[i]
		member.Handoff = &handoffReceipt{
			Source: "not-reported", Workspace: member.Cwd, Branch: member.Branch, Push: "unknown",
		}
		if err != nil {
			member.Handoff.Detail = "Could not open handoff records: " + truncateBudget(err.Error(), 200)
			continue
		}
		path, pathErr := store.Path(member.ID)
		if pathErr == nil {
			// Stat first: ErrNotFound can also mean a damaged log with no usable
			// records. That must not masquerade as a producer who reported nothing.
			_, pathErr = os.Stat(path)
		}
		if errors.Is(pathErr, os.ErrNotExist) {
			continue
		}
		record, readErr := store.Latest(member.ID)
		if readErr != nil {
			member.Handoff.Detail = "Could not read handoff: " + truncateBudget(readErr.Error(), 200)
			continue
		}
		member.Handoff = receiptFrom(record, *member)
	}
}

func receiptFrom(record verdict.Record, member teamMember) *handoffReceipt {
	receipt := &handoffReceipt{
		Source: "agent-reported", Seq: record.Seq, At: record.EmittedAt,
		Outcome: truncateBudget(record.Verdict, 100), Workspace: member.Cwd,
		Branch: member.Branch, Push: "unknown",
	}
	if raw, ok := record.Meta["handoff"]; ok {
		encoded, err := json.Marshal(raw)
		var fields map[string]any
		if err != nil || json.Unmarshal(encoded, &fields) != nil || fields == nil {
			receipt.Detail = "The agent's handoff details are not an object; inspect its verdict."
			return receipt
		}
		receipt.Summary = receiptText(fields["summary"], 600)
		receipt.Commits = receiptList(fields["commits"])
		receipt.Tests = receiptList(fields["tests"])
		receipt.Artifacts = receiptList(fields["artifacts"])
		receipt.Remaining = receiptList(fields["remaining"])
		if push, ok := fields["push"].(string); ok && (push == "pushed" || push == "not-pushed") {
			receipt.Push = push
		}
	}
	if record.SkippedRecords > 0 {
		receipt.Detail = fmt.Sprintf("%d unreadable records skipped; this is the newest usable report.", record.SkippedRecords)
	}
	return receipt
}

func receiptText(value any, budget int) string {
	text, _ := value.(string)
	return truncateBudget(text, budget)
}

func receiptList(value any) []string {
	items, _ := value.([]any)
	var result []string
	for _, item := range items {
		if text := receiptText(item, 240); text != "" {
			result = append(result, text)
		}
		if len(result) == 12 {
			break
		}
	}
	return result
}
