package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// A stage another layer measured is part of the stage still running here. If it
// were simply added, the breakdown would sum to more than the request took and
// the operator would be chasing time that does not exist.
func TestStageTimerDoesNotDoubleCountAnInnerStage(t *testing.T) {
	timer := newStageTimer()
	time.Sleep(20 * time.Millisecond)
	timer.markFor("ledger", 10*time.Millisecond)
	timer.mark("live")
	timer.mark("store")

	breakdown := timer.breakdown()
	sum := milliseconds(t, breakdown, "ledger_ms") + milliseconds(t, breakdown, "live_ms") +
		milliseconds(t, breakdown, "store_ms")
	if total := milliseconds(t, breakdown, "total_ms"); sum > total+2 {
		t.Fatalf("stages sum to %dms against a total of %dms: %#v", sum, total, breakdown)
	}
	if got := milliseconds(t, breakdown, "ledger_ms"); got != 10 {
		t.Fatalf("inner stage = %dms, want the 10ms it reported", got)
	}
}

func TestStageTimerRepeatsAddUp(t *testing.T) {
	timer := newStageTimer()
	timer.markFor("ledger", 30*time.Millisecond)
	timer.markFor("ledger", 12*time.Millisecond)
	if got := milliseconds(t, timer.breakdown(), "ledger_ms"); got != 42 {
		t.Fatalf("repeated stage = %dms, want them added: a listing that folds the ledger three times must read as three", got)
	}
}

// The founder's twelve-second listing could not be attributed from outside the
// daemon, and production daemons run without pprof. ?timing=1 is how the same
// numbers the log line carries can be asked for directly.
func TestHistoryListingReportsItsStagesWhenAsked(t *testing.T) {
	daemon, manager := newTimingDaemon(t)
	defer manager.Close()

	response := serve(t, daemon, http.MethodGet, "/api/history?timing=1", nil, "127.0.0.1:4321", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Timing map[string]any `json:"timing"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"total_ms", "ledger_ms", "store_ms", "ledger_cached"} {
		if _, present := body.Timing[stage]; !present {
			t.Fatalf("timing is missing %s: %#v", stage, body.Timing)
		}
	}

	// Diagnosis is opt-in: an ordinary listing carries no timing at all.
	plain := serve(t, daemon, http.MethodGet, "/api/history", nil, "127.0.0.1:4321", nil)
	if bytes.Contains(plain.Body.Bytes(), []byte(`"timing"`)) {
		t.Fatalf("an ordinary listing carried a timing document: %s", plain.Body.String())
	}
}

// The listing asked the registry for the live sessions three times — the
// tracking loop, the failure observation and the listing itself — and each of
// those folds the ledger and probes every runner it has lost contact with. On
// the owner's machine that was most of the twelve seconds.
func TestHistoryListingListsTheLiveSessionsOnce(t *testing.T) {
	daemon, manager := newTimingDaemon(t)
	defer manager.Close()
	counter := &countingRegistry{Manager: manager, listTimer: manager}
	daemon.registry = counter

	if response := serve(t, daemon, http.MethodGet, "/api/history", nil, "127.0.0.1:4321", nil); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if counter.lists != 1 {
		t.Fatalf("one listing asked the registry %d times; every extra one re-folds the ledger", counter.lists)
	}
}

// A registry that counts how often the handler asks it to list. It is the real
// manager underneath, so what is being counted is the handler's own behaviour.
type countingRegistry struct {
	*sessionruntime.Manager
	listTimer *sessionruntime.Manager
	lists     int
}

func (r *countingRegistry) ListTimed(includeExited bool) ([]state.SessionInfo, sessionruntime.ListTiming) {
	r.lists++
	return r.listTimer.ListTimed(includeExited)
}

func (r *countingRegistry) List(includeExited bool) []state.SessionInfo {
	r.lists++
	return r.listTimer.List(includeExited)
}

// milliseconds reads one stage out of a breakdown, which carries durations
// beside facts like ledger_cached and is therefore not uniformly numeric.
func milliseconds(t *testing.T, breakdown map[string]any, stage string) int64 {
	t.Helper()
	switch value := breakdown[stage].(type) {
	case int64:
		return value
	case float64:
		return int64(value)
	default:
		t.Fatalf("%s = %#v, want a duration in milliseconds", stage, breakdown[stage])
		return 0
	}
}

func newTimingDaemon(t *testing.T) (*Server, *sessionruntime.Manager) {
	t.Helper()
	server, manager, _ := newTimingDaemonWithLedger(t)
	return server, manager
}

func newTimingDaemonWithLedger(t *testing.T) (*Server, *sessionruntime.Manager, *ledger.Store) {
	t.Helper()
	root := t.TempDir()
	// This daemon reads its own empty world. Without it the history store
	// discovers the provider directories of whoever is running the test, which
	// is both slow and none of the test's business.
	t.Setenv("HOME", root)
	config := state.Config{
		Host: "127.0.0.1", Port: 8787,
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 120, DefaultRows: 40,
		StateRoot: filepath.Join(root, "state"), UserStateRoot: filepath.Join(root, "user-state"),
		RunnerStateDir: filepath.Join(root, "state", "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
		TokenPath: filepath.Join(root, "state", "token"), WebDir: filepath.Join(root, "web"),
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(config, prototest.NewLauncher(), sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		LedgerReader: store, Retention: store.Retention(),
	})
	return New(config, manager), manager, store
}

// The flag exists so an operator reading a slow listing can tell a fold from a
// cache hit without inferring it from the numbers. Its meaning is exactly "the
// ledger has not moved since the last fold" — including at startup, where the
// daemon's own restart bookkeeping has already folded it once.
func TestListingSaysWhetherTheLedgerWasCached(t *testing.T) {
	daemon, manager, store := newTimingDaemonWithLedger(t)
	defer manager.Close()

	if cached, ok := timeListing(t, daemon)["ledger_cached"].(bool); !ok || !cached {
		t.Fatalf("a listing over an unchanged ledger reported ledger_cached=%#v", cached)
	}

	// An appended event is a miss, once, and the listing after it is a hit
	// again. Nothing about this is timed.
	if err := store.Observations().RecordIdle(context.Background(), ledger.Observation{
		Meta: ledger.Meta{LaneID: "00000000-0000-4000-8000-000000000001"},
	}); err != nil {
		t.Fatal(err)
	}
	if cached, ok := timeListing(t, daemon)["ledger_cached"].(bool); !ok || cached {
		t.Fatalf("a listing after an appended event reported ledger_cached=%#v", cached)
	}
	if cached, ok := timeListing(t, daemon)["ledger_cached"].(bool); !ok || !cached {
		t.Fatalf("the listing after the refold reported ledger_cached=%#v", cached)
	}
}

// A cold-run harness against this machine's own state, for the twelve-second
// listing the owner measured. It is opt-in (SESSIONS_PROFILE_HISTORY_DIRS=1)
// and never runs in CI, because it reads directories that belong to the person
// running it.
//
// It writes nothing outside its temp directory: the ledger, the history cache
// and the small runner metadata sidecars are copied, and the large transcripts
// are deliberately left where they are. It prints stage durations and counts,
// never a path, a title, or a line of anybody's conversation.
//
//	SESSIONS_PROFILE_HISTORY_DIRS=1 go test ./internal/api/ \
//	  -run HistoryListingStagesAgainstRealState -v
func TestHistoryListingStagesAgainstRealState(t *testing.T) {
	if os.Getenv("SESSIONS_PROFILE_HISTORY_DIRS") != "1" {
		t.Skip("set SESSIONS_PROFILE_HISTORY_DIRS=1 to profile against this machine's own state")
	}
	// Two independent copies of the same state, so the second daemon is not
	// warmed by what the first one wrote.
	unwarmed, unwarmedManager := newProfileDaemon(t)
	defer unwarmedManager.Close()
	cold := timeListing(t, unwarmed)
	warm := timeListing(t, unwarmed)
	t.Logf("without a startup warm: first %v", cold)
	t.Logf("without a startup warm: second %v", warm)

	warmed, warmedManager := newProfileDaemon(t)
	defer warmedManager.Close()
	// What sessionsd now does at startup, synchronously here so the harness can
	// time what a person's first request costs after it.
	_, _ = warmed.integrationEndpoints.History(warmed.registry.List(true))
	first := timeListing(t, warmed)
	second := timeListing(t, warmed)
	t.Logf("after a startup warm: first %v", first)
	t.Logf("after a startup warm: second %v", second)

	// The claim this harness exists to check: the first listing a person sees
	// is within 3x the second.
	firstTotal, secondTotal := milliseconds(t, first, "total_ms"), milliseconds(t, second, "total_ms")
	if firstTotal > 3*max64(secondTotal, 1) {
		t.Errorf("the first listing after a startup warm is %.1fx the second (%dms against %dms); the breakdown above says which stage",
			float64(firstTotal)/float64(max64(secondTotal, 1)), firstTotal, secondTotal)
	}
}

// newProfileDaemon builds a daemon over a copy of this machine's own state. It
// writes nothing outside its temp directory: the ledger, the history cache and
// the small runner metadata sidecars are copied, and the large transcripts are
// deliberately left where they are.
func newProfileDaemon(t *testing.T) (*Server, *sessionruntime.Manager) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	runnerDir := filepath.Join(root, "state", "runners")
	if err := os.MkdirAll(runnerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	copied := copySmallSidecars(t, filepath.Join(home, ".local", "state", "sessions", "runners"), runnerDir)
	cacheCopied := copyFileIfPresent(t,
		filepath.Join(home, ".local", "state", "sessions", "history-cache.json"),
		filepath.Join(root, "state", "history-cache.json"))
	ledgerPath := filepath.Join(root, "ledger.sqlite3")
	// macOS keeps the ledger under Application Support; the Unix layout is the
	// fallback. The WAL is copied too, or the fold under measurement would be
	// missing everything not yet checkpointed.
	ledgerCopied := false
	for _, candidate := range []string{
		filepath.Join(home, "Library", "Application Support", "sessions", "ledger", "lanes.sqlite3"),
		filepath.Join(home, ".local", "state", "sessions", "ledger", "lanes.sqlite3"),
	} {
		if copyFileIfPresent(t, candidate, ledgerPath) {
			ledgerCopied = true
			copyFileIfPresent(t, candidate+"-wal", ledgerPath+"-wal")
			copyFileIfPresent(t, candidate+"-shm", ledgerPath+"-shm")
			break
		}
	}
	t.Logf("copied %d runner sidecars, cache=%v ledger=%v", copied, cacheCopied, ledgerCopied)

	config := state.Config{
		Host: "127.0.0.1", Port: 8787,
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 120, DefaultRows: 40,
		StateRoot: filepath.Join(root, "state"), UserStateRoot: filepath.Join(root, "state"),
		RunnerStateDir: runnerDir, LaunchAgentsDir: filepath.Join(root, "agents"),
		TokenPath: filepath.Join(root, "state", "token"), WebDir: filepath.Join(root, "web"),
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: ledgerPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := sessionruntime.NewManager(config, prototest.NewLauncher(), sessionruntime.ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		LedgerReader: store, Retention: store.Retention(),
	})
	return New(config, manager), manager
}

func timeListing(t *testing.T, daemon *Server) map[string]any {
	t.Helper()
	response := serve(t, daemon, http.MethodGet, "/api/history?timing=1", nil, "127.0.0.1:4321", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	var body struct {
		Sessions []json.RawMessage `json:"sessions"`
		Timing   map[string]any    `json:"timing"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	t.Logf("listing returned %d rows", len(body.Sessions))
	return body.Timing
}

// copySmallSidecars copies the runner metadata a listing reads and skips the
// transcripts, which are large and which this harness has no reason to touch.
func copySmallSidecars(t *testing.T, from, to string) int {
	t.Helper()
	entries, err := os.ReadDir(from)
	if err != nil {
		return 0
	}
	copied := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		if copyFileIfPresent(t, filepath.Join(from, name), filepath.Join(to, name)) {
			copied++
		}
	}
	return copied
}

func copyFileIfPresent(t *testing.T, from, to string) bool {
	t.Helper()
	raw, err := os.ReadFile(from)
	if err != nil {
		return false
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return true
}

func max64(value, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}

// The first listing after a restart pays for what this process has not seen
// yet. Warming at startup is the difference between a person waiting for that
// and a daemon doing it while nobody is looking — so long as the daemon is not
// doing it *beside* the person, which is what the warm now stands aside for.
func TestWarmHistoryRunsBeforeAnybodyAsks(t *testing.T) {
	daemon, manager := newTimingDaemon(t)
	defer manager.Close()

	daemon.WarmHistory(nil)
	// The warm is a goroutine on purpose: a daemon must serve immediately, and
	// a very large history must not hold the listener closed. What this asserts
	// is that it completes and that a listing after it still answers.
	time.Sleep(historyWarmGrace + 200*time.Millisecond)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		response := serve(t, daemon, http.MethodGet, "/api/history?timing=1", nil, "127.0.0.1:4321", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d", response.Code)
		}
		var body struct {
			Timing map[string]any `json:"timing"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if milliseconds(t, body.Timing, "total_ms") < 2_000 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a listing after the warm was still slow")
}
