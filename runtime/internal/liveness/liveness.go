// Package liveness owns the single answer to one question: is the runner
// process recorded for a session still running?
//
// Before this package the answer existed three times -- in internal/session,
// internal/recovery, and internal/watch -- with three sets of comments
// promising the others that they agreed. They did not have to: nothing made
// them agree, and each copy could drift into calling a live session dead. A
// wrong "dead" is not a cosmetic defect here. It reaps a live session's
// artifacts, hands the user a session displayed as ended while its process is
// still working, and writes a permanent runner_lost fact into an append-only
// ledger.
//
// The answer is deliberately biased. Every ambiguous outcome resolves to
// "alive": an unreadable command line, a PID that exists but cannot be opened,
// a probe that fails for reasons of its own. Refusing to reap a dead runner
// costs one stale record that the next pass retries. Reaping a live one
// destroys work.
package liveness

import (
	"context"
	"path/filepath"
	"strings"
)

// Runner is the recorded identity of one session's runner process: the
// session it belongs to, the PID it reported, and the command it was launched
// with. All three are needed, because a PID alone cannot tell a live runner
// from an unrelated process that inherited its number.
type Runner struct {
	SessionID string
	PID       int
	Command   string
}

// RunnerAlive reports whether SessionID's runner process is running right now.
//
// This is the question every caller actually has, and the only one this
// package answers affirmatively. "A process exists at that PID" is not the
// same question and is not sufficient: PIDs are recycled, and a recycled PID
// running something unrelated must not keep a session pinned as live.
func RunnerAlive(ctx context.Context, runner Runner) bool {
	if runner.PID <= 0 {
		return false
	}
	if !ProcessAlive(runner.PID) {
		return false
	}
	return CommandMatches(ProcessCommand(ctx, runner.PID), runner.SessionID, runner.Command)
}

// CommandMatches decides whether the process observed at a recorded PID still
// belongs to the session that recorded it.
//
// It is exposed separately because the session manager probes through
// injectable seams (Options.ProcessAlive / Options.ProcessCommand) so its
// tests can drive discovery without real processes; the identity rule must
// stay shared even where the probe is not.
func CommandMatches(command, sessionID, expectedCommand string) bool {
	// An unknown command line is absence of evidence, not evidence of PID
	// reuse. The legacy TypeScript runner is matched by name because it
	// carries neither the session id nor the provider command in its argv.
	if command == "" ||
		strings.Contains(command, "runner.js") ||
		strings.Contains(command, "runner.ts") {
		return true
	}
	if sessionID != "" && strings.Contains(command, sessionID) {
		return true
	}
	// Every runner is the sessions-runner image whatever it hosts -- a PTY, a
	// headless pipe, or a structured provider conversation -- and the Windows
	// probe can only report that image path, because the session id lives in
	// a command line this package deliberately does not read out of another
	// process. Without this, every Windows session fails the match and is
	// reaped as PID reuse.
	if strings.Contains(strings.ToLower(command), "sessions-runner") {
		return true
	}
	expectedBase := filepath.Base(strings.TrimSpace(expectedCommand))
	return expectedBase != "" && expectedBase != "." && strings.Contains(command, expectedBase)
}
