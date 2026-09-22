package main

import (
	"fmt"
	"net/url"
	"strconv"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

func (a *app) cmdRead(args []string) error {
	cursor, present := pluck(&args, "--cursor")
	if present && cursor == "" {
		return fail(1, "--cursor requires the next_cursor from an earlier read")
	}
	limit := 20
	if raw, present := pluck(&args, "--limit"); present {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			return fail(1, "--limit must be between 1 and 100")
		}
		limit = value
	}
	if len(args) != 1 {
		return fail(1, "usage: sessions read <name-or-id> [--cursor CURSOR] [--limit 1..100]")
	}
	resolved, err := a.resolveHistoryReference(args[0])
	if err != nil {
		return err
	}
	id := resolved.Reference
	if _, err := a.useQualifiedHistoryReference(&id); err != nil {
		return err
	}
	query := url.Values{"limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var page integrations.ReadResponse
	if err := a.getJSON("/api/history/"+escapeID(id)+"/read?"+query.Encode(), &page); err != nil {
		return err
	}
	if a.wantJSON {
		return writeJSON(a.stdout, page, true)
	}
	for _, message := range page.Messages {
		fmt.Fprintf(a.stdout, "[%s]\n%s\n\n", message.Role, message.Text)
		if message.Continued {
			fmt.Fprintln(a.stdout, "(message continues on the next page)")
		}
	}
	if len(page.Messages) == 0 {
		fmt.Fprintln(a.stdout, "(no new readable messages in this page)")
	}
	if page.SkippedRecords > 0 {
		fmt.Fprintf(a.stdout, "Warning: %d malformed records skipped.\n", page.SkippedRecords)
	}
	if page.MirrorDamaged {
		fmt.Fprintf(a.stdout, "Warning: saved transcript is incomplete: %s\n", page.MirrorDetail)
	}
	if page.PendingRecord {
		fmt.Fprintln(a.stdout, "A provider record is still incomplete; retry with this cursor.")
	}
	if page.HasMore {
		fmt.Fprintln(a.stdout, "More history is available; continue with this cursor.")
	}
	_, err = fmt.Fprintf(a.stdout, "next_cursor: %s\n", page.NextCursor)
	return err
}
