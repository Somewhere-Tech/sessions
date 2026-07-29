package state

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeTagsCanonicalizesAndCopies(t *testing.T) {
	input := map[string]string{" Product.Line ": "  Sessions  ", "release": "macOS"}
	tags, err := NormalizeTags(input)
	if err != nil {
		t.Fatal(err)
	}
	if tags["product.line"] != "Sessions" || tags["release"] != "macOS" {
		t.Fatalf("NormalizeTags() = %#v", tags)
	}
	input["release"] = "changed"
	if tags["release"] != "macOS" {
		t.Fatal("NormalizeTags returned a map aliased to its input")
	}
}

func TestTagsCanEditRetainedMetadataWithoutLiveRunner(t *testing.T) {
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	if err := EnsureDir(runnerDir); err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	if err := WriteMetadata(filepath.Join(runnerDir, id+".json"), Metadata{ID: id, Cmd: "/bin/sh", Cwd: root}); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(Config{RunnerStateDir: runnerDir}, nil)
	updated, err := registry.UpdateTags(id, map[string]string{"product": "Sessions"})
	if err != nil || updated["product"] != "Sessions" {
		t.Fatalf("UpdateTags() = %#v, %v", updated, err)
	}
	read, err := registry.Tags(id)
	if err != nil || read["product"] != "Sessions" {
		t.Fatalf("Tags() = %#v, %v", read, err)
	}
}

func TestNormalizeTagsRejectsInvalidSets(t *testing.T) {
	tests := []struct {
		name string
		tags map[string]string
	}{
		{name: "empty key", tags: map[string]string{" ": "value"}},
		{name: "invalid key character", tags: map[string]string{"product line": "sessions"}},
		{name: "empty value", tags: map[string]string{"product": " "}},
		{name: "key too long", tags: map[string]string{strings.Repeat("a", MaxSessionTagKey+1): "value"}},
		{name: "value too long", tags: map[string]string{"product": strings.Repeat("a", MaxSessionTagValue+1)}},
	}
	tooMany := make(map[string]string)
	for index := 0; index <= MaxSessionTags; index++ {
		tooMany[string(rune('a'+index%26))+strings.Repeat("x", index/26)] = "value"
	}
	tests = append(tests, struct {
		name string
		tags map[string]string
	}{name: "too many", tags: tooMany})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NormalizeTags(test.tags); err == nil {
				t.Fatalf("NormalizeTags(%#v) unexpectedly succeeded", test.tags)
			}
		})
	}
}

func TestTagMetadataRejectsPathTraversalIDs(t *testing.T) {
	registry := NewRegistry(Config{RunnerStateDir: t.TempDir()}, nil)
	for _, id := range []string{"../token", `..\\token`, ".", ""} {
		if _, err := registry.Tags(id); err == nil {
			t.Fatalf("Tags(%q) unexpectedly succeeded", id)
		}
		if _, err := registry.UpdateTags(id, map[string]string{"product": "Sessions"}); err == nil {
			t.Fatalf("UpdateTags(%q) unexpectedly succeeded", id)
		}
	}
}
