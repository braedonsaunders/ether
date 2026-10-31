package store

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"time"
)

// SQLite has one writer. Left to SQLite, connections that want to write at
// the same moment poll the lock with growing sleeps (up to 100 ms each), so
// a waiter can sleep well past the moment the lock came free, and a burst of
// writers starves whoever arrived first. writeGate makes this process's
// connections to one database file take turns in Go instead: a writer waits
// on a channel and runs as soon as the previous one finishes. Reads never
// wait on it. Another process writing the same file is still handled by
// SQLite's busy_timeout.
type writeGate struct{ slot chan struct{} }

// writeGateWait matches busy_timeout: a writer that waits longer fails the
// way SQLite would have.
const writeGateWait = 10 * time.Second

var errWriteGateTimeout = errors.New("database is locked (waited 10s for this process's other writer)")

var (
	gatesMu sync.Mutex
	gates   = map[string]*writeGate{}
)

// gateFor returns the gate shared by every connection to the database a
// DSN names. In-memory databases are private to their connection and
// ungated.
func gateFor(dsn string) *writeGate {
	if strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory") {
		return nil
	}
	path := strings.TrimPrefix(dsn, "file:")
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	gatesMu.Lock()
	defer gatesMu.Unlock()
	g := gates[path]
	if g == nil {
		g = &writeGate{slot: make(chan struct{}, 1)}
		gates[path] = g
	}
	return g
}

func (g *writeGate) acquire(ctx context.Context) error {
	select {
	case g.slot <- struct{}{}:
		return nil
	default:
	}
	t := time.NewTimer(writeGateWait)
	defer t.Stop()
	select {
	case g.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return errWriteGateTimeout
	}
}

func (g *writeGate) release() { <-g.slot }

// writes reports whether a statement modifies the database (and so takes
// SQLite's write lock outside a transaction). PRAGMAs are not gated: a WAL
// checkpoint runs beside writers and must never make a commit wait.
func writes(query string) bool {
	q := strings.TrimLeft(query, " \t\r\n(")
	if len(q) < 6 {
		return false
	}
	switch strings.ToUpper(q[:6]) {
	case "INSERT", "UPDATE", "DELETE", "REPLAC", "CREATE", "ANALYZ":
		return true
	}
	// WITH ... INSERT/UPDATE/DELETE and the like.
	if strings.EqualFold(q[:4], "WITH") {
		u := strings.ToUpper(q)
		return strings.Contains(u, "INSERT ") || strings.Contains(u, "UPDATE ") || strings.Contains(u, "DELETE ")
	}
	return false
}

// gatedTx releases the gate when its transaction ends.
type gatedTx struct {
	driver.Tx
	conn *stmtConn
}

func (t gatedTx) Commit() error {
	defer t.conn.endTx()
	return t.Tx.Commit()
}

func (t gatedTx) Rollback() error {
	defer t.conn.endTx()
	return t.Tx.Rollback()
}

// gatedRows releases the gate when a writing query's rows are closed
// (INSERT ... RETURNING).
type gatedRows struct {
	rowsExtras
	release func()
	once    sync.Once
}

func (r *gatedRows) Close() error {
	err := r.Rows.Close()
	r.once.Do(r.release)
	return err
}
