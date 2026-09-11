package state

import (
	"fmt"
	"os"
	"path/filepath"
)

// openRunnerLog opens <RunnerStateDir>/<id>.log for append. launchd points
// StandardOutPath and StandardErrorPath at the same file, so a launcher that
// starts the process itself has to open it: a runner that dies before
// publishing its socket otherwise leaves no diagnostic at all, which is the one
// case where the person most needs to know where their session went.
//
// A failure here is reported rather than swallowed: the same directory is where
// the runner must publish its metadata, so an unwritable one is a launch
// problem, not a logging preference.
func openRunnerLog(runnerStateDir, id string) (*os.File, error) {
	if err := EnsureDir(runnerStateDir); err != nil {
		return nil, fmt.Errorf("create runner state directory %s: %w", runnerStateDir, err)
	}
	path := filepath.Join(runnerStateDir, id+".log")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open runner log %s: %w", path, err)
	}
	return file, nil
}
