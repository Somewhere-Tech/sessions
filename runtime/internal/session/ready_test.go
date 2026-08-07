package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// The first request of a new session is pasted the moment waitReady returns,
// so waitReady returning early IS the lost-first-message bug: Claude with
// --remote-control was still drawing its TUI at the 800ms mark, the paste
// landed inside initialization, and the alt-screen redraw ate it while every
// layer reported success. These tests drive the observation loop with a fake
// program instead of a real PTY, because the property under test is the
// decision — silence after speech — not the plumbing.

// A program still talking must hold readiness, and readiness must arrive only
// after it goes quiet.
func TestReadinessWaitsOutAProgramStillInitializing(t *testing.T) {
	var lastData atomic.Int64
	lastData.Store(time.Now().UnixMilli())
	stopTalking := time.Now().Add(600 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		// The fake claude: streams banner and handshake output for 600ms,
		// then its composer is up and it falls silent.
		for time.Now().Before(stopTalking) {
			lastData.Store(time.Now().UnixMilli())
			time.Sleep(20 * time.Millisecond)
		}
		close(done)
	}()

	started := time.Now()
	awaitProviderQuiet(context.Background(), nil, func() (int64, bool) {
		return lastData.Load(), false
	}, 100*time.Millisecond, 250*time.Millisecond)
	elapsed := time.Since(started)
	<-done

	minimum := stopTalking.Sub(started) + 250*time.Millisecond
	if elapsed < minimum-50*time.Millisecond {
		t.Fatalf("ready after %v while the program was still initializing; the first "+
			"request would have been pasted into the redraw that eats it (needed ~%v)",
			elapsed, minimum)
	}
	if elapsed > minimum+2*time.Second {
		t.Fatalf("ready took %v, far beyond quiet-after-silence (~%v)", elapsed, minimum)
	}
}

// A program that was already idle when the wait began answers at the floor —
// the fast warm-machine case keeps exactly its old latency.
func TestReadinessAnswersAtTheFloorForAnAlreadyQuietProgram(t *testing.T) {
	idleSince := time.Now().Add(-2 * time.Second).UnixMilli()
	started := time.Now()
	awaitProviderQuiet(context.Background(), nil, func() (int64, bool) {
		return idleSince, false
	}, 120*time.Millisecond, 200*time.Millisecond)
	elapsed := time.Since(started)
	if elapsed < 120*time.Millisecond {
		t.Fatalf("returned before the floor: %v", elapsed)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("an already-quiet program took %v to be called ready", elapsed)
	}
}

// The 30-second cap the HTTP contract has always documented is enforced by the
// caller's context; a program that never stops talking must not hold a create
// forever.
func TestReadinessGivesUpAtTheContextDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	started := time.Now()
	awaitProviderQuiet(ctx, nil, func() (int64, bool) {
		// Never quiet: output is always "just now".
		return time.Now().UnixMilli(), false
	}, 50*time.Millisecond, 200*time.Millisecond)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("the wait outlived its context by %v", elapsed)
	}
}

// A structured runtime's first event is a stronger signal than silence and
// must win immediately; a session that exits must stop being waited for.
func TestReadinessReturnsEarlyForStructuredEventsAndExits(t *testing.T) {
	structured := make(chan struct{}, 1)
	structured <- struct{}{}
	started := time.Now()
	awaitProviderQuiet(context.Background(), structured, func() (int64, bool) {
		return time.Now().UnixMilli(), false
	}, time.Hour, time.Hour)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("a structured event took %v to end the wait", elapsed)
	}

	started = time.Now()
	awaitProviderQuiet(context.Background(), nil, func() (int64, bool) {
		return time.Now().UnixMilli(), true
	}, 50*time.Millisecond, time.Hour)
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("an exited session held the wait for %v", elapsed)
	}
}
