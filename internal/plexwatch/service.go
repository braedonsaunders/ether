package plexwatch

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("plexwatch", 1, `
CREATE TABLE plexwatch_account (
  user_id INTEGER PRIMARY KEY,
  plex_id INTEGER NOT NULL,
  username TEXT NOT NULL DEFAULT '',
  token TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 0,
  eligibility TEXT NOT NULL DEFAULT 'unchecked',
  checked_at INTEGER NOT NULL DEFAULT 0,
  revoked INTEGER NOT NULL DEFAULT 0,
  import_cursor TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE plexwatch_pin (
  user_id INTEGER PRIMARY KEY,
  pin_id INTEGER NOT NULL,
  code TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE plexwatch_sent (
  user_id INTEGER NOT NULL,
  guid TEXT NOT NULL,
  kind TEXT NOT NULL,
  at_ms INTEGER NOT NULL,
  rating INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, guid, kind, at_ms, rating)
);`)
	store.Register("plexwatch", 2, `
ALTER TABLE plexwatch_account ADD COLUMN token_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plexwatch_sent ADD COLUMN sent_at INTEGER NOT NULL DEFAULT 0;
CREATE TABLE plexwatch_fence (
  user_id INTEGER PRIMARY KEY,
  generation INTEGER NOT NULL DEFAULT 1,
  inflight INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE plexwatch_unmapped (
  user_id INTEGER NOT NULL,
  guid TEXT NOT NULL,
  view_state TEXT NOT NULL DEFAULT '',
  viewed_at INTEGER NOT NULL DEFAULT 0,
  rating INTEGER NOT NULL DEFAULT 0,
  rated_at INTEGER NOT NULL DEFAULT 0,
  reason TEXT NOT NULL,
  seen_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, guid)
);`)
	store.Register("plexwatch", 3, `
ALTER TABLE plexwatch_fence ADD COLUMN inflight_until INTEGER NOT NULL DEFAULT 0;`)
	store.Register("plexwatch", 4, `
CREATE TABLE plexwatch_permit (
  permit_id TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  generation INTEGER NOT NULL,
  until_at INTEGER NOT NULL
);
CREATE INDEX plexwatch_permit_live ON plexwatch_permit(user_id, until_at);`)
	store.Register("plexwatch", 5, `
ALTER TABLE plexwatch_account RENAME COLUMN enabled TO direction;
ALTER TABLE plexwatch_account ADD COLUMN import_interval INTEGER NOT NULL DEFAULT 10;
ALTER TABLE plexwatch_account ADD COLUMN next_import INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plexwatch_account ADD COLUMN last_import INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plexwatch_account ADD COLUMN last_send INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plexwatch_account ADD COLUMN import_problem TEXT NOT NULL DEFAULT '';
ALTER TABLE plexwatch_account ADD COLUMN send_problem TEXT NOT NULL DEFAULT '';
ALTER TABLE plexwatch_account ADD COLUMN problem_code INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plexwatch_account ADD COLUMN import_failures INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plexwatch_account ADD COLUMN import_lease INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plexwatch_account ADD COLUMN next_allowed INTEGER NOT NULL DEFAULT 0;
CREATE INDEX plexwatch_import_due ON plexwatch_account(next_import, user_id);`)
	// Discover transport. plexwatch_remote is the last account state Ether
	// read from (or wrote to) Plex per title: imports act only on what changed
	// since then, and ours=1 marks a row Ether itself just wrote, so its own
	// change is not read back as a Plex change. plexwatch_expand queues shows
	// whose watched-episode count moved, so only their episodes are read.
	// import_cursor now holds the sweep's last rating key; the old opaque
	// scrobbles cursor and the echo/unmatched tables of that transport go.
	store.Register("plexwatch", 6, `
CREATE TABLE plexwatch_remote (
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  watched INTEGER NOT NULL DEFAULT 0,
  viewed_at INTEGER NOT NULL DEFAULT 0,
  rating INTEGER NOT NULL DEFAULT 0,
  leaves INTEGER NOT NULL DEFAULT 0,
  observed_at INTEGER NOT NULL DEFAULT 0,
  ours INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, rating_key)
);
CREATE TABLE plexwatch_expand (
  user_id INTEGER NOT NULL,
  show_key INTEGER NOT NULL,
  after_key INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, show_key)
);
ALTER TABLE plexwatch_account ADD COLUMN last_sweep INTEGER NOT NULL DEFAULT 0;
UPDATE plexwatch_account SET import_cursor='', import_problem='', send_problem='', problem_code=0,
  import_failures=0, next_allowed=0, next_import=0, import_lease=0, eligibility='unchecked';
DROP TABLE plexwatch_sent;
DROP TABLE plexwatch_unmapped;`)
	// consent is the account's "Sync My Watch State and Ratings" setting as
	// last read (-1 not read yet, 0 off, 1 on) and consent_checked when.
	// active_after is the show cursor of the pass over the viewer's active
	// shows that follows each sweep (-1: not running). view_offset (ms) and
	// view_state are the resume position and Discover viewState last read or
	// written per title.
	store.Register("plexwatch", 7, `
ALTER TABLE plexwatch_account ADD COLUMN consent INTEGER NOT NULL DEFAULT -1;
ALTER TABLE plexwatch_account ADD COLUMN consent_checked INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plexwatch_account ADD COLUMN active_after INTEGER NOT NULL DEFAULT -1;
ALTER TABLE plexwatch_remote ADD COLUMN view_offset INTEGER NOT NULL DEFAULT 0;
ALTER TABLE plexwatch_remote ADD COLUMN view_state TEXT NOT NULL DEFAULT '';`)
	// Applies after state_plexwatch; standalone state need not import account sync.
	store.Register("watchsend", 1, `CREATE TABLE plexwatch_attempt (
  attempt_id TEXT PRIMARY KEY,
  seq INTEGER NOT NULL,
  user_id INTEGER NOT NULL,
  rating_key INTEGER NOT NULL,
  plex_id INTEGER NOT NULL,
  generation INTEGER NOT NULL,
  guid TEXT NOT NULL,
  aspect TEXT NOT NULL,
  occurred_at INTEGER NOT NULL,
  payload TEXT NOT NULL,
  admitted_at INTEGER NOT NULL,
  receipt_at INTEGER NOT NULL DEFAULT 0,
  reconcile_attempts INTEGER NOT NULL DEFAULT 0,
  next_reconcile INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX plexwatch_attempt_conflict ON plexwatch_attempt(plex_id,guid,aspect)
  WHERE receipt_at=0 AND guid<>'';
CREATE INDEX plexwatch_attempt_local ON plexwatch_attempt(user_id,rating_key,aspect,receipt_at);
INSERT INTO plexwatch_attempt(attempt_id,seq,user_id,rating_key,plex_id,generation,guid,aspect,occurred_at,payload,admitted_at,next_reconcile)
 SELECT 'legacy:' || CAST(seq AS TEXT), seq, user_id, rating_key,
 COALESCE((SELECT plex_id FROM plexwatch_account a WHERE a.user_id=state_sync_journal.user_id),0),
 consent_generation, '', aspect, occurred_at, '', 0, 0 FROM state_sync_journal WHERE delivered_at=0 AND never_sent=0;
UPDATE state_sync_journal SET dead=1, last_error='uncertain_legacy', claimed_by='', claimed_at=0 WHERE delivered_at=0 AND never_sent=0;`)
	// Discover writes are idempotent and readable back, so an unconfirmed
	// send needs no hold table: the journal row itself, with never_sent=0, is
	// re-checked against Plex before it is sent again. Held rows of the old
	// transport become that kind of row when they are still a title's newest
	// intent; their consent generation is checked at delivery like any other.
	// Rows parked as not eligible or credential-refused (old-transport
	// reasons) are revived the same way. Older duplicates stay dead.
	store.Register("watchsend", 2, `
UPDATE state_sync_journal SET dead=0, attempts=0, next_attempt=0, claimed_by='', claimed_at=0, last_error='',
  never_sent=CASE WHEN last_error IN ('uncertain_send','uncertain_legacy') THEN 0 ELSE never_sent END
 WHERE delivered_at=0 AND superseded=0 AND dead=1
   AND last_error IN ('uncertain_send','uncertain_legacy','credential_forbidden','credential_unavailable','not_eligible')
   AND NOT EXISTS (SELECT 1 FROM state_sync_journal n
     WHERE n.user_id=state_sync_journal.user_id AND n.rating_key=state_sync_journal.rating_key
       AND n.aspect=state_sync_journal.aspect AND n.seq>state_sync_journal.seq);
DROP TABLE plexwatch_attempt;
DROP TABLE plexwatch_permit;`)
}

// tokenSealLabel is the sealing context of stored account tokens. It names
// the first transport and stays fixed: changing it would orphan every
// stored token, which is the same Plex account token used today.
const tokenSealLabel = "scrobbles.plex.tv"

// GUIDMap resolves a local title to a verified plex:// GUID and the reverse
// for a GUID already stored for exactly one title.
type GUIDMap interface {
	Resolve(ctx context.Context, key core.RatingKey) (string, error)
	ExactKey(ctx context.Context, guid string) (core.RatingKey, error)
}

// AccountGUIDs is GUIDMap plus a local, paged walk of verified identities,
// which bounds the background import to titles Ether already matched.
type AccountGUIDs interface {
	GUIDMap
	VerifiedRange(ctx context.Context, after, below core.RatingKey, limit int) ([]plexguid.Verified, error)
}

// Options configure a Service. BaseURL is the test origin for every Plex
// host. Leave it empty in production.
type Options struct {
	DB       *sql.DB
	State    *state.State
	Sealer   config.FieldSealer
	GUID     AccountGUIDs
	NodeID   string
	ClientID string
	BaseURL  string
	HTTP     *http.Client
	Logger   *slog.Logger
	Now      func() time.Time
	// Watchlist lists a viewer's watchlist titles (optional). Its shows
	// join the shows with recent activity whose episodes each sweep reads.
	Watchlist func(ctx context.Context, userID int64) ([]core.RatingKey, error)
	// Continue mirrors a Continue Watching removal (removed) or restore
	// read from the account into stores beyond state (optional).
	Continue func(ctx context.Context, userID int64, key core.RatingKey, removed bool) error
}

// Service is one server's Plex account watch-state integration.
type Service struct {
	db        *sql.DB
	state     *state.State
	sealer    config.FieldSealer
	guid      AccountGUIDs
	client    *Client
	watchlist func(ctx context.Context, userID int64) ([]core.RatingKey, error)
	continued func(ctx context.Context, userID int64, key core.RatingKey, removed bool) error
	node      string
	log       *slog.Logger
	now       func() time.Time
	claimFor  time.Duration
	pace      time.Duration
	// importBudget caps the Plex reads of one import pass.
	importBudget int
	mu           sync.Mutex
	closed       chan struct{}
	lastCompact  time.Time
}

// New starts nothing. Call Run to deliver and RunImports to import.
func New(opts Options) (*Service, error) {
	if opts.DB == nil || opts.State == nil {
		return nil, errors.New("plexwatch: database and state are required")
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	node := opts.NodeID
	if node == "" {
		node = newID()[:16]
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	c, err := newClient(opts.BaseURL, opts.ClientID, opts.HTTP, now)
	if err != nil {
		return nil, err
	}
	return &Service{
		db: opts.DB, state: opts.State, sealer: opts.Sealer, guid: opts.GUID,
		client: c, watchlist: opts.Watchlist, continued: opts.Continue,
		node: node, log: log, now: now, claimFor: 2 * time.Minute,
		pace: 250 * time.Millisecond, importBudget: importRequests, closed: make(chan struct{}),
	}, nil
}

// Close stops Run and RunImports.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

// Status is the viewer-facing connection, with no token and no history.
// Problem is "" or one of "token" (connect again), "consent" (the account's
// "Sync My Watch State and Ratings" setting is off in Plex), "rate_limited"
// or "network".
type Status struct {
	Connected      bool   `json:"connected"`
	PlexID         int64  `json:"plexId,omitempty"`
	Username       string `json:"username,omitempty"`
	Direction      int    `json:"direction"`
	ImportInterval int    `json:"importInterval"`
	LastImport     int64  `json:"lastImport"`
	LastFullImport int64  `json:"lastFullImport"`
	LastSend       int64  `json:"lastSend"`
	NextImport     int64  `json:"nextImport"`
	Problem        string `json:"problem"`
	NeedsReconnect bool   `json:"needsReconnect"`
	Pending        int    `json:"pending"`
	Failed         int    `json:"failed"`
	Restricted     bool   `json:"restricted"`
	PINCode        string `json:"pinCode,omitempty"`
	PINURL         string `json:"pinUrl,omitempty"`
	PINExpires     string `json:"pinExpires,omitempty"`
	AccountMatch   bool   `json:"accountMatch"`
}

// failureReasons are journal outcomes a viewer can retry. Rows dropped
// because sync was turned off, reconnected or disconnected are not failures.
var failureReasons = []string{"rejected", "missing_guid", "ambiguous_guid"}

type accountRow struct {
	userID          int64
	plexID          int64
	username        string
	token           string
	direction       int
	revoked         bool
	cursor          string
	tokenGeneration int64
	nextAllowed     int64
	consent         int   // -1 not read yet, 0 off, 1 on
	consentChecked  int64 // unix nanoseconds
	activeAfter     int64 // show cursor of the active-show pass, -1 idle
}

func (s *Service) Status(ctx context.Context, userID, plexID int64) (Status, error) {
	if err := viewerIDs(userID, plexID); err != nil {
		return Status{}, err
	}
	st := Status{AccountMatch: true, ImportInterval: 10}
	row, ok, err := s.loadAccount(ctx, userID)
	if err != nil {
		return Status{}, err
	}
	if ok {
		st.Connected = row.token != "" && !row.revoked
		st.PlexID = row.plexID
		st.Username = row.username
		st.Direction = row.direction
		var importProblem, sendProblem string
		if err := s.db.QueryRowContext(ctx, `SELECT import_interval,last_import,last_sweep,last_send,next_import,import_problem,send_problem FROM plexwatch_account WHERE user_id=?`, userID).
			Scan(&st.ImportInterval, &st.LastImport, &st.LastFullImport, &st.LastSend, &st.NextImport, &importProblem, &sendProblem); err != nil {
			return Status{}, err
		}
		st.NextImport /= int64(time.Second)
		st.Problem = importProblem
		if st.Problem == "" {
			st.Problem = sendProblem
		}
		if row.consent == 0 {
			st.Problem = "consent"
		}
		if row.revoked {
			st.Problem, st.NeedsReconnect = "token", true
		}
		st.AccountMatch = row.plexID == plexID
		if row.plexID != plexID {
			st.Connected = false
			st.Direction = 0
		}
	}
	pending, _, err := s.state.PendingSyncCount(ctx, userID)
	if err != nil {
		return Status{}, err
	}
	st.Pending = pending
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM state_sync_journal WHERE user_id=? AND dead=1 AND delivered_at=0 AND superseded=0 AND last_error IN (?,?,?)`,
		userID, failureReasons[0], failureReasons[1], failureReasons[2]).Scan(&st.Failed); err != nil {
		return Status{}, err
	}
	if pin, ok, err := s.loadPIN(ctx, userID); err != nil {
		return Status{}, err
	} else if ok && s.now().Before(pin.expires) {
		st.PINCode = pin.code
		st.PINURL = s.client.authURL(pin.code)
		st.PINExpires = pin.expires.UTC().Format(time.RFC3339)
	}
	return st, nil
}

// BeginPIN starts a sign-in that must be approved as plexID, not as whatever
// account the owner happens to be signed into.
func (s *Service) BeginPIN(ctx context.Context, userID, plexID int64) (Status, error) {
	if err := viewerIDs(userID, plexID); err != nil {
		return Status{}, err
	}
	pin, err := s.client.createPIN(ctx)
	if err != nil {
		return Status{}, err
	}
	sealed, err := config.SealWith(s.sealer, pin.Code, "plexwatch", "pin", strconv.FormatInt(userID, 10))
	if err != nil {
		return Status{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO plexwatch_pin(user_id, pin_id, code, expires_at) VALUES(?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET pin_id=excluded.pin_id, code=excluded.code, expires_at=excluded.expires_at`,
		userID, pin.ID, sealed, pin.ExpiresAt.UnixNano())
	if err != nil {
		return Status{}, err
	}
	return s.Status(ctx, userID, plexID)
}

// FinishPIN stores the account only when its Plex id equals plexID. The
// viewer approving the PIN as themselves is the consent to sync. Connecting
// the same account again (after Plex refused the old token) keeps the
// chosen direction, the import cursor and the last-read Plex state.
func (s *Service) FinishPIN(ctx context.Context, userID, plexID int64) (Status, error) {
	if err := viewerIDs(userID, plexID); err != nil {
		return Status{}, err
	}
	pin, ok, err := s.loadPIN(ctx, userID)
	if err != nil {
		return Status{}, err
	}
	if !ok || !s.now().Before(pin.expires) {
		return Status{}, errors.New("plexwatch: no active sign-in")
	}
	token, err := s.client.pollPIN(ctx, pin.id, pin.code)
	if err != nil {
		return Status{}, err
	}
	if token == "" {
		return s.Status(ctx, userID, plexID)
	}
	acct, err := s.client.account(ctx, token)
	if err != nil {
		if errors.Is(err, ErrRevoked) {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM plexwatch_pin WHERE user_id=?`, userID)
		}
		return Status{}, err
	}
	if acct.ID != plexID {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM plexwatch_pin WHERE user_id=?`, userID)
		return Status{}, ErrAccountMismatch
	}
	sealed, err := config.SealWith(s.sealer, token, "plexwatch", "token", strconv.FormatInt(userID, 10), tokenSealLabel)
	if err != nil {
		return Status{}, err
	}
	now := s.now().UnixNano()
	err = s.withFence(ctx, userID, func(tx *sql.Tx) error {
		var oldPlex int64
		switch err := tx.QueryRowContext(ctx, `SELECT plex_id FROM plexwatch_account WHERE user_id=?`, userID).Scan(&oldPlex); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		case oldPlex != plexID:
			if err := s.forgetRemoteTx(ctx, tx, userID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE plexwatch_account SET direction=0, import_cursor='', last_sweep=0, active_after=-1 WHERE user_id=?`, userID); err != nil {
				return err
			}
		}
		var gen int64
		if err := tx.QueryRowContext(ctx, `UPDATE plexwatch_fence SET generation=generation+1 WHERE user_id=? RETURNING generation`, userID).Scan(&gen); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_account(user_id, plex_id, username, token, direction, checked_at, revoked, updated_at, token_generation)
			VALUES(?,?,?,?,0,?,0,?,?)
			ON CONFLICT(user_id) DO UPDATE SET plex_id=excluded.plex_id, username=excluded.username, token=excluded.token,
			  checked_at=excluded.checked_at, revoked=0, updated_at=excluded.updated_at, token_generation=excluded.token_generation,
			  import_lease=0, next_allowed=0, next_import=0, import_problem='', send_problem='', problem_code=0, import_failures=0,
			  consent=-1, consent_checked=0`,
			userID, plexID, acct.Username, sealed, now, now, gen); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_pin WHERE user_id=?`, userID); err != nil {
			return err
		}
		return s.state.DropPendingSyncTx(ctx, tx, userID, "replaced")
	})
	if err != nil {
		return Status{}, err
	}
	st, err := s.Status(ctx, userID, plexID)
	st.Restricted = acct.Restricted
	return st, err
}

// SetDirection selects outbound (bit 0) and inbound (bit 1) sync.
// Enabling outbound does not upload existing history.
func (s *Service) SetDirection(ctx context.Context, userID, plexID int64, direction, interval int) (Status, error) {
	if direction < 0 || direction > 3 || (interval != 5 && interval != 10 && interval != 30 && interval != 60) {
		return Status{}, errors.New("plexwatch: invalid sync settings")
	}
	row, err := s.requireMatch(ctx, userID, plexID)
	if err != nil {
		return Status{}, err
	}
	now := s.now()
	err = s.withFence(ctx, userID, func(tx *sql.Tx) error {
		var current int64
		var was int
		if err := tx.QueryRowContext(ctx, `SELECT f.generation, a.direction FROM plexwatch_fence f JOIN plexwatch_account a ON a.user_id=f.user_id WHERE f.user_id=?`, userID).Scan(&current, &was); err != nil {
			return err
		}
		if current != row.tokenGeneration {
			return errStaleGeneration
		}
		next := current
		if direction&1 == 0 && was&1 != 0 {
			// Turning sending off moves the fence: an intent already queued, or
			// one whose admission began under the old generation, is not sent.
			if err := tx.QueryRowContext(ctx, `UPDATE plexwatch_fence SET generation=generation+1 WHERE user_id=? RETURNING generation`, userID).Scan(&next); err != nil {
				return err
			}
			if err := s.state.DropPendingSyncTx(ctx, tx, userID, "disabled"); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE plexwatch_account SET direction=?, import_interval=?, token_generation=?, updated_at=?,
			next_import=CASE WHEN (direction & 2)=0 AND (? & 2)<>0 THEN ? WHEN import_interval<>? AND (? & 2)<>0 THEN ? ELSE next_import END
			WHERE user_id=? AND plex_id=? AND token_generation=? AND revoked=0 AND token<>''`,
			direction, interval, next, now.UnixNano(),
			direction, now.UnixNano(), interval, direction, now.Add(time.Duration(interval)*time.Minute).UnixNano(),
			userID, plexID, current)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return errStaleGeneration
		}
		return nil
	})
	if err != nil {
		return Status{}, err
	}
	return s.Status(ctx, userID, plexID)
}

// Disconnect removes the stored account token, cancels pending delivery and
// forgets what was read from that account.
func (s *Service) Disconnect(ctx context.Context, userID, plexID int64) (Status, error) {
	if err := viewerIDs(userID, plexID); err != nil {
		return Status{}, err
	}
	if err := s.withFence(ctx, userID, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE plexwatch_fence SET generation=generation+1 WHERE user_id=?`, userID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_account WHERE user_id=? AND plex_id=?`, userID, plexID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return errStaleGeneration
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_pin WHERE user_id=?`, userID); err != nil {
			return err
		}
		if err := s.forgetRemoteTx(ctx, tx, userID); err != nil {
			return err
		}
		return s.state.DropPendingSyncTx(ctx, tx, userID, "disconnected")
	}); err != nil && !errors.Is(err, errStaleGeneration) {
		return Status{}, err
	}
	return s.Status(ctx, userID, plexID)
}

// Retry makes failed sends claimable again and clears a rate-limit or
// network pause so the next pass runs now.
func (s *Service) Retry(ctx context.Context, userID, plexID int64) (Status, error) {
	row, err := s.requireMatch(ctx, userID, plexID)
	if err != nil {
		return Status{}, err
	}
	if err := s.state.RetryDeadSync(ctx, userID, failureReasons); err != nil {
		return Status{}, err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE plexwatch_account SET next_allowed=0, send_problem='', import_failures=0,
		next_import=CASE WHEN (direction & 2)<>0 THEN ? ELSE next_import END
		WHERE user_id=? AND token_generation=?`, s.now().UnixNano(), userID, row.tokenGeneration); err != nil {
		return Status{}, err
	}
	return s.Status(ctx, userID, plexID)
}

// errStaleGeneration means the stored account changed under the caller.
var errStaleGeneration = errors.New("plexwatch: stored account changed")

// revoke records that Plex refused the stored token for generation. The
// token is erased and the fence moves, so nothing queued is sent with it;
// the chosen direction stays for when the viewer connects again. It returns
// ErrRevoked when this call made the transition, nil when it was stale.
func (s *Service) revoke(ctx context.Context, userID, generation int64) error {
	revoked := false
	err := s.withFence(ctx, userID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE plexwatch_account SET token='', revoked=1, updated_at=?
			WHERE user_id=? AND token_generation=? AND revoked=0`, s.now().UnixNano(), userID, generation)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plexwatch_fence SET generation=generation+1 WHERE user_id=?`, userID); err != nil {
			return err
		}
		if err := s.state.DropPendingSyncTx(ctx, tx, userID, "revoked"); err != nil {
			return err
		}
		revoked = true
		return nil
	})
	if err != nil {
		return err
	}
	if revoked {
		s.log.Info("plex account needs to be connected again", "user", userID)
		return ErrRevoked
	}
	return nil
}

func (s *Service) requireMatch(ctx context.Context, userID, plexID int64) (accountRow, error) {
	if err := viewerIDs(userID, plexID); err != nil {
		return accountRow{}, err
	}
	row, ok, err := s.loadAccount(ctx, userID)
	if err != nil {
		return accountRow{}, err
	}
	if !ok || row.revoked || row.token == "" {
		return accountRow{}, ErrNotConnected
	}
	if row.plexID != plexID {
		return accountRow{}, ErrAccountMismatch
	}
	return row, nil
}

// ErrNotConnected means the viewer has no usable Plex account connection.
var ErrNotConnected = errors.New("plexwatch: viewer has not connected a Plex account")

func viewerIDs(userID, plexID int64) error {
	if userID <= 0 || plexID <= 0 {
		return errors.New("plexwatch: viewer has no Plex account id")
	}
	return nil
}

func (s *Service) loadAccount(ctx context.Context, userID int64) (accountRow, bool, error) {
	var row accountRow
	var revoked int
	var sealed string
	err := s.db.QueryRowContext(ctx, `SELECT plex_id, username, token, direction, revoked, import_cursor, token_generation, next_allowed, consent, consent_checked, active_after
		FROM plexwatch_account WHERE user_id=?`, userID).
		Scan(&row.plexID, &row.username, &sealed, &row.direction, &revoked, &row.cursor, &row.tokenGeneration, &row.nextAllowed, &row.consent, &row.consentChecked, &row.activeAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return accountRow{}, false, nil
	}
	if err != nil {
		return accountRow{}, false, err
	}
	row.userID = userID
	row.revoked = revoked != 0
	if sealed != "" {
		plain, _, err := config.OpenWith(s.sealer, sealed, "plexwatch", "token", strconv.FormatInt(userID, 10), tokenSealLabel)
		if err != nil {
			return accountRow{}, false, err
		}
		row.token = plain
	}
	return row, true, nil
}

type pinRow struct {
	id      int64
	code    string
	expires time.Time
}

func (s *Service) loadPIN(ctx context.Context, userID int64) (pinRow, bool, error) {
	var sealed string
	var id, exp int64
	err := s.db.QueryRowContext(ctx, `SELECT pin_id, code, expires_at FROM plexwatch_pin WHERE user_id=?`, userID).Scan(&id, &sealed, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return pinRow{}, false, nil
	}
	if err != nil {
		return pinRow{}, false, err
	}
	code, _, err := config.OpenWith(s.sealer, sealed, "plexwatch", "pin", strconv.FormatInt(userID, 10))
	if err != nil {
		return pinRow{}, false, err
	}
	return pinRow{id: id, code: code, expires: time.Unix(0, exp)}, true, nil
}

// withFence runs an account transition with the viewer's fence row locked
// (FOR UPDATE on PostgreSQL). Send admission takes the same lock, so a
// disable cannot commit between admission's check and its commit.
func (s *Service) withFence(ctx context.Context, userID int64, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_fence(user_id, generation, inflight) VALUES(?,1,0)
		ON CONFLICT(user_id) DO NOTHING`, userID); err != nil {
		return err
	}
	if _, err := s.lockFence(ctx, tx, userID); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) lockFence(ctx context.Context, tx *sql.Tx, userID int64) (int64, error) {
	q := `SELECT generation FROM plexwatch_fence WHERE user_id=?` + store.ForUpdate(store.DialectOf(s.db))
	var gen int64
	err := tx.QueryRowContext(ctx, q, userID).Scan(&gen)
	return gen, err
}

func (s *Service) forgetRemoteTx(ctx context.Context, tx *sql.Tx, userID int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_remote WHERE user_id=?`, userID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_expand WHERE user_id=?`, userID)
	return err
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
