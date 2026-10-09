package integrations

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func readFixtureRecord(role, text string) string {
	encoded, _ := json.Marshal(map[string]any{"type": role, "message": map[string]any{"role": role, "content": text}})
	return string(encoded) + "\n"
}

func readFixture(t *testing.T, path, token string, limit int) ReadResponse {
	t.Helper()
	page, err := readConversationFile(context.Background(), path, "claude", "claude:conversation", token, limit)
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func writeReadFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestReadIndependentReadersReplayAndAppend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conversation.jsonl")
	content := readFixtureRecord("user", "question") + readFixtureRecord("assistant", "answer")
	writeReadFixture(t, path, content)
	first := readFixture(t, path, "", 1)
	secondReader := readFixture(t, path, "", 1)
	if !reflect.DeepEqual(first, secondReader) || !first.HasMore {
		t.Fatal("readers consumed each other's position")
	}
	second := readFixture(t, path, first.NextCursor, 1)
	if !reflect.DeepEqual(second, readFixture(t, path, first.NextCursor, 1)) {
		t.Fatal("retry did not replay lost response")
	}
	if second.Messages[0].Text != "answer" || second.HasMore {
		t.Fatalf("second=%+v", second)
	}
	writeReadFixture(t, path, content+readFixtureRecord("assistant", "new answer"))
	next := readFixture(t, path, second.NextCursor, 20)
	if len(next.Messages) != 1 || next.Messages[0].Text != "new answer" {
		t.Fatalf("next=%+v", next)
	}
	if len(readFixture(t, path, next.NextCursor, 20).Messages) != 0 {
		t.Fatal("duplicate at EOF")
	}
}

func TestReadSurvivesSameConversationMovedFileButDetectsReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "one.jsonl")
	content := readFixtureRecord("user", "old") + readFixtureRecord("assistant", "answer")
	writeReadFixture(t, path, content)
	first := readFixture(t, path, "", 1)
	newPath := filepath.Join(t.TempDir(), "replacement-runner.jsonl")
	writeReadFixture(t, newPath, content)
	if readFixture(t, newPath, first.NextCursor, 20).Messages[0].Text != "answer" {
		t.Fatal("changed runner lost conversation")
	}
	writeReadFixture(t, newPath, readFixtureRecord("user", "new")+readFixtureRecord("assistant", "answer"))
	_, err := readConversationFile(context.Background(), newPath, "claude", "claude:conversation", first.NextCursor, 20)
	if !errors.Is(err, ErrReadChanged) {
		t.Fatalf("reset error=%v", err)
	}
	_, err = readConversationFile(context.Background(), path, "claude", "claude:other", first.NextCursor, 20)
	if !errors.Is(err, ErrReadCursor) {
		t.Fatalf("wrong conversation error=%v", err)
	}
}

func TestReadIncompleteRecordWaitsThenDelivers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	content := readFixtureRecord("user", "hello")
	partial := `{"type":"assistant","message":{"role":"assistant","content":"grow`
	writeReadFixture(t, path, content+partial)
	first := readFixture(t, path, "", 20)
	if !first.PendingRecord || first.HasMore || len(first.Messages) != 1 {
		t.Fatalf("partial=%+v", first)
	}
	writeReadFixture(t, path, content+partial+`ing"}}`+"\n")
	second := readFixture(t, path, first.NextCursor, 20)
	if len(second.Messages) != 1 || second.Messages[0].Text != "growing" {
		t.Fatalf("grown=%+v", second)
	}
}

func TestReadLargeMessageFragmentsAreLossless(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	content := strings.Repeat("🌱", 50000)
	writeReadFixture(t, path, readFixtureRecord("assistant", content))
	var got strings.Builder
	token := ""
	for pages := 0; pages < 10; pages++ {
		page := readFixture(t, path, token, 20)
		for _, message := range page.Messages {
			if !utf8.ValidString(message.Text) || len(message.Text) > 64<<10 || message.ByteOffset != got.Len() {
				t.Fatalf("invalid fragment %+v", message)
			}
			got.WriteString(message.Text)
		}
		if !page.HasMore {
			break
		}
		token = page.NextCursor
	}
	if got.String() != content {
		t.Fatalf("received %d/%d bytes", got.Len(), len(content))
	}
}

func TestReadMalformedAndToolOnlyRecordsAreExplicit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "records.jsonl")
	writeReadFixture(t, path, "not-json\n"+`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read"}]}}`+"\n"+readFixtureRecord("assistant", "real reply"))
	page := readFixture(t, path, "", 1)
	if len(page.Messages) != 1 || page.Messages[0].Text != "real reply" || page.SkippedRecords != 1 {
		t.Fatalf("page=%+v", page)
	}
}

func TestReadBoundsAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge.jsonl")
	writeReadFixture(t, path, strings.Repeat("x", readRecordLimit+1))
	_, err := readConversationFile(context.Background(), path, "claude", "id", "", 20)
	if !errors.Is(err, ErrReadRecordLarge) {
		t.Fatalf("large error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = readConversationFile(ctx, path, "claude", "id", "", 20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	for _, token := range []string{"bad", strings.Repeat("a", 4097), (readCursor{Version: 1, Conversation: "id", Offset: -1}).encode()} {
		if _, err := decodeReadCursor(token, "id"); !errors.Is(err, ErrReadCursor) {
			t.Fatalf("cursor error=%v", err)
		}
	}
}

func TestReadCodexSkipsUsageAndToolRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout-fixture.jsonl")
	writeReadFixture(t, path, strings.Join([]string{
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"question"}]}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{}","call_id":"tool-1"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}`,
		`{"type":"event_msg","payload":{"type":"task_complete"}}`,
	}, "\n")+"\n")
	page, err := readConversationFile(context.Background(), path, "codex", "codex:conversation", "", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 2 || page.Messages[0].Text != "question" || page.Messages[1].Text != "answer" || page.HasMore {
		t.Fatalf("Codex page=%+v", page)
	}
}

func TestReadDetectsAnEditedStreamingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stream.jsonl")
	prefix := readFixtureRecord("user", strings.Repeat("prefix", 100))
	writeReadFixture(t, path, prefix+readFixtureRecord("assistant", "initial text"))
	page := readFixture(t, path, "", 20)
	writeReadFixture(t, path, prefix+readFixtureRecord("assistant", "changed text"))
	_, err := readConversationFile(context.Background(), path, "claude", "claude:conversation", page.NextCursor, 20)
	if !errors.Is(err, ErrReadChanged) {
		t.Fatalf("edited record error=%v", err)
	}
}
