package delivery

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const testOperationID = "11111111-2222-4333-8444-555555555555"

func TestOperationIsDurableAndIdempotent(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	clock := time.UnixMilli(100)
	store.now = func() time.Time { return clock }

	first, created, err := store.Begin(testOperationID, "session-a", "deploy once")
	if err != nil || !created || first.Status != StatusPending {
		t.Fatalf("Begin() = %+v, %t, %v", first, created, err)
	}
	clock = time.UnixMilli(200)
	accepted, err := store.Complete(testOperationID, StatusAccepted, true, false, "")
	if err != nil || accepted.Status != StatusAccepted || !accepted.Delivered || accepted.Retry {
		t.Fatalf("Complete() = %+v, %v", accepted, err)
	}

	restarted := New(root)
	again, created, err := restarted.Begin(testOperationID, "session-a", "deploy once")
	if err != nil || created || again.Status != StatusAccepted {
		t.Fatalf("idempotent Begin() = %+v, %t, %v", again, created, err)
	}
	if mode := fileMode(t, filepath.Join(root, "delivery-operations", testOperationID+".json")); mode != 0o600 {
		t.Fatalf("operation mode = %o, want 600", mode)
	}
}

func TestPendingOperationStaysUnknownAcrossRestart(t *testing.T) {
	root := t.TempDir()
	if _, created, err := New(root).Begin(testOperationID, "session-a", "maybe delivered"); err != nil || !created {
		t.Fatalf("Begin() created=%t err=%v", created, err)
	}
	record, created, err := New(root).Begin(testOperationID, "session-a", "maybe delivered")
	if err != nil || created || record.Status != StatusPending || record.Retry {
		t.Fatalf("pending retry = %+v, %t, %v", record, created, err)
	}
}

func TestOperationIDCannotBeReusedForDifferentMessage(t *testing.T) {
	store := New(t.TempDir())
	if _, _, err := store.Begin(testOperationID, "session-a", "first"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Begin(testOperationID, "session-a", "second"); err == nil {
		t.Fatal("different content reused an operation id")
	}
	if _, _, err := store.Begin(testOperationID, "session-b", "first"); err == nil {
		t.Fatal("different target reused an operation id")
	}
	if _, _, err := store.Begin(testOperationID, "session-a", "first", "steer"); err == nil {
		t.Fatal("different send mode reused an operation id")
	}
	if _, created, err := store.Begin(testOperationID, "session-a", "first", "auto"); err != nil || created {
		t.Fatalf("explicit auto should match an ordinary send: created=%v err=%v", created, err)
	}
}

func TestInvalidOperationIDCannotEscapeRoot(t *testing.T) {
	root := t.TempDir()
	if _, _, err := New(root).Begin("../../escape", "session", "text"); err == nil {
		t.Fatal("unsafe operation id accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "escape.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe path exists: %v", err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// Later evidence may settle uncertainty; nothing may reopen a decided outcome
// or make an already-sent message look safe to send again.
func TestConfirmAcceptedOnlyResolvesUncertaintyAndOnlyWithEvidence(t *testing.T) {
	for _, test := range []struct {
		name       string
		complete   func(*Store) error
		acceptance string
		wantErr    bool
		wantStatus Status
	}{
		{
			name: "unknown is resolved by the boundary that accepted it",
			complete: func(s *Store) error {
				_, err := s.Complete(testOperationID, StatusUnknown, false, false, "lost")
				return err
			},
			acceptance: "provider", wantStatus: StatusAccepted,
		},
		{
			name: "an answer without a boundary is not evidence",
			complete: func(s *Store) error {
				_, err := s.Complete(testOperationID, StatusUnknown, false, false, "lost")
				return err
			},
			acceptance: "", wantErr: true, wantStatus: StatusUnknown,
		},
		{
			name: "a refusal keeps its safe-to-retry meaning",
			complete: func(s *Store) error {
				_, err := s.Complete(testOperationID, StatusNotDelivered, false, true, "refused")
				return err
			},
			acceptance: "provider", wantStatus: StatusNotDelivered,
		},
		{
			name:       "a pending operation still belongs to its caller",
			complete:   func(*Store) error { return nil },
			acceptance: "provider", wantStatus: StatusPending,
		},
		{
			name: "text-only terminal delivery is not upgraded by a later boundary",
			complete: func(s *Store) error {
				_, err := s.Complete(testOperationID, StatusTextOnly, true, false, "Enter was not sent")
				return err
			},
			acceptance: "provider", wantStatus: StatusTextOnly,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := New(root)
			if _, _, err := store.Begin(testOperationID, "session-a", "ship once"); err != nil {
				t.Fatal(err)
			}
			if err := test.complete(store); err != nil {
				t.Fatal(err)
			}
			record, err := store.ConfirmAccepted(testOperationID, test.acceptance, "runner answered late")
			if test.wantErr {
				if err == nil {
					t.Fatalf("ConfirmAccepted() = %+v, want an error", record)
				}
			} else if err != nil {
				t.Fatalf("ConfirmAccepted() error = %v", err)
			}

			// Whatever the call returned, the durable record is what matters.
			stored, err := New(root).Get(testOperationID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Status != test.wantStatus {
				t.Fatalf("stored status = %q, want %q", stored.Status, test.wantStatus)
			}
			if test.wantStatus == StatusAccepted && (!stored.Delivered || stored.Retry || stored.Acceptance != "provider") {
				t.Fatalf("resolved record = %+v", stored)
			}
			if test.wantStatus == StatusNotDelivered && !stored.Retry {
				t.Fatalf("refusal lost its retry guidance: %+v", stored)
			}
		})
	}
}

// A refusal Sessions proved happened before any input reached the provider
// promises that sending the same operation again is safe. Asking again must
// therefore execute it again; every other outcome is only ever read back.
func TestBeginReexecutesOnlyAProvenRefusal(t *testing.T) {
	store := New(t.TempDir())
	if _, created, err := store.Begin(testOperationID, "session", "hello"); err != nil || !created {
		t.Fatalf("first begin = %v %v", created, err)
	}
	if _, err := store.Complete(testOperationID, StatusNotDelivered, false, true, "turn active"); err != nil {
		t.Fatal(err)
	}
	again, created, err := store.Begin(testOperationID, "session", "hello")
	if err != nil || !created || again.Status != StatusPending || again.Attempts != 1 || again.Reason != "" {
		t.Fatalf("retry of a proven refusal = %+v created %v err %v, want a fresh pending execution", again, created, err)
	}
	if _, _, err := store.Begin(testOperationID, "session", "different"); err == nil {
		t.Fatal("a different message reused a refused operation id")
	}

	for _, outcome := range []struct {
		status    Status
		delivered bool
		retry     bool
	}{
		{StatusUnknown, false, false},
		{StatusNotDelivered, false, false},
		{StatusTextOnly, true, false},
		{StatusAccepted, true, false},
	} {
		store := New(t.TempDir())
		if _, _, err := store.Begin(testOperationID, "session", "hello"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Complete(testOperationID, outcome.status, outcome.delivered, outcome.retry, "x"); err != nil {
			t.Fatal(err)
		}
		record, created, err := store.Begin(testOperationID, "session", "hello")
		if err != nil || created || record.Status != outcome.status {
			t.Fatalf("%s: begin again = %+v created %v err %v, want the stored receipt and no execution", outcome.status, record, created, err)
		}
	}
}
