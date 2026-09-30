package session

import (
	"net/url"
	"regexp"
	"strings"
	"sync"
)

var accountLoginURLPattern = regexp.MustCompile(`https://[^\s\x1b<>"']+`)

func allowedAccountLoginURL(tool, raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	if tool == "codex" {
		return u.Host == "auth.openai.com" || u.Host == "chatgpt.com"
	}
	return (u.Host == "claude.com" || u.Host == "claude.ai" || u.Host == "platform.claude.com" || u.Host == "console.anthropic.com") && strings.Contains(u.Path, "/oauth/")
}

type accountLoginOutput struct {
	mu     sync.Mutex
	op     *accountLoginOperation
	buffer string
}

func (w *accountLoginOutput) Write(bytes []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buffer += string(bytes)
	// Keep split URLs until a whitespace terminator arrives, without retaining
	// an unbounded provider log or publishing partial OAuth state.
	if len(w.buffer) > 32<<10 {
		w.buffer = w.buffer[len(w.buffer)-(32<<10):]
	}
	for _, match := range accountLoginURLPattern.FindAllStringIndex(w.buffer, -1) {
		if match[1] == len(w.buffer) {
			continue
		}
		raw := w.buffer[match[0]:match[1]]
		if allowedAccountLoginURL("claude", raw) {
			w.op.update(func(s *AccountLoginStatus) { s.State, s.URL = "waiting", raw })
		}
	}
	return len(bytes), nil
}
