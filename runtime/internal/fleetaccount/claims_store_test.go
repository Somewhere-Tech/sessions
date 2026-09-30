package fleetaccount

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixedClaimClock(now time.Time) func() time.Time {
	return func() time.Time { return now }
}

func TestClaimReplayStorePreservesUnreadableInvalidAndNewerState(t *testing.T) {
	fixtures := []string{
		`not JSON`, `{"version":2,"expires":{}}`, `{"version":1,"expires":null}`,
		`{"version":1,"expires":{"not-a-hash":5}}`, strings.Repeat("x", maxClaimReplayBytes+1),
	}
	for index, value := range fixtures {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "fixture-replay")
			if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
				t.Fatal(err)
			}
			store := claimReplayStore{path: path}
			now := time.Now()
			if err := store.consume("fixture", "nonce", now.Add(claimWindow), fixedClaimClock(now)); !errors.Is(err, ErrClaimStore) {
				t.Fatalf("invalid state = %v, want unavailable", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, []byte(value)) {
				t.Fatalf("invalid replay state changed: %v", err)
			}
		})
	}
}

func TestClaimReplayStoreWriteFailureDoesNotAcceptOrReplaceState(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix directory permission enforcement")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "fixture-replay")
	before := []byte(`{"version":1,"expires":{}}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0o700) })
	store := claimReplayStore{path: path}
	now := time.Now()
	if err := store.consume("fixture", "nonce", now.Add(claimWindow), fixedClaimClock(now)); !errors.Is(err, ErrClaimStore) {
		t.Fatalf("write failure = %v, want unavailable", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("write failure replaced replay state: %v", err)
	}
}

func TestClaimReplayStoreSerializesClaimAndRefusesExpiredAcceptance(t *testing.T) {
	store := claimReplayStore{path: filepath.Join(t.TempDir(), "fixture-replay")}
	now := time.Now().Truncate(time.Second)
	expires := now.Add(claimWindow)
	var wait sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- store.consume("fixture", "same-nonce", expires, fixedClaimClock(expires))
		}()
	}
	wait.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrClaimReplay) {
			t.Fatalf("concurrent consume = %v", err)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d identical claims, want one", accepted)
	}
	if err := store.consume("fixture", "new-nonce", expires, fixedClaimClock(expires.Add(time.Second))); !errors.Is(err, ErrClaimExpired) {
		t.Fatalf("acceptance after timestamp expiry = %v, want expired", err)
	}
	if _, err := store.load(); err != nil {
		t.Fatal(err)
	}
}

func TestClaimReplayStoreBoundsEntriesAndPrunesOnlyAfterExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture-replay")
	now := time.Now().Truncate(time.Second)
	document := claimReplayDocument{Version: claimReplayVersion, Expires: map[string]int64{}}
	for index := range maxClaimReplayEntries {
		document.Expires[fmt.Sprintf("%064x", index)] = now.Unix()
	}
	if err := writePrivateJSON(path, document); err != nil {
		t.Fatal(err)
	}
	store := claimReplayStore{path: path}
	if err := store.consume("fixture", "nonce", now.Add(claimWindow), fixedClaimClock(now)); !errors.Is(err, ErrClaimStore) {
		t.Fatalf("full store at inclusive expiry = %v, want unavailable", err)
	}
	if err := store.consume("fixture", "nonce", now.Add(claimWindow), fixedClaimClock(now.Add(time.Second))); err != nil {
		t.Fatalf("expired entries were not pruned: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("private replay state permissions = %v, %v", info, err)
	}
}

func TestClaimReplayStoreSamplesClockAfterLockWait(t *testing.T) {
	store := claimReplayStore{path: filepath.Join(t.TempDir(), "fixture-replay")}
	expires := time.Now().Truncate(time.Second).Add(claimWindow)
	var clock atomic.Int64
	clock.Store(expires.Unix())
	store.mu.Lock()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		result <- store.consume("fixture", "nonce", expires, func() time.Time { return time.Unix(clock.Load(), 0) })
	}()
	<-started
	clock.Add(1)
	store.mu.Unlock()
	if err := <-result; !errors.Is(err, ErrClaimExpired) {
		t.Fatalf("claim expired while awaiting replay lock = %v, want expired", err)
	}
	if _, err := os.Stat(store.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired claim wrote replay state: %v", err)
	}
}
