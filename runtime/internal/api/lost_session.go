package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

// sessionCanBeEnded includes a durable runner-lost record even when no
// in-memory Session remains. That is the one non-live target `sessions kill`
// intentionally accepts: ending it closes the retained ledger record and has
// no process side effect. Every other missing id remains a 404.
func sessionCanBeEnded(registry sessionService, id string) bool {
	if _, ok := registry.Get(id); ok {
		return true
	}
	for _, info := range registry.List(true) {
		if info.ID == id && !info.Exited && info.RunnerGone {
			return true
		}
	}
	return false
}

// sendSessionEndFailure keeps an operational conflict out of the daemon's 500
// bucket. The log retains the exact implementation error; the caller gets a
// stable explanation and a safe way to establish whether anything ended.
func (s *Server) sendSessionEndFailure(
	response http.ResponseWriter,
	subject string,
	nextAction string,
	err error,
	corsOrigin string,
) {
	log.Printf("sessionsd: end %s failed: %v", subject, err)
	message := fmt.Sprintf(
		"Sessions could not safely end %s. Check the sessionsd log, then %s",
		subject, nextAction,
	)
	var guard *sessionruntime.MassKillError
	if errors.As(err, &guard) {
		message = err.Error()
	}
	s.sendJSON(response, http.StatusConflict, map[string]any{"ok": false, "error": message}, corsOrigin)
}
