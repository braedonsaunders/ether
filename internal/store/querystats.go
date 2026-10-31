package store

import (
	"context"
	"database/sql/driver"
	"io"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// QueryStats accumulates the database work done on behalf of one unit of
// work, usually one HTTP request. Both drivers (SQLite and PostgreSQL)
// record into the QueryStats carried by the statement's context, so a
// handler's cost is measured where it is paid: statement count, rows read
// back into Go, time inside the driver, and time spent waiting for the write
// lock. It is safe for concurrent use; a request may fan out.
//
// The counters are what the performance gate asserts on: a request path
// whose statement or row count grows with the library is O(library).
type QueryStats struct {
	queries  atomic.Int64
	rows     atomic.Int64
	dbNanos  atomic.Int64
	lockWait atomic.Int64

	mu    sync.Mutex
	stmts map[string]*StatementStat
}

// StatementStat is one SQL text's share of a QueryStats.
type StatementStat struct {
	SQL string `json:"sql"`
	// Site is the first caller outside database/sql and this package, set
	// only while CaptureCallSites is on (the performance harness).
	Site  string        `json:"site,omitempty"`
	Count int64         `json:"count"`
	Rows  int64         `json:"rows"`
	Time  time.Duration `json:"time"`
}

type queryStatsKey struct{}

// WithQueryStats returns ctx carrying a fresh QueryStats. Work already
// carrying stats keeps recording into the outer one as well, so a nested
// measurement never hides cost from the request that paid for it.
func WithQueryStats(ctx context.Context) (context.Context, *QueryStats) {
	s := &QueryStats{}
	if outer := QueryStatsFrom(ctx); outer != nil {
		return context.WithValue(ctx, queryStatsKey{}, &chainedStats{inner: s, outer: outer}), s
	}
	return context.WithValue(ctx, queryStatsKey{}, s), s
}

// QueryStatsFrom returns the QueryStats ctx carries, or nil.
func QueryStatsFrom(ctx context.Context) *QueryStats {
	switch v := ctx.Value(queryStatsKey{}).(type) {
	case *QueryStats:
		return v
	case *chainedStats:
		return v.inner
	}
	return nil
}

// recorder is what a driver records into: a QueryStats, or a nested pair.
type recorder interface {
	record(query string, rows int64, d time.Duration)
	waited(d time.Duration)
}

type chainedStats struct{ inner, outer *QueryStats }

func (c *chainedStats) record(q string, rows int64, d time.Duration) {
	c.inner.record(q, rows, d)
	c.outer.record(q, rows, d)
}

func (c *chainedStats) waited(d time.Duration) {
	c.inner.waited(d)
	c.outer.waited(d)
}

func recorderFrom(ctx context.Context) recorder {
	if ctx == nil {
		return nil
	}
	switch v := ctx.Value(queryStatsKey{}).(type) {
	case *QueryStats:
		return v
	case *chainedStats:
		return v
	}
	return nil
}

var captureSites atomic.Bool

// CaptureCallSites makes every statement's first execution in a QueryStats
// record where it was issued. It costs a stack walk per new statement, so
// only the performance harness turns it on.
func CaptureCallSites(on bool) { captureSites.Store(on) }

// callSite returns "file:line" of the first frame outside the runtime,
// database/sql and this package.
func callSite() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		fn := f.Function
		if !strings.HasPrefix(fn, "runtime.") && !strings.HasPrefix(fn, "database/sql.") && !strings.HasPrefix(fn, storePackage) {
			file := f.File
			if i := strings.Index(file, "/internal/"); i >= 0 {
				file = file[i+1:]
			} else if i := strings.Index(file, "/cmd/"); i >= 0 {
				file = file[i+1:]
			}
			return file + ":" + strconv.Itoa(f.Line)
		}
		if !more {
			return ""
		}
	}
}

const storePackage = "github.com/braedonsaunders/ether/internal/store."

// maxTrackedStatements bounds the per-statement map: a request that runs
// thousands of distinct SQL texts (an IN list built per call) is still
// counted in the totals; only the breakdown stops growing.
const maxTrackedStatements = 256

func (s *QueryStats) record(query string, rows int64, d time.Duration) {
	s.queries.Add(1)
	s.rows.Add(rows)
	s.dbNanos.Add(int64(d))
	s.mu.Lock()
	st := s.stmts[query]
	if st == nil && len(s.stmts) < maxTrackedStatements {
		if s.stmts == nil {
			s.stmts = make(map[string]*StatementStat)
		}
		st = &StatementStat{SQL: query}
		if captureSites.Load() {
			st.Site = callSite()
		}
		s.stmts[query] = st
	}
	if st != nil {
		st.Count++
		st.Rows += rows
		st.Time += d
	}
	s.mu.Unlock()
}

func (s *QueryStats) waited(d time.Duration) { s.lockWait.Add(int64(d)) }

// Queries is the number of statements executed.
func (s *QueryStats) Queries() int64 { return s.queries.Load() }

// Rows is the number of result rows read back from the database.
func (s *QueryStats) Rows() int64 { return s.rows.Load() }

// DBTime is the time spent inside the driver (executing, stepping rows).
func (s *QueryStats) DBTime() time.Duration { return time.Duration(s.dbNanos.Load()) }

// LockWait is the time spent waiting to begin a transaction (SQLite's write
// lock, a PostgreSQL BEGIN).
func (s *QueryStats) LockWait() time.Duration { return time.Duration(s.lockWait.Load()) }

// Statements returns the per-SQL breakdown, most expensive first.
func (s *QueryStats) Statements() []StatementStat {
	s.mu.Lock()
	out := make([]StatementStat, 0, len(s.stmts))
	for _, st := range s.stmts {
		out = append(out, *st)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time > out[j].Time
		}
		return out[i].SQL < out[j].SQL
	})
	return out
}

// countedRows wraps driver rows to count the rows read and the time spent
// stepping them, and records the statement when the rows are closed.
type countedRows struct {
	driver.Rows
	rec   recorder
	query string
	n     int64
	spent time.Duration
	done  bool
}

func (r *countedRows) Next(dest []driver.Value) error {
	start := time.Now()
	err := r.Rows.Next(dest)
	r.spent += time.Since(start)
	if err == nil {
		r.n++
	} else if err == io.EOF {
		r.finish()
	}
	return err
}

func (r *countedRows) Close() error {
	start := time.Now()
	err := r.Rows.Close()
	r.spent += time.Since(start)
	r.finish()
	return err
}

func (r *countedRows) finish() {
	if r.done {
		return
	}
	r.done = true
	r.rec.record(r.query, r.n, r.spent)
}

// The optional column and result-set interfaces are always implemented and
// fall back to database/sql's own defaults when the wrapped rows lack them,
// so wrapping never changes what ColumnTypes or NextResultSet report.

func (r *countedRows) HasNextResultSet() bool {
	if n, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return n.HasNextResultSet()
	}
	return false
}

func (r *countedRows) NextResultSet() error {
	if n, ok := r.Rows.(driver.RowsNextResultSet); ok {
		return n.NextResultSet()
	}
	return io.EOF
}

func (r *countedRows) ColumnTypeDatabaseTypeName(i int) string {
	if c, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return c.ColumnTypeDatabaseTypeName(i)
	}
	return ""
}

var anyType = reflect.TypeFor[any]()

func (r *countedRows) ColumnTypeScanType(i int) reflect.Type {
	if c, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return c.ColumnTypeScanType(i)
	}
	return anyType
}

func (r *countedRows) ColumnTypeNullable(i int) (nullable, ok bool) {
	if c, ok := r.Rows.(driver.RowsColumnTypeNullable); ok {
		return c.ColumnTypeNullable(i)
	}
	return false, false
}

func (r *countedRows) ColumnTypeLength(i int) (length int64, ok bool) {
	if c, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return c.ColumnTypeLength(i)
	}
	return 0, false
}

func (r *countedRows) ColumnTypePrecisionScale(i int) (precision, scale int64, ok bool) {
	if c, ok := r.Rows.(driver.RowsColumnTypePrecisionScale); ok {
		return c.ColumnTypePrecisionScale(i)
	}
	return 0, 0, false
}

// countRows wraps rows for rec, starting from the time already spent
// executing the query.
func countRows(rows driver.Rows, rec recorder, query string, spent time.Duration) driver.Rows {
	return &countedRows{Rows: rows, rec: rec, query: query, spent: spent}
}
