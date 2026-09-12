package ledger

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// From the Mini's own 30-second self-profile (11 September, 594 sessions,
// 71 MB ledger, 263,512 events): 94% of the samples were under
// `ledger.(*Store).append` ← `RecordActivity` ← the per-session observe worker,
// in `SELECT MAX(at_ms) FROM lane_events WHERE lane_id = ? AND type = ? AND
// at_ms <= ?` — reading a lane's whole event history off disk for every
// provider event, because the only indexes were (lane_id, seq) and (type, seq).
//
// Two things fix it: an index that makes the question cheap, and not asking it.
// This is the second one, and it is the one that matters at 594 sessions.
func TestCoalescingAsksTheDatabaseOncePerLaneAndThenNotAtAll(t *testing.T) {
	const lanes, perLane = 200, 60
	store := seededStore(t, lanes, perLane)
	now := time.Now().UnixMilli()

	// The first activity for each lane is the one question worth asking.
	for lane := range lanes {
		record(t, store, laneID(lane), now)
	}
	if probes := store.CoalesceProbes(); probes != lanes {
		t.Fatalf("the first activity per lane cost %d probes, want %d", probes, lanes)
	}

	// Twenty more rounds inside the coalescing window: nothing is appended and
	// nothing is read.
	before := countEvents(t, store)
	for round := range 20 {
		for lane := range lanes {
			record(t, store, laneID(lane), now+int64(round))
		}
	}
	if probes := store.CoalesceProbes(); probes != lanes {
		t.Fatalf("%d rounds of coalesced activity cost %d probes, want the original %d",
			20, probes, lanes)
	}
	if after := countEvents(t, store); after != before {
		t.Fatalf("coalesced activity appended %d events", after-before)
	}

	// Past the window it appends again — still without asking, because the
	// process knows what it wrote.
	for lane := range lanes {
		record(t, store, laneID(lane), now+2_000)
	}
	if appended := countEvents(t, store) - before; appended != lanes {
		t.Fatalf("a new window appended %d events, want one per lane", appended)
	}
	if probes := store.CoalesceProbes(); probes != lanes {
		t.Fatalf("appending past the window cost %d probes, want the original %d", probes, lanes)
	}
}

// The remembered answer has to be the query's answer. An event older than what
// this process has already written is the case memory cannot serve, so it asks.
func TestAnOutOfOrderEventStillAsksTheDatabase(t *testing.T) {
	store := seededStore(t, 2, 4)
	now := time.Now().UnixMilli()
	record(t, store, laneID(0), now)
	probes := store.CoalesceProbes()

	// A provider timestamp from before the last one: memory holds a newer
	// value, which is not the answer to "newest not after this one".
	record(t, store, laneID(0), now-5_000)
	if store.CoalesceProbes() != probes+1 {
		t.Fatalf("an out-of-order event answered from memory; it must ask: probes %d → %d",
			probes, store.CoalesceProbes())
	}
}

// TestObserveLoopsAtFleetScale is the measurement, not a guard: 600 lanes of
// 400 events each, 600 concurrent observe loops, and what one activity
// observation costs in each of the three worlds — as installed on the Mini,
// with the index, and with the index and the memory. Run it deliberately:
//
//	SESSIONS_LEDGER_LOAD=1 go test ./internal/ledger/ -run ObserveLoopsAtFleetScale -v
func TestObserveLoopsAtFleetScale(t *testing.T) {
	if os.Getenv("SESSIONS_LEDGER_LOAD") != "1" {
		t.Skip("set SESSIONS_LEDGER_LOAD=1 to measure 600 observe loops against a 240,000-event ledger")
	}
	const lanes, perLane = 600, 400
	store := seededStore(t, lanes, perLane)

	for _, mode := range []struct {
		name    string
		indexed bool
		memory  bool
		rounds  int
	}{
		// The unindexed probe costs tens of milliseconds each, so it is asked
		// fewer times; the comparison is the cost of one observation.
		{name: "no index, no memory (as installed)", indexed: false, memory: false, rounds: 2},
		{name: "indexed, no memory", indexed: true, memory: false, rounds: 40},
		{name: "indexed and remembered (this change)", indexed: true, memory: true, rounds: 40},
	} {
		setIndex(t, store, mode.indexed)
		resetCoalesce(store)
		store.coalesce.mu.Lock()
		store.coalesce.probes = 0
		store.coalesce.mu.Unlock()

		cpu, observations, wall := runObserveLoops(t, store, lanes, mode.rounds, mode.memory)
		each := cpu / time.Duration(max64(observations, 1))
		// What the Mini would pay: 594 sessions with a provider event each
		// every 100 ms is ~6,000 observations a second, which is a busy fleet.
		projected := float64(each) * 6_000 / float64(time.Second)
		t.Logf("%-38s %7d observations in %5s, %6d probes, %7.2f CPU-s, %8s each → %5.2f cores at 6k/s",
			mode.name, observations, wall.Round(time.Millisecond), store.CoalesceProbes(),
			cpu.Seconds(), each.Round(time.Microsecond), projected)
		// The burst watcher captures a profile when the process holds 0.8 of a
		// core for twenty seconds. After this change, the observe workers must
		// not be what makes that happen.
		if mode.memory && projected >= 0.8 {
			t.Fatalf("600 observe loops would still burn %.2f cores; the burst watcher would fire", projected)
		}
	}
}

// runObserveLoops is what the per-session observe worker does to the ledger:
// one activity observation per provider event, per lane, concurrently. Each
// lane's rounds straddle the coalescing window, so both the append and the
// coalesce-away paths are measured.
func runObserveLoops(
	t *testing.T, store *Store, lanes, rounds int, memory bool,
) (time.Duration, int64, time.Duration) {
	t.Helper()
	ctx := context.Background()
	var observations atomic.Int64
	started := time.Now()
	before := processCPUForTest(t)
	var workers sync.WaitGroup
	for lane := range lanes {
		workers.Add(1)
		go func() {
			defer workers.Done()
			id := laneID(lane)
			at := time.Now().UnixMilli()
			for round := range rounds {
				if !memory {
					// The behaviour before this change: every observation asks
					// the database for the window.
					resetCoalesce(store)
				}
				_ = store.Observations().RecordActivity(ctx, Activity{
					Meta: Meta{LaneID: id, AtMS: at + int64(round)*250}, Source: ActivityProviderEvent,
				})
				observations.Add(1)
			}
		}()
	}
	workers.Wait()
	return processCPUForTest(t) - before, observations.Load(), time.Since(started)
}

func resetCoalesce(store *Store) {
	store.coalesce.mu.Lock()
	defer store.coalesce.mu.Unlock()
	store.coalesce.latest = nil
}

func max64(value, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}

func setIndex(t *testing.T, store *Store, indexed bool) {
	t.Helper()
	statement := "DROP INDEX IF EXISTS lane_events_lane_type_at"
	if indexed {
		statement = "CREATE INDEX IF NOT EXISTS lane_events_lane_type_at ON lane_events(lane_id, type, at_ms)"
	}
	if _, err := store.db.ExecContext(context.Background(), statement); err != nil {
		t.Fatal(err)
	}
}

func record(t *testing.T, store *Store, id string, atMS int64) {
	t.Helper()
	if err := store.Observations().RecordActivity(context.Background(), Activity{
		Meta: Meta{LaneID: id, AtMS: atMS}, Source: ActivityProviderEvent,
	}); err != nil {
		t.Fatal(err)
	}
}

func laneID(index int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
}

func countEvents(t *testing.T, store *Store) int64 {
	t.Helper()
	var count int64
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM lane_events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// seededStore is a ledger with a fleet's worth of history already in it. The
// events go in one transaction because the point of the fixture is what reading
// them costs, not what writing them does.
func seededStore(t *testing.T, lanes, perLane int) *Store {
	t.Helper()
	root := t.TempDir()
	store, err := Open(context.Background(), Options{Path: filepath.Join(root, "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	insert, err := transaction.PrepareContext(ctx, `
INSERT INTO lane_events(event_id, lane_id, type, at_ms, actor, schema_version, payload_json)
VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-24 * time.Hour).UnixMilli()
	for lane := range lanes {
		id := laneID(lane)
		for event := range perLane {
			if _, err := insert.ExecContext(ctx,
				fmt.Sprintf("%s-%06d", id, event), id, string(EventActivity),
				base+int64(event*1_000), string(ActorProvider), SchemaVersion,
				`{"source":"provider_event"}`,
			); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	return store
}

func processCPUForTest(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	user := time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond
	system := time.Duration(usage.Stime.Sec)*time.Second + time.Duration(usage.Stime.Usec)*time.Microsecond
	return user + system
}

// What the Mini pays once, on the first start that has the index: 263,512
// events is the size of its ledger. Run it deliberately:
//
//	SESSIONS_LEDGER_LOAD=1 go test ./internal/ledger/ -run BuildingTheIndex -v
func TestBuildingTheIndexOnAMiniSizedLedger(t *testing.T) {
	if os.Getenv("SESSIONS_LEDGER_LOAD") != "1" {
		t.Skip("set SESSIONS_LEDGER_LOAD=1 to build an index over a quarter of a million events")
	}
	const lanes, perLane = 600, 440
	store := seededStore(t, lanes, perLane)
	path := store.Path()
	setIndex(t, store, false)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var written bytes.Buffer
	previous, flags := log.Writer(), log.Flags()
	log.SetOutput(&written)
	log.SetFlags(0)
	started := time.Now()
	reopened, err := Open(context.Background(), Options{Path: path})
	took := time.Since(started)
	log.SetOutput(previous)
	log.SetFlags(flags)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })

	t.Logf("opening a %d-event ledger that needs the index took %s", lanes*perLane, took.Round(time.Millisecond))
	t.Log(strings.TrimSpace(written.String()))
	if !strings.Contains(written.String(), "[ledger] building index lane_events_lane_type_at") {
		t.Fatalf("the build was silent: %q", written.String())
	}

	// And the second open says nothing, because there is nothing to build.
	var quiet bytes.Buffer
	log.SetOutput(&quiet)
	again, err := Open(context.Background(), Options{Path: path})
	log.SetOutput(previous)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if strings.Contains(quiet.String(), "building index") {
		t.Fatalf("a ledger that already has its indexes announced a build: %q", quiet.String())
	}
}
