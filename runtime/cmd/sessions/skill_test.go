package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func bundledSkillPath() string {
	return filepath.Join("..", "..", "..", "skills", "sessions", "SKILL.md")
}

// An agent that reads only the skill still has to get the waiting contract
// right. Without the exit codes it writes `if rc == 0` and reports a timed-out
// delegate as a finished one, which is the failure the codes exist to prevent.
func TestBundledSkillTeachesTheExitCodeContract(t *testing.T) {
	skill, err := os.ReadFile(bundledSkillPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(skill)
	for _, required := range []string{
		"the condition was satisfied",
		"usage error",
		"the daemon could not be reached",
		"timed out without observing the condition",
		"the target is gone or ended failed",
		"if rc == 0",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("SKILL.md never explains %q, so an agent following only the skill cannot branch on the outcome", required)
		}
	}
	for code := 0; code <= 4; code++ {
		if !strings.Contains(text, "| "+strconv.Itoa(code)+" |") {
			t.Errorf("SKILL.md does not document exit code %d", code)
		}
	}
}

// The flagship agent features are the reason to reach for Sessions instead of
// a native subagent. A skill that omits them sends an agent to a lesser tool.
func TestBundledSkillCoversCrossProviderDelegationAndFanOut(t *testing.T) {
	skill, err := os.ReadFile(bundledSkillPath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(skill)
	for _, required := range []string{"send", "--from", "--all", "--any", "--with claude|codex", "transcript-recovery"} {
		if !strings.Contains(text, required) {
			t.Errorf("SKILL.md never mentions %q", required)
		}
	}
}

// -a is the canonical state flag on ls, list, and lanes. --include-closed is a
// retained alias and may be named as one, but a command an agent copies has to
// be the spelling the product actually teaches.
func TestBundledSkillUsesTheCanonicalIncludeEndedSpellingInExamples(t *testing.T) {
	file, err := os.Open(bundledSkillPath())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber, inFence := 0, false
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if !inFence {
			continue
		}
		if strings.Contains(line, "--include-closed") || strings.Contains(line, "--include-exited") {
			t.Errorf("SKILL.md:%d teaches a non-canonical state flag instead of -a: %s", lineNumber, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestBundledSkillKeepsSessionsFlagsOutOfRunChildArguments(t *testing.T) {
	path := bundledSkillPath()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		if !strings.Contains(line, "sessions ") || !strings.Contains(line, " run ") {
			continue
		}
		separator := strings.Index(line, " -- ")
		if separator >= 0 && strings.Contains(line[separator+4:], "--json") {
			t.Errorf("%s:%d passes Sessions --json to the child command: %s", path, lineNumber, line)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}
