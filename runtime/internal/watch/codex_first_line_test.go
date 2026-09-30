package watch

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCodexFirstLinePreservesBoundedReaderSemantics(t *testing.T) {
	for _, test := range []struct {
		name, content, want string
	}{
		{"empty", "", ""},
		{"first only", "first\nsecond\n", "first"},
		{"whitespace", " \t first \r\nsecond", "first"},
		{"unterminated", strings.Repeat("x", 23*1024), strings.Repeat("x", 23*1024)},
		{"exact limit", strings.Repeat("x", codexFirstLineBytes), strings.Repeat("x", codexFirstLineBytes)},
		{"newline at limit", strings.Repeat("x", codexFirstLineBytes-1) + "\nsecond", strings.Repeat("x", codexFirstLineBytes-1)},
		{"over limit", strings.Repeat("x", codexFirstLineBytes+10) + "\nsecond", strings.Repeat("x", codexFirstLineBytes)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rollout.jsonl")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := readCodexFirstLine(path)
			old, oldErr := legacyCodexFirstLine(path)
			if err != nil || oldErr != nil || got != test.want || got != old {
				t.Fatalf("reader mismatch: lengths got=%d old=%d want=%d; errors=%v/%v", len(got), len(old), len(test.want), err, oldErr)
			}
		})
	}
}

func TestCodexFirstLinePreservesMetadataAndErrors(t *testing.T) {
	exactLimitContext := codexFirstLineBytes - len(syntheticCodexMetadata(t, 0))
	for _, size := range []int{0, 23 * 1024, exactLimitContext, codexFirstLineBytes} {
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		line := syntheticCodexMetadata(t, size)
		if err := os.WriteFile(path, []byte(line+"\n{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		payload := readCodexSessionMetaPayload(path)
		if size == codexFirstLineBytes {
			if payload != nil {
				t.Fatal("over-limit metadata was accepted")
			}
			continue
		}
		if payload["id"] != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" || payload["base_instructions"] != strings.Repeat("x", size) {
			t.Fatal("supported metadata lost identity or launch context")
		}
		if meta := readCodexSessionMeta(path); meta == nil || meta.cwd != "/synthetic/project" || meta.id != payload["id"] {
			t.Fatal("conversation identity changed")
		}
	}
	for _, path := range []string{filepath.Join(t.TempDir(), "missing"), t.TempDir()} {
		if _, err := readCodexFirstLine(path); err == nil {
			t.Fatal("unreadable metadata should return an error")
		}
		if payload := readCodexSessionMetaPayload(path); payload != nil {
			t.Fatal("unreadable metadata should not produce a payload")
		}
	}
}

func syntheticCodexMetadata(tb testing.TB, contextBytes int) string {
	tb.Helper()
	record := map[string]any{"type": "session_meta", "payload": map[string]any{
		"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "cwd": "/synthetic/project",
		"timestamp": "2026-09-29T00:00:00Z", "base_instructions": strings.Repeat("x", contextBytes),
	}}
	data, err := json.Marshal(record)
	if err != nil {
		tb.Fatal(err)
	}
	return string(data)
}

// This pins the prior implementation for allocation comparisons only.
func legacyCodexFirstLine(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(io.LimitReader(file, codexFirstLineBytes), codexFirstLineBytes)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(strings.TrimSuffix(line, "\n")), nil
}

func BenchmarkCodexFirstLine(b *testing.B) {
	for _, size := range []int{0, 23 * 1024, 60 * 1024} {
		line := syntheticCodexMetadata(b, size)
		path := filepath.Join(b.TempDir(), "rollout.jsonl")
		if err := os.WriteFile(path, []byte(line+"\n{}\n"), 0o600); err != nil {
			b.Fatal(err)
		}
		for _, reader := range []struct {
			name string
			read func(string) (string, error)
		}{{"legacy", legacyCodexFirstLine}, {"bounded-small", readCodexFirstLine}} {
			b.Run(reader.name+"/context-"+strconv.Itoa(size), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(line)))
				for i := 0; i < b.N; i++ {
					got, err := reader.read(path)
					if err != nil || got != line {
						b.Fatal("synthetic first line changed", err)
					}
				}
			})
		}
	}
}
