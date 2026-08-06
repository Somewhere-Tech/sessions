package usage

import (
	"os"
	"path/filepath"
	"testing"
)

// A tier that belongs to an unselected profile must never price the machine.
// The previous line scanner returned the first `service_tier=` it saw anywhere
// in the file, so a single [profiles.cheap] block repriced the whole ledger.
func TestCodexConfigTierIsTableAware(t *testing.T) {
	tests := []struct {
		name   string
		config string
		tier   string
	}{
		{
			name:   "root tier",
			config: "service_tier = \"priority\"\n",
			tier:   "priority",
		},
		{
			name:   "profile tier does not leak to the machine default",
			config: "model = \"gpt-5.3-codex\"\n\n[profiles.cheap]\nservice_tier = \"priority\"\n",
		},
		{
			name:   "profile tier does not override a root tier",
			config: "service_tier = \"flex\"\n\n[profiles.cheap]\nservice_tier = \"priority\"\n",
			tier:   "flex",
		},
		{
			name:   "selected profile decides",
			config: "profile = \"cheap\"\nservice_tier = \"flex\"\n\n[profiles.cheap]\nservice_tier = \"priority\"\n",
			tier:   "priority",
		},
		{
			name:   "selected profile inherits the root tier when silent",
			config: "profile = \"quiet\"\nservice_tier = \"priority\"\n\n[profiles.quiet]\nmodel = \"gpt-5.3-codex\"\n",
			tier:   "priority",
		},
		{
			name:   "tier under a nested table stays there",
			config: "[profiles.cheap.overrides]\nservice_tier = \"priority\"\n",
		},
		{
			name:   "tier under an array of tables stays there",
			config: "[[mcp_servers]]\nservice_tier = \"priority\"\n",
		},
		{
			name:   "dotted key keeps its table",
			config: "profiles.cheap.service_tier = \"priority\"\n",
		},
		{
			name:   "dotted root key is read",
			config: "profiles.cheap.service_tier = \"flex\"\nservice_tier = \"priority\"\n",
			tier:   "priority",
		},
		{
			name:   "inline table keeps its table",
			config: "profiles = { cheap = { service_tier = \"priority\" } }\n",
		},
		{
			name:   "hash inside a quoted value is not a comment",
			config: "notify = \"say #done\"\nservice_tier = \"priority\" # premium lane\n",
			tier:   "priority",
		},
		{
			name:   "equals and brackets inside a quoted value are not structure",
			config: "instructions = \"[profiles.cheap] service_tier = fast\"\nservice_tier = \"priority\"\n",
			tier:   "priority",
		},
		{
			name:   "multi-line string is not parsed as configuration",
			config: "notes = \"\"\"\n[profiles.cheap]\nservice_tier = \"priority\"\n\"\"\"\nservice_tier = \"flex\"\n",
			tier:   "flex",
		},
		{
			name:   "array values are stepped over",
			config: "tools = [\"a\", \"# not a comment\", \"]\"]\nservice_tier = \"priority\"\n",
			tier:   "priority",
		},
		{
			name:   "literal string keeps its backslashes",
			config: "path = 'C:\\codex\\n'\nservice_tier = \"priority\"\n",
			tier:   "priority",
		},
		{
			name:   "commented tier is not a tier",
			config: "# service_tier = \"priority\"\n",
		},
		{
			name:   "quoted profile name",
			config: "profile = \"my lane\"\n\n[profiles.\"my lane\"]\nservice_tier = \"priority\"\n",
			tier:   "priority",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(test.config), 0o600); err != nil {
				t.Fatal(err)
			}
			if tier := codexConfigTier(dir); tier != test.tier {
				t.Fatalf("codexConfigTier = %q, want %q", tier, test.tier)
			}
			if got, want := fastServiceTier(codexConfigTier(dir)), fastServiceTier(test.tier); got != want {
				t.Fatalf("fastServiceTier(codexConfigTier) = %v, want %v", got, want)
			}
		})
	}
}

func TestCodexConfigTierEnablesPremiumPricing(t *testing.T) {
	root := t.TempDir()
	if fastServiceTier(codexConfigTier(root)) {
		t.Fatal("missing config unexpectedly enabled fast mode")
	}
	if fastServiceTier(codexConfigTier("")) != false || codexConfigTier("") != "" {
		t.Fatal("an empty config dir was resolved against the working directory")
	}
	for _, tier := range []string{"fast", "priority"} {
		if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("service_tier = \""+tier+"\" # comment\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if !fastServiceTier(codexConfigTier(root)) {
			t.Fatalf("service_tier %q did not enable fast pricing", tier)
		}
	}
	for _, tier := range []string{"flex", "default", ""} {
		if err := os.WriteFile(filepath.Join(root, "config.toml"), []byte("service_tier = \""+tier+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if fastServiceTier(codexConfigTier(root)) {
			t.Fatalf("service_tier %q unexpectedly enabled fast pricing", tier)
		}
	}
}

func TestParseTOMLKeepsValuesAndTables(t *testing.T) {
	document := parseTOML(`
# leading comment
root = "one"
escaped = "a\"b\tc\u0041"
literal = 'raw\nvalue'
number = 42
enabled = true
inline = { key = "value", nested = { deep = "yes" } }

["quoted table"]
key = "value"

[server]
host = "localhost"
`)
	tests := []struct {
		table, key, want string
	}{
		{"", "root", "one"},
		{"", "escaped", "a\"b\tcA"},
		{"", "literal", `raw\nvalue`},
		{"", "number", "42"},
		{"", "enabled", "true"},
		{"inline", "key", "value"},
		{"inline.nested", "deep", "yes"},
		{"quoted table", "key", "value"},
		{"server", "host", "localhost"},
	}
	for _, test := range tests {
		got, ok := document.value(test.table, test.key)
		if !ok || got != test.want {
			t.Fatalf("value(%q, %q) = %q %v, want %q", test.table, test.key, got, ok, test.want)
		}
	}
	if _, ok := document.value("", "host"); ok {
		t.Fatal("a table key leaked into the root table")
	}
}

// Malformed input must leave pricing on the conservative path rather than on a
// guess, and must never hang the scan.
func TestParseTOMLStopsOnMalformedInput(t *testing.T) {
	for _, source := range []string{
		"service_tier = \"unterminated\n",
		"[unterminated\nservice_tier = \"priority\"\n",
		"service_tier =",
		"= \"priority\"",
		"tools = [\"unterminated\n",
		"inline = { key = \n",
		"escaped = \"\\q\"\nservice_tier = \"priority\"\n",
		"",
	} {
		document := parseTOML(source)
		if tier, ok := document.value("", "service_tier"); ok && tier == "priority" {
			t.Fatalf("malformed input %q produced a premium tier", source)
		}
	}
}
