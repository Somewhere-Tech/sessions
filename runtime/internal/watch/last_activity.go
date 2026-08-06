package watch

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"time"
)

// conversationTailBytes bounds the tail read that recovers a conversation's own
// last-activity timestamp. Provider records are small and one is written per
// turn, so the final record is comfortably inside this window; measured over
// the 203 Claude and Codex conversations on the development machine (1.4 GB in
// total, largest file 1.1 GB) a 64 KiB tail found a timestamp in 202 of them,
// at roughly half a millisecond per file including JSON decoding. The one miss
// is a 149-byte transcript holding a single bridge-session record that carries
// no timestamp at all, which is exactly the case the mtime fallback is for.
const conversationTailBytes = 64 << 10

// ConversationRecordedActivity returns the latest timestamp the conversation
// wrote into itself.
//
// File modification time cannot answer this. It is metadata about the file, not
// about the conversation, and any copy destroys it: `cp -R` without -p, an
// rsync without -t, a restore from backup, or moving history to a new machine
// all stamp every conversation with the moment of the copy. That was observed
// directly — a plain `cp -R` of this machine's history made all 203
// conversations report "22s ago", which is to say the primary ordering of the
// conversation browser became meaningless at the exact moment the product
// promise of interchangeable machines was being exercised.
//
// Both providers stamp their own records: Codex writes a top-level RFC3339
// `timestamp` on every rollout line, Claude writes one on every transcript
// record. That is the durable answer, and it travels with the bytes.
//
// The read is a single seek to the end of the file, so it costs the same on a
// gigabyte transcript as on a small one. The maximum over the window is taken
// rather than the last line: a torn final record from a power cut must not make
// a conversation look older than the record before it.
func ConversationRecordedActivity(path string) (time.Time, bool) {
	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return time.Time{}, false
	}

	offset := max(int64(0), info.Size()-conversationTailBytes)
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return time.Time{}, false
	}
	window, err := io.ReadAll(io.LimitReader(file, conversationTailBytes))
	if err != nil && len(window) == 0 {
		return time.Time{}, false
	}
	if offset > 0 {
		// The window opened mid-record. A partial JSON line is not a record and
		// must not be parsed as one.
		if newline := bytes.IndexByte(window, '\n'); newline >= 0 {
			window = window[newline+1:]
		} else {
			window = nil
		}
	}

	latest := time.Time{}
	for _, line := range bytes.Split(window, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record struct {
			Timestamp json.RawMessage `json:"timestamp"`
		}
		if json.Unmarshal(line, &record) != nil || len(record.Timestamp) == 0 {
			continue
		}
		stamp, ok := parseRecordTimestamp(record.Timestamp)
		if ok && stamp.After(latest) {
			latest = stamp
		}
	}
	return latest, !latest.IsZero()
}

// parseRecordTimestamp accepts the two spellings the providers use: an RFC3339
// string, and an epoch number in seconds or milliseconds. The magnitude split
// is the same one the transcript normalizer uses, so one record cannot be read
// as two different instants depending on which reader saw it.
func parseRecordTimestamp(raw json.RawMessage) (time.Time, bool) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if strings.TrimSpace(text) == "" {
			return time.Time{}, false
		}
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed, true
		}
		return time.Time{}, false
	}
	var number float64
	if json.Unmarshal(raw, &number) != nil || number <= 0 {
		return time.Time{}, false
	}
	if number < 100_000_000_000 {
		seconds := int64(number)
		return time.Unix(seconds, int64((number-float64(seconds))*float64(time.Second))), true
	}
	return time.UnixMilli(int64(number)), true
}
