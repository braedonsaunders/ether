package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// minServerVersion is PostgreSQL 14 (server_version_num).
const minServerVersion = 140000

// migrationLockKey is the first half of the two-key transaction advisory
// lock taken while migrating ("Ethr"); the second half is a hash of the
// target schema, so nodes sharing one schema migrate once while unrelated
// schemas (test runs) do not serialise on each other.
const migrationLockKey int32 = 0x45746872

var reQueryPassword = regexp.MustCompile(`(?i)((?:^|[?&])(?:ssl)?password=)[^&]*`)

func isPostgresURL(u string) bool {
	u = strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(u, "postgres://") || strings.HasPrefix(u, "postgresql://")
}

// RedactURL hides the password (and any password query parameter) in a
// database URL so it can be logged or shown. Non-URL text that looks like a
// key=value DSN has its password value masked.
func RedactURL(raw string) string {
	if raw == "" {
		return ""
	}
	// A database URL has no fragment: one means a password holding an
	// unencoded '#' (or '@' and '#') that url.Parse split off, leaving
	// part or all of the password outside the userinfo it would mask.
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" && u.Fragment == "" && !strings.Contains(raw, "#") {
		if u.User != nil {
			if _, has := u.User.Password(); has {
				u.User = url.UserPassword(u.User.Username(), "xxxxx")
			}
		}
		q := u.Query()
		for k := range q {
			if strings.EqualFold(k, "password") || strings.EqualFold(k, "sslpassword") {
				q.Set(k, "xxxxx")
			}
		}
		u.RawQuery = q.Encode()
		return u.String()
	}
	if i := strings.Index(raw, "://"); i >= 0 {
		// Unparseable: mask everything between the user name's ':' and the
		// last '@', which may itself be part of the password.
		rest := raw[i+3:]
		if at := strings.LastIndexByte(rest, '@'); at >= 0 {
			if c := strings.IndexByte(rest[:at], ':'); c >= 0 && !strings.ContainsAny(rest[:c], "/?#") {
				rest = rest[:c+1] + "xxxxx" + rest[at:]
			}
		}
		return raw[:i+3] + reQueryPassword.ReplaceAllString(rest, "${1}xxxxx")
	}
	fields := strings.Fields(raw)
	for i, f := range fields {
		if k, _, ok := strings.Cut(f, "="); ok && strings.Contains(strings.ToLower(k), "password") {
			fields[i] = k + "=xxxxx"
		}
	}
	return strings.Join(fields, " ")
}

// parsePostgresConfig requires verified TLS unless the owner explicitly selects
// another mode in the connection string. pgx otherwise defaults to "prefer",
// which silently retries without encryption. A default supplied in the DSN also
// takes precedence over ambient PGSSLMODE, keeping service behavior predictable.
func parsePostgresConfig(raw string) (*pgx.ConnConfig, error) {
	if isPostgresURL(raw) {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, err
		}
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return nil, err
		}
		if q.Get("sslmode") == "" {
			q.Set("sslmode", "verify-full")
			u.RawQuery = q.Encode()
		}
		raw = u.String()
	} else {
		// pgx's keyword parser uses the last occurrence, preserving an
		// explicit owner mode while providing a secure default otherwise.
		raw = "sslmode=verify-full " + raw
	}
	return pgx.ParseConfig(raw)
}

// openPostgres connects to an owner-supplied PostgreSQL database, checks
// the server and migrates the schema under a cluster-wide lock.
func openPostgres(ctx context.Context, rawURL string, log *slog.Logger, gate MigrationGate) (*sql.DB, error) {
	cfg, err := parsePostgresConfig(rawURL)
	if err != nil {
		// pgx errors can echo the connection string; never surface it.
		return nil, fmt.Errorf("store: invalid postgres URL %s", RedactURL(rawURL))
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	if cfg.RuntimeParams["application_name"] == "" {
		cfg.RuntimeParams["application_name"] = "ether"
	}
	db := sql.OpenDB(newPGConnector(cfg, nil))
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := preparePostgres(ctx, db, log.With("database", RedactURL(rawURL)), gate); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// preparePostgres verifies the server and applies migrations.
func preparePostgres(ctx context.Context, db *sql.DB, log *slog.Logger, gate MigrationGate) error {
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return fmt.Errorf("store: connect to postgres: %w", err)
	}
	if err := checkPostgresServer(ctx, db, log); err != nil {
		return err
	}
	if err := migratePostgres(ctx, db, gate); err != nil {
		return err
	}
	warnSchemaAhead(ctx, db, log, "")
	return nil
}

// checkPostgresServer requires PostgreSQL 14+ and warns when the database
// collation would order text differently from SQLite's byte order.
func checkPostgresServer(ctx context.Context, db *sql.DB, log *slog.Logger) error {
	var num string
	if err := db.QueryRowContext(ctx, `SHOW server_version_num`).Scan(&num); err != nil {
		return fmt.Errorf("store: read postgres version: %w", err)
	}
	v, _ := strconv.Atoi(strings.TrimSpace(num))
	if v < minServerVersion {
		return fmt.Errorf("store: PostgreSQL %s is too old; Ether requires 14 or newer", num)
	}
	var collate string
	if err := db.QueryRowContext(ctx, `SELECT datcollate FROM pg_database WHERE datname = current_database()`).Scan(&collate); err != nil {
		log.Warn("store: could not read database collation", "err", err)
		return nil
	}
	if !byteOrderCollation(collate) {
		log.Warn("store: PostgreSQL database collation is not C; text ordering will differ from standalone servers. Create the database with LC_COLLATE 'C' (or the builtin C.UTF-8 locale).", "collation", collate)
	}
	return nil
}

func byteOrderCollation(c string) bool {
	switch strings.ToUpper(strings.TrimSpace(c)) {
	case "C", "POSIX", "C.UTF-8", "C.UTF8":
		return true
	}
	return false
}

// migratePostgres applies all pending migrations in one transaction holding
// a transaction-scoped advisory lock, so concurrently starting nodes
// migrate exactly once and a failed migration leaves nothing half-applied.
// gate, when set, is consulted under the lock before anything is applied.
func migratePostgres(ctx context.Context, db *sql.DB, gate MigrationGate) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(?, hashtext(current_schema()))`, migrationLockKey); err != nil {
		return fmt.Errorf("store: migration lock: %w", err)
	}
	if _, err := tx.ExecContext(ctx, TranslateDDL(createMigrationsTable)); err != nil {
		return err
	}
	applied := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT component, version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c string
		var v int
		if err := rows.Scan(&c, &v); err != nil {
			rows.Close()
			return err
		}
		applied[c+"/"+strconv.Itoa(v)] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if gate != nil {
		var pending []string
		for _, m := range registered() {
			if key := m.component + "/" + strconv.Itoa(m.version); !applied[key] {
				pending = append(pending, key)
			}
		}
		if len(pending) > 0 {
			if err := gate(ctx, tx, pending); err != nil {
				return err
			}
		}
	}
	seeded := map[string]bool{}
	for _, m := range registered() {
		key := m.component + "/" + strconv.Itoa(m.version)
		if applied[key] {
			continue
		}
		for _, match := range reSeedInsert.FindAllStringSubmatch(m.sqlFor(Postgres), -1) {
			seeded[match[1]] = true
			seeded[strings.ToLower(match[1])] = true // PostgreSQL folds unquoted identifiers.
		}
		// No arguments: pgx sends this over the simple query protocol, which
		// accepts several ;-separated statements in one round trip.
		// sqliteApply is not called on PostgreSQL. sqlFor returns the
		// RegisterDialect override when one exists (auth/4 drops NOT NULL).
		if _, err := tx.ExecContext(ctx, m.sqlFor(Postgres)); err != nil {
			return fmt.Errorf("migration %s/%d: %w", m.component, m.version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(component, version) VALUES (?, ?)`, m.component, m.version); err != nil {
			return err
		}
		applied[key] = true
	}
	if len(seeded) > 0 {
		if err := advanceIdentities(ctx, tx, seeded); err != nil {
			return err
		}
	}
	return tx.Commit()
}
