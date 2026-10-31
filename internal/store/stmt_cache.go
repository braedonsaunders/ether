package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
	"time"
)

func init() {
	probe, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		panic(err)
	}
	inner := probe.Driver()
	_ = probe.Close()
	sql.Register("sqlite-cached", &stmtDriver{inner: inner})
}

// stmtDriver hides the SQLite connection's Queryer and Execer so database/sql
// prepares each statement. Prepared statements are cached per connection, so a
// page does not re-parse SQL it already ran on that connection.
type stmtDriver struct{ inner driver.Driver }

func (d *stmtDriver) Open(name string) (driver.Conn, error) {
	inner, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return &stmtConn{inner: inner, free: map[string][]driver.Stmt{}, gate: gateFor(name)}, nil
}

const stmtPoolCap = 4

type stmtConn struct {
	inner driver.Conn
	mu    sync.Mutex
	free  map[string][]driver.Stmt
	// gate admits one writer at a time among this process's connections to
	// the database; inTx is set while this connection's write transaction
	// holds it. database/sql uses a connection from one goroutine at a time.
	gate *writeGate
	inTx bool
}

func (c *stmtConn) endTx() {
	if c.inTx {
		c.inTx = false
		c.gate.release()
	}
}

// admitWrite takes the gate for one statement outside a transaction. The
// returned release must be called when the statement is done.
func (c *stmtConn) admitWrite(ctx context.Context, query string) (func(), error) {
	if c.gate == nil || c.inTx || !writes(query) {
		return func() {}, nil
	}
	if err := c.gate.acquire(ctx); err != nil {
		return nil, err
	}
	return c.gate.release, nil
}

func (c *stmtConn) Prepare(query string) (driver.Stmt, error) {
	return c.prepare(query)
}

func (c *stmtConn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	return c.prepare(query)
}

func (c *stmtConn) prepare(query string) (driver.Stmt, error) {
	c.mu.Lock()
	if n := len(c.free[query]); n > 0 {
		stmt := c.free[query][n-1]
		c.free[query] = c.free[query][:n-1]
		c.mu.Unlock()
		return stmtHold{owner: c, query: query, inner: stmt}, nil
	}
	c.mu.Unlock()
	// Prepare off the cache lock so one in-flight query cannot block another
	// connection from taking a statement.
	prepared, err := c.inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	return stmtHold{owner: c, query: query, inner: prepared}, nil
}

func (c *stmtConn) release(query string, stmt driver.Stmt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.free == nil {
		c.free = map[string][]driver.Stmt{}
	}
	if len(c.free[query]) >= stmtPoolCap {
		_ = stmt.Close()
		return
	}
	c.free[query] = append(c.free[query], stmt)
}

func (c *stmtConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *stmtConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	// BEGIN IMMEDIATE is where a writer waits for SQLite's write lock.
	if rec := recorderFrom(ctx); rec != nil {
		start := time.Now()
		defer func() { rec.waited(time.Since(start)) }()
	}
	gated := c.gate != nil && !opts.ReadOnly
	if gated {
		if err := c.gate.acquire(ctx); err != nil {
			return nil, err
		}
	}
	var tx driver.Tx
	var err error
	if begin, ok := c.inner.(driver.ConnBeginTx); ok {
		tx, err = begin.BeginTx(ctx, opts)
	} else {
		tx, err = c.inner.Begin()
	}
	if !gated {
		return tx, err
	}
	if err != nil {
		c.gate.release()
		return nil, err
	}
	c.inTx = true
	return gatedTx{Tx: tx, conn: c}, nil
}

func (c *stmtConn) ResetSession(ctx context.Context) error {
	if reset, ok := c.inner.(driver.SessionResetter); ok {
		return reset.ResetSession(ctx)
	}
	return nil
}

func (c *stmtConn) Ping(ctx context.Context) error {
	if ping, ok := c.inner.(driver.Pinger); ok {
		return ping.Ping(ctx)
	}
	return nil
}

func (c *stmtConn) IsValid() bool {
	if valid, ok := c.inner.(driver.Validator); ok {
		return valid.IsValid()
	}
	return true
}

func (c *stmtConn) Close() error {
	c.mu.Lock()
	for _, list := range c.free {
		for _, stmt := range list {
			_ = stmt.Close()
		}
	}
	c.free = map[string][]driver.Stmt{}
	c.mu.Unlock()
	return c.inner.Close()
}

type stmtHold struct {
	owner *stmtConn
	query string
	inner driver.Stmt
}

func (s stmtHold) Close() error {
	s.owner.release(s.query, s.inner)
	return nil
}

func (s stmtHold) NumInput() int { return s.inner.NumInput() }

func (s stmtHold) Exec(args []driver.Value) (driver.Result, error) {
	return s.inner.Exec(args)
}

func (s stmtHold) Query(args []driver.Value) (driver.Rows, error) {
	return s.inner.Query(args)
}

func (s stmtHold) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	release, err := s.owner.admitWrite(ctx, s.query)
	if err != nil {
		return nil, err
	}
	defer release()
	if rec := recorderFrom(ctx); rec != nil {
		start := time.Now()
		res, err := s.exec(ctx, args)
		rec.record(s.query, 0, time.Since(start))
		rec.wrote()
		return res, err
	}
	return s.exec(ctx, args)
}

func (s stmtHold) exec(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if exec, ok := s.inner.(driver.StmtExecContext); ok {
		return exec.ExecContext(ctx, args)
	}
	values := make([]driver.Value, len(args))
	for i := range args {
		values[i] = args[i].Value
	}
	return s.inner.Exec(values)
}

func (s stmtHold) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	gated := s.owner.gate != nil && !s.owner.inTx && writes(s.query)
	release, err := s.owner.admitWrite(ctx, s.query)
	if err != nil {
		return nil, err
	}
	rows, err := s.queryRows(ctx, args)
	if err != nil {
		release()
		return nil, err
	}
	if gated {
		return &gatedRows{rowsExtras: rowsExtras{rows}, release: release}, nil
	}
	return rows, nil
}

func (s stmtHold) queryRows(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if rec := recorderFrom(ctx); rec != nil {
		start := time.Now()
		rows, err := s.run(ctx, args)
		if err != nil {
			rec.record(s.query, 0, time.Since(start))
			return nil, err
		}
		return countRows(rows, rec, s.query, time.Since(start)), nil
	}
	return s.run(ctx, args)
}

func (s stmtHold) run(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if query, ok := s.inner.(driver.StmtQueryContext); ok {
		return query.QueryContext(ctx, args)
	}
	values := make([]driver.Value, len(args))
	for i := range args {
		values[i] = args[i].Value
	}
	return s.inner.Query(values)
}
