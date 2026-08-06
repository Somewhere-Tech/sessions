package usage

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A ledger whose PRAGMA table_info read fails part way through. Real causes -
// a failing disk, a truncated page, a database file yanked away - are not
// reproducible on demand, so the fault is injected at the driver instead.

const faultyReadMessage = "injected disk I/O error"

type faultyDriver struct {
	mu         sync.Mutex
	statements map[string][]string
}

var faulty = &faultyDriver{statements: map[string][]string{}}

func init() { sql.Register("usage-faulty", faulty) }

func (d *faultyDriver) Open(name string) (driver.Conn, error) { return &faultyConn{name: name}, nil }

func (d *faultyDriver) record(name, query string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.statements[name] = append(d.statements[name], query)
}

func (d *faultyDriver) executed(name string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.statements[name]...)
}

type faultyConn struct{ name string }

func (c *faultyConn) Prepare(query string) (driver.Stmt, error) {
	return &faultyStmt{conn: c, query: query}, nil
}
func (c *faultyConn) Close() error              { return nil }
func (c *faultyConn) Begin() (driver.Tx, error) { return nil, errors.New("no transactions") }

type faultyStmt struct {
	conn  *faultyConn
	query string
}

func (s *faultyStmt) Close() error  { return nil }
func (s *faultyStmt) NumInput() int { return 0 }

func (s *faultyStmt) Exec(args []driver.Value) (driver.Result, error) {
	faulty.record(s.conn.name, s.query)
	return driver.RowsAffected(0), nil
}

func (s *faultyStmt) Query(args []driver.Value) (driver.Rows, error) {
	faulty.record(s.conn.name, s.query)
	if !strings.HasPrefix(strings.ToUpper(s.query), "PRAGMA TABLE_INFO") {
		return nil, errors.New("unexpected query: " + s.query)
	}
	return &faultyRows{}, nil
}

// faultyRows hands back one healthy column and then fails, which is exactly
// the shape of a read that reports "no such column" without saying so.
type faultyRows struct{ served int }

func (r *faultyRows) Columns() []string {
	return []string{"cid", "name", "type", "notnull", "dflt_value", "pk"}
}
func (r *faultyRows) Close() error { return nil }

func (r *faultyRows) Next(dest []driver.Value) error {
	if r.served > 0 {
		return errors.New(faultyReadMessage)
	}
	r.served++
	dest[0] = int64(0)
	dest[1] = "event_key"
	dest[2] = "TEXT"
	dest[3] = int64(1)
	dest[4] = nil
	dest[5] = int64(1)
	return nil
}

var faultyLedgers int

// openFaultyLedger returns the ledger and the name every statement run against
// it is recorded under.
func openFaultyLedger(t *testing.T) (*sql.DB, string) {
	t.Helper()
	faultyLedgers++
	name := "faulty-" + strconv.Itoa(faultyLedgers)
	db, err := sql.Open("usage-faulty", name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	return db, name
}
