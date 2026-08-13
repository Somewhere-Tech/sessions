package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

const (
	structuredHistoryLimit = proto.MaxStructuredReplayEvents
	structuredReadBlock    = 64 * 1024
)

// readStructuredHistoryTail restores only the live replay window. The full
// append-only JSONL remains on disk for history/search/recovery; keeping every
// event resident made long-running provider sessions consume unbounded memory
// and made every daemon restart replay that memory a second time.
func readStructuredHistoryTail(file *os.File) ([]json.RawMessage, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	position := info.Size()
	chunks := make([][]byte, 0, 4)
	totalBytes := 0
	newlines := 0
	for position > 0 && newlines <= structuredHistoryLimit {
		readSize := int64(structuredReadBlock)
		if position < readSize {
			readSize = position
		}
		position -= readSize
		chunk := make([]byte, readSize)
		if _, err := file.ReadAt(chunk, position); err != nil && err != io.EOF {
			return nil, err
		}
		chunks = append(chunks, chunk)
		totalBytes += len(chunk)
		newlines += bytes.Count(chunk, []byte{'\n'})
		// One structured frame cannot exceed the runner protocol's scanner
		// allowance. Refuse a corrupt unterminated record without scanning an
		// arbitrarily large file into memory.
		if newlines == 0 && totalBytes > structuredScannerBuffer {
			return nil, fmt.Errorf("structured history record exceeds %d bytes", structuredScannerBuffer)
		}
	}
	window := make([]byte, totalBytes)
	offset := 0
	for index := len(chunks) - 1; index >= 0; index-- {
		offset += copy(window[offset:], chunks[index])
	}
	if position > 0 {
		if boundary := bytes.IndexByte(window, '\n'); boundary >= 0 {
			window = window[boundary+1:]
		}
	}
	lines := bytes.Split(window, []byte{'\n'})
	history := make([]json.RawMessage, 0, min(len(lines), structuredHistoryLimit))
	for _, candidate := range lines {
		line := bytes.TrimSpace(candidate)
		if len(line) == 0 || !json.Valid(line) {
			continue
		}
		history = append(history, append(json.RawMessage(nil), line...))
		if len(history) > structuredHistoryLimit {
			copy(history, history[len(history)-structuredHistoryLimit:])
			history = history[:structuredHistoryLimit]
		}
	}
	if _, err := file.Seek(0, io.SeekEnd); err != nil {
		return nil, err
	}
	return history, nil
}

func retainStructuredEvent(history []json.RawMessage, raw json.RawMessage) []json.RawMessage {
	cloned := append(json.RawMessage(nil), raw...)
	if len(history) < structuredHistoryLimit {
		return append(history, cloned)
	}
	copy(history, history[1:])
	history[len(history)-1] = cloned
	return history
}
