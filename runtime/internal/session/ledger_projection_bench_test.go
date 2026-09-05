package session

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

// The lane count stays fixed while its append-only observation history grows.
// Setup uses one transaction so the measurement concerns reads, not fsync.
func BenchmarkManagerLedgerStatesGrowingHistory(b *testing.B) {
	for _, count := range []int{1000, 10000, 223675} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			store := benchmarkProjectionStore(b, count)
			manager := &Manager{ledgerReader: store}
			ctx := context.Background()
			if _, err := manager.ledgerStates(ctx); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				states, err := manager.ledgerStates(ctx)
				if err != nil || len(states) != 521 {
					b.Fatalf("states=%d err=%v", len(states), err)
				}
			}
		})
	}
}

func BenchmarkManagerLedgerStatesAfterExternalAppend(b *testing.B) {
	store := benchmarkProjectionStore(b, 223675)
	writer, err := ledger.Open(context.Background(), ledger.Options{Path: store.Path()})
	if err != nil {
		b.Fatal(err)
	}
	defer writer.Close()
	manager := &Manager{ledgerReader: store}
	ctx := context.Background()
	if _, err := manager.ledgerStates(ctx); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		name := fmt.Sprintf("new-%d", i)
		if err := writer.Observations().RecordRenamed(ctx, ledger.Rename{Meta: ledger.Meta{LaneID: "lane-000"}, Name: name}); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		states, err := manager.ledgerStates(ctx)
		if err != nil || len(states) != 521 || states[0].Name != name {
			b.Fatalf("missed appended state: %v", err)
		}
	}
}

func BenchmarkManagerLedgerStatesColdProjection(b *testing.B) {
	store := benchmarkProjectionStore(b, 223675)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		reader, err := ledger.Open(context.Background(), ledger.Options{Path: store.Path()})
		if err != nil {
			b.Fatal(err)
		}
		manager := &Manager{ledgerReader: reader}
		b.StartTimer()
		states, err := manager.ledgerStates(context.Background())
		if err != nil || len(states) != 521 {
			b.Fatalf("states=%d err=%v", len(states), err)
		}
		b.StopTimer()
		if err := reader.Close(); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkProjectionStore(b *testing.B, count int) *ledger.Store {
	b.Helper()
	path := filepath.Join(b.TempDir(), "lanes.sqlite3")
	store, err := ledger.Open(context.Background(), ledger.Options{Path: path})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`INSERT INTO lane_events(event_id,lane_id,type,at_ms,actor,schema_version,payload_json) VALUES (?,?,?,?, 'daemon',1,?)`)
	if err != nil {
		b.Fatal(err)
	}
	defer statement.Close()
	for i := 0; i < count; i++ {
		kind, payload := "activity", `{"source":"provider_event"}`
		if i < 521 {
			kind, payload = "created", `{"tool":"terminal","cwd":"/tmp","creator_kind":"external","creator_id":"fixture"}`
		}
		if _, err := statement.Exec(fmt.Sprint(i), fmt.Sprintf("lane-%03d", i%521), kind, i+1, payload); err != nil {
			b.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return store
}
