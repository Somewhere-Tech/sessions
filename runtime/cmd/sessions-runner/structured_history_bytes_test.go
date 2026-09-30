package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
)

func TestStructuredHistoryTailBoundsLargeEventsWithoutChangingDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large-history.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	payload := strings.Repeat("x", 3*proto.MaxStructuredReplayBytes/8)
	for index := 0; index < 5; index++ {
		if _, err := fmt.Fprintf(file, "{\"index\":%d,\"payload\":%q}\n", index, payload); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	history, err := readStructuredHistoryTail(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("restored history = %d records, want newest 2", len(history))
	}
	retainedBytes := len(history[0]) + len(history[1])
	if retainedBytes > proto.MaxStructuredReplayBytes {
		t.Fatalf("restored payload = %d bytes, limit = %d", retainedBytes, proto.MaxStructuredReplayBytes)
	}
	for offset, want := range []int{3, 4} {
		var event struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(history[offset], &event); err != nil {
			t.Fatal(err)
		}
		if event.Index != want {
			t.Fatalf("history[%d].index = %d, want %d", offset, event.Index, want)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("reading the bounded tail changed append-only history content")
	}
	if info, err := file.Stat(); err != nil {
		t.Fatal(err)
	} else if info.Size() != int64(len(before)) {
		t.Fatalf("history size = %d after read, want %d", info.Size(), len(before))
	}
}

func TestStructuredHistoryTailKeepsNewestOversizedRecordWhole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oversized-history.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if _, err := file.WriteString("{\"index\":0,\"payload\":\"old\"}\n"); err != nil {
		t.Fatal(err)
	}
	oversized := fmt.Sprintf("{\"index\":1,\"payload\":%q}", strings.Repeat("y", proto.MaxStructuredReplayBytes+1024))
	if _, err := file.WriteString(oversized + "\n"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	history, err := readStructuredHistoryTail(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 {
		t.Fatalf("restored history = %d records, want only newest oversized record", len(history))
	}
	if string(history[0]) != oversized {
		t.Fatalf("newest oversized record was not retained whole: got %d bytes, want %d", len(history[0]), len(oversized))
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("reading the oversized tail changed append-only history content")
	}
}
