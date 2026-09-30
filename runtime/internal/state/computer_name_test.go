package state

import "testing"

func TestNormalizeComputerNameStripsLocalSuffix(t *testing.T) {
	for input, want := range map[string]string{
		"MacBook-Pro-10.local": "MacBook-Pro-10",
		"Studio.LOCAL":         "Studio",
		"studio.localhost":     "studio.localhost",
		"  Workstation  ":      "Workstation",
		".local":               "",
	} {
		if got := normalizeComputerName(input); got != want {
			t.Fatalf("normalizeComputerName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestComputerNameIsNeverEmpty(t *testing.T) {
	if got := ComputerName(); got == "" {
		t.Fatal("ComputerName returned an empty display name")
	}
}
