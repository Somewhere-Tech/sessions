package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexInputMatchIgnoresUnrelatedPayloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	records := []string{
		`{"type":"response_item","payload":{"type":"function_call_output","output":"wanted"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"input_text","text":"wanted"}]}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","text":"wanted"}]}}`,
		`{"type":"event_msg","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"wanted"}]}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rolloutHasCodexUserInput(path, "wanted") {
		t.Fatal("matched a non-user-text record")
	}
	records = append(records, `{"type":"response_item","payload":{"type":"message","role":"user","extra":{"ignored":true},"content":[null,7,{"type":"input_text","text":42},{"type":"input_text","text":"wan"},{"type":"input_text","text":"ted"}]}}`)
	if err := os.WriteFile(path, []byte(strings.Join(records, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !rolloutHasCodexUserInput(path, "wanted") {
		t.Fatal("did not concatenate valid user-text blocks while ignoring invalid blocks")
	}
}

func BenchmarkCodexInputMatchLargeToolOutputs(b *testing.B) {
	path := filepath.Join(b.TempDir(), "rollout.jsonl")
	line := `{"type":"response_item","payload":{"type":"function_call_output","output":"` + strings.Repeat("x", 256*1024) + `"}}` + "\n"
	data := strings.Repeat(line, 16)
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if rolloutHasCodexUserInput(path, "not present") {
			b.Fatal("unexpected match")
		}
	}
}
