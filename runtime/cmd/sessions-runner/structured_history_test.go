package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestStructuredHistoryRestoresOnlyBoundedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for index := 0; index < structuredHistoryLimit+75; index++ {
		if _, err := fmt.Fprintf(file, "{\"index\":%d}\n", index); err != nil {
			t.Fatal(err)
		}
	}
	history, err := readStructuredHistoryTail(file)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != structuredHistoryLimit {
		t.Fatalf("restored history = %d, want %d", len(history), structuredHistoryLimit)
	}
	var first, last struct {
		Index int `json:"index"`
	}
	if err := json.Unmarshal(history[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(history[len(history)-1], &last); err != nil {
		t.Fatal(err)
	}
	if first.Index != 75 || last.Index != structuredHistoryLimit+74 {
		t.Fatalf("restored range = %d..%d", first.Index, last.Index)
	}
	if _, err := file.WriteString("{\"after\":true}\n"); err != nil {
		t.Fatalf("history file was not left append-ready: %v", err)
	}
}

func TestRetainStructuredEventDropsOnlyOldestMemoryEntry(t *testing.T) {
	history := make([]json.RawMessage, 0, structuredHistoryLimit)
	for index := 0; index <= structuredHistoryLimit; index++ {
		history = retainStructuredEvent(history, json.RawMessage(fmt.Sprintf("{\"index\":%d}", index)))
	}
	if len(history) != structuredHistoryLimit {
		t.Fatalf("retained history = %d, want %d", len(history), structuredHistoryLimit)
	}
	if string(history[0]) != "{\"index\":1}" || string(history[len(history)-1]) != fmt.Sprintf("{\"index\":%d}", structuredHistoryLimit) {
		t.Fatalf("unexpected retained bounds: first=%s last=%s", history[0], history[len(history)-1])
	}
}
