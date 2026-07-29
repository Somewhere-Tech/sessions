// Command testhelper is built by the ledger crash-simulation test. It opens a
// real write transaction, inserts a row, announces that the transaction is
// live, and waits to be killed without committing.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: testhelper <database> <lane-id>")
		os.Exit(2)
	}
	database, err := sql.Open("sqlite", os.Args[1])
	if err != nil {
		panic(err)
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	if _, err := database.Exec("PRAGMA journal_mode=WAL"); err != nil {
		panic(err)
	}
	if _, err := database.Exec("PRAGMA synchronous=FULL"); err != nil {
		panic(err)
	}
	if _, err := database.Exec("PRAGMA busy_timeout=5000"); err != nil {
		panic(err)
	}
	transaction, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		panic(err)
	}
	_, err = transaction.Exec(`
INSERT INTO lane_events(event_id, lane_id, type, at_ms, actor, schema_version, payload_json)
VALUES (?, ?, 'runner_lost', ?, 'daemon', 1, '{}')`, "crash-helper-uncommitted", os.Args[2], time.Now().UnixMilli())
	if err != nil {
		panic(err)
	}
	fmt.Println("transaction-open")
	for {
		time.Sleep(time.Hour)
	}
}
