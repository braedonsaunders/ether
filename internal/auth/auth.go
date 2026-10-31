// Package auth implements core.Auth: it maps the X-Plex-Token a Plex app
// presents to an Ether user, and keeps the users table with per-user
// settings (stream limit, content rating cap, languages, sections).
//
// A token is recognised, in order, as
//
//  1. the server's own token (the owner),
//  2. a server access token synced from plex.tv for the owner or a friend,
//  3. any other plex.tv token, validated against plex.tv (GET /api/v2/user)
//     and checked for access to this server (owner id, synced friend, or
//     the server in that token's resources list).
//
// Results of (3) are cached (positive and negative) so plex.tv is asked at
// most once per token per TTL; during a plex.tv outage a previously valid
// token keeps working for StaleTTL.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/clusterbus"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/flight"
	_ "github.com/braedonsaunders/ether/internal/identity/schema"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("auth", 1, `
CREATE TABLE auth_users (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	plex_id            INTEGER NOT NULL UNIQUE,
	uuid               TEXT    NOT NULL DEFAULT '',
	username           TEXT    NOT NULL DEFAULT '',
	title              TEXT    NOT NULL DEFAULT '',
	email              TEXT    NOT NULL DEFAULT '',
	thumb              TEXT    NOT NULL DEFAULT '',
	is_owner           INTEGER NOT NULL DEFAULT 0,
	is_home            INTEGER NOT NULL DEFAULT 0,
	restricted         INTEGER NOT NULL DEFAULT 0,
	active             INTEGER NOT NULL DEFAULT 1,
	origin             TEXT    NOT NULL DEFAULT 'friend',
	plex_sections      TEXT    NOT NULL DEFAULT '[]',
	stream_limit       INTEGER NOT NULL DEFAULT 0,
	max_content_rating TEXT    NOT NULL DEFAULT '',
	subtitle_languages TEXT    NOT NULL DEFAULT '[]',
	audio_languages    TEXT    NOT NULL DEFAULT '[]',
	allowed_sections   TEXT    NOT NULL DEFAULT '[]',
	created_at         INTEGER NOT NULL,
	updated_at         INTEGER NOT NULL,
	last_seen_at       INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE auth_tokens (
	token_hash TEXT    PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
	source     TEXT    NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE INDEX auth_tokens_user ON auth_tokens(user_id);
`)
	// At most one owner, enforced by the database so concurrent SetOwner
	// calls on different nodes cannot both win. Older databases could hold
	// several owner rows; keep the most recently updated one.
	store.Register("auth", 2, `
UPDATE auth_users SET is_owner=0 WHERE is_owner=1 AND id <> (
	SELECT id FROM auth_users WHERE is_owner=1 ORDER BY updated_at DESC, id DESC LIMIT 1);
CREATE UNIQUE INDEX auth_users_one_owner ON auth_users(is_owner) WHERE is_owner=1;
`)
	// SQLite enforces foreign keys for the whole migration transaction, and
	// PRAGMA foreign_keys cannot be changed inside one. auth_tokens and
	// auth_delegations exist by version 3 and are copied below. auth_sessions
	// is created only after every auth version on a fresh database, but a
	// delivered 9503 database already has it referencing auth_users with
	// ON DELETE CASCADE. RegisterAfter cannot run anything between auth
	// versions, so the SQLite apply preserves that table when it is present.
	store.Register("auth", 4, sqliteAuthV4)
	store.RegisterDialect("auth", 4, store.Postgres, `ALTER TABLE auth_users ALTER COLUMN plex_id DROP NOT NULL;`)
	store.RegisterSQLiteApply("auth", 4, applySQLiteAuthV4)

}

// sqliteAuthV4 rebuilds auth_users so plex_id may be null. It copies the
// dependents that version 3 always has. auth_sessions is not named here:
// a fresh database does not have that table yet.
const sqliteAuthV4 = `
CREATE TEMP TABLE auth_tokens_saved AS SELECT * FROM auth_tokens;
CREATE TEMP TABLE auth_delegations_saved AS SELECT * FROM auth_delegations;
DROP TABLE auth_tokens;
DROP TABLE auth_delegations;
CREATE TABLE auth_users_new (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	plex_id            INTEGER UNIQUE,
	uuid               TEXT    NOT NULL DEFAULT '',
	username           TEXT    NOT NULL DEFAULT '',
	title              TEXT    NOT NULL DEFAULT '',
	email              TEXT    NOT NULL DEFAULT '',
	thumb              TEXT    NOT NULL DEFAULT '',
	is_owner           INTEGER NOT NULL DEFAULT 0,
	is_home            INTEGER NOT NULL DEFAULT 0,
	restricted         INTEGER NOT NULL DEFAULT 0,
	active             INTEGER NOT NULL DEFAULT 1,
	origin             TEXT    NOT NULL DEFAULT 'friend',
	plex_sections      TEXT    NOT NULL DEFAULT '[]',
	stream_limit       INTEGER NOT NULL DEFAULT 0,
	max_content_rating TEXT    NOT NULL DEFAULT '',
	subtitle_languages TEXT    NOT NULL DEFAULT '[]',
	audio_languages    TEXT    NOT NULL DEFAULT '[]',
	allowed_sections   TEXT    NOT NULL DEFAULT '[]',
	created_at         INTEGER NOT NULL,
	updated_at         INTEGER NOT NULL,
	last_seen_at       INTEGER NOT NULL DEFAULT 0
);
INSERT INTO auth_users_new SELECT * FROM auth_users;
DROP TABLE auth_users;
ALTER TABLE auth_users_new RENAME TO auth_users;
CREATE UNIQUE INDEX auth_users_one_owner ON auth_users(is_owner) WHERE is_owner=1;
CREATE TABLE auth_tokens (token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE, source TEXT NOT NULL, created_at INTEGER NOT NULL);
INSERT INTO auth_tokens SELECT * FROM auth_tokens_saved;
DROP TABLE auth_tokens_saved;
CREATE INDEX auth_tokens_user ON auth_tokens(user_id);
CREATE TABLE auth_delegations (token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE, expires_at INTEGER NOT NULL);
INSERT INTO auth_delegations SELECT * FROM auth_delegations_saved;
DROP TABLE auth_delegations_saved;
CREATE INDEX auth_delegations_user ON auth_delegations(user_id);
`

var sqliteAuthSessionColumns = []string{"token_hash", "user_id", "created_at", "expires_at"}

// applySQLiteAuthV4 is the SQLite body of auth/4. Delivered databases keep
// every existing auth_sessions row, including expired ones still stored.
// Revoked sessions are already deleted and are not inserted. Tables other
// than tokens, delegations, and sessions that reference auth_users stop the
// rebuild; this binary does not have a proven copy plan for them.
func applySQLiteAuthV4(ctx context.Context, tx *sql.Tx) error {
	children, err := sqliteTablesReferencing(ctx, tx, "auth_users")
	if err != nil {
		return err
	}
	var unexpected []string
	sessions := false
	for _, name := range children {
		switch name {
		case "auth_tokens", "auth_delegations":
		case "auth_sessions":
			sessions = true
		default:
			unexpected = append(unexpected, name)
		}
	}
	if len(unexpected) > 0 {
		// Before any copy or DROP. An unhandled child must not cascade.
		return fmt.Errorf("auth/4: refusing rebuild because unhandled tables reference auth_users: %s", strings.Join(unexpected, ", "))
	}
	if sessions {
		cols, err := sqliteTableColumns(ctx, tx, "auth_sessions")
		if err != nil {
			return err
		}
		if strings.Join(cols, ",") != strings.Join(sqliteAuthSessionColumns, ",") {
			return fmt.Errorf("auth/4: auth_sessions columns are %s, want %s", strings.Join(cols, ","), strings.Join(sqliteAuthSessionColumns, ","))
		}
		if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE auth_sessions_saved AS SELECT token_hash, user_id, created_at, expires_at FROM auth_sessions`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, sqliteAuthV4); err != nil {
		return err
	}
	if !sessions {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO auth_sessions(token_hash, user_id, created_at, expires_at)
 SELECT token_hash, user_id, created_at, expires_at FROM auth_sessions_saved`); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `DROP TABLE auth_sessions_saved`)
	return err
}

func sqliteTablesReferencing(ctx context.Context, tx *sql.Tx, parent string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range names {
		fks, err := tx.QueryContext(ctx, `SELECT "table" FROM pragma_foreign_key_list(`+quoteSQLiteIdent(name)+`)`)
		if err != nil {
			return nil, err
		}
		referenced := false
		for fks.Next() {
			var table string
			if err := fks.Scan(&table); err != nil {
				fks.Close()
				return nil, err
			}
			if table == parent {
				referenced = true
			}
		}
		err = fks.Err()
		fks.Close()
		if err != nil {
			return nil, err
		}
		if referenced {
			out = append(out, name)
		}
	}
	return out, nil
}

func sqliteTableColumns(ctx context.Context, tx *sql.Tx, table string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(`+quoteSQLiteIdent(table)+`) ORDER BY cid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

func quoteSQLiteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// User origins.
const (
	originFriend    = "friend"    // from the plex.tv friend sync
	originValidated = "validated" // first seen through plex.tv validation
)

// Token sources in auth_tokens.
const (
	sourceServer = "server" // the server token (owner)
	sourceFriend = "friend" // synced access tokens (owner's or a friend's)
)

// Validator checks tokens Ether does not already know against plex.tv.
type Validator interface {
	// Account returns the account behind token. An error matching
	// core.ErrUnauthorized means the token is invalid; other errors mean
	// plex.tv could not answer.
	Account(ctx context.Context, token string) (*plextv.Account, error)
	// HasServer reports whether token's resources list includes this
	// server (i.e. the account was granted access).
	HasServer(ctx context.Context, token string) (bool, error)
}

// PlexTVValidator is the production Validator.
type PlexTVValidator struct {
	Client    *plextv.Client
	MachineID string
}

// Account implements Validator.
func (v PlexTVValidator) Account(ctx context.Context, token string) (*plextv.Account, error) {
	return v.Client.User(ctx, token)
}

// HasServer implements Validator.
func (v PlexTVValidator) HasServer(ctx context.Context, token string) (bool, error) {
	_, err := v.Client.FindServer(ctx, token, v.MachineID)
	if errors.Is(err, core.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Options configures the Service.
type Options struct {
	// Validator checks unknown tokens; nil means only server and synced
	// tokens are accepted.
	Validator Validator
	// CacheTTL is how long a plex.tv-validated token is trusted (15m).
	CacheTTL time.Duration
	// NegativeTTL is how long a rejected token is remembered (2m).
	NegativeTTL time.Duration
	// StaleTTL lets a validated token keep working this long past CacheTTL
	// while plex.tv is unreachable (24h).
	StaleTTL time.Duration
	// MaxCacheEntries bounds the token caches (10000).
	MaxCacheEntries int
	// MaxConcurrentValidations bounds distinct unknown tokens being checked
	// with plex.tv (16). Excess requests fail fast; cached tokens still work.
	MaxConcurrentValidations int
	Logger                   *slog.Logger
	Now                      func() time.Time
}

type cacheEntry struct {
	userID  int64
	expires time.Time
	seq     uint64
}

// Service implements core.Auth and plextv.UserSync.
type Service struct {
	db  *sql.DB
	o   Options
	log *slog.Logger

	mu          sync.RWMutex
	users       map[int64]*row  // by local id
	byPlex      map[int64]int64 // plex id -> local id
	tokens      map[string]int64
	pos         map[string]cacheEntry
	neg         map[string]time.Time
	inflight    flight.Group[string, validation]
	ownerPlexID int64
	gen         atomic.Uint64

	incarnation uint64
	entrySeq    uint64
	receiptSeq  uint64
	receipts    map[uint64]receipt
	idsByHash   map[string][]uint64
	tokenGen    map[string]uint64

	membershipMu sync.Mutex // serializes validation writes with authoritative revocation
	reloadMu     sync.Mutex // orders database snapshots through publication; acquired before mu
	seenMu       sync.Mutex
	seen         map[int64]time.Time // pending last_seen_at writes

	invalMu sync.Mutex
	inval   *clusterbus.Invalidator // nil on a standalone server
}

// Generation changes when a household member loses access.
func (s *Service) Generation() uint64 {
	if s == nil {
		return 0
	}
	return s.gen.Load()
}

type row struct {
	core.User
	Active       bool
	Origin       string
	Home         bool
	Restricted   bool
	PlexSections []int
	LastSeen     time.Time
}

var (
	_ core.Auth       = (*Service)(nil)
	_ plextv.UserSync = (*Service)(nil)
)

// New loads users and known tokens from db (migrated with store.Migrate).
func New(ctx context.Context, db *sql.DB, o Options) (*Service, error) {
	if db == nil {
		return nil, errors.New("auth: db required")
	}
	if o.CacheTTL <= 0 {
		o.CacheTTL = 15 * time.Minute
	}
	if o.NegativeTTL <= 0 {
		o.NegativeTTL = 2 * time.Minute
	}
	if o.StaleTTL <= 0 {
		o.StaleTTL = 24 * time.Hour
	}
	if o.MaxCacheEntries <= 0 {
		o.MaxCacheEntries = 10000
	}
	if o.MaxConcurrentValidations <= 0 {
		o.MaxConcurrentValidations = 16
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	var inc [8]byte
	if _, err := rand.Read(inc[:]); err != nil {
		return nil, err
	}
	incarnation := binary.BigEndian.Uint64(inc[:])
	if incarnation == 0 {
		incarnation = 1
	}
	s := &Service{db: db, o: o, log: o.Logger.With("component", "auth"),
		pos: map[string]cacheEntry{}, neg: map[string]time.Time{},
		seen: map[int64]time.Time{}, incarnation: incarnation,
		receipts: map[uint64]receipt{}, idsByHash: map[string][]uint64{},
		tokenGen: map[string]uint64{}}
	if err := backfillPlexLinks(ctx, db); err != nil {
		return nil, err
	}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// HashToken is how tokens are stored: never in plain text.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

const userCols = `id, COALESCE(plex_id, 0), uuid, username, title, email, thumb, is_owner, is_home, restricted, active, origin,
	plex_sections, stream_limit, max_content_rating, subtitle_languages, audio_languages, allowed_sections, last_seen_at, CASE WHEN created_at>0 THEN created_at ELSE COALESCE((SELECT MIN(event_time) FROM (SELECT verified_at AS event_time FROM identity_links WHERE viewer_id=auth_users.id AND verified_at>0 UNION ALL SELECT created_at AS event_time FROM auth_tokens WHERE user_id=auth_users.id AND created_at>0 UNION ALL SELECT last_seen_at AS event_time WHERE last_seen_at>0) AS known_times),0) END, updated_at`

func scanRow(sc interface{ Scan(...any) error }) (*row, error) {
	var r row
	var isOwner, home, restricted, active int
	var plexSecs, subs, auds, secs string
	var lastSeen, created, updated int64
	if err := sc.Scan(&r.ID, &r.PlexID, &r.UUID, &r.Username, &r.Title, &r.Email, &r.Thumb, &isOwner, &home,
		&restricted, &active, &r.Origin, &plexSecs, &r.StreamLimit, &r.MaxContentRating, &subs, &auds, &secs, &lastSeen, &created, &updated); err != nil {
		return nil, err
	}
	r.IsOwner, r.Home, r.Restricted, r.Active = isOwner == 1, home == 1, restricted == 1, active == 1
	_ = json.Unmarshal([]byte(plexSecs), &r.PlexSections)
	_ = json.Unmarshal([]byte(subs), &r.SubtitleLanguages)
	_ = json.Unmarshal([]byte(auds), &r.AudioLanguages)
	_ = json.Unmarshal([]byte(secs), &r.AllowedSections)
	if created > 0 {
		r.CreatedAt = time.Unix(created, 0).UTC()
	}
	if updated > 0 {
		r.UpdatedAt = time.Unix(updated, 0).UTC()
	} else if created > 0 {
		r.UpdatedAt = r.CreatedAt
	}
	if lastSeen > 0 {
		r.LastSeen = time.Unix(lastSeen, 0)
	}
	return &r, nil
}

// SetBus connects the cluster bus (nil disconnects): after every write this
// service tells the other nodes to reload users and tokens, and it reloads
// when they write. A standalone server never calls it.
func (s *Service) SetBus(bus clusterbus.Bus) {
	iv := clusterbus.NewInvalidator(bus, "auth", s.log, s.applyRemote)
	s.invalMu.Lock()
	old := s.inval
	s.inval = iv
	s.invalMu.Unlock()
	old.Close()
}

// Close disconnects the cluster bus.
func (s *Service) Close() { s.SetBus(nil) }

// notify tells other nodes this one changed users or tokens. hint is
// "" (users/tokens changed), "caches" (drop every cached validation) or
// "token:<hash>" (forget one token's cached validation).
func (s *Service) notify(hint string) {
	s.invalMu.Lock()
	iv := s.inval
	s.invalMu.Unlock()
	iv.Notify(hint)
}

// applyRemote follows another node's write: reload users and tokens (which
// also drops cached validations of users who lost access) and forget
// negative results, which may predate access granted elsewhere.
func (s *Service) applyRemote(ctx context.Context, hints []string, all bool) {
	if err := s.reload(ctx); err != nil {
		s.log.Warn("auth: reload after another node's change", "err", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.neg = map[string]time.Time{}
	if all {
		s.pos = map[string]cacheEntry{}
		s.retireAllCacheLocked()
		return
	}
	for _, h := range hints {
		switch {
		case h == "caches":
			s.pos = map[string]cacheEntry{}
			s.retireAllCacheLocked()
		case strings.HasPrefix(h, "token:"):
			hash := strings.TrimPrefix(h, "token:")
			delete(s.pos, hash)
			s.retireCacheLocked(hash)
		}
	}
}

// reload rebuilds the in-memory users and token maps from the database.
func (s *Service) reload(ctx context.Context) error {
	// An older reload must publish before a newer one reads the database.
	// Otherwise it can restore revoked users and tokens after SyncFriends
	// has already published their removal and returned successfully.
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM auth_users`)
	if err != nil {
		return fmt.Errorf("auth: load users: %w", err)
	}
	users := map[int64]*row{}
	byPlex := map[int64]int64{}
	var owner int64
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			rows.Close()
			return err
		}
		users[r.ID] = r
		if r.PlexID != 0 {
			byPlex[r.PlexID] = r.ID
		}
		if r.IsOwner {
			owner = r.PlexID
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	trows, err := s.db.QueryContext(ctx, `SELECT token_hash, user_id FROM auth_tokens`)
	if err != nil {
		return fmt.Errorf("auth: load tokens: %w", err)
	}
	tokens := map[string]int64{}
	for trows.Next() {
		var h string
		var id int64
		if err := trows.Scan(&h, &id); err != nil {
			trows.Close()
			return err
		}
		tokens[h] = id
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	oldTokens := s.tokens
	for h, id := range oldTokens {
		if tokens[h] != id {
			s.retireHashLocked(h)
			s.tokenGen[h]++
		}
	}
	for h := range tokens {
		if _, ok := oldTokens[h]; ok {
			continue
		}
		if s.tokenGen[h] == 0 {
			s.tokenGen[h] = 1
		} else {
			s.retireHashLocked(h)
			s.tokenGen[h]++
		}
	}
	s.users, s.byPlex, s.tokens, s.ownerPlexID = users, byPlex, tokens, owner
	// Cached validations may point at users whose state changed.
	for h, e := range s.pos {
		if u := users[e.userID]; u == nil || !u.Active {
			delete(s.pos, h)
			s.retireCacheLocked(h)
		}
	}
	s.mu.Unlock()
	return nil
}

// effective returns the core.User view of a row.
func (r *row) effective() core.User {
	u := r.User
	u.SubtitleLanguages = append([]string{}, r.SubtitleLanguages...)
	u.AudioLanguages = append([]string{}, r.AudioLanguages...)
	u.AllowedSections = append([]int{}, r.AllowedSections...)
	if len(u.AllowedSections) == 0 && len(r.PlexSections) > 0 && !r.IsOwner {
		// No Ether-side restriction: honour the sections the owner shared
		// on plex.tv.
		u.AllowedSections = append([]int{}, r.PlexSections...)
	}
	return u
}

// Authenticate implements core.Auth. It keeps the historical user and error
// result; Accept is the same decision plus the receipt minted inside it.
func (s *Service) Authenticate(ctx context.Context, token string) (*core.User, error) {
	u, _, err := s.Accept(ctx, token)
	return u, err
}

// Accept authenticates token and returns the receipt minted by that acceptance.
// Callers must keep the receipt from this call. Reconstructing one from the
// request does not establish which branch accepted it.
func (s *Service) Accept(ctx context.Context, token string) (*core.User, Acceptance, error) {
	token = strings.TrimSpace(token)
	if token == "" || core.IsGrantCredential(token) {
		return nil, Acceptance{}, core.ErrUnauthorized
	}
	if strings.HasPrefix(token, delegationPrefix) {
		u, expires, err := s.authenticateDelegation(ctx, token)
		if err != nil {
			return nil, Acceptance{}, err
		}
		h := HashToken(token)
		s.mu.Lock()
		acc := s.mintLocked(h, originDelegation, u.ID, uint64(expires.Unix()), expires)
		s.mu.Unlock()
		return u, acc, nil
	}
	if IsOwnerSession(token) {
		// Console sessions only open the admin API (OwnerSession), and must
		// never be forwarded to plex.tv for validation.
		return nil, Acceptance{}, core.ErrUnauthorized
	}
	h := HashToken(token)
	now := s.o.Now()

	s.mu.RLock()
	if id, ok := s.tokens[h]; ok {
		u := s.activePlexUser(id)
		gen := s.tokenGen[h]
		var acc Acceptance
		var found bool
		if u != nil {
			acc, found = s.findLocked(h, originDurable, u.ID, gen, time.Time{})
		}
		s.mu.RUnlock()
		if u == nil {
			return nil, Acceptance{}, core.ErrUnauthorized
		}
		if !found || acc.viewer != u.ID {
			s.mu.Lock()
			u = s.activePlexUser(s.tokens[h])
			gen = s.tokenGen[h]
			if u == nil {
				s.mu.Unlock()
				return nil, Acceptance{}, core.ErrUnauthorized
			}
			acc = s.mintLocked(h, originDurable, u.ID, gen, time.Time{})
			s.mu.Unlock()
		}
		s.touch(u.ID, now)
		return u, acc, nil
	}
	if e, ok := s.pos[h]; ok && now.Before(e.expires) {
		u := s.activePlexUser(e.userID)
		acc, found := s.findEntryReceipt(h, e.seq, e.expires)
		s.mu.RUnlock()
		if u == nil {
			return nil, Acceptance{}, core.ErrUnauthorized
		}
		if !found {
			s.mu.Lock()
			e, ok = s.pos[h]
			u = nil
			if ok && now.Before(e.expires) {
				u = s.activePlexUser(e.userID)
			}
			if u == nil {
				s.mu.Unlock()
				return nil, Acceptance{}, core.ErrUnauthorized
			}
			acc = s.mintLocked(h, originPositive, u.ID, e.seq, e.expires)
			s.mu.Unlock()
		}
		s.touch(u.ID, now)
		return u, acc, nil
	}
	if exp, ok := s.neg[h]; ok && now.Before(exp) {
		s.mu.RUnlock()
		return nil, Acceptance{}, core.ErrUnauthorized
	}
	s.mu.RUnlock()

	if s.o.Validator == nil {
		return nil, Acceptance{}, core.ErrUnauthorized
	}
	return s.validate(ctx, token, h)
}

// activeUser returns a copy of an active user; callers hold s.mu.
func (s *Service) activeUser(id int64) *core.User {
	r := s.users[id]
	if r == nil || !r.Active {
		return nil
	}
	u := r.effective()
	return &u
}

// activePlexUser excludes local-only DLNA guests from Plex credentials.
// Callers hold s.mu, as for activeUser.
func (s *Service) activePlexUser(id int64) *core.User {
	if row := s.users[id]; row == nil || row.Origin == "dlna_guest" {
		return nil
	}
	return s.activeUser(id)
}

// validate asks plex.tv, collapsing concurrent requests for one token.
func (s *Service) validate(ctx context.Context, token, h string) (*core.User, Acceptance, error) {
	if err := ctx.Err(); err != nil {
		return nil, Acceptance{}, err
	}
	// Detached, so one impatient client does not fail the others waiting
	// on the same token. Waiters share that one acceptance receipt.
	out, err := s.inflight.DoDetachedLimit(ctx, h, s.o.MaxConcurrentValidations, func(ctx context.Context) (validation, error) {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		return s.validateOnce(ctx, token, h)
	})
	// Only the direct admission refusal takes this path; validation errors may
	// wrap other causes and retain their existing stale-fallback semantics.
	if err == flight.ErrCapacity {
		return s.stale(h, errors.New("auth: token validation capacity reached"))
	}
	return out.user, out.acc, err
}

func (s *Service) validateOnce(ctx context.Context, token, h string) (validation, error) {
	acct, err := s.o.Validator.Account(ctx, token)
	if err != nil {
		if errors.Is(err, core.ErrUnauthorized) {
			s.reject(h)
			return validation{}, core.ErrUnauthorized
		}
		u, acc, staleErr := s.stale(h, err)
		return validation{user: u, acc: acc}, staleErr
	}
	// Membership changes must remain available while plex.tv is slow. Only
	// local membership decisions and writes hold this lock, never network I/O.
	s.membershipMu.Lock()
	defer s.membershipMu.Unlock()
	s.mu.RLock()
	owner := s.ownerPlexID
	localID, known := s.byPlex[acct.ID]
	var knownFriend, revoked bool
	if known {
		revoked = s.users[localID] != nil && !s.users[localID].Active
		knownFriend = s.users[localID] != nil && s.users[localID].Active && s.users[localID].Origin == originFriend
	}
	s.mu.RUnlock()

	if revoked {
		s.reject(h)
		return validation{}, core.ErrUnauthorized
	}
	switch {
	case owner != 0 && acct.ID == owner:
		// The owner signed in on another device.
	case knownFriend:
		// A friend from the last sync, using a personal token.
	default:
		s.membershipMu.Unlock()
		ok, err := s.o.Validator.HasServer(ctx, token)
		s.membershipMu.Lock()
		if err != nil {
			if errors.Is(err, core.ErrUnauthorized) {
				s.reject(h)
				return validation{}, core.ErrUnauthorized
			}
			u, acc, staleErr := s.stale(h, err)
			return validation{user: u, acc: acc}, staleErr
		}
		if !ok {
			if known {
				if _, err := s.db.ExecContext(ctx, `UPDATE auth_users SET active=0, plex_shared=0 WHERE id=? AND is_owner=0`, localID); err != nil {
					return validation{}, err
				}
				s.gen.Add(1)
				if err := s.reload(ctx); err != nil {
					return validation{}, err
				}
				s.notify("")
			}
			s.log.Info("token without access to this server", "plexId", acct.ID, "username", acct.Username)
			s.reject(h)
			return validation{}, core.ErrUnauthorized
		}
		// A sync may have revoked this account during HasServer. The upsert
		// preserves active=0 on existing rows; activeUser below then refuses it.
		id, err := s.upsertValidated(ctx, acct)
		if err != nil {
			return validation{}, err
		}
		localID = id
	}
	if owner != 0 && acct.ID == owner {
		s.mu.RLock()
		localID = s.byPlex[owner]
		s.mu.RUnlock()
	}
	expires := s.o.Now().Add(s.o.CacheTTL)
	s.mu.Lock()
	s.evictLocked()
	s.entrySeq++
	seq := s.entrySeq
	s.retireCacheLocked(h)
	s.pos[h] = cacheEntry{userID: localID, expires: expires, seq: seq}
	delete(s.neg, h)
	u := s.activePlexUser(localID)
	var acc Acceptance
	if u != nil {
		acc = s.mintLocked(h, originFresh, u.ID, seq, expires)
	}
	s.mu.Unlock()
	if u == nil {
		return validation{}, core.ErrUnauthorized
	}
	s.touch(u.ID, s.o.Now())
	return validation{user: u, acc: acc}, nil
}

func (s *Service) reject(h string) {
	s.mu.Lock()
	s.evictLocked()
	s.neg[h] = s.o.Now().Add(s.o.NegativeTTL)
	delete(s.pos, h)
	s.retireCacheLocked(h)
	s.mu.Unlock()
}

// stale serves an expired positive entry while plex.tv is unreachable.
func (s *Service) stale(h string, cause error) (*core.User, Acceptance, error) {
	now := s.o.Now()
	s.mu.Lock()
	e, ok := s.pos[h]
	var u *core.User
	var acc Acceptance
	if ok && now.Before(e.expires.Add(s.o.StaleTTL)) {
		u = s.activePlexUser(e.userID)
		if u != nil {
			acc = s.mintLocked(h, originStale, u.ID, e.seq, e.expires.Add(s.o.StaleTTL))
		}
	}
	s.mu.Unlock()
	if u != nil {
		s.log.Warn("plex.tv unreachable; using cached token validation", "err", cause)
		return u, acc, nil
	}
	return nil, Acceptance{}, fmt.Errorf("auth: validate token with plex.tv: %w", cause)
}

// evictLocked bounds the caches; callers hold s.mu for writing.
func (s *Service) evictLocked() {
	if len(s.pos)+len(s.neg) < s.o.MaxCacheEntries {
		return
	}
	now := s.o.Now()
	for h, e := range s.pos {
		if now.After(e.expires.Add(s.o.StaleTTL)) {
			delete(s.pos, h)
			s.retireCacheLocked(h)
		}
	}
	for h, exp := range s.neg {
		if now.After(exp) {
			delete(s.neg, h)
		}
	}
	// Still full: drop arbitrary entries (map order is random).
	for h := range s.neg {
		if len(s.pos)+len(s.neg) < s.o.MaxCacheEntries*3/4 {
			break
		}
		delete(s.neg, h)
	}
	for h := range s.pos {
		if len(s.pos)+len(s.neg) < s.o.MaxCacheEntries*3/4 {
			break
		}
		delete(s.pos, h)
		s.retireCacheLocked(h)
	}
}

// Invalidate forgets cached validations of token.
func (s *Service) Invalidate(token string) {
	h := HashToken(token)
	s.mu.Lock()
	delete(s.pos, h)
	delete(s.neg, h)
	s.retireCacheLocked(h)
	s.mu.Unlock()
	s.notify("token:" + h)
}

// touch records activity; written to the database by FlushSeen.
func (s *Service) touch(id int64, at time.Time) {
	s.seenMu.Lock()
	if at.After(s.seen[id]) {
		s.seen[id] = at
	}
	s.seenMu.Unlock()
}

// FlushSeen persists last-seen times (call periodically and at shutdown).
// Every node flushes its own observations; the update only ever moves
// last_seen_at forward, so nodes flushing in any order keep the latest.
func (s *Service) FlushSeen(ctx context.Context) error {
	s.seenMu.Lock()
	pending := s.seen
	s.seen = map[int64]time.Time{}
	s.seenMu.Unlock()
	for id, at := range pending {
		if _, err := s.db.ExecContext(ctx, `UPDATE auth_users SET last_seen_at=? WHERE id=? AND last_seen_at<?`, at.Unix(), id, at.Unix()); err != nil {
			// Keep this and the remaining observations for the next flush.
			// Authentication may have added newer values in the meantime.
			s.seenMu.Lock()
			for userID, observed := range pending {
				if observed.After(s.seen[userID]) {
					s.seen[userID] = observed
				}
			}
			s.seenMu.Unlock()
			return err
		}
		delete(pending, id)
		s.mu.Lock()
		if r := s.users[id]; r != nil && at.After(r.LastSeen) {
			r.LastSeen = at
		}
		s.mu.Unlock()
	}
	return nil
}

func (s *Service) upsertValidated(ctx context.Context, a *plextv.Account) (int64, error) {
	now := s.o.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
INSERT INTO auth_users (plex_id, uuid, username, title, email, thumb, is_home, restricted, active, origin, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(plex_id) DO UPDATE SET uuid=excluded.uuid, username=excluded.username, title=excluded.title,
	email=excluded.email, thumb=excluded.thumb, updated_at=excluded.updated_at`,
		a.ID, a.UUID, a.Username, a.DisplayName(), a.Email, a.Thumb, b2i(a.Home), b2i(a.Restricted), originValidated, now, now)
	if err != nil {
		return 0, fmt.Errorf("auth: save user: %w", err)
	}
	if err := upsertPlexLink(ctx, tx, a.ID, a.DisplayName(), now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if err := s.reload(ctx); err != nil {
		return 0, err
	}
	s.notify("")
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byPlex[a.ID], nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// BootstrapOwner creates the owner from config without network access (on
// startup, before plex.tv is reachable) and registers the server token.
func (s *Service) BootstrapOwner(ctx context.Context, p config.Plex) error {
	if p.OwnerPlexID == 0 {
		return nil
	}
	s.mu.RLock()
	id, ok := s.byPlex[p.OwnerPlexID]
	var r *row
	if ok {
		r = s.users[id]
	}
	s.mu.RUnlock()
	acct := plextv.Account{ID: p.OwnerPlexID, Username: p.OwnerUsername, Title: p.OwnerUsername}
	if r != nil {
		// Keep the richer profile already stored.
		acct.UUID, acct.Email, acct.Thumb, acct.Title = r.UUID, r.Email, r.Thumb, r.Title
		if acct.Username == "" {
			acct.Username = r.Username
		}
	}
	return s.SetOwner(ctx, acct, p.Token)
}

// SetOwner implements plextv.UserSync: it makes a the owner and registers
// serverToken as the owner's token. A unique index admits one owner row, so
// when another node changes the owner concurrently the losing transaction is
// retried against the winner's committed state.
func (s *Service) SetOwner(ctx context.Context, a plextv.Account, serverToken string) error {
	if a.ID == 0 {
		return errors.New("auth: owner without plex id")
	}
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if err = s.setOwnerTx(ctx, a, serverToken); err == nil || !isUniqueViolation(err) {
			break
		}
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.pos = map[string]cacheEntry{}
	s.neg = map[string]time.Time{}
	s.mu.Unlock()
	err = s.reload(ctx)
	s.notify("caches")
	return err
}

// isUniqueViolation reports a unique-constraint failure from SQLite or
// PostgreSQL (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE constraint failed") || strings.Contains(m, "23505") ||
		strings.Contains(m, "duplicate key value")
}

func (s *Service) setOwnerTx(ctx context.Context, a plextv.Account, serverToken string) error {
	now := s.o.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// A Plex claim attaches to the existing local owner, retaining its viewer
	// ID and all state. A Plex ID already linked to another viewer must fail
	// rather than silently merging two people's state.
	if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET plex_id=? WHERE is_owner=1 AND plex_id IS NULL`, a.ID); err != nil {
		return fmt.Errorf("auth: attach Plex owner: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET is_owner=0, origin=CASE WHEN origin='owner' THEN 'friend' ELSE origin END,
		active=CASE WHEN is_owner=1 THEN 0 ELSE active END WHERE is_owner=1 AND plex_id<>?`, a.ID); err != nil {
		return err
	}
	title := a.DisplayName()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO auth_users (plex_id, uuid, username, title, email, thumb, is_owner, is_home, active, origin, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, 1, 'owner', ?, ?)
ON CONFLICT(plex_id) DO UPDATE SET
	uuid=CASE WHEN excluded.uuid<>'' THEN excluded.uuid ELSE auth_users.uuid END,
	username=CASE WHEN excluded.username<>'' THEN excluded.username ELSE auth_users.username END,
	title=CASE WHEN excluded.title<>'' THEN excluded.title ELSE auth_users.title END,
	email=CASE WHEN excluded.email<>'' THEN excluded.email ELSE auth_users.email END,
	thumb=CASE WHEN excluded.thumb<>'' THEN excluded.thumb ELSE auth_users.thumb END,
	is_owner=1, active=1, owner_enabled=1, plex_shared=1, origin='owner', updated_at=excluded.updated_at
WHERE auth_users.origin<>'dlna_guest'`,
		a.ID, a.UUID, a.Username, title, a.Email, a.Thumb, b2i(a.Home), now, now); err != nil {
		return fmt.Errorf("auth: save owner: %w", err)
	}
	if err := upsertPlexLink(ctx, tx, a.ID, title, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE source=?`, sourceServer); err != nil {
		return err
	}
	// A former owner's console sessions end with their ownership.
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_sessions WHERE user_id NOT IN (SELECT id FROM auth_users WHERE is_owner=1)`); err != nil {
		return err
	}
	if serverToken != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_tokens (token_hash, user_id, source, created_at)
			SELECT CAST(? AS TEXT), id, CAST(? AS TEXT), CAST(? AS BIGINT) FROM auth_users WHERE plex_id=?
			ON CONFLICT(token_hash) DO UPDATE SET user_id=excluded.user_id, source=excluded.source, created_at=excluded.created_at`,
			HashToken(serverToken), sourceServer, now, a.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SyncFriends implements plextv.UserSync: it replaces the synced access
// tokens, upserts friends' profiles (never their Ether settings) and
// deactivates every non-owner absent from the authoritative membership list,
// including viewers first discovered through personal-token validation.
func (s *Service) SyncFriends(ctx context.Context, friends []plextv.Friend) error {
	s.membershipMu.Lock()
	defer s.membershipMu.Unlock()
	now := s.o.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE auth_membership_epoch SET epoch=epoch+1 WHERE id=1`); err != nil {
		return err
	}
	var ownerLocal int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM auth_users WHERE is_owner=1`).Scan(&ownerLocal); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE source=?`, sourceFriend); err != nil {
		return err
	}
	// Upsert in plex id order so concurrent syncs on different nodes take
	// row locks in the same order.
	friends = append([]plextv.Friend(nil), friends...)
	sort.SliceStable(friends, func(i, j int) bool { return friends[i].PlexID < friends[j].PlexID })
	keep := map[int64]bool{}
	for _, f := range friends {
		var uid int64
		switch {
		case f.Owned:
			if ownerLocal == 0 {
				continue
			}
			uid = ownerLocal
		case f.PlexID == 0:
			continue
		default:
			secs, _ := json.Marshal(sectionIDs(f.Sections, f.AllLibs))
			if err := tx.QueryRowContext(ctx, `
INSERT INTO auth_users (plex_id, uuid, username, title, email, thumb, is_home, restricted, active, plex_shared, origin, plex_sections, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'friend', ?, ?, ?)
ON CONFLICT(plex_id) DO UPDATE SET
	uuid=CASE WHEN excluded.uuid<>'' THEN excluded.uuid ELSE auth_users.uuid END,
	username=CASE WHEN excluded.username<>'' THEN excluded.username ELSE auth_users.username END,
	title=CASE WHEN CAST(? AS TEXT)<>'' THEN CAST(? AS TEXT) ELSE auth_users.title END,
	email=CASE WHEN excluded.email<>'' THEN excluded.email ELSE auth_users.email END,
	thumb=CASE WHEN excluded.thumb<>'' THEN excluded.thumb ELSE auth_users.thumb END,
	is_home=excluded.is_home, restricted=excluded.restricted,
	plex_shared=CASE WHEN auth_users.is_owner=1 THEN 1 ELSE excluded.active END,
 active=CASE WHEN auth_users.is_owner=1 THEN 1 WHEN auth_users.owner_enabled=1 THEN excluded.active ELSE 0 END,
	origin=CASE WHEN auth_users.origin='owner' THEN auth_users.origin ELSE 'friend' END,
	plex_sections=excluded.plex_sections, updated_at=excluded.updated_at
WHERE auth_users.origin<>'dlna_guest'
RETURNING id`,
				f.PlexID, f.UUID, f.Username, f.DisplayName(), f.Email, f.Thumb, b2i(f.Home), b2i(f.Restricted),
				b2i(!f.Pending), b2i(!f.Pending), string(secs), now, now, f.Title, f.Title).Scan(&uid); err != nil {
				return fmt.Errorf("auth: save friend %d: %w", f.PlexID, err)
			}
			if !f.Pending {
				if err := upsertPlexLink(ctx, tx, f.PlexID, f.DisplayName(), now); err != nil {
					return err
				}
			}
			if f.Pending {
				continue // invited but not accepted: no access yet
			}
			keep[uid] = true
		}
		if f.Token == "" {
			continue // Membership is authoritative even without a shared token.
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_tokens (token_hash, user_id, source, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(token_hash) DO UPDATE SET user_id=excluded.user_id, source=excluded.source, created_at=excluded.created_at`,
			HashToken(f.Token), uid, sourceFriend, now); err != nil {
			return err
		}
	}
	// Friends no longer in the list lost access.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM auth_users WHERE is_owner=0 AND plex_id IS NOT NULL`)
	if err != nil {
		return err
	}
	var gone []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !keep[id] {
			gone = append(gone, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range gone {
		if _, err := tx.ExecContext(ctx, `UPDATE auth_users SET active=0, plex_shared=0, updated_at=? WHERE id=?`, now, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if len(gone) > 0 {
		s.gen.Add(1)
		s.log.Info("friends lost access", "count", len(gone))
	}
	err = s.reload(ctx)
	s.notify("")
	return err
}

// sectionIDs converts plex.tv section keys ("1", "/library/sections/1") to
// ints; nil when everything is shared.
func sectionIDs(keys []string, all bool) []int {
	if all {
		return []int{}
	}
	out := []int{}
	for _, k := range keys {
		k = strings.TrimRight(k, "/")
		if i := strings.LastIndex(k, "/"); i >= 0 {
			k = k[i+1:]
		}
		if n, err := strconv.Atoi(k); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// Users implements core.Auth: active users, owner first, then by name.
func (s *Service) Users(ctx context.Context) ([]core.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]core.User, 0, len(s.users))
	for _, r := range s.users {
		if r.Active {
			out = append(out, r.effective())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsOwner != out[j].IsOwner {
			return out[i].IsOwner
		}
		ti, tj := strings.ToLower(out[i].Title), strings.ToLower(out[j].Title)
		if ti != tj {
			return ti < tj
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ActiveUser returns a current access snapshot, including for stream grants.
func (s *Service) ActiveUser(ctx context.Context, id int64) (*core.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if u := s.activeUser(id); u != nil {
		return u, nil
	}
	return nil, core.ErrUnauthorized
}

// PolicyUser reads durable membership and restrictions without the process cache.
func (s *Service) PolicyUser(ctx context.Context, id int64) (*core.User, error) {
	r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM auth_users WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	if !r.Active {
		return nil, core.ErrUnauthorized
	}
	u := r.effective()
	return &u, nil
}

// UserInfo is a user plus bookkeeping for the admin UI.
type UserInfo struct {
	core.User
	Active       bool      `json:"active"`
	Origin       string    `json:"origin"`
	Home         bool      `json:"home"`
	Restricted   bool      `json:"restricted"`
	PlexSections []int     `json:"plexSections"`
	LastSeen     time.Time `json:"lastSeen,omitempty"`
}

// AllUsers lists every known user including those who lost access.
func (s *Service) AllUsers(ctx context.Context) ([]UserInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]UserInfo, 0, len(s.users))
	for _, r := range s.users {
		out = append(out, UserInfo{User: r.effective(), Active: r.Active, Origin: r.Origin, Home: r.Home,
			Restricted: r.Restricted, PlexSections: append([]int{}, r.PlexSections...), LastSeen: r.LastSeen})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// User returns one user by local id (active or not).
func (s *Service) User(ctx context.Context, id int64) (*core.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r := s.users[id]
	if r == nil {
		return nil, core.ErrNotFound
	}
	u := r.effective()
	return &u, nil
}

// Owner implements core.Auth.
func (s *Service) Owner(ctx context.Context) (*core.User, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var r *row
	for _, candidate := range s.users {
		if candidate.IsOwner {
			r = candidate
			break
		}
	}
	if r == nil {
		return nil, core.ErrNotFound
	}
	u := r.effective()
	return &u, nil
}

// UpdateUser implements core.Auth. It changes only per-user settings:
// stream limit, content rating cap, languages and allowed sections.
// Identity fields come from plex.tv and are ignored.
func (s *Service) UpdateUser(ctx context.Context, u core.User) error {
	if u.StreamLimit < 0 {
		return fmt.Errorf("auth: stream limit must be >= 0")
	}
	norm := func(xs []string) []string {
		out := []string{}
		seen := map[string]bool{}
		for _, x := range xs {
			x = strings.ToLower(strings.TrimSpace(x))
			if x != "" && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
		return out
	}
	subs, _ := json.Marshal(norm(u.SubtitleLanguages))
	auds, _ := json.Marshal(norm(u.AudioLanguages))
	secs := u.AllowedSections
	if secs == nil {
		secs = []int{}
	}
	secJSON, _ := json.Marshal(secs)
	res, err := s.db.ExecContext(ctx, `UPDATE auth_users SET stream_limit=?, max_content_rating=?, subtitle_languages=?,
		audio_languages=?, allowed_sections=?, updated_at=? WHERE id=?`,
		u.StreamLimit, strings.TrimSpace(u.MaxContentRating), string(subs), string(auds), string(secJSON), s.o.Now().Unix(), u.ID)
	if err != nil {
		return fmt.Errorf("auth: update user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.ErrNotFound
	}
	s.gen.Add(1)
	err = s.reload(ctx)
	s.notify("")
	return err
}
