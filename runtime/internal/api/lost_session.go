package api

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
