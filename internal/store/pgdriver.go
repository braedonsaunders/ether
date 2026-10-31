package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// PostgresDriverName is the database/sql driver registered by this package:
// pgx's stdlib driver with `?` placeholders rebound to $n at execute time,
// so components keep one SQL text for both engines.
const PostgresDriverName = "ether-pgx"

func init() { sql.Register(PostgresDriverName, &pgDriver{}) }

var sharedRebind rebindCache

// pgDriver is the registered driver; Open takes a pgx connection string.
type pgDriver struct{}

func (d *pgDriver) Open(name string) (driver.Conn, error) {
	c, err := d.OpenConnector(name)
	if err != nil {
		return nil, err
	}
	return c.Connect(context.Background())
}

func (d *pgDriver) OpenConnector(name string) (driver.Connector, error) {
	cfg, err := parsePostgresConfig(name)
	if err != nil {
		return nil, err
	}
	return newPGConnector(cfg, nil), nil
}

// pgConnector wraps pgx's connector. onClose, when set, runs once from
// sql.DB.Close (database/sql calls Close on connectors that implement
// io.Closer) — the test harness drops its throwaway schema there.
type pgConnector struct {
	inner   driver.Connector
	onClose func() error
}

func newPGConnector(cfg *pgx.ConnConfig, onClose func() error) *pgConnector {
	return &pgConnector{inner: stdlib.GetConnector(*cfg), onClose: onClose}
}

var _ io.Closer = (*pgConnector)(nil)

func (c *pgConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &pgConn{inner: conn.(*stdlib.Conn)}, nil
}

func (c *pgConnector) Driver() driver.Driver { return &pgDriver{} }

func (c *pgConnector) Close() error {
	if c.onClose == nil {
		return nil
	}
	f := c.onClose
	c.onClose = nil
	return f()
}

// pgConn forwards to the pgx stdlib connection, rebinding every query.
type pgConn struct{ inner *stdlib.Conn }

var (
	_ driver.Conn               = (*pgConn)(nil)
	_ driver.ConnPrepareContext = (*pgConn)(nil)
	_ driver.ConnBeginTx        = (*pgConn)(nil)
	_ driver.ExecerContext      = (*pgConn)(nil)
	_ driver.QueryerContext     = (*pgConn)(nil)
	_ driver.Pinger             = (*pgConn)(nil)
	_ driver.SessionResetter    = (*pgConn)(nil)
	_ driver.NamedValueChecker  = (*pgConn)(nil)
)

// PgxConn exposes the underlying pgx connection (for use via sql.Conn.Raw).
func (c *pgConn) PgxConn() *pgx.Conn { return c.inner.Conn() }

func (c *pgConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(sharedRebind.get(query))
}

func (c *pgConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	return c.inner.PrepareContext(ctx, sharedRebind.get(query))
}

func (c *pgConn) Close() error { return c.inner.Close() }

func (c *pgConn) Begin() (driver.Tx, error) {
	return c.inner.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *pgConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if rec := recorderFrom(ctx); rec != nil {
		start := time.Now()
		defer func() { rec.waited(time.Since(start)) }()
	}
	return c.inner.BeginTx(ctx, opts)
}

func (c *pgConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if rec := recorderFrom(ctx); rec != nil {
		start := time.Now()
		defer func() {
			rec.record(query, 0, time.Since(start))
			rec.wrote()
		}()
	}
	res, err := c.inner.ExecContext(ctx, sharedRebind.get(query), args)
	if err != nil {
		return nil, err
	}
	return pgResult{res}, nil
}

func (c *pgConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rec := recorderFrom(ctx)
	if rec == nil {
		return c.inner.QueryContext(ctx, sharedRebind.get(query), args)
	}
	start := time.Now()
	rows, err := c.inner.QueryContext(ctx, sharedRebind.get(query), args)
	if err != nil {
		rec.record(query, 0, time.Since(start))
		return nil, err
	}
	return countRows(rows, rec, query, time.Since(start)), nil
}

func (c *pgConn) Ping(ctx context.Context) error { return c.inner.Ping(ctx) }

func (c *pgConn) ResetSession(ctx context.Context) error { return c.inner.ResetSession(ctx) }

// CheckNamedValue passes values straight to pgx, except bool: the schema
// stores flags in BIGINT columns (as SQLite stores them in INTEGER), so a
// Go bool binds as 0/1 the way it does on SQLite.
func (c *pgConn) CheckNamedValue(nv *driver.NamedValue) error {
	if b, ok := nv.Value.(bool); ok {
		if b {
			nv.Value = int64(1)
		} else {
			nv.Value = int64(0)
		}
	}
	return nil
}

// ErrNoLastInsertID is returned by Result.LastInsertId on PostgreSQL; use
// INSERT ... RETURNING instead.
var ErrNoLastInsertID = errors.New("store: LastInsertId is not supported on PostgreSQL; use RETURNING")

type pgResult struct{ driver.Result }

func (r pgResult) LastInsertId() (int64, error) { return 0, ErrNoLastInsertID }
