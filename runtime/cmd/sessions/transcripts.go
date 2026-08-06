package main

import (
	"fmt"
	"sort"

	backupstore "github.com/somewhere-tech/sessions/runtime/internal/backup"
	sessionstate "github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

const transcriptsUsage = "usage: sessions transcripts [--apply | --dry-run]"

type transcriptReport struct {
	Session      string `json:"session"`
	Tool         string `json:"tool"`
	ProviderPath string `json:"provider_path,omitempty"`
	Status       string `json:"status"`
	Records      int    `json:"records,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

type transcriptsResult struct {
	Applied       bool `json:"applied"`
	Examined      int  `json:"examined"`
	Copyable      int  `json:"copyable"`
	Copied        int  `json:"copied"`
	AlreadyKept   int  `json:"already_kept"`
	Unrecoverable int  `json:"unrecoverable"`
	Unverified    int  `json:"unverified"`
	Incomplete    int  `json:"incomplete"`
	// KeptDamaged counts conversations Sessions holds a copy of that the copy
	// itself reports is missing records. They are excluded from AlreadyKept
	// rather than added to it, because AlreadyKept is what the closing summary
	// promises has survived, and a copy that stopped recording has not.
	KeptDamaged int                `json:"kept_damaged"`
	Sessions    []transcriptReport `json:"sessions"`
}

const (
	transcriptStatusCopied        = "copied"
	transcriptStatusWouldCopy     = "would-copy"
	transcriptStatusAlreadyKept   = "already-kept"
	transcriptStatusKeptDamaged   = "kept-damaged"
	transcriptStatusUnrecoverable = "unrecoverable"
	transcriptStatusIncomplete    = "incomplete"
	transcriptStatusUnverified    = "unverified"
)

// cmdTranscripts copies conversations Sessions can still read into storage
// Sessions owns.
//
// A watched session needs nothing: its watcher re-reads the provider file from
// offset zero on every attach, so it is mirrored already. This exists for the
// conversations nobody is watching -- ended sessions whose provider transcript
// is still on disk. Those are precisely the ones the provider's retention
// timer deletes next, and after that they cannot be recovered by anyone.
//
// It is a dry run by default, like gc, because it is a bulk operation over the
// user's own history and they should see what it will touch first.
func (a *app) cmdTranscripts(args []string) error {
	apply := removeFirst(&args, "--apply")
	dryRun := removeFirst(&args, "--dry-run")
	if apply && dryRun {
		return fail(exitUsage, "--apply and --dry-run cannot be combined")
	}
	if len(args) != 0 {
		return fail(exitUsage, "%s", transcriptsUsage)
	}

	config, err := sessionstate.ConfigFromEnv()
	if err != nil {
		return fail(exitUsage, "%s", err)
	}
	projects, err := watch.ClaudeProjectsDir()
	if err != nil {
		return fail(exitUsage, "resolve the Claude projects directory: %s", err)
	}

	resolver := backupstore.Resolver{
		ClaudeProjectsDir: projects,
		RunnerStateDir:    config.RunnerStateDir,
	}
	sessions := backupstore.CollectSessions(nil, config.RunnerStateDir)
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })

	result := transcriptsResult{Applied: apply}
	for _, session := range sessions {
		mirrorPath := watch.TranscriptMirrorPath(config.RunnerStateDir, session.ID)
		if mirrorPath == "" {
			continue
		}
		path, tool := resolver.Resolve(session)
		if tool != "claude" {
			continue
		}
		result.Examined++
		report := transcriptReport{Session: session.ID, Tool: tool}

		if watch.TranscriptMirrorUsable(mirrorPath) {
			// A mirror that stopped recording is still a mirror, so it is still
			// reported as kept rather than offered for re-copying: backfilling
			// into a capped mirror stores nothing, and a mirror is never
			// rewritten. What changes is the word, because "already kept" is a
			// promise that the conversation survived, and this one did not
			// survive whole.
			health := watch.ReadTranscriptMirrorHealth(mirrorPath)
			if health.Degraded() {
				report.Status = transcriptStatusKeptDamaged
				report.Detail = health.Detail()
				report.Records = health.Records
				result.KeptDamaged++
			} else {
				report.Status = transcriptStatusAlreadyKept
				result.AlreadyKept++
			}
			result.Sessions = append(result.Sessions, report)
			continue
		}
		if path == "" {
			// Neither the provider nor a mirror has it. Nothing to rescue,
			// and unlike every other branch here that is still true after
			// the transcript mirror exists: the mirror check above already
			// claimed every conversation Sessions kept a copy of.
			report.Status = transcriptStatusUnrecoverable
			result.Unrecoverable++
			result.Sessions = append(result.Sessions, report)
			continue
		}

		// Reading requires only a best guess, but copying enshrines one. When
		// a project bucket holds a single transcript the resolver returns it
		// even if it belongs to a different conversation -- fine for showing
		// something, wrong to store forever, because once the provider prunes,
		// the mirror becomes the answer. Only an exact provider-id match is
		// copied; anything less is reported and left alone.
		launchID := session.ClaudeSessionID
		if launchID == "" {
			launchID = session.ID
		}
		resolution := watch.ResolveClaudeCWD(projects, session.CWD, launchID)
		if resolution.Reason != watch.ClaudeExact {
			report.Status = transcriptStatusUnverified
			report.Detail = string(resolution.Reason)
			result.Unverified++
			result.Sessions = append(result.Sessions, report)
			continue
		}
		path = resolution.Path
		report.ProviderPath = path
		result.Copyable++
		if !apply {
			report.Status = transcriptStatusWouldCopy
			result.Sessions = append(result.Sessions, report)
			continue
		}

		backfill, backfillErr := watch.BackfillTranscriptMirror(path, watch.TranscriptMirrorOptions{
			Path:              mirrorPath,
			SessionID:         session.ID,
			ProviderSessionID: session.ClaudeSessionID,
			Tool:              tool,
		})
		report.Records = backfill.Copied
		switch {
		case backfillErr != nil:
			// Partial rescue is still rescue, so this is reported rather than
			// treated as a failure that abandons the rest of the sweep.
			report.Status = transcriptStatusIncomplete
			report.Detail = backfillErr.Error()
			result.Incomplete++
		case !backfill.Complete():
			report.Status = transcriptStatusIncomplete
			report.Detail = "some records could not be stored"
			result.Incomplete++
		default:
			report.Status = transcriptStatusCopied
			result.Copied++
		}
		result.Sessions = append(result.Sessions, report)
	}

	if a.wantJSON {
		return writeJSON(a.stdout, result, true)
	}
	return a.writeTranscriptsText(result)
}

func (a *app) writeTranscriptsText(result transcriptsResult) error {
	if result.Examined == 0 {
		_, err := fmt.Fprintln(a.stdout, "no Claude conversations found in this machine's session records")
		return err
	}
	for _, report := range result.Sessions {
		switch report.Status {
		case transcriptStatusWouldCopy:
			fmt.Fprintf(a.stdout, "%s  would copy from %s\n", shortID(report.Session), report.ProviderPath)
		case transcriptStatusCopied:
			fmt.Fprintf(a.stdout, "%s  copied %d records\n", shortID(report.Session), report.Records)
		case transcriptStatusIncomplete:
			fmt.Fprintf(a.stdout, "%s  copied %d records, incomplete: %s\n",
				shortID(report.Session), report.Records, report.Detail)
		case transcriptStatusKeptDamaged:
			// Named per session, unlike the other kept conversations. A count
			// alone would leave the user unable to act: the whole point is that
			// this particular conversation is not the one they think they have.
			fmt.Fprintf(a.stdout, "%s  kept but damaged: %s\n", shortID(report.Session), report.Detail)
		}
	}
	damaged := ""
	if result.KeptDamaged > 0 {
		damaged = fmt.Sprintf(", %d kept but damaged", result.KeptDamaged)
	}
	fmt.Fprintf(a.stdout,
		"\n%d conversations examined: %d already kept%s, %d unverified, %d unrecoverable, %d %s.\n",
		result.Examined, result.AlreadyKept, damaged, result.Unverified, result.Unrecoverable, result.Copyable,
		map[bool]string{true: "copied", false: "copyable"}[result.Applied],
	)
	if result.Incomplete > 0 {
		fmt.Fprintf(a.stdout, "%s copied only in part; see the detail above.\n",
			pluralConversations(result.Incomplete, "was", "were"))
	}
	if result.Unverified > 0 {
		fmt.Fprintf(a.stdout,
			"%s left alone because the provider transcript could not be matched by id; "+
				"copying a guess would make it permanent.\n",
			pluralConversations(result.Unverified, "was", "were"))
	}
	if result.AlreadyKept > 0 {
		// The count alone reads like bookkeeping. What it actually means is
		// the thing this command exists for: those conversations now survive
		// the provider deleting its own transcript, and there is a command
		// that brings one back. Saying which command matters, because the
		// provider's own resume is exactly what will not work for them.
		fmt.Fprintf(a.stdout,
			"%s a Sessions copy and %s the provider deleting its own transcript; "+
				"bring one back with `sessions resume <id>`, which replays Sessions' copy. "+
				"A native provider resume cannot reach a conversation the provider has deleted.\n",
			pluralConversations(result.AlreadyKept, "has", "have"),
			map[bool]string{true: "survives", false: "survive"}[result.AlreadyKept == 1])
	}
	if result.KeptDamaged > 0 {
		// The one thing this command must never do is let a copy that stopped
		// recording pass as a copy that survived. Say what it holds, say that
		// re-running cannot mend it -- a mirror is append-only and is never
		// rewritten, so there is no repair to offer -- and point at the copy
		// that may still be complete while it still exists.
		fmt.Fprintf(a.stdout,
			"%s a Sessions copy that stopped recording, so it holds part of the conversation and nothing after that point. "+
				"Running this again cannot mend it: a copy is only ever appended to, never rewritten. "+
				"While the provider still has its own transcript, that one is complete -- `sessions source <id>` names the file "+
				"a conversation is read from, and `sessions source <id> --raw` writes it out somewhere Sessions does not manage. "+
				"A copy that failed on write means the runner state directory was full or unwritable; fixing that is what stops "+
				"the next conversation losing records the same way.\n",
			pluralConversations(result.KeptDamaged, "has", "have"))
	}
	if result.Unrecoverable > 0 {
		// Precisely this branch and no other: no provider transcript and no
		// Sessions copy. Naming both is what keeps the sentence true now that
		// a missing provider transcript on its own no longer means loss.
		fmt.Fprintf(a.stdout,
			"%s no provider transcript and no Sessions copy; nothing can restore %s.\n",
			pluralConversations(result.Unrecoverable, "has", "have"),
			map[bool]string{true: "it", false: "them"}[result.Unrecoverable == 1])
	}
	if !result.Applied && result.Copyable > 0 {
		fmt.Fprintln(a.stdout, "This was a dry run. Run `sessions transcripts --apply` to keep them.")
	}
	return nil
}

func shortID(id string) string {
	return prefixString(id, 8)
}

// pluralConversations keeps the closing summary readable. A user reading "The
// 1 unrecoverable conversations" learns the sentence was assembled rather than
// written, and stops trusting the number in it.
func pluralConversations(count int, singularVerb, pluralVerb string) string {
	if count == 1 {
		return fmt.Sprintf("1 conversation %s", singularVerb)
	}
	return fmt.Sprintf("%d conversations %s", count, pluralVerb)
}
