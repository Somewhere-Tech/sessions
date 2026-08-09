package watch

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A Sessions session whose conversation lives only in the provider's file has
// no record of its own. Claude Code deletes project transcripts on a retention
// timer, a renamed working directory orphans the bucket it wrote into, and two
// different working directories can encode to the same bucket and make the
// resolver refuse to choose. In every one of those cases the process survived
// and the conversation did not.
//
// A TranscriptMirror is Sessions' own durable, append-only copy of the provider
// transcript for one session. Two properties make it useful rather than merely
// redundant:
//
//   - It stores provider lines byte for byte, in observed order. The mirror is
//     therefore itself a legal Claude transcript: every existing reader (raw
//     source, search extraction, usage accounting) works on it by substituting
//     the path, with no format branch anywhere.
//   - It only ever appends. The provider may truncate, compact, or replace its
//     file underneath the watcher; the mirror keeps the union of everything it
//     has ever observed, deduplicated by the provider's own record identity.
//
// The mirror is a fallback, never an addition. Exactly one transcript path is
// ever resolved for a session, so nothing downstream can count a conversation
// twice. See ResolveClaudeWithMirror.
const (
	// TranscriptMirrorSuffix and TranscriptMirrorMetaSuffix name the mirror and
	// its sidecar inside the runner state directory, alongside <id>.events.
	TranscriptMirrorSuffix     = ".transcript.jsonl"
	TranscriptMirrorMetaSuffix = ".transcript.meta.json"

	// transcriptMirrorVersion guards the sidecar shape.
	transcriptMirrorVersion = 1

	// DefaultTranscriptMirrorCapBytes bounds one session's mirror. The earlier
	// 512 MiB default was smaller than a 1.15 GiB rollout observed in ordinary
	// dogfooding, so a healthy backfill could stop before it had preserved the
	// conversation. Four GiB keeps a deliberate per-session disk guard while
	// leaving substantial headroom above the largest real provider history we
	// have measured. Reaching it remains explicit and visible in mirror health.
	DefaultTranscriptMirrorCapBytes int64 = 4 << 30

	transcriptScanLineCap = 64 << 20
)

// TranscriptMirrorMeta is the sidecar record describing one mirror. It carries
// the provenance a bare JSONL copy cannot: which provider file was observed,
// which provider conversation it is, and how many times that file was replaced
// underneath the watcher.
type TranscriptMirrorMeta struct {
	Version           int    `json:"version"`
	SessionID         string `json:"sessionId,omitempty"`
	ProviderSessionID string `json:"providerSessionId,omitempty"`
	ProviderPath      string `json:"providerPath,omitempty"`
	Tool              string `json:"tool,omitempty"`

	Records int   `json:"records"`
	Bytes   int64 `json:"bytes"`

	FirstObservedAt int64 `json:"firstObservedAt,omitempty"`
	LastObservedAt  int64 `json:"lastObservedAt,omitempty"`

	// Generations counts observed provider-file replacements or truncations.
	// A non-zero value is the durable evidence that the provider rewrote the
	// conversation and that the mirror, not the provider file, holds the union.
	Generations int `json:"generations"`

	// Capped reports that CapBytes was reached and appends stopped.
	Capped   bool  `json:"capped,omitempty"`
	CapBytes int64 `json:"capBytes,omitempty"`

	// WriteErrors counts append failures. A mirror that cannot be written is a
	// silent data-loss bug, so the count is durable rather than log-only.
	WriteErrors int    `json:"writeErrors,omitempty"`
	LastError   string `json:"lastError,omitempty"`
}

// TranscriptMirrorOptions configures a mirror. Path is required; everything
// else is provenance recorded into the sidecar.
type TranscriptMirrorOptions struct {
	Path              string
	SessionID         string
	ProviderSessionID string
	Tool              string
	CapBytes          int64
}

// TranscriptMirror is safe for concurrent use. The Claude tail writes from one
// goroutine, but Meta and Close are reachable from watcher shutdown.
type TranscriptMirror struct {
	mu       sync.Mutex
	path     string
	metaPath string
	file     *os.File
	seen     map[string]struct{}
	pass     map[string]int
	meta     TranscriptMirrorMeta
	capBytes int64
	dirty    bool
	closed   bool
}

// TranscriptMirrorPath is the mirror location for a session inside the runner
// state directory. It sits beside <id>.events so a session's durable terminal
// output and its durable conversation share one lifetime and one backup unit.
func TranscriptMirrorPath(stateDir, sessionID string) string {
	if stateDir == "" || sessionID == "" {
		return ""
	}
	return filepath.Join(stateDir, sessionID+TranscriptMirrorSuffix)
}

// TranscriptMirrorMetaPath is the sidecar path for a mirror.
func TranscriptMirrorMetaPath(mirrorPath string) string {
	if mirrorPath == "" {
		return ""
	}
	return strings.TrimSuffix(mirrorPath, TranscriptMirrorSuffix) + TranscriptMirrorMetaSuffix
}

// OpenTranscriptMirror opens or resumes a mirror. Resuming rebuilds the record
// identity set from the mirror itself, which is what makes appending idempotent
// across daemon restarts: the watcher re-reads the provider file from offset
// zero on every attach, and every record it has already stored is recognized.
func OpenTranscriptMirror(options TranscriptMirrorOptions) (*TranscriptMirror, error) {
	if strings.TrimSpace(options.Path) == "" {
		return nil, errors.New("transcript mirror: path required")
	}
	if options.CapBytes <= 0 {
		options.CapBytes = DefaultTranscriptMirrorCapBytes
	}
	if err := os.MkdirAll(filepath.Dir(options.Path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(options.Path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	// A conversation is at least as sensitive as the structured runner history,
	// which is also 0600. Re-apply on every open so a pre-existing file created
	// under a looser umask is corrected.
	_ = os.Chmod(options.Path, 0o600)

	mirror := &TranscriptMirror{
		path:     options.Path,
		metaPath: TranscriptMirrorMetaPath(options.Path),
		file:     file,
		seen:     make(map[string]struct{}),
		pass:     make(map[string]int),
		capBytes: options.CapBytes,
	}
	mirror.meta = loadTranscriptMirrorMeta(mirror.metaPath)
	mirror.meta.Version = transcriptMirrorVersion
	mirror.meta.CapBytes = options.CapBytes
	if options.SessionID != "" {
		mirror.meta.SessionID = options.SessionID
	}
	if options.ProviderSessionID != "" {
		mirror.meta.ProviderSessionID = options.ProviderSessionID
	}
	if options.Tool != "" {
		mirror.meta.Tool = options.Tool
	}

	records, size, err := scanTranscriptMirror(options.Path, mirror.seen)
	if err != nil {
		file.Close()
		return nil, err
	}
	// The mirror file is authoritative over a sidecar that a crash may have left
	// behind mid-write.
	mirror.meta.Records = records
	mirror.meta.Bytes = size
	mirror.meta.Capped = size >= options.CapBytes
	mirror.dirty = true
	return mirror, nil
}

// scanTranscriptMirror rebuilds the record-identity set from an existing
// mirror and returns its record count and byte size.
func scanTranscriptMirror(path string, into map[string]struct{}) (int, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	defer file.Close()

	records := 0
	var size int64
	// Ordinals are rebuilt in stored order, which is the same order a replay of
	// the provider file presents them in. That is what makes a duplicate-bearing
	// mirror reopen to exactly the state it was written with.
	ordinals := make(map[string]int)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), transcriptScanLineCap)
	for scanner.Scan() {
		line := scanner.Bytes()
		size += int64(len(line)) + 1
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		if into != nil {
			into[transcriptRecordIdentity(line, ordinals)] = struct{}{}
		}
		records++
	}
	if err := scanner.Err(); err != nil {
		// A partially written trailing line is recoverable: everything scanned
		// so far is still known, and Append will simply re-store the remainder.
		return records, size, nil
	}
	return records, size, nil
}

// TranscriptRecordKey is the content identity of one transcript record. Claude
// stamps every conversation record with a uuid, which is stable across the
// provider rewriting or recreating its file. Records without one fall back to a
// content hash.
//
// A content hash alone is NOT a record identity, because a transcript legally
// contains the same bytes more than once. Use transcriptRecordIdentity for
// storage decisions; this function is the shared first half of it.
func TranscriptRecordKey(line []byte) string {
	var probe struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(line, &probe) == nil && probe.UUID != "" {
		return "u:" + probe.UUID
	}
	sum := sha256.Sum256(line)
	return "h:" + hex.EncodeToString(sum[:])
}

// transcriptRecordIdentity is the identity a mirror stores against. It is a
// multiset identity, not a set identity, and that distinction is load bearing.
//
// A uuid is a true unique identity: Claude issues one per conversation record,
// and two lines carrying the same uuid are the same record seen twice. Those
// deduplicate globally.
//
// Everything else is content-hashed, and a content hash repeats legitimately.
// Real Claude transcripts on this machine repeat non-uuid lines heavily: of
// 6607 uuid-less records, 3879 are byte-identical to an earlier one -- almost
// entirely the "mode", "permission-mode", "agent-name" and "custom-title"
// state records. Those are exactly the records native `claude --resume` reads
// back to restore model, permission mode, and agent, and they are last-one-wins.
// Collapsing them to their first occurrence does not merely shrink the file; it
// silently rewinds the restored state to an earlier value. A conversation that
// ended in bypassPermissions would resume in normal mode.
//
// Codex rollouts have no uuid field at all, so every one of their records is
// content-hashed and this is the only thing keeping a Codex mirror lossless.
//
// The ordinal is the count of this content within the current replay pass, so a
// record that genuinely appears three times is stored three times, while a
// re-read of the same prefix produces the same ordinals and stores nothing new.
func transcriptRecordIdentity(line []byte, ordinals map[string]int) string {
	key := TranscriptRecordKey(line)
	if strings.HasPrefix(key, "u:") {
		return key
	}
	ordinals[key]++
	return key + "#" + strconv.Itoa(ordinals[key])
}

// Append stores one provider line verbatim if it has not been stored before.
// It reports whether the line was newly written. The line must not contain a
// newline; the caller splits records.
func (m *TranscriptMirror) Append(line []byte) (bool, error) {
	if m == nil {
		return false, nil
	}
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.file == nil {
		return false, os.ErrClosed
	}
	key := transcriptRecordIdentity(line, m.pass)
	if _, duplicate := m.seen[key]; duplicate {
		return false, nil
	}
	if m.meta.Bytes+int64(len(line))+1 > m.capBytes {
		if !m.meta.Capped {
			m.meta.Capped = true
			m.dirty = true
			m.writeMetaLocked()
		}
		return false, nil
	}

	record := make([]byte, 0, len(line)+1)
	record = append(record, line...)
	record = append(record, '\n')
	written, err := m.file.Write(record)
	if err != nil {
		m.meta.WriteErrors++
		m.meta.LastError = err.Error()
		m.dirty = true
		m.writeMetaLocked()
		return false, err
	}

	m.seen[key] = struct{}{}
	m.meta.Records++
	m.meta.Bytes += int64(written)
	now := time.Now().UnixMilli()
	if m.meta.FirstObservedAt == 0 {
		m.meta.FirstObservedAt = now
	}
	m.meta.LastObservedAt = now
	m.dirty = true
	return true, nil
}

// BeginPass declares that the caller is about to replay a provider transcript
// from its first record again. It resets the per-pass occurrence counters that
// give repeated content its ordinal.
//
// Every reader that restarts at byte zero must call this first. Without it a
// second pass would number the first duplicate line as occurrence N+1 rather
// than 1 and append a copy of the whole conversation's repeated records on
// every reattach. With it, a repeated pass over unchanged content stores
// nothing, which is the property the watcher, the daemon restart path, and
// repeated backfills all depend on.
func (m *TranscriptMirror) BeginPass() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pass = make(map[string]int, len(m.pass))
}

// NoteProviderPath records which provider file the mirror is copying. It is
// the only durable link back to the bucket the conversation came from, and it
// survives that bucket being pruned or orphaned.
func (m *TranscriptMirror) NoteProviderPath(path string) {
	if m == nil || path == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.meta.ProviderPath == path {
		return
	}
	m.meta.ProviderPath = path
	if m.meta.ProviderSessionID == "" {
		base := filepath.Base(path)
		m.meta.ProviderSessionID = strings.TrimSuffix(base, ".jsonl")
	}
	m.dirty = true
	m.writeMetaLocked()
}

// NoteRotation records that the provider replaced or truncated its file. The
// mirror needs no other reaction: the watcher re-reads from offset zero and
// Append deduplicates, so the mirror ends up holding the union of the old and
// new content rather than whichever version the provider kept.
func (m *TranscriptMirror) NoteRotation() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.meta.Generations++
	m.dirty = true
	m.writeMetaLocked()
}

// Meta returns a copy of the current sidecar record.
func (m *TranscriptMirror) Meta() TranscriptMirrorMeta {
	if m == nil {
		return TranscriptMirrorMeta{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.meta
}

// Path returns the mirror file path.
func (m *TranscriptMirror) Path() string {
	if m == nil {
		return ""
	}
	return m.path
}

// Sync flushes the mirror and its sidecar to disk.
func (m *TranscriptMirror) Sync() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.syncLocked()
}

func (m *TranscriptMirror) syncLocked() error {
	if m.file == nil || m.closed {
		return nil
	}
	err := m.file.Sync()
	m.writeMetaLocked()
	return err
}

// Close flushes and releases the mirror. Unlike the structured runner history,
// a mirror is never unlinked on exit: the whole point is that it outlives the
// session, the runner, and the provider's own copy.
func (m *TranscriptMirror) Close() error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil
	}
	_ = m.syncLocked()
	m.closed = true
	file := m.file
	m.file = nil
	if file == nil {
		return nil
	}
	return file.Close()
}

func (m *TranscriptMirror) writeMetaLocked() {
	if !m.dirty || m.metaPath == "" {
		return
	}
	payload, err := json.Marshal(m.meta)
	if err != nil {
		return
	}
	payload = append(payload, '\n')
	temporary := m.metaPath + ".tmp"
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		return
	}
	if err := os.Rename(temporary, m.metaPath); err != nil {
		_ = os.Remove(temporary)
		return
	}
	m.dirty = false
}

func loadTranscriptMirrorMeta(path string) TranscriptMirrorMeta {
	if path == "" {
		return TranscriptMirrorMeta{Version: transcriptMirrorVersion}
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return TranscriptMirrorMeta{Version: transcriptMirrorVersion}
	}
	var meta TranscriptMirrorMeta
	if json.Unmarshal(payload, &meta) != nil {
		return TranscriptMirrorMeta{Version: transcriptMirrorVersion}
	}
	return meta
}

// ReadTranscriptMirrorMeta reads a mirror sidecar without opening the mirror.
func ReadTranscriptMirrorMeta(mirrorPath string) (TranscriptMirrorMeta, bool) {
	metaPath := TranscriptMirrorMetaPath(mirrorPath)
	if metaPath == "" {
		return TranscriptMirrorMeta{}, false
	}
	payload, err := os.ReadFile(metaPath)
	if err != nil {
		return TranscriptMirrorMeta{}, false
	}
	var meta TranscriptMirrorMeta
	if json.Unmarshal(payload, &meta) != nil {
		return TranscriptMirrorMeta{}, false
	}
	return meta, true
}

// TranscriptMirrorHealth is the read side of the health the sidecar already
// records. The mirror durably knows when it stopped accepting appends and when
// an append failed, but until something reads those fields back a mirror that
// stopped recording presents itself exactly like one that did not -- which is
// the single failure the mirror exists to prevent.
//
// Known is the field that keeps this honest. A mirror whose sidecar is absent
// or undecodable has unknown health, and unknown is not unhealthy: mirrors
// written by an earlier build have no sidecar, and a sidecar lost to a crash
// says nothing about the conversation stored beside it. Reporting those as
// damaged would teach users to ignore the warning that matters.
type TranscriptMirrorHealth struct {
	// Known reports that a sidecar was found and decoded. Every other field is
	// meaningless when it is false.
	Known bool

	// Capped reports that the mirror reached its size cap and stopped storing
	// records. Everything the provider produced after that point was never
	// copied.
	Capped   bool
	CapBytes int64

	// WriteErrors counts appends that failed. Each one is a provider record
	// the mirror does not hold.
	WriteErrors int
	LastError   string

	Records     int
	Generations int
}

// Degraded reports that the mirror is known to be missing conversation it was
// asked to store. It is deliberately false for unknown health.
func (h TranscriptMirrorHealth) Degraded() bool {
	return h.Known && (h.Capped || h.WriteErrors > 0)
}

// Detail states what went wrong, in the plain terms a user needs to judge
// whether the copy they are looking at is the whole conversation. Callers add
// the next action, which differs by where they are read.
func (h TranscriptMirrorHealth) Detail() string {
	switch {
	case !h.Known:
		return ""
	case h.Capped && h.WriteErrors > 0:
		return fmt.Sprintf(
			"Sessions' copy reached its %s size limit and stopped storing records, and %s also failed; records after that point were never copied (last error: %s)",
			transcriptMirrorCapText(h.CapBytes), pluralWrites(h.WriteErrors), h.LastError)
	case h.Capped:
		return fmt.Sprintf(
			"Sessions' copy reached its %s size limit and stopped storing records; everything the provider wrote after that point was never copied",
			transcriptMirrorCapText(h.CapBytes))
	case h.WriteErrors > 0:
		return fmt.Sprintf(
			"%s to Sessions' copy failed, so that many provider records are missing from it (last error: %s)",
			pluralWrites(h.WriteErrors), h.LastError)
	}
	return ""
}

func pluralWrites(count int) string {
	if count == 1 {
		return "1 write"
	}
	return fmt.Sprintf("%d writes", count)
}

// transcriptMirrorCapText renders the cap the way the user would recognize it.
// An unrecorded cap is possible in a sidecar written before CapBytes was
// persisted, and inventing "0 MB" there would read as a bug in Sessions.
func transcriptMirrorCapText(capBytes int64) string {
	if capBytes <= 0 {
		return "configured"
	}
	const mebibyte = 1 << 20
	if capBytes >= mebibyte {
		return fmt.Sprintf("%d MB", capBytes/mebibyte)
	}
	return fmt.Sprintf("%d bytes", capBytes)
}

// ReadTranscriptMirrorHealth reads a mirror's recorded health without opening
// the mirror. A missing or undecodable sidecar yields unknown health rather
// than an error, because the caller's question is always "can I trust this
// copy" and "I cannot tell" is a real, distinct answer to it.
func ReadTranscriptMirrorHealth(mirrorPath string) TranscriptMirrorHealth {
	meta, ok := ReadTranscriptMirrorMeta(mirrorPath)
	if !ok {
		return TranscriptMirrorHealth{}
	}
	return TranscriptMirrorHealth{
		Known:       true,
		Capped:      meta.Capped,
		CapBytes:    meta.CapBytes,
		WriteErrors: meta.WriteErrors,
		LastError:   meta.LastError,
		Records:     meta.Records,
		Generations: meta.Generations,
	}
}

// TranscriptMirrorUsable reports whether a mirror holds recorded conversation.
// An empty or missing mirror is not a usable fallback, and offering one would
// turn "the provider deleted it" into a more confusing "Sessions has it, but
// it is blank".
//
// It deliberately says nothing about health: a capped mirror is still the best
// copy of the conversation that exists, and refusing to serve it would destroy
// what it did manage to keep. Callers that present a mirror to a person should
// pair this with ReadTranscriptMirrorHealth and say so.
func TranscriptMirrorUsable(mirrorPath string) bool {
	if mirrorPath == "" {
		return false
	}
	info, err := os.Stat(mirrorPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return false
	}
	return true
}

// ClaudeMirror is the resolution reason for a conversation served from
// Sessions' own copy because the provider's file could not be resolved.
const ClaudeMirror ClaudeResolveReason = "sessions-mirror"

// ClaudeSidecarPath resolves the provider file from the exact path the mirror
// recorded observing. See ResolveClaudeWithMirror.
const ClaudeSidecarPath ClaudeResolveReason = "sidecar-provider-path"

// ClaudeSidecarID resolves the provider file from the provider conversation id
// the mirror recorded, used as an exact filename match.
const ClaudeSidecarID ClaudeResolveReason = "sidecar-provider-id"

// ResolveClaudeWithMirror is the durable form of ResolveClaudeCWD. It prefers
// the provider file whenever the provider file can still be identified, so the
// live conversation and native `claude --resume` stay authoritative and nothing
// is ever served from two places at once. Only when the provider resolution
// yields no path at all does it fall back to Sessions' own copy.
//
// That ordering is what keeps search and usage honest: a session still resolves
// to exactly one transcript path, so there is no second copy to double-count.
// Before falling back it spends the evidence the mirror already recorded. The
// sidecar keeps the exact provider path the watcher observed and the provider
// conversation id it belonged to, both written from direct observation rather
// than inference. Either one identifies a single file by name, which clears an
// ambiguous bucket at the same bar `sessions transcripts` uses: an exact
// provider-id match, never a guess. Without this, a session whose bucket is
// shared by another working directory serves its conversation from the mirror
// while the live provider file sits unread beside it -- and a mirror is a
// snapshot, so a still-running session would stop updating.
func ResolveClaudeWithMirror(projects, cwd, launchUUID, mirrorPath string) ClaudeResolution {
	resolution := ResolveClaudeCWD(projects, cwd, launchUUID)
	if resolution.Path != "" {
		return resolution
	}

	if meta, ok := ReadTranscriptMirrorMeta(mirrorPath); ok {
		if path := meta.ProviderPath; path != "" && claudeTranscriptPresent(path) {
			return ClaudeResolution{Path: path, Reason: ClaudeSidecarPath}
		}
		// The recorded path can be stale -- the bucket was renamed, or the file
		// moved between machines -- while the conversation id stays valid. Retry
		// resolution with it as the launch UUID, which only ever matches
		// "<id>.jsonl" exactly.
		if id := meta.ProviderSessionID; id != "" && id != launchUUID {
			if retry := ResolveClaudeCWD(projects, cwd, id); retry.Reason == ClaudeExact {
				return ClaudeResolution{Path: retry.Path, Reason: ClaudeSidecarID}
			}
		}
	}

	if TranscriptMirrorUsable(mirrorPath) {
		return ClaudeResolution{Path: mirrorPath, Reason: ClaudeMirror}
	}
	return resolution
}

// claudeTranscriptPresent reports whether a recorded provider path still names
// a readable, non-empty regular file. An empty one is treated as absent for the
// same reason TranscriptMirrorUsable rejects an empty mirror: resolving to a
// blank file is more confusing than resolving to nothing.
func claudeTranscriptPresent(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// TranscriptMirrorRecords reads a mirror back as parsed events. Readers that
// want the raw bytes should read the file directly: it is provider-shaped
// JSONL and needs no translation.
func TranscriptMirrorRecords(mirrorPath string) ([]SessionEvent, error) {
	file, err := os.Open(mirrorPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	events := make([]SessionEvent, 0, 64)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), transcriptScanLineCap)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var event SessionEvent
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return events, fmt.Errorf("read transcript mirror %s: %w", mirrorPath, err)
	}
	return events, nil
}
