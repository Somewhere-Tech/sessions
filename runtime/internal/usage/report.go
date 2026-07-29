package usage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type sessionBinding struct {
	id   string
	tags map[string]string
}

type aggregate struct {
	row       ReportRow
	models    map[string]struct{}
	providers map[string]struct{}
}

func (s *Service) Report(ctx context.Context, options ReportOptions) (Report, error) {
	if options.Group == "" {
		options.Group = "daily"
	}
	if options.Mode == "" {
		options.Mode = ModeAuto
	}
	if !oneOf(options.Group, "daily", "weekly", "monthly", "session", "tag", "provider", "model") {
		return Report{}, fmt.Errorf("invalid usage group %q", options.Group)
	}
	if options.Group == "tag" && strings.TrimSpace(options.Dimension) == "" {
		return Report{}, fmt.Errorf("tag reports need a dimension (for example dimension=product)")
	}
	if !oneOf(options.Mode, ModeAuto, ModeCalculate, ModeDisplay) {
		return Report{}, fmt.Errorf("invalid cost mode %q", options.Mode)
	}
	scan, err := s.Sync(ctx)
	if err != nil {
		return Report{}, err
	}
	db, err := s.database(ctx)
	if err != nil {
		return Report{}, err
	}
	bindings := s.sessionBindings()
	query := `SELECT provider, provider_session_id, timestamp_ms, model,
input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens,
reasoning_tokens, recorded_cost_usd, calculated_cost_usd, pricing_found
FROM usage_entries WHERE 1=1`
	args := make([]any, 0, 3)
	if !options.Since.IsZero() {
		query += " AND timestamp_ms >= ?"
		args = append(args, options.Since.UnixMilli())
	}
	if !options.Until.IsZero() {
		query += " AND timestamp_ms < ?"
		args = append(args, options.Until.UnixMilli())
	}
	if options.Provider != "" {
		query += " AND provider = ?"
		args = append(args, options.Provider)
	}
	query += " ORDER BY timestamp_ms ASC"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return Report{}, err
	}
	defer rows.Close()
	aggregates := make(map[string]*aggregate)
	for rows.Next() {
		var provider, providerSessionID, model string
		var timestampMS int64
		var tokens Tokens
		var recorded sql.NullFloat64
		var calculated float64
		var pricingFound bool
		if err := rows.Scan(&provider, &providerSessionID, &timestampMS, &model,
			&tokens.Input, &tokens.Output, &tokens.CacheCreation, &tokens.CacheRead,
			&tokens.Reasoning,
			&recorded, &calculated, &pricingFound); err != nil {
			return Report{}, err
		}
		binding := bindings[provider+":"+providerSessionID]
		key, start := usageGroupKey(options, time.UnixMilli(timestampMS), provider, providerSessionID, model, binding)
		group := aggregates[key]
		if group == nil {
			group = &aggregate{row: ReportRow{Key: key, Start: start}, models: make(map[string]struct{}), providers: make(map[string]struct{})}
			aggregates[key] = group
		}
		group.providers[provider] = struct{}{}
		if model != "" {
			group.models[model] = struct{}{}
		}
		group.row.Tokens.Input += tokens.Input
		group.row.Tokens.Output += tokens.Output
		group.row.Tokens.CacheCreation += tokens.CacheCreation
		group.row.Tokens.CacheRead += tokens.CacheRead
		group.row.Tokens.Reasoning += tokens.Reasoning
		if recorded.Valid {
			group.row.RecordedCostUSD += recorded.Float64
		}
		group.row.CalculatedCostUSD += calculated
		switch options.Mode {
		case ModeDisplay:
			if recorded.Valid {
				group.row.CostUSD += recorded.Float64
			}
		case ModeCalculate:
			group.row.CostUSD += calculated
		default:
			if recorded.Valid {
				group.row.CostUSD += recorded.Float64
			} else {
				group.row.CostUSD += calculated
			}
		}
		group.row.Entries++
		if !pricingFound && !(options.Mode == ModeDisplay || (options.Mode == ModeAuto && recorded.Valid)) {
			group.row.MissingPricing++
		}
		if options.Group == "session" {
			group.row.ProviderSessionID = providerSessionID
			if binding.id != "" {
				group.row.SessionID = binding.id
				group.row.Tags = state.CloneTags(binding.tags)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return Report{}, err
	}
	report := Report{SchemaVersion: 1, Machine: s.options.Machine, GeneratedAt: s.options.Now().UTC().Format(time.RFC3339), Group: options.Group,
		Mode: options.Mode, Dimension: strings.ToLower(strings.TrimSpace(options.Dimension)), Pricing: provenance, Scan: scan,
		Rows: make([]ReportRow, 0, len(aggregates)), Totals: ReportRow{Key: "total", Models: []string{}},
	}
	for _, group := range aggregates {
		for model := range group.models {
			group.row.Models = append(group.row.Models, model)
		}
		sort.Strings(group.row.Models)
		if len(group.providers) == 1 {
			for provider := range group.providers {
				group.row.Provider = provider
			}
		}
		report.Rows = append(report.Rows, group.row)
		addRow(&report.Totals, group.row)
	}
	sort.Slice(report.Rows, func(left, right int) bool {
		if options.Group == "session" || options.Group == "tag" || options.Group == "provider" || options.Group == "model" {
			if report.Rows[left].CostUSD == report.Rows[right].CostUSD {
				return report.Rows[left].Key < report.Rows[right].Key
			}
			return report.Rows[left].CostUSD > report.Rows[right].CostUSD
		}
		return report.Rows[left].Key < report.Rows[right].Key
	})
	modelSet := make(map[string]struct{})
	for _, row := range report.Rows {
		for _, model := range row.Models {
			modelSet[model] = struct{}{}
		}
	}
	for model := range modelSet {
		report.Totals.Models = append(report.Totals.Models, model)
	}
	sort.Strings(report.Totals.Models)
	return report, nil
}

func usageGroupKey(options ReportOptions, timestamp time.Time, provider, providerSessionID, model string, binding sessionBinding) (string, string) {
	local := timestamp.In(time.Local)
	switch options.Group {
	case "weekly":
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location())
		mondayOffset := (int(day.Weekday()) + 6) % 7
		start := day.AddDate(0, 0, -mondayOffset).Format("2006-01-02")
		return start, start
	case "monthly":
		start := local.Format("2006-01")
		return start, start
	case "session":
		if binding.id != "" {
			return binding.id, ""
		}
		return provider + ":" + providerSessionID, ""
	case "tag":
		value := binding.tags[strings.ToLower(strings.TrimSpace(options.Dimension))]
		if value == "" {
			value = "(untagged)"
		}
		return value, ""
	case "provider":
		return provider, ""
	case "model":
		if strings.TrimSpace(model) == "" {
			return "(unknown model)", ""
		}
		return model, ""
	default:
		start := local.Format("2006-01-02")
		return start, start
	}
}

func (s *Service) sessionBindings() map[string]sessionBinding {
	result := make(map[string]sessionBinding)
	entries, err := os.ReadDir(s.options.RunnerStateDir)
	if err != nil {
		return result
	}
	for _, item := range entries {
		if item.IsDir() || !strings.HasSuffix(item.Name(), ".json") {
			continue
		}
		metadata, err := state.ReadRunnerMetadata(filepath.Join(s.options.RunnerStateDir, item.Name()))
		if err != nil {
			continue
		}
		binding := sessionBinding{id: metadata.Info.ID, tags: state.CloneTags(metadata.Tags)}
		if metadata.Info.ClaudeSessionID != "" {
			result["claude:"+metadata.Info.ClaudeSessionID] = binding
		}
		if metadata.Info.ConversationID != "" {
			result["codex:"+metadata.Info.ConversationID] = binding
		}
		for index, argument := range metadata.Info.Args {
			if index+1 >= len(metadata.Info.Args) {
				break
			}
			if argument == "--session-id" || argument == "--resume" {
				key := "claude:"
				if strings.Contains(strings.ToLower(metadata.Info.Cmd), "codex") {
					key = "codex:"
				}
				result[key+metadata.Info.Args[index+1]] = binding
			}
		}
	}
	return result
}

func addRow(total *ReportRow, row ReportRow) {
	total.Tokens.Input += row.Tokens.Input
	total.Tokens.Output += row.Tokens.Output
	total.Tokens.CacheCreation += row.Tokens.CacheCreation
	total.Tokens.CacheRead += row.Tokens.CacheRead
	total.Tokens.Reasoning += row.Tokens.Reasoning
	total.CostUSD += row.CostUSD
	total.RecordedCostUSD += row.RecordedCostUSD
	total.CalculatedCostUSD += row.CalculatedCostUSD
	total.Entries += row.Entries
	total.MissingPricing += row.MissingPricing
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
