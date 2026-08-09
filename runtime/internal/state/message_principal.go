package state

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MessagePrincipal names who put text into a session through Sessions' own
// input path. It is transport provenance, not a guess about content: the
// caller that already knows whether the write carried source-session
// attribution decides it, and nothing downstream re-derives it.
//
// The reason a principal can be known at all is that Sessions is not the only
// writer of a provider conversation. A provider injects its own scheduled and
// internal prompts directly into its transcript, where they are recorded as
// user turns and are indistinguishable from a person's. Those never reach this
// package's input path. Everything sent by a person or by another session
// does, so the input boundary is the one place where the answer is a fact
// rather than an inference.
type MessagePrincipal string

const (
	// PrincipalHuman is input that arrived through Sessions carrying no
	// source-session attribution: a person at a keyboard, a composer, or
	// `sessions send` run by hand.
	PrincipalHuman MessagePrincipal = "human"
	// PrincipalAgent is input that arrived through Sessions carrying a source
	// session, meaning one session relayed it to another.
	PrincipalAgent MessagePrincipal = "agent"
)

// messagePrincipalPersistInterval bounds how often a stamp is written to disk.
//
// Every keystroke of an attached terminal is an input, and these fields are
// read as an age -- "last human contact 3h ago" -- so a metadata write per
// keystroke would buy precision nobody can see at a cost everybody pays. A
// restart can lose up to this much of the last stamp's precision; it cannot
// lose the fact that a human spoke, because the first message after any quiet
// period is always further than this from the value already on disk.
const messagePrincipalPersistInterval = 30 * time.Second

// carriesMessageText separates a message from the keystroke that submits one.
//
// This matters for correctness, not tidiness. An attributed submit delivers the
// agent's text with attribution and then sends a bare "\r" without it, because
// one provider turn must produce exactly one relay fact. Treating that Enter as
// unattributed input would stamp the agent's own message as human contact and
// recreate, at the input boundary, the exact confusion these fields exist to
// remove. Whitespace-only payloads therefore stamp nothing; the text they
// submit already did.
func carriesMessageText(data string) bool { return strings.TrimSpace(data) != "" }

// pointerMillis reads an optional timestamp as a plain instant, where absent
// means "never".
func pointerMillis(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// noteMessagePrincipal advances one principal clock and reports whether the new
// value is far enough from what is on disk to be worth persisting. Clocks only
// move forward, so an out-of-order or duplicated stamp cannot walk one back.
func (s *Session) noteMessagePrincipal(principal MessagePrincipal, at int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp, persisted := &s.info.LastHumanMessageAt, &s.persistedHumanAt
	if principal == PrincipalAgent {
		stamp, persisted = &s.info.LastAgentMessageAt, &s.persistedAgentAt
	}
	if *stamp != nil && **stamp >= at {
		return false
	}
	value := at
	*stamp = &value
	if at-*persisted < messagePrincipalPersistInterval.Milliseconds() {
		return false
	}
	*persisted = at
	return true
}

// messagePrincipals reads both clocks for the writer below.
func (s *Session) messagePrincipals() (human, agent *int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneInt64Pointer(s.info.LastHumanMessageAt), cloneInt64Pointer(s.info.LastAgentMessageAt)
}

// RecordInputPrincipal stamps who just sent one payload into a live session.
//
// It is called from the session layer immediately after the runner accepted the
// bytes, so every surface -- CLI, native app, WebSocket mux, attached terminal
// -- is covered by construction rather than by each of them remembering to
// report itself.
func (r *Registry) RecordInputPrincipal(id string, principal MessagePrincipal, data string) {
	if !carriesMessageText(data) {
		return
	}
	session, ok := r.Get(id)
	if !ok {
		return
	}
	if !session.noteMessagePrincipal(principal, time.Now().UnixMilli()) {
		return
	}
	if err := r.persistMessagePrincipals(id, session); err != nil {
		// A stamp that cannot be persisted is still true for this daemon's
		// lifetime, and refusing the message over it would be a far larger
		// failure than an age that resets on restart.
		log.Printf("[input] persist message principal for %s: %v", id, err)
	}
}

// persistMessagePrincipals writes the two clocks into the runner metadata
// document under the cross-process metadata lock shared with runner writes.
func (r *Registry) persistMessagePrincipals(id string, session *Session) error {
	if !validMetadataID(id) || r.config.RunnerStateDir == "" {
		return nil
	}
	path := filepath.Join(r.config.RunnerStateDir, id+".json")
	_, err := updateMetadata(path, func(metadata *Metadata) error {
		metadata.LastHumanMessageAt, metadata.LastAgentMessageAt = session.messagePrincipals()
		return nil
	})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("persist session message principals: %w", err)
	}
	return nil
}
