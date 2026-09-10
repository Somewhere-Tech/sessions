package api

// What a daemon still holds after it has answered.
//
// The Mini sits at ~2.37 GB resident with 561 records, 188 live sessions and
// ~40 runner processes, while the MacBook at 109 records and 15 live sits near
// 250 MB. That gap is retained state, not the per-call transients earlier work
// removed, so what matters is what is still reachable after a full listing, a
// search and a preview — and which structure holds it.
//
// The scale run is opt-in (SESSIONS_RETAINED_SCALE=1) because it builds the
// Mini's state; the bounded test below runs everywhere and fails if the
// per-session windows stop bounding what a busy session retains.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// retainedScale is the synthetic fleet a run builds.
type retainedScale struct {
	records           int
	live              int
	outputKiBPerLive  int
	claudeKiBPerLive  int
	codexRollouts     int
	claudeTranscripts int
	providerKiB       int
	largeClaudeMiB    int
	// shortLinesPerLive writes narrow lines instead of a solid block, because a
	// terminal's scrollback may cost per character written or per line scrolled
	// and only a fixture that writes both can tell those apart.
	shortLinesPerLive int
}

func envInt(name string, fallback int) int {
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && value >= 0 {
		return value
	}
	return fallback
}

// miniScale is the Mini's shape: 560 records of which 190 are live, and a
// provider history of 300 Codex rollouts and 300 Claude transcripts.
func miniScale() retainedScale {
	return retainedScale{
		records:           envInt("SESSIONS_RETAINED_RECORDS", 560),
		live:              envInt("SESSIONS_RETAINED_LIVE", 190),
		outputKiBPerLive:  envInt("SESSIONS_RETAINED_OUTPUT_KIB", 256),
		claudeKiBPerLive:  envInt("SESSIONS_RETAINED_CLAUDE_KIB", 256),
		codexRollouts:     envInt("SESSIONS_RETAINED_CODEX", 300),
		claudeTranscripts: envInt("SESSIONS_RETAINED_CLAUDE_FILES", 300),
		providerKiB:       envInt("SESSIONS_RETAINED_PROVIDER_KIB", 128),
		largeClaudeMiB:    envInt("SESSIONS_RETAINED_LARGE_MIB", 4),
		shortLinesPerLive: envInt("SESSIONS_RETAINED_LINES", 0),
	}
}

type retainedReading struct {
	heapInuse  uint64
	heapAlloc  uint64
	stackInuse uint64
	sys        uint64
	objects    uint64
}

// readRetained forces two collections before reading: the first frees what the
// request just dropped, the second frees what finalizers released.
func readRetained() retainedReading {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return retainedReading{
		heapInuse: stats.HeapInuse, heapAlloc: stats.HeapAlloc,
		stackInuse: stats.StackInuse, sys: stats.Sys,
		objects: stats.HeapObjects,
	}
}

func mib(bytes uint64) float64 { return float64(bytes) / (1024 * 1024) }

const feedChunkBytes = 64 * 1024

// feedSession returns how many output and structured events it emitted, so a
// caller can wait for the session to have absorbed them before measuring.
func feedSession(t *testing.T, runner *prototest.Runner, scale retainedScale) (outputs, structured int) {
	t.Helper()
	// Short lines, when asked for: a terminal's scrollback cost may follow the
	// characters written or the lines scrolled, and only a fixture that writes
	// both can tell those apart.
	if lines := scale.shortLinesPerLive; lines > 0 {
		var builder strings.Builder
		for line := 0; line < lines; line++ {
			fmt.Fprintf(&builder, "line %d\r\n", line)
			if builder.Len() >= feedChunkBytes {
				runner.AddOutput(builder.String())
				outputs++
				builder.Reset()
			}
		}
		if builder.Len() > 0 {
			runner.AddOutput(builder.String())
			outputs++
		}
	} else {
		// 64 KiB per event: a busy provider turn is closer to this than to a
		// keystroke, and it keeps a multi-megabyte feed to a few hundred events.
		chunk := strings.Repeat("x", feedChunkBytes)
		for written := 0; written < scale.outputKiBPerLive*1024; written += len(chunk) {
			runner.AddOutput(chunk)
			outputs++
		}
	}
	event := map[string]any{
		"type":    "assistant",
		"message": map[string]any{"role": "assistant", "content": strings.Repeat("x", feedChunkBytes)},
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for written := 0; written < scale.claudeKiBPerLive*1024; written += len(encoded) {
		runner.AddClaudeEvent(event)
		structured++
	}
	return outputs, structured
}

// writeClaudeTranscript writes one discoverable Claude conversation of about
// approxBytes under the isolated home.
func writeClaudeTranscript(t *testing.T, home, uuid string, approxBytes int) {
	t.Helper()
	cwd := filepath.Join(home, "project")
	dir := filepath.Join(home, ".claude", "projects", watch.EncodeClaudeCWD(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cwdJSON, err := json.Marshal(cwd)
	if err != nil {
		t.Fatal(err)
	}
	filler := strings.Repeat("history ", 60)
	user := `{"type":"user","cwd":` + string(cwdJSON) + `,"sessionId":"` + uuid + `","message":{"role":"user","content":"ask ` + filler + `"}}` + "\n"
	assistant := `{"type":"assistant","sessionId":"` + uuid + `","message":{"role":"assistant","content":"answer ` + filler + `"}}` + "\n"
	var builder strings.Builder
	for builder.Len() < approxBytes {
		builder.WriteString(user)
		builder.WriteString(assistant)
	}
	if err := os.WriteFile(filepath.Join(dir, uuid+".jsonl"), []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeCodexRollout writes one discoverable Codex rollout of about approxBytes.
func writeCodexRollout(t *testing.T, home, uuid string, approxBytes int, at time.Time) {
	t.Helper()
	cwd := filepath.Join(home, "project")
	dir := filepath.Join(home, ".codex", "sessions", at.Format("2006"), at.Format("01"), at.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stamp := at.Format(time.RFC3339Nano)
	meta, err := json.Marshal(map[string]any{
		"timestamp": stamp, "type": "session_meta",
		"payload": map[string]any{"id": uuid, "cwd": cwd, "timestamp": stamp},
	})
	if err != nil {
		t.Fatal(err)
	}
	filler := strings.Repeat("rollout ", 60)
	message, err := json.Marshal(map[string]any{
		"timestamp": stamp, "type": "response_item",
		"payload": map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": filler}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var builder strings.Builder
	builder.Write(meta)
	builder.WriteByte('\n')
	for builder.Len() < approxBytes {
		builder.Write(message)
		builder.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(dir, "rollout-"+uuid+".jsonl"), []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// buildRetainedFleet creates the sessions, ends the ones that are not live, and
// writes the provider history. It returns the live session ids in creation
// order and the id of one history record a preview can be taken of.
func buildRetainedFleet(t *testing.T, daemon *testDaemon, scale retainedScale, home string) []string {
	t.Helper()
	ctx := context.Background()
	project := filepath.Join(home, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	live := make([]string, 0, scale.live)
	for index := 0; index < scale.records; index++ {
		info, err := daemon.registry.Create(ctx, state.CreateSessionRequest{
			Cmd: "/bin/sh", Cwd: project,
			Name: fmt.Sprintf("record %d", index),
		})
		if err != nil {
			t.Fatalf("create session %d: %v", index, err)
		}
		if index < scale.live {
			live = append(live, info.ID)
			continue
		}
		// Ended sessions keep their record on disk, which is what a listing
		// reads; only the live ones keep a session in memory.
		daemon.registry.Kill(ctx, info.ID, false)
	}
	// An ended session keeps its object — mirror and windows included — for the
	// registry's 30 s grace. Measuring before that is over would attribute
	// memory to records that are already gone.
	if scale.records > scale.live {
		deadline := time.Now().Add(3 * time.Minute)
		for len(daemon.registry.List(true)) > scale.live {
			if time.Now().After(deadline) {
				t.Fatalf("registry still holds %d sessions, want %d", len(daemon.registry.List(true)), scale.live)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	feedFleet(t, daemon, live, scale)
	at := time.Now().Add(-time.Hour)
	for index := 0; index < scale.claudeTranscripts; index++ {
		size := scale.providerKiB * 1024
		if index == 0 && scale.largeClaudeMiB > 0 {
			size = scale.largeClaudeMiB * 1024 * 1024
		}
		writeClaudeTranscript(t, home, fmt.Sprintf("00000000-0000-4000-8000-%012d", index), size)
	}
	for index := 0; index < scale.codexRollouts; index++ {
		writeCodexRollout(t, home, fmt.Sprintf("11111111-0000-4000-8000-%012d", index), scale.providerKiB*1024, at)
	}
	return live
}

// feedFleet gives every live session its events and waits until each has
// absorbed them. The runner hands events over asynchronously, and a mirror that
// is still digesting has not yet retained what it will keep.
func feedFleet(t *testing.T, daemon *testDaemon, live []string, scale retainedScale) {
	t.Helper()
	type expectation struct {
		session    *state.Session
		outputs    uint32
		structured int64
	}
	expected := make([]expectation, 0, len(live))
	for _, id := range live {
		runner := daemon.launcher.Runner(id)
		if runner == nil {
			t.Fatalf("no fake runner for live session %s", id)
		}
		session, ok := daemon.registry.Get(id)
		if !ok {
			t.Fatalf("live session %s vanished", id)
		}
		// Counts are cumulative, so a second round waits for its own events
		// rather than for events an earlier round already delivered.
		before := expectation{session: session, outputs: session.OutputSeq(), structured: session.ClaudeEventCount()}
		outputs, structured := feedSession(t, runner, scale)
		before.outputs += uint32(outputs)
		before.structured += int64(structured)
		expected = append(expected, before)
	}
	for _, want := range expected {
		deadline := time.Now().Add(2 * time.Minute)
		for want.session.ClaudeEventCount() < want.structured || want.session.OutputSeq() < want.outputs {
			if time.Now().After(deadline) {
				t.Fatalf("session absorbed %d/%d structured and %d/%d output events",
					want.session.ClaudeEventCount(), want.structured, want.session.OutputSeq(), want.outputs)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
}

// answerOnce performs the reads the daemon is asked for in the field: the
// session listing, the history listing, one search and one preview.
func answerOnce(t *testing.T, daemon *testDaemon) {
	t.Helper()
	for _, target := range []string{"/api/sessions", "/api/history", "/api/search?q=rollout&limit=50"} {
		response := serve(t, daemon.handler, http.MethodGet, target, nil, "127.0.0.1:1", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", target, response.Code, truncate(response.Body.String()))
		}
	}
	listing := serve(t, daemon.handler, http.MethodGet, "/api/history", nil, "127.0.0.1:1", nil)
	var history struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	decodeBody(t, listing, &history)
	if len(history.Sessions) == 0 {
		t.Fatal("history listing returned no records")
	}
	// Preview a conversation that has a transcript. A managed record whose
	// session never wrote provider history has nothing to preview, and asking
	// for one would measure a 404 rather than a read.
	for _, session := range history.Sessions {
		if !strings.HasPrefix(session.ID, "provider:") {
			continue
		}
		preview := serve(t, daemon.handler,
			http.MethodGet, "/api/history/"+session.ID+"/preview", nil, "127.0.0.1:1", nil)
		if preview.Code != http.StatusOK {
			t.Fatalf("preview status=%d body=%s", preview.Code, truncate(preview.Body.String()))
		}
		return
	}
}

func truncate(body string) string {
	if len(body) > 300 {
		return body[:300] + "…"
	}
	return body
}

// forgetFixtureBuffers drops what the fake runners recorded. They keep every
// event a test fed them, unbounded and by design, and that fixture-side memory
// is not the daemon's.
func forgetFixtureBuffers(daemon *testDaemon, live []string) {
	for _, id := range live {
		if runner := daemon.launcher.Runner(id); runner != nil {
			runner.Forget()
		}
	}
}

// Output that has scrolled past the window must stop costing memory. A session
// keeps a terminal mirror, a replay window and a structured window, each with
// its own bound; what it must never do is keep growing with everything its
// runner ever printed. Feeding the same fleet again, five times over, is the
// difference between a bound and a leak.
func TestScrolledOutputStopsCostingMemory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	// 5,000 lines is the mirror's whole scrollback, so the first round fills
	// every bound this session has.
	first := retainedScale{records: 4, live: 4, shortLinesPerLive: 5_000, claudeKiBPerLive: 4 * 1024}
	live := buildRetainedFleet(t, &daemon, first, home)
	answerOnce(t, &daemon)
	forgetFixtureBuffers(&daemon, live)
	filled := readRetained()

	feedFleet(t, &daemon, live, retainedScale{shortLinesPerLive: 25_000, claudeKiBPerLive: 16 * 1024})
	answerOnce(t, &daemon)
	forgetFixtureBuffers(&daemon, live)
	refilled := readRetained()

	// A quarter is slack for allocator state and the listing's own work. Five
	// times the output would be four hundred per cent if any of these buffers
	// kept what scrolled past it, so the slack cannot hide the failure this
	// test exists for.
	ceiling := filled.heapInuse + filled.heapInuse/4
	t.Logf("after the first round %.1f MiB; after five times more output %.1f MiB (ceiling %.1f MiB)",
		mib(filled.heapInuse), mib(refilled.heapInuse), mib(ceiling))
	if refilled.heapInuse > ceiling {
		t.Fatalf("retained %.1f MiB after five times more output, want at most %.1f MiB (was %.1f MiB)",
			mib(refilled.heapInuse), mib(ceiling), mib(filled.heapInuse))
	}
}

// The scale run. Opt-in, because it builds the Mini's state in this process.
func TestRetainedMemoryAtFleetScale(t *testing.T) {
	if os.Getenv("SESSIONS_RETAINED_SCALE") == "" {
		t.Skip("set SESSIONS_RETAINED_SCALE=1 to measure a fleet-scale daemon")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemon := newTestDaemon(t)
	scale := miniScale()
	empty := readRetained()
	live := buildRetainedFleet(t, &daemon, scale, home)
	built := readRetained()
	answerOnce(t, &daemon)
	answered := readRetained()
	forgetFixtureBuffers(&daemon, live)
	settled := readRetained()
	// What the same fleet holds once the daemon has taken back the terminal
	// emulators of the sessions nothing is touching.
	hibernated := daemon.registry.HibernateIdleMirrors(0)
	afterHibernation := readRetained()

	t.Logf("scale: %d records, %d live, %d KiB output + %d KiB structured per live session, %d Codex rollouts, %d Claude transcripts",
		scale.records, scale.live, scale.outputKiBPerLive, scale.claudeKiBPerLive, scale.codexRollouts, scale.claudeTranscripts)
	for _, row := range []struct {
		label   string
		reading retainedReading
	}{
		{"empty daemon", empty},
		{"fleet built", built},
		{"after listing, search and preview", answered},
		{"fixture buffers dropped", settled},
		{fmt.Sprintf("%d mirrors hibernated", hibernated), afterHibernation},
	} {
		t.Logf("%-34s heapInuse %8.1f MiB  heapAlloc %8.1f MiB  stack %6.1f MiB  sys %8.1f MiB  objects %d",
			row.label, mib(row.reading.heapInuse), mib(row.reading.heapAlloc),
			mib(row.reading.stackInuse), mib(row.reading.sys), row.reading.objects)
	}
	perLive := float64(settled.heapInuse-min(settled.heapInuse, empty.heapInuse)) / float64(max(scale.live, 1))
	perLiveHibernated := float64(afterHibernation.heapInuse-min(afterHibernation.heapInuse, empty.heapInuse)) / float64(max(scale.live, 1))
	t.Logf("retained per live session: %.2f MiB awake, %.2f MiB hibernated",
		perLive/(1024*1024), perLiveHibernated/(1024*1024))

	if path := os.Getenv("SESSIONS_HEAP_PROFILE"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		runtime.GC()
		if err := pprof.WriteHeapProfile(file); err != nil {
			t.Fatal(err)
		}
		t.Logf("heap profile written to %s", path)
	}
}

// measureLedgerProjection reports what the ledger's in-memory current-state
// projection retains for laneCount lanes. It is one of the candidates for a
// daemon's resident memory, and a candidate is only ruled out by measuring it.
func measureLedgerProjection(t *testing.T, laneCount int) uint64 {
	t.Helper()
	ctx := context.Background()
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(t.TempDir(), "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for index := 0; index < laneCount; index++ {
		lane := fmt.Sprintf("22222222-0000-4000-8000-%012d", index)
		uuid := lane
		if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
			Meta: ledger.Meta{LaneID: lane, AtMS: time.Now().UnixMilli()},
			Name: fmt.Sprintf("lane %d", index),
			Tool: "claude", Cwd: "/tmp/project", LaneUUID: uuid,
			CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
		}); err != nil {
			t.Fatalf("record lane %d: %v", index, err)
		}
	}
	before := readRetained()
	states, err := store.CurrentStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != laneCount {
		t.Fatalf("projection holds %d lanes, want %d", len(states), laneCount)
	}
	// Drop the returned snapshot: what is being measured is what the store
	// keeps, not what one caller was handed.
	states = nil
	_ = states
	after := readRetained()
	runtime.KeepAlive(store)
	return after.heapInuse - min(after.heapInuse, before.heapInuse)
}

func TestLedgerProjectionRetentionAtFleetScale(t *testing.T) {
	if os.Getenv("SESSIONS_RETAINED_SCALE") == "" {
		t.Skip("set SESSIONS_RETAINED_SCALE=1 to measure the ledger projection")
	}
	lanes := envInt("SESSIONS_RETAINED_RECORDS", 560)
	retained := measureLedgerProjection(t, lanes)
	t.Logf("ledger projection retains %.2f MiB for %d lanes (%.0f B per lane)",
		mib(retained), lanes, float64(retained)/float64(max(lanes, 1)))
}
