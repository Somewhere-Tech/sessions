package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func bundledSkillPath() string {
	return filepath.Join("..", "..", "..", "skills", "sessions", "SKILL.md")
}

func readBundledSkill(t *testing.T) string {
	t.Helper()
	skill, err := os.ReadFile(bundledSkillPath())
	if err != nil {
		t.Fatalf("read bundled skill: %v", err)
	}
	return string(skill)
}

// The skill has no generator and nothing diffs it against the code, unlike
// help.go, which produces docs/CLI.md under a `git diff --exit-code` gate. So
// every command detail written here is a claim with no way to fail when it
// stops being true -- it just quietly becomes a confident lie to whichever
// agent reads it. These tests enforce the division: orientation lives in the
// skill, contract lives in help.
func TestBundledSkillSendsAgentsToTheGeneratedHelp(t *testing.T) {
	skill := readBundledSkill(t)
	if !strings.Contains(skill, "sessions help") {
		t.Fatal("the skill never points at `sessions help`, which is the only description of this CLI that CI keeps honest")
	}
}

// Duplicating the exit-code table here is the specific rot this file guards
// against: adding a code would leave the skill teaching an incomplete set with
// nothing to catch it. The skill must say the codes matter and where to read
// them, not enumerate them.
func TestBundledSkillDoesNotRestateTheExitCodeTable(t *testing.T) {
	skill := readBundledSkill(t)
	if !strings.Contains(skill, "exit") {
		t.Fatal("the skill never mentions that the exit status carries the outcome")
	}
	enumerated := regexp.MustCompile(`(?m)^\s*\|?\s*[0-4]\s*\|`)
	if enumerated.MatchString(skill) {
		t.Fatal("the skill enumerates exit codes; that table belongs in help.go, " +
			"which generates docs/CLI.md under a CI diff gate")
	}
}

// Flags are the fastest-moving surface in this CLI -- this program alone
// renamed the include-ended and owner axes and added several flags. A skill
// that teaches them will teach an old spelling.
func TestBundledSkillDoesNotTeachFlagSpellings(t *testing.T) {
	skill := readBundledSkill(t)
	for _, flag := range []string{
		"--include-closed", "--include-exited", "--all-owners",
		"--any", "--all", "--summary", "--timeout", "--force",
	} {
		if strings.Contains(skill, flag) {
			t.Fatalf("the skill teaches %s; flag spellings belong in `sessions help <command>`, "+
				"which is generated from the code", flag)
		}
	}
}

// A skill that grows back into a manual has re-created the problem. The cap is
// deliberately generous; it exists to make the next person ask whether what
// they are adding belongs in help.go instead.
func TestBundledSkillStaysOrientationSized(t *testing.T) {
	skill := readBundledSkill(t)
	const maxLines = 80
	if lines := strings.Count(strings.TrimSpace(skill), "\n") + 1; lines > maxLines {
		t.Fatalf("SKILL.md is %d lines, over the %d-line budget: it is drifting back into a manual, "+
			"and a manual here has no generator and no diff gate", lines, maxLines)
	}
}

// Sessions being sacred is a rule about judgement, not a command detail, so it
// belongs in the skill and cannot go stale.
func TestBundledSkillKeepsTheRulesThatOutrankConvenience(t *testing.T) {
	skill := strings.ToLower(readBundledSkill(t))
	for _, rule := range []string{"sacred", "--json", "scrape"} {
		if !strings.Contains(skill, rule) {
			t.Fatalf("the skill no longer states the %q rule", rule)
		}
	}
}
