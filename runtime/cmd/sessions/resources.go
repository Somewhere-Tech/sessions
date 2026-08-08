package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// This is a sibling verb rather than `sessions usage --resources`, and the
// distinction is not cosmetic.
//
// `sessions usage` is an accounting report. It indexes a durable store, it is
// grouped by day, week, month, session, provider or model, it takes --since and
// --until, and every number in it is a total accumulated over a period. A flag
// on that command would inherit all of it: `sessions usage --resources --since
// 2026-07-01` would have to mean something, and it cannot -- there is no stored
// history of memory, and there is no such thing as the resident memory of last
// Tuesday. A flag that silently ignores the options around it is worse than a
// separate word.
//
// What this command reports is a measurement of the machine right now: what is
// resident, what is burning CPU, and which sessions account for it. Different
// tense, different source, different verb. `sessions usage --resources` is
// accepted only as an error that points here, because it is the obvious thing
// to try.

const resourcesUsageText = "usage: sessions resources [-n N] [--json]"

type resourceRow struct {
	value   session
	memory  uint64
	percent float64
	hasCPU  bool
}

func (a *app) cmdResources(args []string) error {
	limit := 10
	rest := args
	if value, ok := pluck(&rest, "-n"); ok {
		parsed, err := parsePositiveCount(value)
		if err != nil {
			return err
		}
		limit = parsed
	}
	if len(rest) != 0 {
		return fail(1, "unknown resources option: %s\n%s", rest[0], resourcesUsageText)
	}

	records, err := a.fetchSessionRecords(false)
	if err != nil {
		return err
	}
	report := buildResourceReport(records, limit, time.Now())
	if a.wantJSON {
		return writeJSON(a.stdout, report, true)
	}
	return writeResourceReport(a.stdout, report)
}

// resourceReport is the JSON shape as well as the table's input. Every total
// is accompanied by the count it was computed from, because a total over three
// of two hundred sessions is a different fact from a total over all of them,
// and a reader given only the total cannot tell which it has.
type resourceReport struct {
	// Sessions is every live session the daemon returned.
	Sessions int `json:"sessions"`
	// Measured is how many of those had a readable process tree. The
	// difference between the two is sessions whose cost is unknown, and it is
	// reported rather than folded into the total as zero.
	Measured int `json:"measured"`
	// Processes is how many OS processes the measured trees covered.
	Processes int `json:"processes"`
	// TotalMemoryBytes is resident memory summed across the measured trees.
	TotalMemoryBytes uint64 `json:"totalMemoryBytes"`
	// TotalCPUPercent is percent of one core summed across the sessions whose
	// rate is known. CPUMeasured is its own count for the same reason Measured
	// exists: a session's memory can be known while its rate is not.
	TotalCPUPercent float64 `json:"totalCpuPercent"`
	CPUMeasured     int     `json:"cpuMeasured"`
	// OldestSampleAgeMS is how stale the least recent sample in this report is.
	// nil when nothing was measured.
	OldestSampleAgeMS *int64             `json:"oldestSampleAgeMs,omitempty"`
	Top               []resourceTopEntry `json:"top"`
}

type resourceTopEntry struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	PID         int      `json:"pid"`
	Tool        string   `json:"tool,omitempty"`
	MemoryBytes uint64   `json:"memoryBytes"`
	CPUPercent  *float64 `json:"cpuPercent,omitempty"`
	Processes   *int     `json:"resourceProcesses,omitempty"`
	SampledAt   *int64   `json:"resourceSampledAt,omitempty"`
}

func buildResourceReport(records []sessionRecord, limit int, now time.Time) resourceReport {
	report := resourceReport{Sessions: len(records), Top: []resourceTopEntry{}}
	rows := make([]resourceRow, 0, len(records))
	var oldest *int64
	for _, record := range records {
		value := record.value
		if value.MemoryBytes == nil {
			continue
		}
		report.Measured++
		report.TotalMemoryBytes += *value.MemoryBytes
		if value.ResourceProcesses != nil {
			report.Processes += *value.ResourceProcesses
		}
		row := resourceRow{value: value, memory: *value.MemoryBytes}
		if value.CPUPercent != nil {
			report.CPUMeasured++
			report.TotalCPUPercent += *value.CPUPercent
			row.percent, row.hasCPU = *value.CPUPercent, true
		}
		if value.ResourceSampledAt != nil {
			age := now.UnixMilli() - *value.ResourceSampledAt
			if age < 0 {
				age = 0
			}
			if oldest == nil || age > *oldest {
				oldest = &age
			}
		}
		rows = append(rows, row)
	}
	report.OldestSampleAgeMS = oldest

	// Biggest consumers by memory. Memory is the axis that decides whether a
	// machine pages, and it is the axis a reader deciding what to put to sleep
	// is actually choosing on; CPU is shown beside it, not sorted on.
	sort.SliceStable(rows, func(left, right int) bool { return rows[left].memory > rows[right].memory })
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	for _, row := range rows {
		report.Top = append(report.Top, resourceTopEntry{
			ID: row.value.ID, Name: row.value.Name, PID: row.value.PID, Tool: row.value.Tool,
			MemoryBytes: row.memory, CPUPercent: row.value.CPUPercent,
			Processes: row.value.ResourceProcesses, SampledAt: row.value.ResourceSampledAt,
		})
	}
	return report
}

func writeResourceReport(output io.Writer, report resourceReport) error {
	if report.Sessions == 0 {
		_, err := io.WriteString(output, "(no live sessions)\n")
		return err
	}
	if report.Measured == 0 {
		_, err := fmt.Fprintf(output, "%d live sessions, none with a readable process. Memory and CPU are unknown, not zero.\n", report.Sessions)
		return err
	}

	rows := [][]string{{"ID", "NAME", "TOOL", "PID", "PROCS", "MEM", "CPU"}}
	for _, entry := range report.Top {
		rows = append(rows, []string{
			prefixString(entry.ID, 8), compactSessionName(entry.Name), entry.Tool, fmt.Sprint(entry.PID),
			formatCount(entry.Processes), formatMemory(&entry.MemoryBytes), formatCPUPercent(entry.CPUPercent),
		})
	}
	if err := writePaddedRows(output, rows); err != nil {
		return err
	}

	memory := formatMemory(&report.TotalMemoryBytes)
	if _, err := fmt.Fprintf(output, "\n%s resident across %d of %d live sessions (%d processes)\n",
		memory, report.Measured, report.Sessions, report.Processes); err != nil {
		return err
	}
	cpu := "-"
	if report.CPUMeasured > 0 {
		cpu = formatCPUPercent(&report.TotalCPUPercent)
	}
	if _, err := fmt.Fprintf(output, "%s of one core across %d of %d live sessions\n",
		cpu, report.CPUMeasured, report.Sessions); err != nil {
		return err
	}
	if unmeasured := report.Sessions - report.Measured; unmeasured > 0 {
		if _, err := fmt.Fprintf(output,
			"%d session(s) have no process the daemon could read; their cost is unknown and is not in the totals above\n",
			unmeasured); err != nil {
			return err
		}
	}
	if report.OldestSampleAgeMS != nil {
		if _, err := fmt.Fprintf(output, "oldest sample %s ago; the daemon measures every %s\n",
			compactDuration(time.Duration(*report.OldestSampleAgeMS)*time.Millisecond), "5s"); err != nil {
			return err
		}
	}
	return nil
}

// formatMemory renders bytes, or "-" when there is nothing to render.
//
// nil is unknown and must never be printed as 0: a reader who sees 0 concludes
// the session is free, and a listing full of free sessions is how a machine
// ends up paging while its manager reports nothing wrong. A tree that genuinely
// measured zero bytes cannot exist -- a process that exists has pages resident
// -- so zero is treated as the same unknown rather than given a separate
// rendering that could never be right.
func formatMemory(bytes *uint64) string {
	if bytes == nil || *bytes == 0 {
		return "-"
	}
	value := float64(*bytes)
	switch {
	case value >= 1<<30:
		return fmt.Sprintf("%.1fG", value/(1<<30))
	case value >= 1<<20:
		return fmt.Sprintf("%.0fM", value/(1<<20))
	case value >= 1<<10:
		return fmt.Sprintf("%.0fK", value/(1<<10))
	default:
		return fmt.Sprintf("%dB", *bytes)
	}
}

// formatCPUPercent renders a rate, or "-" when there is none.
//
// Zero is kept here, unlike in formatMemory, because a measured zero is a real
// and common answer: an idle agent burning no CPU over the sample window is
// exactly the fact this column exists to expose. Only nil is unknown.
func formatCPUPercent(percent *float64) string {
	if percent == nil {
		return "-"
	}
	if *percent > 0 && *percent < 0.1 {
		// Below a tenth of a percent, "0.0%" reads as measured-zero. It is
		// measured-almost-zero, and the difference matters when the question is
		// whether a process is polling.
		return "<0.1%"
	}
	return fmt.Sprintf("%.1f%%", *percent)
}

func formatCount(value *int) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprint(*value)
}

// recordsHaveResources follows the PROFILE and PIN columns' rule: a column that
// is a dash on every row is noise on every row. A daemon too old to report
// resources, or one on a platform that cannot sample, leaves ls exactly as it
// was.
func recordsHaveResources(records []sessionRecord) bool {
	for _, record := range records {
		if record.value.MemoryBytes != nil {
			return true
		}
	}
	return false
}

func compactDuration(value time.Duration) string {
	switch {
	case value < time.Second:
		return fmt.Sprintf("%dms", value.Milliseconds())
	case value < time.Minute:
		return fmt.Sprintf("%.0fs", value.Seconds())
	case value < time.Hour:
		return fmt.Sprintf("%.0fm", value.Minutes())
	default:
		return fmt.Sprintf("%.1fh", value.Hours())
	}
}

func parsePositiveCount(value string) (int, error) {
	trimmed := strings.TrimSpace(value)
	count := 0
	if _, err := fmt.Sscanf(trimmed, "%d", &count); err != nil || count <= 0 {
		return 0, fail(1, "-n needs a positive count")
	}
	return count, nil
}
