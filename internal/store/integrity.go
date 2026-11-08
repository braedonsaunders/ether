package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// maxIntegrityFindings bounds what an integrity check keeps: one damaged
// page can produce thousands of lines, and the first few say enough.
const maxIntegrityFindings = 10

// Integrity is the result of an integrity check of a SQLite database.
type Integrity struct {
	// OK is false when the database is damaged.
	OK bool `json:"ok"`
	// Skipped is set on PostgreSQL, which owns its own physical integrity
	// (OK is then true).
	Skipped bool `json:"skipped,omitempty"`
	// Findings are SQLite's first problem reports when OK is false.
	Findings []string  `json:"findings,omitempty"`
	Checked  time.Time `json:"checked"`
	Took     float64   `json:"tookSeconds"`
}

// CorruptionError is returned by Maintain when the database fails its
// integrity check.
type CorruptionError struct{ Integrity Integrity }

func (e *CorruptionError) Error() string {
	return "database integrity check failed: " + strings.Join(e.Integrity.Findings, "; ")
}

// QuickCheck runs SQLite's PRAGMA quick_check, which reads every page and
// catches the "database disk image is malformed" class of damage without
// integrity_check's index cross-checks (about twice as fast). It never
// fails on damage: a database too broken for the check to run is reported
// as a finding. The error is only for a check that could not run at all
// (cancelled, or the database is closed). PostgreSQL is skipped.
func QuickCheck(ctx context.Context, db *sql.DB) (Integrity, error) {
	start := time.Now()
	res := Integrity{OK: true, Checked: start.UTC()}
	if err := db.PingContext(ctx); err != nil {
		res.Took = time.Since(start).Seconds()
		return res, err
	}
	if DialectOf(db) == Postgres {
		res.Skipped = true
		return res, nil
	}
	// One snapshot of every page: bulk writers wait it out (WaitWALBudget)
	// instead of growing a WAL the checkpointer cannot drain meanwhile.
	defer BeginLongRead(db, "the database integrity check")()
	// FTS5 xIntegrity can reuse a stale segment structure on a pooled
	// connection after another connection merges segments. Use a new reader
	// of the same WAL database, not a copy or immutable view. The PRAGMA holds
	// one SQLite read snapshot for the whole check.
	checkDB, closeCheck, err := integrityReader(ctx, db)
	defer closeCheck()
	var rows *sql.Rows
	if err == nil {
		rows, err = checkDB.QueryContext(ctx, `PRAGMA quick_check(`+fmt.Sprint(maxIntegrityFindings)+`)`)
	}
	if err == nil {
		for rows.Next() {
			var line string
			if err = rows.Scan(&line); err != nil {
				break
			}
			if !strings.EqualFold(line, "ok") {
				res.Findings = append(res.Findings, line)
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
	}
	res.Took = time.Since(start).Seconds()
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, sql.ErrConnDone) || strings.Contains(err.Error(), "database is closed") {
			return res, err
		}
		if !IsCorrupt(err) {
			return res, err
		}
		res.Findings = append(res.Findings, err.Error())
	}
	res.OK = len(res.Findings) == 0
	return res, nil
}

// IsCorrupt reports whether err is SQLite reporting a damaged or foreign
// file (SQLITE_CORRUPT or SQLITE_NOTADB).
func IsCorrupt(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "malformed") || strings.Contains(msg, "not a database") ||
		strings.Contains(msg, "sqlite_corrupt") || strings.Contains(msg, "sqlite_notadb") ||
		strings.Contains(msg, "database corruption")
}

// integrityReader leaves in-memory databases on their original connection:
// there is no file to reopen. File readers use the same pinned SQLite driver
// without the production pool's statement or virtual-table caches.
func integrityReader(ctx context.Context, db *sql.DB) (*sql.DB, func(), error) {
	var path string
	err := db.QueryRowContext(ctx, `SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path)
	if err != nil {
		return nil, func() {}, err
	}
	if path == "" {
		return db, func() {}, nil
	}
	reader, err := sql.Open("sqlite", integrityReaderDSN(path))
	if err != nil {
		return nil, func() {}, err
	}
	reader.SetMaxOpenConns(1)
	return reader, func() { _ = reader.Close() }, nil
}

// integrityReaderDSN is the read-only SQLite URI of a database file.
func integrityReaderDSN(path string) string {
	q := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(3000)"}}
	return sqliteFileURI(path) + "?" + q.Encode()
}

// sqliteFileURI is a database path as an SQLite file: URI, escaped (a data
// directory may contain '#', '?' or '%'). A Windows drive path (C:\...)
// becomes file:///C:/..., the form SQLite's Windows VFS documents; given to
// url.URL as is it renders as file://C:%5C..., which SQLite reads as a URI
// authority and refuses.
func sqliteFileURI(path string) string {
	if len(path) >= 2 && path[1] == ':' && 'a' <= path[0]|0x20 && path[0]|0x20 <= 'z' {
		path = "/" + strings.ReplaceAll(path, `\`, "/")
	}
	return (&url.URL{Scheme: "file", Path: path}).String()
}
