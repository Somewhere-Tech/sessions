package main

import (
	"fmt"
	"strings"

	verdictprotocol "github.com/somewhere-tech/sessions/runtime/internal/verdict"
)

func (a *app) cmdVerdict(args []string) error {
	if len(args) == 0 {
		return fail(1, "usage: sessions verdict <id> [--json]\n       sessions verdict emit <id> --json '{...}'  # omit JSON to read stdin")
	}
	if args[0] == "emit" {
		return a.cmdVerdictEmit(args[1:])
	}
	if len(args) != 1 || args[0] == "" {
		return fail(1, "usage: sessions verdict <id> [--json]")
	}
	id, err := a.resolveSessionOrLaneID(args[0])
	if err != nil {
		return err
	}
	record, err := a.latestVerdict(id)
	if err != nil {
		return err
	}
	if record == nil {
		return fail(1, "no verdict for '%s'", id)
	}
	return a.writeVerdict(*record)
}

func (a *app) cmdVerdictEmit(args []string) error {
	if len(args) == 0 || args[0] == "" || len(args) > 2 {
		return fail(1, "usage: sessions verdict emit <id> --json '{...}'  # omit JSON to read stdin")
	}
	id, err := a.resolveSessionOrLaneID(args[0])
	if err != nil {
		return err
	}
	reader := a.stdin
	if len(args) == 2 {
		reader = strings.NewReader(args[1])
	}
	document, err := verdictprotocol.Decode(reader)
	if err != nil {
		return fail(1, "%s", err)
	}
	var record verdictprotocol.Record
	if err := a.postJSON("/api/sessions/"+escapeID(id)+"/verdict", document, &record, 2); err != nil {
		return err
	}
	return a.writeVerdict(record)
}

func (a *app) writeVerdict(record verdictprotocol.Record) error {
	if a.wantJSON {
		return writeJSON(a.stdout, record, true)
	}
	if _, err := fmt.Fprintf(a.stdout, "%s  seq=%d  %s\n", record.Verdict, record.Seq, record.EmittedAt); err != nil {
		return err
	}
	for _, finding := range record.Findings {
		location := ""
		if finding.File != "" {
			location = " (" + finding.File
			if finding.Line != nil {
				location += fmt.Sprintf(":%d", *finding.Line)
			}
			location += ")"
		}
		if _, err := fmt.Fprintf(a.stdout, "  %s: %s%s\n", finding.Severity, finding.Title, location); err != nil {
			return err
		}
		if finding.Detail != "" {
			if _, err := fmt.Fprintf(a.stdout, "    %s\n", finding.Detail); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a *app) resolveSessionOrLaneID(idOrPrefix string) (string, error) {
	sessions, err := a.listSessions(true)
	if err != nil {
		return "", err
	}
	for _, candidate := range sessions {
		if candidate.ID == idOrPrefix {
			return candidate.ID, nil
		}
	}
	matches := make([]session, 0)
	for _, candidate := range sessions {
		if strings.HasPrefix(candidate.ID, idOrPrefix) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 1 {
		return matches[0].ID, nil
	}
	if len(matches) > 1 {
		var lines strings.Builder
		for _, candidate := range matches {
			fmt.Fprintf(&lines, "  %s  %s\n", prefixString(candidate.ID, 8), a.sessionLabel(candidate))
		}
		return "", fail(1, "ambiguous session prefix '%s' — matches:\n%srun `sessions ls`", idOrPrefix, lines.String())
	}
	if err := verdictprotocol.ValidateID(idOrPrefix); err != nil {
		return "", fail(1, "%s", err)
	}
	return idOrPrefix, nil
}
