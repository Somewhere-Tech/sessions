package state

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// What a runner that never appeared was actually doing.
//
// From the owner's recoveries on 11 September: a runner that failed to start
// surfaced as `runner did not create socket within 60s`, sixty seconds later,
// with no cause. The causes are knowable and each has a different answer: the
// provider is holding a trust prompt, the working directory is gone, the
// binary cannot be executed, or the process exited and said why on its way
// out. The runner writes all of it to <id>.log, which nothing was reading.

// runnerLogTailLines is how much of the log is worth quoting. Enough for a
// trust prompt or a stack of Node's, and small enough for an error message.
const runnerLogTailLines = 12

// startupFailure reads the runner's own log and returns the most specific
// cause it can name, or "" when the log says nothing useful. The text is the
// runner's, not a paraphrase: a cause this layer invented would be one more
// thing to disbelieve.
func startupFailure(runnerStateDir, id string) string {
	tail := runnerLogTail(runnerStateDir, id)
	if tail == "" {
		return ""
	}
	lower := strings.ToLower(tail)
	switch {
	case strings.Contains(lower, "do you trust"), strings.Contains(lower, "trust the files"):
		return "the provider is waiting for you to trust this folder — open the session's terminal and answer it"
	case strings.Contains(lower, "no such file or directory"), strings.Contains(lower, "chdir"):
		return "the session's working directory could not be entered"
	case strings.Contains(lower, "permission denied"):
		return "the provider could not be run: permission denied"
	}
	return "the runner printed: " + tail
}

// runnerLogTail is the last few lines of <id>.log, flattened onto one line so
// it can travel inside an error without turning it into a wall.
func runnerLogTail(runnerStateDir, id string) string {
	file, err := os.Open(For(runnerStateDir, id).Log)
	if err != nil {
		return ""
	}
	defer file.Close()
	lines := make([]string, 0, runnerLogTailLines)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if len(lines) == runnerLogTailLines {
			lines = lines[1:]
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, " · ")
}

// startupError is the error a launch reports when the socket never appeared.
// It leads with the cause when there is one, because "did not create socket
// within 60s" tells the person nothing they can act on.
func startupError(runnerStateDir, id, socketPath string, waited string, lastErr error) error {
	if cause := startupFailure(runnerStateDir, id); cause != "" {
		return fmt.Errorf("session %s did not start: %s", id, cause)
	}
	return fmt.Errorf("runner did not create socket within %s: %s: %w", waited, socketPath, lastErr)
}
