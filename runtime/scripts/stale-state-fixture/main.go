// Command stale-state-fixture creates a scratch-only daemon state large enough
// to exercise runner discovery. It refuses every root outside /tmp so it cannot
// manufacture records or launch artifacts in a real Sessions home.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type fixtureConfig struct {
	root     string
	sessions int
	live     int
	pid      int
}

func main() {
	config := parseFlags()
	if err := generate(config); err != nil {
		fmt.Fprintln(os.Stderr, "stale-state-fixture:", err)
		os.Exit(1)
	}
	fmt.Printf("created %d records with %d stale live-runner artifact sets under %s\n", config.sessions, config.live, config.root)
}

func parseFlags() fixtureConfig {
	var config fixtureConfig
	flag.StringVar(&config.root, "root", "", "new scratch root below /tmp")
	flag.IntVar(&config.sessions, "sessions", 450, "total ledger records")
	flag.IntVar(&config.live, "live", 180, "records with stale runner artifacts")
	flag.IntVar(&config.pid, "pid", 1, "PID recorded in stale metadata")
	flag.Parse()
	return config
}

func generate(config fixtureConfig) error {
	root, err := validateRoot(config)
	if err != nil {
		return err
	}
	config.root = root
	paths := fixturePaths(root)
	for _, path := range []string{paths.home, paths.runners, paths.agents} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: paths.ledger})
	if err != nil {
		return err
	}
	defer store.Close()
	for index := 0; index < config.sessions; index++ {
		if err := writeRecord(store, paths, config, index); err != nil {
			return fmt.Errorf("record %d: %w", index, err)
		}
	}
	return nil
}

func validateRoot(config fixtureConfig) (string, error) {
	if config.sessions < 1 || config.live < 0 || config.live > config.sessions || config.pid < 1 {
		return "", errors.New("require sessions > 0, 0 <= live <= sessions, and pid > 0")
	}
	if config.root == "" {
		return "", errors.New("--root is required")
	}
	root, err := filepath.Abs(config.root)
	if err != nil {
		return "", err
	}
	if root == "/tmp" || !strings.HasPrefix(root, "/tmp/") {
		return "", fmt.Errorf("root %q must be a child of /tmp", root)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("root %q must not already exist", root)
	}
	return root, nil
}

type fixturePathSet struct {
	home    string
	runners string
	agents  string
	ledger  string
}

func fixturePaths(root string) fixturePathSet {
	home := filepath.Join(root, "home")
	return fixturePathSet{
		home: home, runners: filepath.Join(root, "runners"),
		agents: filepath.Join(home, "Library", "LaunchAgents"),
		ledger: filepath.Join(root, "lanes.sqlite3"),
	}
}

func writeRecord(store *ledger.Store, paths fixturePathSet, config fixtureConfig, index int) error {
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
	ctx := context.Background()
	created := ledger.Created{
		Meta: ledger.Meta{LaneID: id}, LaneUUID: id, Name: fmt.Sprintf("fixture-%03d", index),
		Kind: "lane", Tool: "lane", Cwd: paths.home,
		CreatorKind: ledger.CreatorExternal, CreatorID: "stale-state-fixture",
	}
	if err := store.Boundaries().RecordCreated(ctx, created); err != nil {
		return err
	}
	if index >= config.live {
		return store.Observations().RecordRunnerExited(ctx, ledger.RunnerExit{Meta: ledger.Meta{LaneID: id}})
	}
	if err := store.Observations().RecordRunnerReady(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: id}}); err != nil {
		return err
	}
	// Discovery records loss even when its destructive guard refuses cleanup.
	// Keep the fixture at that steady state: the record remains non-exited and
	// visible, while every stale coordination artifact is still present.
	if err := store.Observations().RecordRunnerLost(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: id}}); err != nil {
		return err
	}
	return writeArtifacts(paths, id, config.pid)
}

func writeArtifacts(paths fixturePathSet, id string, pid int) error {
	runnerPaths := state.For(paths.runners, id)
	metadata := state.Metadata{
		ID: id, Name: "stale " + id, Kind: "lane", Cmd: "/fixture/provider-never-running",
		Cwd: paths.home, Cols: 120, Rows: 40, CreatedAt: 1, PID: pid, SockPath: runnerPaths.Socket,
	}
	if err := state.WriteMetadata(runnerPaths.Meta, metadata); err != nil {
		return err
	}
	if err := os.WriteFile(runnerPaths.Socket, nil, 0o600); err != nil {
		return err
	}
	plist := state.RunnerPlistPath(paths.agents, id)
	if err := os.WriteFile(plist, []byte("fixture launch agent\n"), 0o600); err != nil {
		return err
	}
	old := time.Now().Add(-time.Minute)
	for _, path := range []string{runnerPaths.Meta, runnerPaths.Socket, plist} {
		if err := os.Chtimes(path, old, old); err != nil {
			return err
		}
	}
	return nil
}
