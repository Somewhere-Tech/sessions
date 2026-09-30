package integrations

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// previewByteBudget and previewMessageBudget mirror the values the HTTP preview
// route passes (internal/api/integrations_handlers.go) and the ones
// runtime/CONTRACT/http-api.md documents: at most the latest 2 MiB, at most 400
// messages.
const (
	previewByteBudget    = 2 * 1024 * 1024
	previewMessageBudget = 400
)

// writeSyntheticClaudeHistory writes a discoverable Claude conversation of at
// least approxBytes. Every record is byte-identical, so the tail window a
// preview reads is the same whatever the total size is: what changes between
// the two fixtures is only how much history sits in front of it.
func writeSyntheticClaudeHistory(t *testing.T, root, uuid string, approxBytes int) (string, int64) {
	t.Helper()
	cwd := filepath.Join(root, "project")
	projectDir := filepath.Join(root, "claude-projects", watch.EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(projectDir, uuid+".jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	cwdJSON, err := json.Marshal(cwd)
	if err != nil {
		t.Fatal(err)
	}
	// One exchange is ~1 KiB, so a long conversation is many thousands of them.
	filler := make([]byte, 480)
	for index := range filler {
		filler[index] = 'a' + byte(index%26)
	}
	user := `{"type":"user","cwd":` + string(cwdJSON) + `,"message":{"role":"user","content":"ask ` + string(filler) + `"}}` + "\n"
	assistant := `{"type":"assistant","message":{"role":"assistant","content":"answer ` + string(filler) + `"}}` + "\n"
	written := 0
	for written < approxBytes {
		if _, err := writer.WriteString(user); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.WriteString(assistant); err != nil {
			t.Fatal(err)
		}
		written += len(user) + len(assistant)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, info.Size()
}

func previewStore(root string) *HistoryStore {
	return NewHistoryStore(HistoryOptions{
		RunnerStateDir:    filepath.Join(root, "runners"),
		ClaudeProjectsDir: filepath.Join(root, "claude-projects"),
		CodexSessionsDir:  filepath.Join(root, "codex-sessions"),
		Machine:           "fixture-mac", DiscoverProviderHistory: true,
	})
}

type previewCost struct {
	bytesPerRead   uint64
	allocsPerRead  uint64
	elapsedPerRead time.Duration
	messages       int
	truncated      bool
	responseBytes  int
}

func measurePreview(t *testing.T, store *HistoryStore, id string, reads int) previewCost {
	t.Helper()
	// One warm read first: the cost under test is reading a conversation, not
	// the one-time cost of whatever the process lazily initialises.
	if _, err := store.TranscriptPreview(nil, id, previewByteBudget, previewMessageBudget); err != nil {
		t.Fatal(err)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	var last TranscriptResponse
	for range reads {
		response, err := store.TranscriptPreview(nil, id, previewByteBudget, previewMessageBudget)
		if err != nil {
			t.Fatal(err)
		}
		last = response
	}
	elapsed := time.Since(start)
	runtime.ReadMemStats(&after)
	encoded, err := json.Marshal(last)
	if err != nil {
		t.Fatal(err)
	}
	return previewCost{
		bytesPerRead:   (after.TotalAlloc - before.TotalAlloc) / uint64(reads),
		allocsPerRead:  (after.Mallocs - before.Mallocs) / uint64(reads),
		elapsedPerRead: elapsed / time.Duration(reads),
		messages:       len(last.Messages),
		truncated:      last.Truncated,
		responseBytes:  len(encoded),
	}
}

// Opening the newest messages of a long conversation must cost what the window
// costs, not what the conversation weighs. The two fixtures differ only in how
// much older history precedes an identical tail.
func TestPreviewCostFollowsTheWindowNotTheHistorySize(t *testing.T) {
	if testing.Short() {
		t.Skip("writes tens of MiB of synthetic history")
	}
	const (
		smallBytes = 4 * 1024 * 1024
		largeBytes = 48 * 1024 * 1024
		reads      = 5
	)
	root := t.TempDir()
	const (
		smallUUID = "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
		largeUUID = "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	)
	_, smallSize := writeSyntheticClaudeHistory(t, root, smallUUID, smallBytes)
	_, largeSize := writeSyntheticClaudeHistory(t, root, largeUUID, largeBytes)
	if largeSize < smallSize*8 {
		t.Fatalf("fixture sizes are too close: %d vs %d", smallSize, largeSize)
	}
	store := previewStore(root)

	small := measurePreview(t, store, "provider:claude:"+smallUUID, reads)
	large := measurePreview(t, store, "provider:claude:"+largeUUID, reads)
	t.Logf("small file %d bytes: %d alloc-bytes/read, %d allocs/read, %v/read, %d messages, %d response bytes",
		smallSize, small.bytesPerRead, small.allocsPerRead, small.elapsedPerRead, small.messages, small.responseBytes)
	t.Logf("large file %d bytes: %d alloc-bytes/read, %d allocs/read, %v/read, %d messages, %d response bytes",
		largeSize, large.bytesPerRead, large.allocsPerRead, large.elapsedPerRead, large.messages, large.responseBytes)

	// The answer is the same window either way, and it says it omitted history.
	for name, cost := range map[string]previewCost{"small": small, "large": large} {
		if cost.messages != previewMessageBudget {
			t.Fatalf("%s preview returned %d messages, want the requested %d", name, cost.messages, previewMessageBudget)
		}
		if !cost.truncated {
			t.Fatalf("%s preview did not report that it omitted older history", name)
		}
	}
	if small.responseBytes != large.responseBytes {
		t.Fatalf("response size follows the file: %d vs %d bytes", small.responseBytes, large.responseBytes)
	}

	// A twelvefold larger conversation must not cost meaningfully more to open.
	// The bound is deliberately loose: this is a regression guard against the
	// whole file being read, parsed, or retained, not a performance target. What
	// a window costs in absolute terms is a separate question, logged above
	// rather than asserted, because decoding JSON records into maps legitimately
	// allocates several times their bytes.
	if large.bytesPerRead > small.bytesPerRead*2 {
		t.Fatalf("allocation per read grew with history size: %d bytes for %d, %d bytes for %d",
			small.bytesPerRead, smallSize, large.bytesPerRead, largeSize)
	}
}

// BenchmarkTranscriptPreviewByHistorySize records ns/op and B/op for the same
// requested window over conversations of different sizes.
func BenchmarkTranscriptPreviewByHistorySize(b *testing.B) {
	for _, size := range []int{4 * 1024 * 1024, 48 * 1024 * 1024} {
		b.Run(fmt.Sprintf("history=%dMiB", size/(1024*1024)), func(b *testing.B) {
			root := b.TempDir()
			const uuid = "33333333-cccc-4ccc-8ccc-cccccccccccc"
			writeSyntheticClaudeHistory(&testing.T{}, root, uuid, size)
			store := previewStore(root)
			id := "provider:claude:" + uuid
			if _, err := store.TranscriptPreview(nil, id, previewByteBudget, previewMessageBudget); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := store.TranscriptPreview(nil, id, previewByteBudget, previewMessageBudget); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
