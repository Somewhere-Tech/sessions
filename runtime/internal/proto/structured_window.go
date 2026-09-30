package proto

import "encoding/json"

// MaxStructuredReplayBytes bounds event payloads as well as event count. One
// oversized newest record is retained whole so the current event is never
// truncated. Transport/scanner limits bound that individual record separately.
const MaxStructuredReplayBytes = 4 * 1024 * 1024

// RetainStructuredHistory owns a copy of raw and releases evicted references.
// Only the in-memory replay window changes; the append-only disk log does not.
// removed lets consumers preserve their absolute replay cursor.
func RetainStructuredHistory(history []json.RawMessage, raw json.RawMessage) (retained []json.RawMessage, removed int) {
	history = append(history, append(json.RawMessage(nil), raw...))
	start, size := len(history)-1, len(raw)
	for start > 0 && len(history)-start < MaxStructuredReplayEvents {
		previous := len(history[start-1])
		if size+previous > MaxStructuredReplayBytes {
			break
		}
		size += previous
		start--
	}
	if start == 0 {
		return history, 0
	}
	length := copy(history, history[start:])
	clear(history[length:])
	return history[:length], start
}
