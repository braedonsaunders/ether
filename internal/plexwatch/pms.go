package plexwatch

import (
	"context"
	"database/sql"
	"errors"
	"github.com/braedonsaunders/ether/internal/store"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/plexguid"
	"github.com/braedonsaunders/ether/internal/state"
)

// Servers syncs watched state with Plex Media Servers the owner runs,
// over each server's own HTTP API and with the owner's token for it.
//
// The owner adds servers and maps each PMS account to one Ether viewer.
// Nothing is guessed from names. Reads are incremental: PMS watch history
// after a stored cursor, and Ether's watch rows after a stored
// (updated_at, rating_key) cursor on the state_watch_updated index. No
// request path lists a whole PMS library or Ether's catalogue.
//
// Echo safety rests on plexwatch_pms_sent, one row per server, viewer and
// title: the watched state the server is known to hold, the local state it
// was last reconciled with, and when Ether itself last wrote it. A write is
// made only when the local watch state changed since that reconciliation
// and the server does not already hold it, so a change imported from server
// A is never sent back to A, while server B (which has no such row) gets it
// once. History rows that are the echo of Ether's own scrobble are ignored.
//
// This path is independent of the per-viewer Plex account sync (Service).
type Servers struct {
	db      *sql.DB
	state   *state.State
	sealer  config.FieldSealer
	guid    GUIDMap
	catalog PMSCatalog
	http    *http.Client
	client  string
	log     *slog.Logger
	now     func() time.Time
	enabled func() bool
	runMu   sync.Mutex
	// viewerExists confirms a mapping target is a real Ether viewer.
	viewerExists func(context.Context, int64) (bool, error)
	closed       chan struct{}
	closeMu      sync.Mutex
}

// PMSCatalog verifies matched titles and resolves IMDb/TVDB identifiers.
type PMSCatalog interface {
	Get(ctx context.Context, key core.RatingKey) (*core.Title, error)
	ByExternalIDForType(ctx context.Context, source, id string, typ core.MediaType) (*core.Title, error)
}

// ServersOptions configure Servers. HTTP defaults to the direct egress route.
type ServersOptions struct {
	// Enabled reads the live global switch. Nil preserves the legacy enabled behavior.
	Enabled  func() bool
	DB       *sql.DB
	State    *state.State
	Sealer   config.FieldSealer
	GUID     GUIDMap
	Catalog  PMSCatalog
	ClientID string
	HTTP     *http.Client
	Logger   *slog.Logger
	Now      func() time.Time
	// ViewerExists reports whether an Ether viewer id is real. Nil skips the check.
	ViewerExists func(context.Context, int64) (bool, error)
}

const (
	pmsMaxServers   = 8
	pmsMaxAccounts  = 200
	pmsUnmatchedCap = 2000
)

// Direction bits, shared with the account path: 1 Ether → Plex, 2 Plex → Ether.
const (
	pmsSend    = 1
	pmsReceive = 2
)

// NewServers starts nothing; call Run under cluster leadership.
func NewServers(o ServersOptions) (*Servers, error) {
	if o.DB == nil || o.State == nil {
		return nil, errors.New("plexwatch: database and state are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	hc := o.HTTP
	if hc == nil {
		hc = egress.Client(egress.CapDirect, 20*time.Second)
	}
	clone := *hc
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if o.ClientID == "" {
		o.ClientID = plexguid.ClientIdentifier("", "")
	}
	return &Servers{db: o.DB, state: o.State, sealer: o.Sealer, guid: o.GUID, catalog: o.Catalog,
		http: &clone, client: o.ClientID, log: o.Logger, now: o.Now, viewerExists: o.ViewerExists,
		enabled: o.Enabled, closed: make(chan struct{})}, nil
}

// Close stops Run.
func (s *Servers) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	return nil
}

// PMSServer is one configured server as the owner console shows it. It never
// carries a token.
type PMSServer struct {
	ID        int64        `json:"id"`
	Name      string       `json:"name"`
	URL       string       `json:"url"`
	MachineID string       `json:"machineId"`
	Version   string       `json:"version"`
	Owner     string       `json:"owner"`
	CheckedAt int64        `json:"checkedAt"`
	Problem   string       `json:"problem"`
	Viewers   []PMSMapping `json:"viewers"`
}

// PMSMapping is one PMS account mapped to one Ether viewer.
type PMSMapping struct {
	AccountID   int64  `json:"accountId"`
	AccountName string `json:"accountName"`
	Owner       bool   `json:"owner"`
	UserID      int64  `json:"userId"`
	Direction   int    `json:"direction"`
	Interval    int    `json:"interval"`
	CanSend     bool   `json:"canSend"`
	LastImport  int64  `json:"lastImport"`
	LastSend    int64  `json:"lastSend"`
	NextSync    int64  `json:"nextSync"`
	Problem     string `json:"problem"`
	Unmatched   int    `json:"unmatched"`
}

// PMSAccount is a PMS account and, when mapped, its viewer.
type PMSAccount struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Owner  bool   `json:"owner"`
	UserID int64  `json:"userId"`
}

// PMSUnmatched is a title one direction could not match by identifier.
type PMSUnmatched struct {
	Direction string `json:"direction"`
	Ref       string `json:"ref"`
	Title     string `json:"title"`
	Reason    string `json:"reason"`
	SeenAt    int64  `json:"seenAt"`
}

// PMSMappingInput changes one account's mapping. UserID 0 removes it.
// Token nil keeps the stored per-account token; "" clears it.
type PMSMappingInput struct {
	UserID    int64
	Direction int
	Interval  int
	Token     *string
}

var (
	// ErrPMSDuplicate means this server is already configured.
	ErrPMSDuplicate = errors.New("plexwatch: this Plex Media Server is already added")
	// ErrPMSDifferentServer means an edited address answers as another server.
	ErrPMSDifferentServer = errors.New("plexwatch: that address is a different Plex Media Server")
	// ErrPMSLimit means the server or account limit is reached.
	ErrPMSLimit = errors.New("plexwatch: limit reached")
	// ErrPMSNeedsToken means sending to Plex for this account needs its own token.
	ErrPMSNeedsToken = errors.New("plexwatch: sending to Plex for this person needs their token for this server")
	// ErrPMSInvalid means the request is malformed.
	ErrPMSInvalid = errors.New("plexwatch: invalid server settings")
)

type pmsServerRow struct {
	id        int64
	name      string
	baseURL   string
	token     string
	machineID string
}

func (s *Servers) seal(plain string, ctx ...string) (string, error) {
	return config.SealWith(s.sealer, plain, append([]string{"plexwatch", "pms"}, ctx...)...)
}

func (s *Servers) open(stored string, ctx ...string) (string, error) {
	if stored == "" {
		return "", nil
	}
	plain, _, err := config.OpenWith(s.sealer, stored, append([]string{"plexwatch", "pms"}, ctx...)...)
	return plain, err
}

func (s *Servers) clientFor(baseURL string) (*pmsClient, error) {
	return newPMSClient(baseURL, s.http, s.client, s.now)
}

// validate checks the address answers as a PMS and the token is its owner's.
func (s *Servers) validate(ctx context.Context, baseURL, token string) (machine, version, owner string, err error) {
	c, err := s.clientFor(baseURL)
	if err != nil {
		return "", "", "", err
	}
	machine, version, err = c.identity(ctx, token)
	if err != nil {
		var st *StatusError
		if errors.As(err, &st) && st.Status == http.StatusUnauthorized {
			return "", "", "", ErrPMSToken
		}
		if errors.As(err, &st) {
			return "", "", "", ErrPMSUnreachable
		}
		return "", "", "", err
	}
	owner, err = c.ownerAccount(ctx, token)
	if err != nil {
		var st *StatusError
		if errors.As(err, &st) {
			return "", "", "", ErrPMSUnreachable
		}
		return "", "", "", err
	}
	return machine, version, owner, nil
}

// Add validates and stores a server. The token is sealed and never returned.
func (s *Servers) Add(ctx context.Context, name, baseURL, token string) (PMSServer, error) {
	name, token = strings.TrimSpace(name), strings.TrimSpace(token)
	u, err := parsePMSURL(baseURL)
	if err != nil {
		return PMSServer{}, err
	}
	if token == "" || len(name) > 100 || len(token) > 512 {
		return PMSServer{}, ErrPMSInvalid
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_pms`).Scan(&n); err != nil {
		return PMSServer{}, err
	}
	if n >= pmsMaxServers {
		return PMSServer{}, ErrPMSLimit
	}
	machine, version, owner, err := s.validate(ctx, u.String(), token)
	if err != nil {
		return PMSServer{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_pms WHERE machine_id=?`, machine).Scan(&n); err != nil {
		return PMSServer{}, err
	}
	if n > 0 {
		return PMSServer{}, ErrPMSDuplicate
	}
	if name == "" {
		name = "Plex Media Server"
	}
	sealed, err := s.seal(token, machine)
	if err != nil {
		return PMSServer{}, err
	}
	now := s.now()
	var id int64
	err = s.db.QueryRowContext(ctx, `INSERT INTO plexwatch_pms(name,base_url,token,machine_id,version,owner_name,checked_at,created_at)
		VALUES(?,?,?,?,?,?,?,?) RETURNING id`, name, u.String(), sealed, machine, version, owner, now.Unix(), now.Unix()).Scan(&id)
	if err != nil {
		return PMSServer{}, err
	}
	return s.Server(ctx, id)
}

// Update renames a server, moves it to a new address or replaces its token.
// An empty token keeps the stored one. The address must still answer as the
// same server, so cursors and matches stay valid.
func (s *Servers) Update(ctx context.Context, id int64, name, baseURL, token string) (PMSServer, error) {
	row, err := s.loadServer(ctx, id)
	if err != nil {
		return PMSServer{}, err
	}
	name, token = strings.TrimSpace(name), strings.TrimSpace(token)
	if len(name) > 100 || len(token) > 512 {
		return PMSServer{}, ErrPMSInvalid
	}
	if name == "" {
		name = row.name
	}
	u, err := parsePMSURL(baseURL)
	if err != nil {
		return PMSServer{}, err
	}
	sealed := ""
	if token == "" {
		if token, err = s.open(row.token, row.machineID); err != nil {
			return PMSServer{}, err
		}
	}
	machine, version, owner, err := s.validate(ctx, u.String(), token)
	if err != nil {
		return PMSServer{}, err
	}
	if machine != row.machineID {
		return PMSServer{}, ErrPMSDifferentServer
	}
	if sealed, err = s.seal(token, machine); err != nil {
		return PMSServer{}, err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE plexwatch_pms SET name=?,base_url=?,token=?,version=?,owner_name=?,checked_at=?,problem='' WHERE id=?`,
		name, u.String(), sealed, version, owner, s.now().Unix(), id)
	if err != nil {
		return PMSServer{}, err
	}
	// A fixed token or address clears token and connection problems now
	// rather than at the next scheduled pass.
	if _, err := s.db.ExecContext(ctx, `UPDATE plexwatch_pms_viewer SET problem='',failures=0,next_allowed=0 WHERE server_id=? AND problem IN ('token','unreachable','server')`, id); err != nil {
		return PMSServer{}, err
	}
	return s.Server(ctx, id)
}

// Remove forgets a server, its mappings, cursors, matches and echo records.
func (s *Servers) Remove(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM plexwatch_pms_viewer WHERE server_id=?`,
		`DELETE FROM plexwatch_pms_item WHERE server_id=?`,
		`DELETE FROM plexwatch_pms_sent WHERE server_id=?`,
		`DELETE FROM plexwatch_pms_unmatched WHERE server_id=?`,
		`DELETE FROM plexwatch_pms WHERE id=?`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Servers) loadServer(ctx context.Context, id int64) (pmsServerRow, error) {
	var r pmsServerRow
	err := s.db.QueryRowContext(ctx, `SELECT id,name,base_url,token,machine_id FROM plexwatch_pms WHERE id=?`, id).
		Scan(&r.id, &r.name, &r.baseURL, &r.token, &r.machineID)
	if errors.Is(err, sql.ErrNoRows) {
		return r, core.ErrNotFound
	}
	return r, err
}

// List returns every server with its mappings.
func (s *Servers) List(ctx context.Context) ([]PMSServer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM plexwatch_pms ORDER BY id LIMIT ?`, pmsMaxServers*2)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]PMSServer, 0, len(ids))
	for _, id := range ids {
		srv, err := s.Server(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, srv)
	}
	return out, nil
}

// Server returns one server and its mappings.
func (s *Servers) Server(ctx context.Context, id int64) (PMSServer, error) {
	var srv PMSServer
	err := s.db.QueryRowContext(ctx, `SELECT id,name,base_url,machine_id,version,owner_name,checked_at,problem FROM plexwatch_pms WHERE id=?`, id).
		Scan(&srv.ID, &srv.Name, &srv.URL, &srv.MachineID, &srv.Version, &srv.Owner, &srv.CheckedAt, &srv.Problem)
	if errors.Is(err, sql.ErrNoRows) {
		return srv, core.ErrNotFound
	}
	if err != nil {
		return srv, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT v.account_id,v.account_name,v.owner,v.user_id,v.direction,v.interval_min,v.token,
		v.last_import,v.last_send,v.next_run,v.problem,
		(SELECT COUNT(*) FROM plexwatch_pms_unmatched u WHERE u.server_id=v.server_id AND u.user_id=v.user_id)
		FROM plexwatch_pms_viewer v WHERE v.server_id=? ORDER BY v.account_id`, id)
	if err != nil {
		return srv, err
	}
	defer rows.Close()
	srv.Viewers = []PMSMapping{}
	for rows.Next() {
		var m PMSMapping
		var owner int
		var token string
		if err := rows.Scan(&m.AccountID, &m.AccountName, &owner, &m.UserID, &m.Direction, &m.Interval, &token,
			&m.LastImport, &m.LastSend, &m.NextSync, &m.Problem, &m.Unmatched); err != nil {
			return srv, err
		}
		m.Owner = owner != 0
		m.CanSend = m.Owner || token != ""
		m.NextSync /= int64(time.Second)
		if m.Direction == 0 {
			m.NextSync = 0
		}
		srv.Viewers = append(srv.Viewers, m)
	}
	return srv, rows.Err()
}

// Accounts lists the server's accounts live, with their current viewer.
func (s *Servers) Accounts(ctx context.Context, id int64) ([]PMSAccount, error) {
	row, err := s.loadServer(ctx, id)
	if err != nil {
		return nil, err
	}
	token, err := s.open(row.token, row.machineID)
	if err != nil {
		return nil, err
	}
	c, err := s.clientFor(row.baseURL)
	if err != nil {
		return nil, err
	}
	list, err := c.accounts(ctx, token)
	if err != nil {
		return nil, mapPMSError(err)
	}
	var ownerName string
	_ = s.db.QueryRowContext(ctx, `SELECT owner_name FROM plexwatch_pms WHERE id=?`, id).Scan(&ownerName)
	mapped := map[int64]int64{}
	rows, err := s.db.QueryContext(ctx, `SELECT account_id,user_id FROM plexwatch_pms_viewer WHERE server_id=?`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a, u int64
		if err := rows.Scan(&a, &u); err != nil {
			rows.Close()
			return nil, err
		}
		mapped[a] = u
	}
	rows.Close()
	out := make([]PMSAccount, 0, len(list))
	for _, a := range list {
		out = append(out, PMSAccount{ID: a.ID, Name: a.Name, Owner: pmsOwnerAccount(a, ownerName), UserID: mapped[a.ID]})
	}
	return out, nil
}

// pmsOwnerAccount: PMS numbers its admin account 1 locally; the plex.tv
// username from /myplex/account is the fallback.
func pmsOwnerAccount(a pmsAccount, ownerName string) bool {
	return a.ID == 1 || (ownerName != "" && strings.EqualFold(a.Name, ownerName))
}

func validPMSInterval(n int) bool { return n == 5 || n == 10 || n == 30 || n == 60 }

// MapAccount maps a PMS account to a viewer and sets direction and interval.
// Turning a direction on starts it from now: existing history is not copied
// either way. Sending for an account other than the owner's needs that
// account's own token for this server.
func (s *Servers) MapAccount(ctx context.Context, id, accountID int64, in PMSMappingInput) (PMSServer, error) {
	row, err := s.loadServer(ctx, id)
	if err != nil {
		return PMSServer{}, err
	}
	if accountID <= 0 {
		return PMSServer{}, ErrPMSInvalid
	}
	if in.UserID == 0 {
		if err := s.unmap(ctx, id, accountID); err != nil {
			return PMSServer{}, err
		}
		return s.Server(ctx, id)
	}
	if in.UserID < 0 || in.Direction < 0 || in.Direction > 3 || !validPMSInterval(in.Interval) {
		return PMSServer{}, ErrPMSInvalid
	}
	if s.viewerExists != nil {
		ok, err := s.viewerExists(ctx, in.UserID)
		if err != nil {
			return PMSServer{}, err
		}
		if !ok {
			return PMSServer{}, core.ErrNotFound
		}
	}
	// The account must exist on the server; its name is stored for display.
	token, err := s.open(row.token, row.machineID)
	if err != nil {
		return PMSServer{}, err
	}
	c, err := s.clientFor(row.baseURL)
	if err != nil {
		return PMSServer{}, err
	}
	list, err := c.accounts(ctx, token)
	if err != nil {
		return PMSServer{}, mapPMSError(err)
	}
	var ownerName string
	_ = s.db.QueryRowContext(ctx, `SELECT owner_name FROM plexwatch_pms WHERE id=?`, id).Scan(&ownerName)
	var acct pmsAccount
	for _, a := range list {
		if a.ID == accountID {
			acct = a
		}
	}
	if acct.ID == 0 {
		return PMSServer{}, core.ErrNotFound
	}
	owner := pmsOwnerAccount(acct, ownerName)

	var cur struct {
		exists    bool
		direction int
		token     string
	}
	err = s.db.QueryRowContext(ctx, `SELECT direction,token FROM plexwatch_pms_viewer WHERE server_id=? AND account_id=?`, id, accountID).Scan(&cur.direction, &cur.token)
	switch {
	case err == nil:
		cur.exists = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return PMSServer{}, err
	}
	sealed := cur.token
	if in.Token != nil {
		plain := strings.TrimSpace(*in.Token)
		if len(plain) > 512 {
			return PMSServer{}, ErrPMSInvalid
		}
		sealed = ""
		if plain != "" {
			// /identity answers without a token; the library list does not.
			if _, err := c.sections(ctx, plain); err != nil {
				return PMSServer{}, mapPMSError(err)
			}
			if sealed, err = s.seal(plain, row.machineID, "account", strconv.FormatInt(accountID, 10)); err != nil {
				return PMSServer{}, err
			}
		}
	}
	if in.Direction&pmsSend != 0 && !owner && sealed == "" {
		return PMSServer{}, ErrPMSNeedsToken
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_pms_viewer WHERE server_id=? AND user_id=? AND account_id<>?`, id, in.UserID, accountID).Scan(&n); err != nil {
		return PMSServer{}, err
	}
	if n > 0 {
		return PMSServer{}, ErrPMSDuplicate
	}
	if !cur.exists {
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_pms_viewer WHERE server_id=?`, id).Scan(&n); err != nil {
			return PMSServer{}, err
		}
		if n >= pmsMaxAccounts {
			return PMSServer{}, ErrPMSLimit
		}
	}
	now := s.now()
	nowNS := now.UnixNano()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PMSServer{}, err
	}
	defer tx.Rollback()
	var prevUser int64
	if cur.exists {
		if err := tx.QueryRowContext(ctx, `SELECT user_id FROM plexwatch_pms_viewer WHERE server_id=? AND account_id=?`, id, accountID).Scan(&prevUser); err != nil {
			return PMSServer{}, err
		}
	}
	if cur.exists && prevUser != in.UserID {
		// A remap is a new pairing: drop the old viewer's echo and unmatched
		// rows on this server and start both cursors over from now.
		if err := s.dropPairTx(ctx, tx, id, prevUser); err != nil {
			return PMSServer{}, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_pms_viewer WHERE server_id=? AND account_id=?`, id, accountID); err != nil {
			return PMSServer{}, err
		}
		cur.exists, cur.direction = false, 0
	}
	if !cur.exists {
		if _, err := tx.ExecContext(ctx, `INSERT INTO plexwatch_pms_viewer(server_id,account_id,account_name,owner,user_id,token,direction,interval_min)
			VALUES(?,?,?,?,?,?,0,?)`, id, accountID, acct.Name, store.BoolInt(owner), in.UserID, sealed, in.Interval); err != nil {
			return PMSServer{}, err
		}
	}
	// A direction switched on starts at now. Ether rows are read from now on;
	// PMS history is read from a few minutes back to absorb clock skew.
	gainSend := in.Direction&pmsSend != 0 && cur.direction&pmsSend == 0
	gainRecv := in.Direction&pmsReceive != 0 && cur.direction&pmsReceive == 0
	_, err = tx.ExecContext(ctx, `UPDATE plexwatch_pms_viewer SET account_name=?,owner=?,token=?,direction=?,interval_min=?,
		out_cursor=CASE WHEN ?<>0 THEN ? ELSE out_cursor END,
		out_key=CASE WHEN ?<>0 THEN 0 ELSE out_key END,
		out_baseline=CASE WHEN ?<>0 THEN ? ELSE out_baseline END,
		in_cursor=CASE WHEN ?<>0 THEN ? ELSE in_cursor END,
		in_skip=CASE WHEN ?<>0 THEN 0 ELSE in_skip END,
		next_run=CASE WHEN ?<>0 THEN ? ELSE next_run END,
		problem=CASE WHEN ?=0 THEN '' ELSE problem END
		WHERE server_id=? AND account_id=?`,
		acct.Name, store.BoolInt(owner), sealed, in.Direction, in.Interval,
		gainSend, nowNS, gainSend, gainSend, nowNS,
		gainRecv, now.Add(-5*time.Minute).Unix(), gainRecv,
		gainSend || gainRecv, nowNS, in.Direction,
		id, accountID)
	if err != nil {
		return PMSServer{}, err
	}
	if err := tx.Commit(); err != nil {
		return PMSServer{}, err
	}
	return s.Server(ctx, id)
}

func (s *Servers) unmap(ctx context.Context, id, accountID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var user int64
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM plexwatch_pms_viewer WHERE server_id=? AND account_id=?`, id, accountID).Scan(&user)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := s.dropPairTx(ctx, tx, id, user); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_pms_viewer WHERE server_id=? AND account_id=?`, id, accountID); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Servers) dropPairTx(ctx context.Context, tx *sql.Tx, id, user int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_pms_sent WHERE server_id=? AND user_id=?`, id, user); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM plexwatch_pms_unmatched WHERE server_id=? AND user_id=?`, id, user)
	return err
}

// Unmatched returns one bounded page of a viewer's unmatched titles.
func (s *Servers) Unmatched(ctx context.Context, id, userID int64, after string) ([]PMSUnmatched, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT dir,ref,title,reason,seen_at FROM plexwatch_pms_unmatched
		WHERE server_id=? AND user_id=? AND (dir || ':' || ref)>? ORDER BY dir, ref LIMIT 100`, id, userID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PMSUnmatched, 0, 16)
	for rows.Next() {
		var u PMSUnmatched
		if err := rows.Scan(&u.Direction, &u.Ref, &u.Title, &u.Reason, &u.SeenAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// mapPMSError turns transport results into the owner-facing sentinels.
func mapPMSError(err error) error {
	var st *StatusError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &st) && st.Status == http.StatusUnauthorized:
		return ErrPMSToken
	case errors.As(err, &st) && st.Status == http.StatusForbidden:
		return ErrPMSNotOwner
	case errors.As(err, &st):
		return ErrPMSUnreachable
	default:
		return err
	}
}
