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
  receipt_at INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX plexwatch_attempt_conflict ON plexwatch_attempt(plex_id,guid,aspect)
  WHERE receipt_at=0 AND guid<>'';
CREATE INDEX plexwatch_attempt_local ON plexwatch_attempt(user_id,rating_key,aspect,receipt_at);
INSERT INTO plexwatch_attempt(attempt_id,seq,user_id,rating_key,plex_id,generation,guid,aspect,occurred_at,payload,admitted_at)
 SELECT 'legacy:' || CAST(seq AS TEXT), seq, user_id, rating_key,
 COALESCE((SELECT plex_id FROM plexwatch_account a WHERE a.user_id=state_sync_journal.user_id),0),
 consent_generation, '', aspect, occurred_at, '', 0 FROM state_sync_journal WHERE delivered_at=0;
UPDATE state_sync_journal SET dead=1, last_error='uncertain_legacy', claimed_by='', claimed_at=0 WHERE delivered_at=0;`)

}

// GUIDMap resolves a local title to a verified plex:// GUID and the reverse
// for a GUID already stored for exactly one title.
type GUIDMap interface {
	Resolve(ctx context.Context, key core.RatingKey) (string, error)
	ExactKey(ctx context.Context, guid string) (core.RatingKey, error)
}

// Options configure a Service. BaseURL is the test origin for every Plex
// host. Leave it empty in production.
type Options struct {
	DB       *sql.DB
	State    *state.State
	Sealer   config.FieldSealer
	GUID     GUIDMap
	NodeID   string
	ClientID string
	BaseURL  string
	HTTP     *http.Client
	Logger   *slog.Logger
	Now      func() time.Time
}

// Service is one server's Plex account watch-state integration.
type Service struct {
	db          *sql.DB
	state       *state.State
	sealer      config.FieldSealer
	guid        GUIDMap
	client      *Client
	node        string
	log         *slog.Logger
	now         func() time.Time
	claimFor    time.Duration
	mu          sync.Mutex
	closed      chan struct{}
	lastCompact time.Time
	// beforeEnable, when set by a test, runs after eligibility is reread and
	// before the enable write.
	beforeEnable func()
	// attemptStart, when set by a test, runs after this attempt owns the
	// claim and the title lock.
	attemptStart func(attempt string)
	// admitFault, when set by a test, fails the fence read inside admission.
	// The error stays retryable.
	admitFault func() error
	// afterFenceRead, when set by a test, runs after the fence row is locked
	// and the intent is eligible, and before the permit insert.
	afterFenceRead func()
	// Fault hooks exercise crash boundaries; production leaves them unset.
	afterAdmission  func() error
	receiptFault    func(string) error
	admissionCommit func(*sql.Tx) error
	receiptCommit   func(*sql.Tx) error
	afterPost       func() error
}

// New starts nothing. Call Run to deliver, or DeliverOnce from tests.
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
		var b [8]byte
		_, _ = rand.Read(b[:])
		node = hex.EncodeToString(b[:])
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
		client: c, node: node, log: log, now: now, claimFor: 2 * time.Minute,
		closed: make(chan struct{}),
	}, nil
}

// Close stops Run.
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
type Status struct {
	Connected    bool   `json:"connected"`
	PlexID       int64  `json:"plexId,omitempty"`
	Username     string `json:"username,omitempty"`
	Enabled      bool   `json:"enabled"`
	Eligibility  string `json:"eligibility"`
	Revoked      bool   `json:"revoked"`
	Pending      int    `json:"pending"`
	Held         int    `json:"held"`
	HeldReason   string `json:"heldReason,omitempty"`
	HeldLegacy   int    `json:"heldLegacy"`
	Dead         int    `json:"dead"`
	InFlight     bool   `json:"inFlight"`
	Skipped      int    `json:"skipped"`
	Restricted   bool   `json:"restricted"`
	PINCode      string `json:"pinCode,omitempty"`
	PINURL       string `json:"pinUrl,omitempty"`
	PINExpires   string `json:"pinExpires,omitempty"`
	AccountMatch bool   `json:"accountMatch"`
}

type accountRow struct {
	userID          int64
	plexID          int64
	username        string
	token           string
	enabled         bool
	eligibility     string
	revoked         bool
	cursor          string
	tokenGeneration int64
}

func (s *Service) Status(ctx context.Context, userID, plexID int64) (Status, error) {
	if err := viewerIDs(userID, plexID); err != nil {
		return Status{}, err
	}
	st := Status{Eligibility: "unchecked", AccountMatch: true}
	row, ok, err := s.loadAccount(ctx, userID)
	if err != nil {
		return Status{}, err
	}
	if ok {
		st.Connected = row.token != "" && !row.revoked
		st.PlexID = row.plexID
		st.Username = row.username
		st.Enabled = row.enabled
		st.Eligibility = row.eligibility
		st.Revoked = row.revoked
		st.AccountMatch = row.plexID == plexID
		if row.plexID != plexID {
			st.Connected = false
			st.Enabled = false
		}
	}
	pending, dead, err := s.state.PendingSyncCount(ctx, userID)
	if err != nil {
		return Status{}, err
	}
	st.Pending, st.Dead = pending, dead

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_attempt WHERE user_id=? AND receipt_at=0`, userID).Scan(&st.Held); err != nil {
		return Status{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_attempt WHERE user_id=? AND receipt_at=0 AND guid=''`, userID).Scan(&st.HeldLegacy); err != nil {
		return Status{}, err
	}
	if st.Held > 0 {
		st.HeldReason = "An earlier send may have changed Plex. Conflicting changes are held; retry and reconnect do not release them."
		if st.HeldLegacy > 0 {
			st.HeldReason = "Legacy undelivered changes lack a recorded target. This account’s affected watch or rating sends are held, even after retry or reconnect."
		}
	}
	var inflight int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_permit WHERE user_id=? AND until_at>?`, userID, s.now().UnixNano()).Scan(&inflight); err != nil {
		return Status{}, err
	}
	st.InFlight = inflight > 0
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_unmapped WHERE user_id=?`, userID).Scan(&st.Skipped); err != nil {
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

// FinishPIN stores the account only when its Plex id equals plexID.
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
		return Status{}, err
	}
	if acct.ID != plexID {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM plexwatch_pin WHERE user_id=?`, userID)
		return Status{}, ErrAccountMismatch
	}
	eligibility := "unchecked"
	switch err := s.client.eligibility(ctx, token, plexID); {
	case err == nil:
		eligibility = "allowed"
	case errors.Is(err, ErrNotEligible):
		eligibility = "refused"
	case errors.Is(err, ErrRevoked):
		_, _ = s.db.ExecContext(ctx, `DELETE FROM plexwatch_pin WHERE user_id=?`, userID)
		return Status{}, ErrRevoked
	default:
		return Status{}, err
	}
	sealed, err := config.SealWith(s.sealer, token, "plexwatch", "token", strconv.FormatInt(userID, 10), "scrobbles.plex.tv")
	if err != nil {
		return Status{}, err
	}
	err = s.withFence(ctx, userID, func(tx *sql.Tx) error {
		var gen int64
		if err := tx.QueryRowContext(ctx, `UPDATE plexwatch_fence SET generation=generation+1 WHERE user_id=? RETURNING generation`, userID).Scan(&gen); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_account(user_id, plex_id, username, token, enabled, eligibility, checked_at, revoked, updated_at, token_generation)
			VALUES(?,?,?,?,0,?,?,0,?,?)
			ON CONFLICT(user_id) DO UPDATE SET plex_id=excluded.plex_id, username=excluded.username, token=excluded.token,
			  enabled=0, eligibility=excluded.eligibility, checked_at=excluded.checked_at, revoked=0, updated_at=excluded.updated_at,
			  token_generation=excluded.token_generation`,
			userID, plexID, acct.Username, sealed, eligibility, s.now().UnixNano(), s.now().UnixNano(), gen); err != nil {
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

// SetEnabled turns delivery on or off. On does not upload existing history.
// Off cancels undelivered intents. A later enable sends only newer changes.
func (s *Service) SetEnabled(ctx context.Context, userID, plexID int64, on bool) (Status, error) {
	row, err := s.requireMatch(ctx, userID, plexID)
	if err != nil {
		return Status{}, err
	}
	if on {
		if err := s.refreshEligibility(ctx, userID, row); err != nil {
			return Status{}, err
		}
		row, err = s.requireMatch(ctx, userID, plexID)
		if err != nil {
			return Status{}, err
		}
		if row.eligibility != "allowed" {
			return Status{}, ErrNotEligible
		}
		if s.beforeEnable != nil {
			s.beforeEnable()
		}
		err = s.withFence(ctx, userID, func(tx *sql.Tx) error {
			res, err := tx.ExecContext(ctx, `UPDATE plexwatch_account SET enabled=1, updated_at=?
				WHERE user_id=? AND plex_id=? AND token_generation=? AND revoked=0 AND token<>'' AND eligibility='allowed'
				AND token_generation=(SELECT generation FROM plexwatch_fence WHERE user_id=?)`,
				s.now().UnixNano(), userID, plexID, row.tokenGeneration, userID)
			if err != nil {
				return err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return errStaleGeneration
			}
			return nil
		})
		if err != nil {
			return Status{}, err
		}
	} else if err := s.rotateDisabled(ctx, userID, plexID, "disabled"); err != nil {
		return Status{}, err
	}
	return s.Status(ctx, userID, plexID)
}

// rotateDisabled bumps the fence, records that generation on the account, and
// cancels pending intents in one transaction.
func (s *Service) rotateDisabled(ctx context.Context, userID, plexID int64, reason string) error {
	return s.withFence(ctx, userID, func(tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT generation FROM plexwatch_fence WHERE user_id=?`, userID).Scan(&current); err != nil {
			return err
		}
		var next int64
		if err := tx.QueryRowContext(ctx, `UPDATE plexwatch_fence SET generation=generation+1 WHERE user_id=? AND generation=? RETURNING generation`, userID, current).Scan(&next); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE plexwatch_account SET enabled=0, token_generation=?, updated_at=?
			WHERE user_id=? AND plex_id=? AND token_generation=?`,
			next, s.now().UnixNano(), userID, plexID, current)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errStaleGeneration
		}
		return s.state.DropPendingSyncTx(ctx, tx, userID, reason)
	})
}

// Disconnect removes the stored account token and cancels pending delivery.
func (s *Service) Disconnect(ctx context.Context, userID, plexID int64) (Status, error) {
	if err := viewerIDs(userID, plexID); err != nil {
		return Status{}, err
	}
	if err := s.withFence(ctx, userID, func(tx *sql.Tx) error {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT generation FROM plexwatch_fence WHERE user_id=?`, userID).Scan(&current); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plexwatch_fence SET generation=generation+1 WHERE user_id=? AND generation=?`, userID, current); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_account WHERE user_id=? AND plex_id=?`, userID, plexID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errStaleGeneration
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_pin WHERE user_id=?`, userID); err != nil {
			return err
		}
		return s.state.DropPendingSyncTx(ctx, tx, userID, "disconnected")
	}); err != nil && !errors.Is(err, errStaleGeneration) {
		return Status{}, err
	}
	return s.Status(ctx, userID, plexID)
}

// Retry rechecks eligibility and makes missing-GUID rows claimable again.
func (s *Service) Retry(ctx context.Context, userID, plexID int64) (Status, error) {
	row, err := s.requireMatch(ctx, userID, plexID)
	if err != nil {
		return Status{}, err
	}
	if err := s.refreshEligibility(ctx, userID, row); err != nil && !errors.Is(err, ErrNotEligible) && !errors.Is(err, ErrRevoked) {
		return Status{}, err
	}
	if err := s.state.RetryDeadSync(ctx, userID, []string{"missing_guid"}); err != nil {
		return Status{}, err
	}
	if err := s.retryUnmapped(ctx, userID); err != nil {
		return Status{}, err
	}
	return s.Status(ctx, userID, plexID)
}

func (s *Service) refreshEligibility(ctx context.Context, userID int64, row accountRow) error {
	switch err := s.client.eligibility(ctx, row.token, row.plexID); {
	case err == nil:
		res, e := s.db.ExecContext(ctx, `UPDATE plexwatch_account SET eligibility='allowed', checked_at=?, updated_at=?
			WHERE user_id=? AND token_generation=? AND revoked=0 AND token<>''`,
			s.now().UnixNano(), s.now().UnixNano(), userID, row.tokenGeneration)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			return errStaleGeneration
		}
		return nil
	case errors.Is(err, ErrNotEligible):
		res, e := s.db.ExecContext(ctx, `UPDATE plexwatch_account SET eligibility='refused', enabled=0, checked_at=?, updated_at=?
			WHERE user_id=? AND token_generation=? AND revoked=0`,
			s.now().UnixNano(), s.now().UnixNano(), userID, row.tokenGeneration)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n == 0 {
			return nil
		}
		return ErrNotEligible
	case errors.Is(err, ErrRevoked):
		return s.revoke(ctx, userID, row.tokenGeneration)
	default:
		return err
	}
}

// errStaleGeneration means the token that was checked is no longer the
// stored one. The result must not clear a revoke or change the replacement.
var errStaleGeneration = errors.New("plexwatch: stored account changed before consent was recorded")

func (s *Service) revoke(ctx context.Context, userID, generation int64) error {
	revoked := false
	err := s.withFence(ctx, userID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE plexwatch_account SET token='', enabled=0, revoked=1, eligibility='unauthorized', updated_at=?
			WHERE user_id=? AND token_generation=?`, s.now().UnixNano(), userID, generation)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return nil
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
		return accountRow{}, errors.New("plexwatch: viewer has not connected a Plex account")
	}
	if row.plexID != plexID {
		return accountRow{}, ErrAccountMismatch
	}
	return row, nil
}

// ProbeToken reads identity and eligibility for a token the caller already
// holds. It does not create a PIN, store the token, or send watch state.
func (s *Service) ProbeToken(ctx context.Context, token string, plexID int64) (remoteAccount, error) {
	if token == "" || plexID <= 0 {
		return remoteAccount{}, errors.New("plexwatch: viewer has no Plex account id")
	}
	acct, err := s.client.account(ctx, token)
	if err != nil {
		return remoteAccount{}, err
	}
	if acct.ID != plexID {
		return remoteAccount{}, ErrAccountMismatch
	}
	if err := s.client.eligibility(ctx, token, plexID); err != nil {
		return acct, err
	}
	return acct, nil
}

func viewerIDs(userID, plexID int64) error {
	if userID <= 0 || plexID <= 0 {
		return errors.New("plexwatch: viewer has no Plex account id")
	}
	return nil
}

func (s *Service) loadAccount(ctx context.Context, userID int64) (accountRow, bool, error) {
	var row accountRow
	var enabled, revoked int
	var sealed string
	err := s.db.QueryRowContext(ctx, `SELECT plex_id, username, token, enabled, eligibility, revoked, import_cursor, token_generation
		FROM plexwatch_account WHERE user_id=?`, userID).
		Scan(&row.plexID, &row.username, &sealed, &enabled, &row.eligibility, &revoked, &row.cursor, &row.tokenGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return accountRow{}, false, nil
	}
	if err != nil {
		return accountRow{}, false, err
	}
	row.userID = userID
	row.enabled = enabled != 0
	row.revoked = revoked != 0
	if sealed != "" {
		plain, _, err := config.OpenWith(s.sealer, sealed, "plexwatch", "token", strconv.FormatInt(userID, 10), "scrobbles.plex.tv")
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
