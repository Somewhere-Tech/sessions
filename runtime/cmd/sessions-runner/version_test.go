package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A plain developer build uses the in-source version rather than release
// linker flags, so keep it aligned with the product version in package.json.
func TestDefaultVersionMatchesProductVersion(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	var product struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &product); err != nil {
		t.Fatal(err)
	}
	if version != product.Version {
		t.Fatalf("runner version = %q, package.json version = %q", version, product.Version)
	}
}
