package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundledSkillKeepsSessionsFlagsOutOfRunChildArguments(t *testing.T) {
	path := filepath.Join("..", "..", "..", "skills", "sessions", "SKILL.md")
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
