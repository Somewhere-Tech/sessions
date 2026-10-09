package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// fakeLogFile is an in-memory history file whose next Write results can be
// scripted: how many bytes it stores and which error it returns.
type fakeLogFile struct {
	data    []byte
	writes  [][]byte
	script  []fakeWrite
	statErr error
}

type fakeWrite struct {
	n   int // bytes stored; -1 stores everything
	err error
}

func (f *fakeLogFile) Write(p []byte) (int, error) {
	f.writes = append(f.writes, append([]byte(nil), p...))
	step := fakeWrite{n: -1}
	if len(f.script) > 0 {
		step, f.script = f.script[0], f.script[1:]
	}
	n := step.n
	if n < 0 || n > len(p) {
		n = len(p)
	}
	f.data = append(f.data, p[:n]...)
	return n, step.err
}

func (f *fakeLogFile) ReadAt(p []byte, offset int64) (int, error) {
	if offset >= int64(len(f.data)) {
		return 0, io.EOF
	}
	n := copy(p, f.data[offset:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *fakeLogFile) Stat() (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return fakeLogInfo(len(f.data)), nil
}

type fakeLogInfo int64

func (s fakeLogInfo) Name() string       { return "history.jsonl" }
func (s fakeLogInfo) Size() int64        { return int64(s) }
func (s fakeLogInfo) Mode() os.FileMode  { return 0o600 }
func (s fakeLogInfo) ModTime() time.Time { return time.Time{} }
func (s fakeLogInfo) IsDir() bool        { return false }
func (s fakeLogInfo) Sys() any           { return nil }

// tailOf reads bytes back through the runner's own cold-start reader.
func tailOf(t *testing.T, data []byte) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	history, err := readStructuredHistoryTail(file)
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, len(history))
	for i, raw := range history {
		lines[i] = string(raw)
	}
	return lines
}

func countRecord(writes [][]byte, record string) int {
	count := 0
	for _, write := range writes {
		count += strings.Count(string(write), record)
	}
	return count
}

// A cold log is appended to without changing a byte already there; an
// unterminated tail, valid or not, gets its own line first.
func TestStructuredAppendRespectsAColdLogsTail(t *testing.T) {
	const next = `{"n":2}`
	for _, tc := range []struct {
		name, existing, want string
		replay               []string
	}{
		{"empty", "", next + "\n", []string{next}},
		{"terminated", "{\"n\":1}\n", "{\"n\":1}\n" + next + "\n", []string{`{"n":1}`, next}},
		{"valid unterminated", `{"n":1}`, "{\"n\":1}\n" + next + "\n", []string{`{"n":1}`, next}},
		{"invalid unterminated", `{"n":`, "{\"n\":\n" + next + "\n", []string{next}},
	} {
		file := &fakeLogFile{data: []byte(tc.existing)}
		var end structuredLogEnd
		if err := appendStructuredRecord(file, &end, []byte(next)); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := string(file.data); got != tc.want || !strings.HasPrefix(got, tc.existing) {
			t.Fatalf("%s: bytes %q, want %q with the old bytes unchanged", tc.name, got, tc.want)
		}
		if got := tailOf(t, file.data); fmt.Sprint(got) != fmt.Sprint(tc.replay) {
			t.Fatalf("%s: replay %q, want %q", tc.name, got, tc.replay)
		}
	}
	// A valid unterminated record is still read before anything is appended.
	if got := tailOf(t, []byte("{\"n\":0}\n{\"n\":1}")); fmt.Sprint(got) != fmt.Sprint([]string{`{"n":0}`, `{"n":1}`}) {
		t.Fatalf("unterminated valid tail = %q", got)
	}
}

// A partial write with an error isolates its fragment; the next record is kept
// once, and the partly written record is never written a second time.
func TestStructuredAppendIsolatesAPartialWrite(t *testing.T) {
	file := &fakeLogFile{script: []fakeWrite{{n: 5, err: errors.New("no space left on device")}}}
	var end structuredLogEnd
	err := appendStructuredRecord(file, &end, []byte(`{"first":"message"}`))
	if !errors.Is(err, errStructuredRecordPartial) || !strings.Contains(err.Error(), "(5 of 20 bytes)") || !end.open {
		t.Fatalf("partial write: err=%v end=%+v", err, end)
	}
	if err := appendStructuredRecord(file, &end, []byte(`{"second":"receipt"}`)); err != nil {
		t.Fatal(err)
	}
	if got, want := string(file.data), "{\"fir\n{\"second\":\"receipt\"}\n"; got != want {
		t.Fatalf("bytes %q, want %q", got, want)
	}
	if countRecord(file.writes, `{"first":"message"}`) != 1 || countRecord(file.writes, `{"second":"receipt"}`) != 1 {
		t.Fatalf("a record was retried: %q", file.writes)
	}
	if got := tailOf(t, file.data); fmt.Sprint(got) != fmt.Sprint([]string{`{"second":"receipt"}`}) {
		t.Fatalf("replay %q lost the next record", got)
	}
}

func TestStructuredAppendReportsAShortWriteWithoutAnError(t *testing.T) {
	file := &fakeLogFile{script: []fakeWrite{{n: 3}}}
	var end structuredLogEnd
	err := appendStructuredRecord(file, &end, []byte(`{"a":1}`))
	if !errors.Is(err, io.ErrShortWrite) || !errors.Is(err, errStructuredRecordPartial) || !end.open {
		t.Fatalf("short write: err=%v end=%+v", err, end)
	}
}

// Nothing stored means nothing to isolate: the next record needs no separator.
func TestStructuredAppendZeroByteFailureLeavesTheLineClosed(t *testing.T) {
	file := &fakeLogFile{data: []byte("{\"a\":1}\n"), script: []fakeWrite{{n: 0, err: errors.New("EIO")}}}
	var end structuredLogEnd
	if err := appendStructuredRecord(file, &end, []byte(`{"b":2}`)); err == nil || end.open {
		t.Fatalf("zero-byte failure: err=%v end=%+v", err, end)
	}
	if err := appendStructuredRecord(file, &end, []byte(`{"c":3}`)); err != nil {
		t.Fatal(err)
	}
	if got := string(file.data); got != "{\"a\":1}\n{\"c\":3}\n" {
		t.Fatalf("bytes %q", got)
	}
}

// A separator that cannot be written means the record is not appended onto the
// damaged line; the next append closes the line first.
func TestStructuredAppendNeverWritesOntoAnOpenLine(t *testing.T) {
	for _, failure := range []fakeWrite{{n: 0, err: errors.New("EIO")}, {n: 0}} {
		file := &fakeLogFile{data: []byte(`{"broken`), script: []fakeWrite{failure}}
		var end structuredLogEnd
		err := appendStructuredRecord(file, &end, []byte(`{"lost":1}`))
		if !errors.Is(err, errStructuredLineOpen) || !end.open {
			t.Fatalf("failed separator: err=%v end=%+v", err, end)
		}
		if failure.err == nil && !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("zero-byte separator with nil error not reported as short: %v", err)
		}
		if failure.err != nil && !errors.Is(err, failure.err) {
			t.Fatalf("separator cause lost: %v", err)
		}
		if countRecord(file.writes, `{"lost":1}`) != 0 {
			t.Fatalf("record written onto an open line: %q", file.writes)
		}
		if err := appendStructuredRecord(file, &end, []byte(`{"kept":2}`)); err != nil {
			t.Fatal(err)
		}
		if got := string(file.data); got != "{\"broken\n{\"kept\":2}\n" {
			t.Fatalf("bytes %q", got)
		}
	}
}

// When the end of the file cannot be read, a separator is written: a blank line
// is harmless to every reader, a missing one costs a record.
func TestStructuredAppendAssumesAnOpenLineWhenUnsure(t *testing.T) {
	file := &fakeLogFile{data: []byte("{\"a\":1}\n"), statErr: errors.New("stat failed")}
	var end structuredLogEnd
	if err := appendStructuredRecord(file, &end, []byte(`{"b":2}`)); err != nil {
		t.Fatal(err)
	}
	if got := tailOf(t, file.data); fmt.Sprint(got) != fmt.Sprint([]string{`{"a":1}`, `{"b":2}`}) {
		t.Fatalf("replay %q", got)
	}
}

// The end of the file is read once per opened file, not once per record.
func TestStructuredAppendReadsTheTailOnlyOnce(t *testing.T) {
	file := &countingLogFile{fakeLogFile: fakeLogFile{data: []byte(`{"a":1}`)}}
	var end structuredLogEnd
	for i := 0; i < 5; i++ {
		if err := appendStructuredRecord(file, &end, []byte(`{"d":"x"}`)); err != nil {
			t.Fatal(err)
		}
	}
	if file.reads != 1 || file.stats != 1 {
		t.Fatalf("tail read %d times, stat %d times", file.reads, file.stats)
	}
}

type countingLogFile struct {
	fakeLogFile
	reads, stats int
}

func (f *countingLogFile) ReadAt(p []byte, offset int64) (int, error) {
	f.reads++
	return f.fakeLogFile.ReadAt(p, offset)
}

func (f *countingLogFile) Stat() (os.FileInfo, error) {
	f.stats++
	return f.fakeLogFile.Stat()
}

// Both structured providers use the boundary on their live path, keep the
// write before memory and broadcast, and keep order and the 4 MiB frame rule.
func TestBothStructuredRunnersIsolateADamagedTail(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		paths := state.For(t.TempDir(), provider+"-session")
		path := paths.Structured
		if provider == "claude" {
			path = paths.ClaudeP
		}
		if err := os.WriteFile(path, []byte(`{"torn":`), 0o600); err != nil {
			t.Fatal(err)
		}
		var appendRecord func(json.RawMessage)
		var history func() []json.RawMessage
		var reopen func() error
		var stale, closeHistory func()
		switch provider {
		case "codex":
			r := &codexAppRunner{paths: paths, logger: log.New(io.Discard, "", 0), clients: map[*client]struct{}{}, ctx: context.Background()}
			reopen = r.openHistory
			appendRecord, history = r.appendStructured, func() []json.RawMessage { return r.history }
			stale = func() { r.historyEnd = structuredLogEnd{known: true} }
			closeHistory = r.closeHistory
		default:
			r := &claudeStructuredRunner{paths: paths, logger: log.New(io.Discard, "", 0), clients: map[*client]struct{}{}, ctx: context.Background()}
			reopen = r.openHistory
			appendRecord, history = r.appendStructured, func() []json.RawMessage { return r.history }
			stale = func() { r.historyEnd = structuredLogEnd{known: true} }
			closeHistory = r.closeHistory
		}
		stale() // a stale "closed" state must not survive opening a file
		if err := reopen(); err != nil {
			t.Fatal(err)
		}
		appendRecord(json.RawMessage(`{"order":1}`))
		appendRecord(json.RawMessage(`{"order":2}`))
		appendRecord(json.RawMessage(`{"big":"` + strings.Repeat("x", proto.MaxFrameLen) + `"}`))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if want := "{\"torn\":\n{\"order\":1}\n{\"order\":2}\n"; string(raw) != want {
			t.Fatalf("%s: bytes %q, want %q", provider, raw, want)
		}
		if got := fmt.Sprintf("%s", history()); got != "[{\"order\":1} {\"order\":2}]" {
			t.Fatalf("%s: memory %s", provider, got)
		}
		closeHistory()
		if err := reopen(); err != nil {
			t.Fatal(err)
		}
		if got := fmt.Sprintf("%s", history()); got != "[{\"order\":1} {\"order\":2}]" {
			t.Fatalf("%s: reopened replay %s", provider, got)
		}
		closeHistory()
	}
}

// Every structured history write goes through the boundary; a direct Write
// would bypass its state.
func TestNoStructuredHistoryWriteBypassesTheBoundary(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(source, []byte("historyFile.Write(")) {
			t.Fatalf("%s writes the structured history file directly", name)
		}
	}
}

// A record Write that reports every byte stored and also returns an error is
// reported as exactly that: not as missing, not as success, and never retried.
func TestStructuredAppendFullCountWithErrorIsReportedHonestly(t *testing.T) {
	eio := errors.New("EIO")
	file := &fakeLogFile{data: []byte("{\"a\":1}\n"), script: []fakeWrite{{n: -1, err: eio}}}
	var end structuredLogEnd
	err := appendStructuredRecord(file, &end, []byte(`{"b":2}`))
	if !errors.Is(err, errStructuredRecordStoredWithError) || !errors.Is(err, eio) ||
		errors.Is(err, errStructuredRecordNotStored) || errors.Is(err, errStructuredRecordPartial) {
		t.Fatalf("full count with error: %v", err)
	}
	if !strings.Contains(err.Error(), "(8 of 8 bytes)") || strings.Contains(err.Error(), "not written:") || end.open {
		t.Fatalf("full count with error: %v end=%+v", err, end)
	}
	if err := appendStructuredRecord(file, &end, []byte(`{"c":3}`)); err != nil {
		t.Fatal(err)
	}
	if got := string(file.data); got != "{\"a\":1}\n{\"b\":2}\n{\"c\":3}\n" {
		t.Fatalf("bytes %q", got)
	}
	if countRecord(file.writes, `{"b":2}`) != 1 || len(file.writes) != 2 {
		t.Fatalf("record retried or separator added: %q", file.writes)
	}
}

// A separator Write that reports its byte stored but returns an error closes
// the line, is not swallowed, and this record is not written; the next record
// needs no separator and this one is never written later.
func TestStructuredAppendSeparatorStoredWithErrorIsNotSwallowed(t *testing.T) {
	eio := errors.New("EIO")
	file := &fakeLogFile{data: []byte(`{"torn`), script: []fakeWrite{{n: -1, err: eio}}}
	var end structuredLogEnd
	err := appendStructuredRecord(file, &end, []byte(`{"this":1}`))
	if !errors.Is(err, errStructuredSeparatorError) || !errors.Is(err, eio) || errors.Is(err, errStructuredLineOpen) {
		t.Fatalf("separator stored with error: %v", err)
	}
	if end.open || string(file.data) != "{\"torn\n" {
		t.Fatalf("end=%+v bytes %q", end, file.data)
	}
	if err := appendStructuredRecord(file, &end, []byte(`{"next":2}`)); err != nil {
		t.Fatal(err)
	}
	if got := string(file.data); got != "{\"torn\n{\"next\":2}\n" {
		t.Fatalf("bytes %q", got)
	}
	if countRecord(file.writes, `{"this":1}`) != 0 || len(file.writes) != 2 {
		t.Fatalf("record written after a separator error, or a redundant separator: %q", file.writes)
	}
}

// Every outcome is distinguishable by its sentinel, with the cause kept.
func TestStructuredAppendOutcomesStayDistinguishable(t *testing.T) {
	eio := errors.New("EIO")
	for _, tc := range []struct {
		name     string
		existing string
		script   []fakeWrite
		want     error
		cause    error
		open     bool
	}{
		{"record none with error", "", []fakeWrite{{n: 0, err: eio}}, errStructuredRecordNotStored, eio, false},
		{"record none without error", "", []fakeWrite{{n: 0}}, errStructuredRecordNotStored, io.ErrShortWrite, false},
		{"record partial with error", "", []fakeWrite{{n: 2, err: eio}}, errStructuredRecordPartial, eio, true},
		{"record partial without error", "", []fakeWrite{{n: 2}}, errStructuredRecordPartial, io.ErrShortWrite, true},
		{"record full with error", "", []fakeWrite{{n: -1, err: eio}}, errStructuredRecordStoredWithError, eio, false},
		{"separator none with error", "{", []fakeWrite{{n: 0, err: eio}}, errStructuredLineOpen, eio, true},
		{"separator none without error", "{", []fakeWrite{{n: 0}}, errStructuredLineOpen, io.ErrShortWrite, true},
		{"separator stored with error", "{", []fakeWrite{{n: -1, err: eio}}, errStructuredSeparatorError, eio, false},
	} {
		file := &fakeLogFile{data: []byte(tc.existing), script: tc.script}
		var end structuredLogEnd
		err := appendStructuredRecord(file, &end, []byte(`{"x":1}`))
		if !errors.Is(err, tc.want) || !errors.Is(err, tc.cause) || end.open != tc.open {
			t.Fatalf("%s: err=%v end=%+v", tc.name, err, end)
		}
		if !strings.HasPrefix(string(file.data), tc.existing) {
			t.Fatalf("%s: existing bytes changed: %q", tc.name, file.data)
		}
		for _, other := range []error{errStructuredLineOpen, errStructuredSeparatorError, errStructuredRecordNotStored, errStructuredRecordPartial, errStructuredRecordStoredWithError} {
			if other != tc.want && errors.Is(err, other) {
				t.Fatalf("%s: also matches %v", tc.name, other)
			}
		}
	}
}

// On both providers a failed append is logged and the event is still kept in
// memory and sent to clients, exactly as before the boundary existed.
func TestStructuredRunnersStillLogAndBroadcastAFailedAppend(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		paths := state.For(t.TempDir(), provider+"-session")
		file, err := os.Create(filepath.Join(paths.Dir, provider+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		_ = file.Close() // every Write and Stat now fails
		var logged bytes.Buffer
		viewer := &client{outbox: make(chan clientWrite, 4), closed: make(chan struct{})}
		clients := map[*client]struct{}{viewer: {}}
		var appendRecord func(json.RawMessage)
		var history func() []json.RawMessage
		switch provider {
		case "codex":
			r := &codexAppRunner{paths: paths, historyFile: file, logger: log.New(&logged, "", 0), clients: clients, ctx: context.Background()}
			appendRecord, history = r.appendStructured, func() []json.RawMessage { return r.history }
		default:
			r := &claudeStructuredRunner{paths: paths, historyFile: file, logger: log.New(&logged, "", 0), clients: clients, ctx: context.Background()}
			appendRecord, history = r.appendStructured, func() []json.RawMessage { return r.history }
		}
		appendRecord(json.RawMessage(`{"kept":1}`))
		if !strings.Contains(logged.String(), "append structured") || !strings.Contains(logged.String(), errStructuredLineOpen.Error()) {
			t.Fatalf("%s: log %q", provider, logged.String())
		}
		if got := fmt.Sprintf("%s", history()); got != `[{"kept":1}]` {
			t.Fatalf("%s: memory %s", provider, got)
		}
		select {
		case write := <-viewer.outbox:
			if !bytes.Contains(write.frame, []byte(`{"kept":1}`)) {
				t.Fatalf("%s: broadcast frame %q", provider, write.frame)
			}
		default:
			t.Fatalf("%s: failed append was not broadcast", provider)
		}
	}
}

// The resume import still returns a failed append to its caller.
func TestCodexResumeImportStillReturnsAppendFailures(t *testing.T) {
	r := newCodexTestRunner(t)
	_ = r.historyFile.Close() // every Write and Stat now fails
	r.historyEnd = structuredLogEnd{}
	r.cfg.configDir = t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	directory := filepath.Join(r.cfg.configDir, "sessions", "2026", "09", "29")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	source := []byte(`{"type":"session_meta","payload":{"id":"` + id + `","cwd":"/tmp"}}
{"type":"response_item","timestamp":"2026-09-29T12:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Keep my original request"}]}}
{"type":"response_item","timestamp":"2026-09-29T12:00:01Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Original answer"}]}}
`)
	if err := os.WriteFile(filepath.Join(directory, "rollout-2026-09-29-"+id+".jsonl"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	err := r.restoreResumeHistory(id)
	if !errors.Is(err, errStructuredLineOpen) || !errors.Is(err, os.ErrClosed) || len(r.history) != 0 {
		t.Fatalf("resume import: err=%v history=%s", err, r.history)
	}
}
