package watch

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Claude Code keeps an undocumented per-process registry at
// ~/.claude/sessions/<pid>.json. Every interactive Claude process writes one
// file there while it runs and refreshes it as its status changes. Verified on
// this machine against live files; an entry looks like:
//
//	{"pid":22440,"sessionId":"3fe0b590-...","cwd":"/Users/uzair/pretty-PTY",
//	 "startedAt":1785878763246,"procStart":"Tue Aug  4 21:26:02 2026",
//	 "version":"2.1.220","peerProtocol":1,"kind":"interactive","entrypoint":"cli",
//	 "name":"pretty-pty-02","nameSource":"derived","status":"waiting",
//	 "updatedAt":1785970422065,"statusUpdatedAt":1785970422065,
//	 "waitingFor":"permission prompt"}
//
// This is the only place Claude's liveness state exists. The project transcript
// says a conversation happened; it never says a process still has it open. That
// gap is why Sessions can currently adopt or resume a conversation the user has
// open in their own terminal, which is destructive: two writers append to one
// JSONL and the resulting interleave is not a conversation either process can
// resume from.
//
// Two rules govern everything in this file:
//
//   - It is READ ONLY. The directory belongs to Claude Code. Sessions never
//     creates, rewrites, or unlinks anything under it, including entries whose
//     process is plainly gone -- a stale entry is Claude's to clean up, and
//     deleting one races a process that is merely slow to refresh.
//   - A registry entry is a claim, not proof. The pid is checked for liveness
//     before the entry is believed, because a crashed Claude leaves its file
//     behind and an unchecked entry would lock the user out of their own
//     conversation forever.
const (
	// ClaudeLiveRegistryDirName is the registry directory under ~/.claude.
	ClaudeLiveRegistryDirName = "sessions"

	// claudeLiveEntryCap bounds how many registry files are parsed in one scan.
	// The real directory holds one file per live Claude process (three on this
	// machine); a directory far past that is corrupt or hostile, not a fleet.
	claudeLiveEntryCap = 4096
)

// Claude's own status vocabulary, observed in live files. Entries written by
// older versions may carry no status at all, which is why StatusUnknown exists
// rather than defaulting to idle.
const (
	ClaudeStatusBusy    = "busy"
	ClaudeStatusWaiting = "waiting"
	ClaudeStatusIdle    = "idle"
)

// ClaudeLiveSession is one parsed registry entry plus what Sessions determined
// about the process behind it. Fields mirror the observed JSON exactly; unknown
// keys are ignored so a Claude release that adds one does not break parsing.
type ClaudeLiveSession struct {
	PID               int    `json:"pid"`
	ProviderSessionID string `json:"sessionId"`
	CWD               string `json:"cwd"`
	StartedAt         int64  `json:"startedAt,omitempty"`
	ProcStart         string `json:"procStart,omitempty"`
	Version           string `json:"version,omitempty"`
	Kind              string `json:"kind,omitempty"`
	Entrypoint        string `json:"entrypoint,omitempty"`
	Name              string `json:"name,omitempty"`
	NameSource        string `json:"nameSource,omitempty"`
	Status            string `json:"status,omitempty"`
	WaitingFor        string `json:"waitingFor,omitempty"`
	UpdatedAt         int64  `json:"updatedAt,omitempty"`
	StatusUpdatedAt   int64  `json:"statusUpdatedAt,omitempty"`
	BridgeSessionID   string `json:"bridgeSessionId,omitempty"`

	// RegistryPath is the file this entry was read from. It is reported so a
	// human-facing message can point at the evidence rather than assert.
	RegistryPath string `json:"registryPath,omitempty"`

	// Alive is the result of probing PID. False means the entry is stale.
	Alive bool `json:"alive"`

	// Owned is true when PID is a Sessions runner or a descendant of one.
	// Sessions launches Claude as a child of its runner, so the registry pid is
	// never a runner pid itself; ownership is decided by ancestry.
	Owned bool `json:"owned"`
}

// Busy reports whether Claude last said it was mid-turn. A waiting entry is
// just as open as a busy one -- it is sitting on a permission prompt -- so this
// is for message wording, never for deciding whether to touch a conversation.
func (s ClaudeLiveSession) Busy() bool { return s.Status == ClaudeStatusBusy }

// ClaudeLiveReason is the structured verdict of an open-elsewhere check.
type ClaudeLiveReason string

const (
	// ClaudeLiveUnknown means the registry could not be consulted: no
	// ~/.claude/sessions directory, or it could not be read. Callers must not
	// read this as "safe" -- it is "no evidence either way", and a destructive
	// action should still confirm with the user.
	ClaudeLiveUnknown ClaudeLiveReason = "registry-unavailable"

	// ClaudeLiveNotOpen means the registry was readable and no live entry
	// claims this conversation. This is the only affirmative all-clear.
	ClaudeLiveNotOpen ClaudeLiveReason = "not-open"

	// ClaudeLiveExternal means a live process outside Sessions has this
	// conversation open. Adopting or resuming it would corrupt it.
	ClaudeLiveExternal ClaudeLiveReason = "open-external"

	// ClaudeLiveOwned means a live process Sessions already owns has it open.
	// Not a corruption risk, but not a fresh resume either: the caller should
	// attach to the existing session instead of starting a second one.
	ClaudeLiveOwned ClaudeLiveReason = "open-owned"

	// ClaudeLiveStale means the only entries claiming this conversation belong
	// to dead processes. Safe to proceed, and worth distinguishing from
	// not-open because it explains a leftover file the user may have seen.
	ClaudeLiveStale ClaudeLiveReason = "stale-entry"
)

// ClaudeLiveQuery configures a registry read. The zero value reads the real
// registry with real process probes; every field is a seam for tests.
type ClaudeLiveQuery struct {
	// Dir overrides ~/.claude/sessions.
	Dir string

	// OwnedPIDs are process IDs Sessions knows are its own runners. A registry
	// entry whose pid is one of these, or a descendant of one, is reported as
	// owned rather than external.
	OwnedPIDs []int

	// Alive overrides the liveness probe.
	Alive func(pid int) bool

	// Parents overrides the pid -> ppid snapshot used for ancestry.
	Parents func() map[int]int
}

func (q ClaudeLiveQuery) dir() (string, error) {
	if strings.TrimSpace(q.Dir) != "" {
		return q.Dir, nil
	}
	return ClaudeLiveRegistryDir()
}

func (q ClaudeLiveQuery) alive() func(int) bool {
	if q.Alive != nil {
		return q.Alive
	}
	return processAlive
}

func (q ClaudeLiveQuery) parents() func() map[int]int {
	if q.Parents != nil {
		return q.Parents
	}
	return processParents
}

// ClaudeOpenCheck is the answer to "is this conversation open somewhere else".
// It is deliberately structured rather than a bool: the difference between
// "no" and "cannot tell" decides whether a caller may proceed silently.
type ClaudeOpenCheck struct {
	ProviderSessionID string           `json:"providerSessionId"`
	Reason            ClaudeLiveReason `json:"reason"`

	// Open is true only for ClaudeLiveExternal and ClaudeLiveOwned.
	Open bool `json:"open"`

	// External is true only for ClaudeLiveExternal. This is the single field a
	// destructive action should gate on.
	External bool `json:"external"`

	// Holder is the live entry that owns the conversation, when there is one.
	Holder *ClaudeLiveSession `json:"holder,omitempty"`

	// Stale lists dead entries that claim the conversation. Reported, never
	// deleted.
	Stale []ClaudeLiveSession `json:"stale,omitempty"`

	// Err records why the registry could not be consulted. Always paired with
	// ClaudeLiveUnknown, and never with a nil-safe "false" that hides it.
	Err error `json:"-"`
}

// ClaudeLiveRegistryDir returns Claude Code's per-process registry directory.
// Resolved at call time so tests and scratch daemons can use a fixture HOME,
// matching ClaudeProjectsDir.
func ClaudeLiveRegistryDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", ClaudeLiveRegistryDirName), nil
}

// ReadClaudeLiveRegistry parses every entry in the registry and annotates each
// with liveness and ownership. Entries are returned in a stable order (live
// first, then by pid) so a caller rendering them produces a stable list.
//
// A missing directory is not an error the caller can act on, but it is also not
// an empty registry: it is returned as fs.ErrNotExist so a caller can tell
// "Claude is not installed / has never run" from "Claude is running nothing".
func ReadClaudeLiveRegistry(query ClaudeLiveQuery) ([]ClaudeLiveSession, error) {
	dir, err := query.dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	alive := query.alive()
	ownership := newClaudeOwnership(query.OwnedPIDs, query.parents())

	sessions := make([]ClaudeLiveSession, 0, len(entries))
	scanned := 0
	for _, entry := range entries {
		if scanned >= claudeLiveEntryCap {
			break
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		scanned++
		path := filepath.Join(dir, entry.Name())
		session, ok := readClaudeLiveEntry(path)
		if !ok {
			continue
		}
		session.Alive = alive(session.PID)
		if session.Alive {
			// Ownership is only meaningful for a live pid. Resolving ancestry
			// for a dead one walks a process table that has already forgotten
			// it and can only produce a wrong answer.
			session.Owned = ownership.owns(session.PID)
		}
		sessions = append(sessions, session)
	}

	sort.SliceStable(sessions, func(left, right int) bool {
		if sessions[left].Alive != sessions[right].Alive {
			return sessions[left].Alive
		}
		return sessions[left].PID < sessions[right].PID
	})
	return sessions, nil
}

// readClaudeLiveEntry parses one registry file. A file mid-write, truncated, or
// holding something other than an entry is skipped rather than failing the
// whole scan: one malformed file must not blind Sessions to the others.
func readClaudeLiveEntry(path string) (ClaudeLiveSession, bool) {
	payload, err := os.ReadFile(path)
	if err != nil {
		return ClaudeLiveSession{}, false
	}
	var session ClaudeLiveSession
	if json.Unmarshal(payload, &session) != nil {
		return ClaudeLiveSession{}, false
	}
	if session.PID <= 0 {
		// Fall back to the filename, which is the pid by construction. An entry
		// whose body lost its pid is still usable evidence of a live process.
		base := strings.TrimSuffix(filepath.Base(path), ".json")
		parsed, convErr := strconv.Atoi(base)
		if convErr != nil || parsed <= 0 {
			return ClaudeLiveSession{}, false
		}
		session.PID = parsed
	}
	if strings.TrimSpace(session.ProviderSessionID) == "" {
		// Without a conversation id the entry cannot answer the only question
		// this file is asked. Keeping it would add noise to every result.
		return ClaudeLiveSession{}, false
	}
	session.RegistryPath = path
	return session, true
}

// ClaudeConversationOpen reports whether one Claude conversation is currently
// held by a live process, and whether that process is Sessions' own.
//
// Callers must branch on Reason, not on Open alone. ClaudeLiveUnknown means the
// registry could not be consulted at all, and treating that as "not open" is
// exactly the silent overwrite this check exists to prevent.
func ClaudeConversationOpen(providerSessionID string, query ClaudeLiveQuery) ClaudeOpenCheck {
	check := ClaudeOpenCheck{ProviderSessionID: providerSessionID}
	if strings.TrimSpace(providerSessionID) == "" {
		// Nothing to match on. Callers reach here for a session whose provider
		// identity was never recorded; that is genuinely unknowable, not clear.
		check.Reason = ClaudeLiveUnknown
		check.Err = errors.New("claude live registry: no provider session id to match")
		return check
	}

	sessions, err := ReadClaudeLiveRegistry(query)
	if err != nil {
		check.Reason = ClaudeLiveUnknown
		if errors.Is(err, fs.ErrNotExist) {
			// Claude has never run under this HOME, or the running version does
			// not keep a registry. Both are "cannot tell", not "clear".
			check.Err = err
			return check
		}
		check.Err = err
		return check
	}

	for _, session := range sessions {
		if !strings.EqualFold(session.ProviderSessionID, providerSessionID) {
			continue
		}
		if !session.Alive {
			check.Stale = append(check.Stale, session)
			continue
		}
		if check.Holder != nil && check.Holder.Owned && !session.Owned {
			// An external holder outranks an owned one: it is the dangerous
			// case, and reporting the owned process would understate the risk.
			holder := session
			check.Holder = &holder
			continue
		}
		if check.Holder == nil {
			holder := session
			check.Holder = &holder
		}
	}

	switch {
	case check.Holder != nil && !check.Holder.Owned:
		check.Reason = ClaudeLiveExternal
		check.Open = true
		check.External = true
	case check.Holder != nil:
		check.Reason = ClaudeLiveOwned
		check.Open = true
	case len(check.Stale) > 0:
		check.Reason = ClaudeLiveStale
	default:
		check.Reason = ClaudeLiveNotOpen
	}
	return check
}

// ClaudeConversationsOpenByCWD returns live entries whose working directory
// matches cwd, after the same alias resolution the project-bucket encoder uses.
// It answers the adjacent question -- "is anything already running here" --
// which recovery needs when a session has no recorded provider id at all.
func ClaudeConversationsOpenByCWD(cwd string, query ClaudeLiveQuery) []ClaudeLiveSession {
	target := normalizeCWD(cwd)
	if target == "" {
		return nil
	}
	sessions, err := ReadClaudeLiveRegistry(query)
	if err != nil {
		return nil
	}
	matches := make([]ClaudeLiveSession, 0, 2)
	for _, session := range sessions {
		if !session.Alive {
			continue
		}
		if normalizeCWD(session.CWD) == target {
			matches = append(matches, session)
		}
	}
	return matches
}

// claudeOwnership resolves "is this pid one of ours" over a single lazily taken
// process-table snapshot. Lazy because the common case -- no owned pids given,
// or the registry entry's pid is already an owned pid -- needs no snapshot at
// all, and taking one costs a subprocess.
type claudeOwnership struct {
	owned   map[int]struct{}
	parents func() map[int]int

	once     sync.Once
	resolved map[int]int
}

func newClaudeOwnership(pids []int, parents func() map[int]int) *claudeOwnership {
	owned := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if pid > 0 {
			owned[pid] = struct{}{}
		}
	}
	return &claudeOwnership{owned: owned, parents: parents}
}

func (o *claudeOwnership) owns(pid int) bool {
	if len(o.owned) == 0 || pid <= 0 {
		return false
	}
	if _, direct := o.owned[pid]; direct {
		return true
	}
	o.once.Do(func() {
		if o.parents != nil {
			o.resolved = o.parents()
		}
	})
	if len(o.resolved) == 0 {
		return false
	}
	// Walk to the root, bounded by the table size so a cyclic or corrupt
	// snapshot cannot spin. Claude under a Sessions runner is one or two hops.
	current := pid
	for step := 0; step < len(o.resolved)+1; step++ {
		parent, ok := o.resolved[current]
		if !ok || parent <= 0 || parent == current {
			return false
		}
		if _, hit := o.owned[parent]; hit {
			return true
		}
		current = parent
	}
	return false
}
