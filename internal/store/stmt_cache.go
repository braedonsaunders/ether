package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"sync"
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
	return &stmtConn{inner: inner, stmts: map[string]*cachedStmt{}}, nil
}

const stmtCacheCap = 512

type cachedStmt struct {
	stmt driver.Stmt
	busy bool
}

type stmtConn struct {
	inner driver.Conn
	mu    sync.Mutex
	stmts map[string]*cachedStmt
}

func (c *stmtConn) Prepare(query string) (driver.Stmt, error) {
	return c.prepare(query)
}

func (c *stmtConn) PrepareContext(_ context.Context, query string) (driver.Stmt, error) {
	return c.prepare(query)
}

func (c *stmtConn) prepare(query string) (driver.Stmt, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.stmts[query]; s != nil && !s.busy {
		s.busy = true
		return stmtHold{owner: c, query: query, inner: s.stmt}, nil
	}
	prepared, err := c.inner.Prepare(query)
	if err != nil {
		return nil, err
	}
	if c.stmts[query] == nil {
		if len(c.stmts) >= stmtCacheCap {
			for k, s := range c.stmts {
				if s.busy {
					continue
				}
				_ = s.stmt.Close()
				delete(c.stmts, k)
				break
			}
		}
		if len(c.stmts) < stmtCacheCap {
			c.stmts[query] = &cachedStmt{stmt: prepared, busy: true}
			return stmtHold{owner: c, query: query, inner: prepared}, nil
		}
	}
	return oneShotStmt{inner: prepared}, nil
}

func (c *stmtConn) release(query string) {
	c.mu.Lock()
	if s := c.stmts[query]; s != nil {
		s.busy = false
	}
	c.mu.Unlock()
}

func (c *stmtConn) Begin() (driver.Tx, error) { return c.inner.Begin() }

func (c *stmtConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if begin, ok := c.inner.(driver.ConnBeginTx); ok {
		return begin.BeginTx(ctx, opts)
	}
	return c.inner.Begin()
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
	for _, s := range c.stmts {
		_ = s.stmt.Close()
	}
	c.stmts = map[string]*cachedStmt{}
	c.mu.Unlock()
	return c.inner.Close()
}

type stmtHold struct {
	owner *stmtConn
	query string
	inner driver.Stmt
}

func (s stmtHold) Close() error {
	s.owner.release(s.query)
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
	if query, ok := s.inner.(driver.StmtQueryContext); ok {
		return query.QueryContext(ctx, args)
	}
	values := make([]driver.Value, len(args))
	for i := range args {
		values[i] = args[i].Value
	}
	return s.inner.Query(values)
}

type oneShotStmt struct{ inner driver.Stmt }

func (s oneShotStmt) Close() error { return s.inner.Close() }
func (s oneShotStmt) NumInput() int { return s.inner.NumInput() }
func (s oneShotStmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.inner.Exec(args)
}
func (s oneShotStmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.inner.Query(args)
}
func (s oneShotStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	if exec, ok := s.inner.(driver.StmtExecContext); ok {
		return exec.ExecContext(ctx, args)
	}
	values := make([]driver.Value, len(args))
	for i := range args {
		values[i] = args[i].Value
	}
	return s.inner.Exec(values)
}
func (s oneShotStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	if query, ok := s.inner.(driver.StmtQueryContext); ok {
		return query.QueryContext(ctx, args)
	}
	values := make([]driver.Value, len(args))
	for i := range args {
		values[i] = args[i].Value
	}
	return s.inner.Query(values)
}
