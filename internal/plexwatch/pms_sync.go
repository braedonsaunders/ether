package plexwatch

import (
	"context"
	"database/sql"
	"errors"
	"github.com/braedonsaunders/ether/internal/store"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/state"
)

// Per-pass bounds. One pass of one mapping reads at most
// pmsHistoryPages×pmsHistoryPage history rows, scans at most pmsOutRows
// changed Ether rows and makes at most pmsOutWrites writes.
const (
	pmsHistoryPage  = 100
	pmsHistoryPages = 5
	pmsOutRows      = 200
	pmsOutWrites    = 50
	pmsDuePerTick   = 8
	// pmsEchoBefore/After bound how far a PMS history row may sit from
	// Ether's own scrobble of that title and still be its echo. PMS stamps
	// the view with its own clock, so both sides allow for skew.
	pmsEchoBefore = 5 * time.Minute
	pmsEchoAfter  = 15 * time.Minute
	// pmsOffsetStep is the smallest resume-position change worth sending.
	pmsOffsetStep = 30 * time.Second
	// pmsNoMatchRetry is how long a title that did not match is left alone.
	pmsNoMatchRetry = 24 * time.Hour
)

// Run syncs due mappings until ctx ends or Close. Start it under cluster
// leadership; the per-mapping lease also serializes overlapping leaders and
// the owner's Sync now.
func (s *Servers) Run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		s.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-tick.C:
		}
	}
}

// RunOnce syncs up to pmsDuePerTick due mappings.
func (s *Servers) syncEnabled() bool { return s.enabled == nil || s.enabled() }

func (s *Servers) RunOnce(ctx context.Context) {
	if !s.syncEnabled() {
		return
	}
	now := s.now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `SELECT server_id,account_id FROM plexwatch_pms_viewer
		WHERE direction<>0 AND next_run<=? AND next_allowed<=? AND lease<=? ORDER BY next_run LIMIT ?`, now, now, now, pmsDuePerTick)
	if err != nil {
		return
	}
	type due struct{ server, account int64 }
	var list []due
	for rows.Next() {
		var d due
		if rows.Scan(&d.server, &d.account) == nil {
			list = append(list, d)
		}
	}
	rows.Close()
	for _, d := range list {
		if ctx.Err() != nil {
			return
		}
		if err := s.syncPair(ctx, d.server, d.account); err != nil && ctx.Err() == nil {
			s.log.Info("plex media server sync paused", "server", d.server, "account", d.account, "err", err.Error())
		}
	}
}

// SyncNow runs every mapping of one server now. A server that asked Ether
// to wait (HTTP 429) is still left alone until it said.
func (s *Servers) SyncNow(ctx context.Context, id int64) (PMSServer, error) {
	if _, err := s.loadServer(ctx, id); err != nil {
		return PMSServer{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT account_id FROM plexwatch_pms_viewer WHERE server_id=? AND direction<>0 ORDER BY account_id`, id)
	if err != nil {
		return PMSServer{}, err
	}
	var accounts []int64
	for rows.Next() {
		var a int64
		if rows.Scan(&a) == nil {
			accounts = append(accounts, a)
		}
	}
	rows.Close()
	for _, a := range accounts {
		if err := s.syncPair(ctx, id, a); err != nil && ctx.Err() != nil {
			return PMSServer{}, err
		}
	}
	return s.Server(ctx, id)
}

type pmsPair struct {
	serverID, accountID, userID int64
	owner                       bool
	direction, interval         int
	failures                    int
	lease                       int64
	client                      *pmsClient
	serverToken, writeToken     string
	inCursor                    int64
	inSkip                      int
	outCursor, outKey           int64
	outBaseline                 int64
	sections                    []pmsSection
	sectionsLoaded              bool
	pulled, pushed              bool
}

func (s *Servers) syncPair(ctx context.Context, serverID, accountID int64) error {
	if !s.syncEnabled() {
		return nil
	}
	now := s.now()
	lease := now.Add(10 * time.Minute).UnixNano()
	res, err := s.db.ExecContext(ctx, `UPDATE plexwatch_pms_viewer SET lease=? WHERE server_id=? AND account_id=? AND direction<>0 AND lease<=? AND next_allowed<=?`,
		lease, serverID, accountID, now.UnixNano(), now.UnixNano())
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	p, err := s.loadPair(ctx, serverID, accountID, lease)
	if err == nil {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		err = s.runPair(pctx, p)
		cancel()
	}
	if ctx.Err() != nil || !s.syncEnabled() {
		// Leadership ended or the owner paused sync: release the lease with a short independent context.
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = s.db.ExecContext(cleanup, `UPDATE plexwatch_pms_viewer SET lease=0 WHERE server_id=? AND account_id=? AND lease=?`, serverID, accountID, lease)
		return ctx.Err()
	}
	if p == nil {
		p = &pmsPair{serverID: serverID, accountID: accountID, lease: lease, interval: 10}
	}
	if ferr := s.finishPair(ctx, p, err); ferr != nil {
		return ferr
	}
	return err
}

func (s *Servers) loadPair(ctx context.Context, serverID, accountID, lease int64) (*pmsPair, error) {
	srv, err := s.loadServer(ctx, serverID)
	if err != nil {
		return nil, err
	}
	p := &pmsPair{serverID: serverID, accountID: accountID, lease: lease}
	var owner int
	var token string
	err = s.db.QueryRowContext(ctx, `SELECT user_id,owner,token,direction,interval_min,failures,in_cursor,in_skip,out_cursor,out_key,out_baseline
		FROM plexwatch_pms_viewer WHERE server_id=? AND account_id=? AND lease=?`, serverID, accountID, lease).
		Scan(&p.userID, &owner, &token, &p.direction, &p.interval, &p.failures, &p.inCursor, &p.inSkip, &p.outCursor, &p.outKey, &p.outBaseline)
	if err != nil {
		return nil, err
	}
	p.owner = owner != 0
	if p.client, err = s.clientFor(srv.baseURL); err != nil {
		return p, err
	}
	// Recheck before each outbound sync request, including during a running pass.
	p.client.enabled = s.syncEnabled
	if p.serverToken, err = s.open(srv.token, srv.machineID); err != nil {
		return p, err
	}
	if p.owner {
		p.writeToken = p.serverToken
	}
	if token != "" {
		if p.writeToken, err = s.open(token, srv.machineID, "account", strconv.FormatInt(accountID, 10)); err != nil {
			return p, err
		}
	}
	return p, nil
}

func (s *Servers) runPair(ctx context.Context, p *pmsPair) error {
	if p.direction&pmsReceive != 0 {
		if err := s.pull(ctx, p); err != nil {
			return err
		}
		p.pulled = true
	}
	if p.direction&pmsSend != 0 && p.writeToken != "" {
		if err := s.push(ctx, p); err != nil {
			return err
		}
		p.pushed = true
	}
	return nil
}

// finishPair records the outcome and schedules the next pass: the configured
// interval after success, exponential backoff after failure, and at least
// the server's Retry-After for 429, applied to every mapping of that server.
func (s *Servers) finishPair(ctx context.Context, p *pmsPair, err error) error {
	now := s.now()
	interval := p.interval
	if !validPMSInterval(interval) {
		interval = 10
	}
	delay := time.Duration(interval) * time.Minute
	problem, failures, nextAllowed := "", 0, int64(0)
	serverProblem := ""
	var st *StatusError
	isStatus := errors.As(err, &st)
	if err != nil {
		failures = p.failures + 1
		delay = time.Minute << min(failures-1, 8)
		switch {
		case errors.Is(err, errPMSAccountToken):
			problem = "account_token" // this person's own token; the server is fine
		case errors.Is(err, ErrPMSToken) || (isStatus && st.Status == http.StatusUnauthorized):
			problem, serverProblem = "token", "token"
		case isStatus && st.Status == http.StatusForbidden:
			problem = "forbidden"
		case isStatus && st.Status == http.StatusTooManyRequests:
			problem = "rate_limited"
		case isStatus && st.Status >= 500:
			problem = "server"
		case errors.Is(err, ErrPMSUnreachable):
			problem, serverProblem = "unreachable", "unreachable"
		default:
			problem = "error"
		}
		if isStatus && st.RetryAfter > delay {
			delay = min(st.RetryAfter, 24*time.Hour)
		}
		nextAllowed = now.Add(delay).UnixNano()
	}
	jitter := time.Duration((p.accountID*31+p.userID)%61) * time.Second
	next := now.Add(delay + jitter).UnixNano()
	pulled, pushed := int64(0), int64(0)
	if p.pulled {
		pulled = now.Unix()
	}
	if p.pushed {
		pushed = now.Unix()
	}
	if _, e := s.db.ExecContext(ctx, `UPDATE plexwatch_pms_viewer SET lease=0,problem=?,failures=?,next_allowed=?,next_run=?,
		last_import=CASE WHEN ?>0 THEN ? ELSE last_import END, last_send=CASE WHEN ?>0 THEN ? ELSE last_send END
		WHERE server_id=? AND account_id=? AND lease=?`,
		problem, failures, nextAllowed, next, pulled, pulled, pushed, pushed, p.serverID, p.accountID, p.lease); e != nil {
		return e
	}
	if isStatus && st.Status == http.StatusTooManyRequests {
		// One server's limit covers every account it serves.
		if _, e := s.db.ExecContext(ctx, `UPDATE plexwatch_pms_viewer SET next_allowed=CASE WHEN next_allowed<? THEN ? ELSE next_allowed END WHERE server_id=?`,
			nextAllowed, nextAllowed, p.serverID); e != nil {
			return e
		}
	}
	if err == nil || serverProblem != "" {
		if _, e := s.db.ExecContext(ctx, `UPDATE plexwatch_pms SET problem=?,checked_at=CASE WHEN ?='' THEN ? ELSE checked_at END WHERE id=?`,
			serverProblem, serverProblem, now.Unix(), p.serverID); e != nil {
			return e
		}
	}
	return nil
}

// pull imports the account's new PMS views after the cursor, oldest first.
// The cursor is (second, rows already read at that second), so many views
// stamped with one second (a season marked watched) page correctly.
func (s *Servers) pull(ctx context.Context, p *pmsPair) error {
	for page := 0; page < pmsHistoryPages; page++ {
		items, err := p.client.history(ctx, p.serverToken, p.accountID, p.inCursor-1, p.inSkip, pmsHistoryPage)
		if err != nil {
			return err
		}
		for _, h := range items {
			at := int64(h.ViewedAt)
			if at < p.inCursor {
				continue // before the cursor: the server ignored the filter
			}
			if err := s.applyHistory(ctx, p, h); err != nil {
				return s.saveInCursor(ctx, p, err)
			}
			if at == p.inCursor {
				p.inSkip++
			} else {
				p.inCursor, p.inSkip = at, 1
			}
		}
		if err := s.saveInCursor(ctx, p, nil); err != nil {
			return err
		}
		if len(items) < pmsHistoryPage {
			return nil
		}
	}
	return nil
}

func (s *Servers) saveInCursor(ctx context.Context, p *pmsPair, cause error) error {
	_, err := s.db.ExecContext(ctx, `UPDATE plexwatch_pms_viewer SET in_cursor=?,in_skip=? WHERE server_id=? AND account_id=? AND lease=?`,
		p.inCursor, p.inSkip, p.serverID, p.accountID, p.lease)
	if cause != nil {
		return cause
	}
	return err
}

type pmsSentRow struct {
	pmsKey        int64
	remoteWatched bool
	localWatched  bool
	localAt       int64
	offsetMS      int64
	sentAt        int64
}

func (s *Servers) loadSent(ctx context.Context, p *pmsPair, key core.RatingKey) (pmsSentRow, bool, error) {
	var r pmsSentRow
	var rw, lw int
	err := s.db.QueryRowContext(ctx, `SELECT pms_key,remote_watched,local_watched,local_at,offset_ms,sent_at FROM plexwatch_pms_sent
		WHERE server_id=? AND user_id=? AND rating_key=?`, p.serverID, p.userID, int64(key)).
		Scan(&r.pmsKey, &rw, &lw, &r.localAt, &r.offsetMS, &r.sentAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	r.remoteWatched, r.localWatched = rw != 0, lw != 0
	return r, err == nil, err
}

func (s *Servers) saveSent(ctx context.Context, p *pmsPair, key core.RatingKey, r pmsSentRow) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO plexwatch_pms_sent(server_id,user_id,rating_key,pms_key,remote_watched,local_watched,local_at,offset_ms,sent_at)
		VALUES(?,?,?,?,?,?,?,?,?)
		ON CONFLICT(server_id,user_id,rating_key) DO UPDATE SET pms_key=excluded.pms_key,remote_watched=excluded.remote_watched,
		  local_watched=excluded.local_watched,local_at=excluded.local_at,offset_ms=excluded.offset_ms,sent_at=excluded.sent_at`,
		p.serverID, p.userID, int64(key), r.pmsKey, store.BoolInt(r.remoteWatched), store.BoolInt(r.localWatched), r.localAt, r.offsetMS, r.sentAt)
	return err
}

// localWatch reads one viewer row: whether it is watched and its watch clock.
func (s *Servers) localWatch(ctx context.Context, userID int64, key core.RatingKey) (bool, int64, error) {
	var count int
	var watchAt, viewed int64
	err := s.db.QueryRowContext(ctx, `SELECT view_count,watch_at,last_viewed_at FROM state_watch WHERE user_id=? AND rating_key=?`, userID, int64(key)).
		Scan(&count, &watchAt, &viewed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, 0, nil
	}
	return count > 0, max(watchAt, viewed), err
}

// applyHistory imports one PMS view, newer-wins, unless it is the echo of
// Ether's own scrobble to this server.
func (s *Servers) applyHistory(ctx context.Context, p *pmsPair, h pmsMetadata) error {
	if h.AccountID > 0 && int64(h.AccountID) != p.accountID {
		return nil // a server that ignored the account filter
	}
	if (h.Type != "movie" && h.Type != "episode") || h.RatingKey <= 0 || h.ViewedAt <= 0 {
		return nil
	}
	pmsKey := int64(h.RatingKey)
	key, err := s.matchPMS(ctx, p, pmsKey, h)
	if err != nil || key == 0 {
		return err
	}
	viewedAt := time.Unix(int64(h.ViewedAt), 0)
	rec, has, err := s.loadSent(ctx, p, key)
	if err != nil {
		return err
	}
	if has && rec.sentAt > 0 && rec.remoteWatched {
		sent := time.Unix(rec.sentAt, 0)
		if !viewedAt.Before(sent.Add(-pmsEchoBefore)) && !viewedAt.After(sent.Add(pmsEchoAfter)) {
			return nil // Ether's own scrobble coming back
		}
	}
	watched := true
	err = s.state.ApplyRemote(ctx, p.userID, key, &watched, viewedAt, nil, time.Time{})
	if err != nil && !errors.Is(err, state.ErrLocalNewer) {
		return err
	}
	localWatched, localAt, err := s.localWatch(ctx, p.userID, key)
	if err != nil {
		return err
	}
	rec.pmsKey, rec.remoteWatched, rec.localWatched, rec.localAt = pmsKey, true, localWatched, localAt
	return s.saveSent(ctx, p, key, rec)
}

type pmsLocalRow struct {
	key       core.RatingKey
	count     int
	offset    time.Duration
	watchAt   int64
	viewed    int64
	updatedAt int64
}

// push sends Ether changes after the cursor. Only rows whose updated_at
// moved are read (state_watch_updated index), never the viewer's library.
func (s *Servers) push(ctx context.Context, p *pmsPair) error {
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key,view_count,view_offset_ns,watch_at,last_viewed_at,updated_at FROM state_watch
		WHERE user_id=? AND updated_at>=? AND (updated_at>? OR rating_key>?) ORDER BY updated_at,rating_key LIMIT ?`,
		p.userID, p.outCursor, p.outCursor, p.outKey, pmsOutRows)
	if err != nil {
		return err
	}
	var batch []pmsLocalRow
	for rows.Next() {
		var r pmsLocalRow
		var key, offset int64
		if err := rows.Scan(&key, &r.count, &offset, &r.watchAt, &r.viewed, &r.updatedAt); err != nil {
			rows.Close()
			return err
		}
		r.key, r.offset = core.RatingKey(key), time.Duration(offset)
		batch = append(batch, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	writes := 0
	var cause error
	for _, r := range batch {
		if r.key.Kind() == core.KindMovie || r.key.Kind() == core.KindEpisode {
			n, err := s.pushRow(ctx, p, r)
			if err != nil {
				cause = err
				break
			}
			writes += n
		}
		p.outCursor, p.outKey = r.updatedAt, int64(r.key)
		if writes >= pmsOutWrites {
			break
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE plexwatch_pms_viewer SET out_cursor=?,out_key=? WHERE server_id=? AND account_id=? AND lease=?`,
		p.outCursor, p.outKey, p.serverID, p.accountID, p.lease)
	if cause != nil {
		return cause
	}
	return err
}

// pushRow reconciles one changed Ether row with the server and returns the
// number of writes made. It writes only when the local watch state changed
// since this server and title were last reconciled and the server does not
// already hold it, which is what keeps imports from echoing.
func (s *Servers) pushRow(ctx context.Context, p *pmsPair, r pmsLocalRow) (int, error) {
	watched := r.count > 0
	localAt := max(r.watchAt, r.viewed)
	rec, has, err := s.loadSent(ctx, p, r.key)
	if err != nil {
		return 0, err
	}
	var remote *pmsMetadata
	read := func() (bool, error) {
		if remote != nil {
			return true, nil
		}
		if rec.pmsKey == 0 {
			k, ok, err := s.pmsKeyFor(ctx, p, r.key)
			if err != nil || !ok {
				return false, err
			}
			rec.pmsKey = k
		}
		md, err := p.client.metadata(ctx, p.writeToken, rec.pmsKey)
		err = p.writeErr(err)
		if isNotFound(err) {
			// The PMS item is gone (deleted or re-added): forget the match.
			_, _ = s.db.ExecContext(ctx, `DELETE FROM plexwatch_pms_item WHERE server_id=? AND pms_key=?`, p.serverID, rec.pmsKey)
			rec.pmsKey = 0
			return false, s.noteUnmatched(ctx, p, "out", r.key.String(), s.localTitle(ctx, r.key), "not_on_server")
		}
		if err != nil {
			return false, err
		}
		remote = &md
		return true, nil
	}
	writes := 0
	switch {
	case watched:
		if has && rec.remoteWatched {
			if !rec.localWatched || rec.localAt != localAt {
				rec.localWatched, rec.localAt = true, localAt
				return 0, s.saveSent(ctx, p, r.key, rec)
			}
			return 0, nil
		}
		if localAt < p.outBaseline {
			return 0, nil // watched before sending was turned on
		}
		ok, err := read()
		if err != nil || !ok {
			return 0, err
		}
		rec.remoteWatched, rec.localWatched, rec.localAt, rec.offsetMS = true, true, localAt, 0
		if remote.ViewCount == 0 {
			if err := p.writeErr(p.client.mark(ctx, p.writeToken, rec.pmsKey, true)); err != nil {
				return 0, s.writeRefused(ctx, p, r.key, rec.pmsKey, err)
			}
			rec.sentAt = s.now().Unix()
			writes++
		}
		return writes, s.saveSent(ctx, p, r.key, rec)
	case has && rec.remoteWatched && rec.localWatched:
		// Watched → unwatched in Ether since the last reconciliation.
		ok, err := read()
		if err != nil || !ok {
			return 0, err
		}
		switch {
		case remote.ViewCount == 0:
			rec.remoteWatched = false
		case int64(remote.LastViewedAt) > time.Unix(0, localAt).Unix():
			// Watched on the server after Ether's unwatch: the server is newer.
		default:
			if err := p.writeErr(p.client.mark(ctx, p.writeToken, rec.pmsKey, false)); err != nil {
				return 0, s.writeRefused(ctx, p, r.key, rec.pmsKey, err)
			}
			rec.remoteWatched, rec.sentAt = false, s.now().Unix()
			writes++
		}
		rec.localWatched, rec.localAt = false, localAt
		if err := s.saveSent(ctx, p, r.key, rec); err != nil {
			return writes, err
		}
	}
	// Resume position: sent for unwatched titles in progress in Ether,
	// never over a title the server has watched.
	if r.offset <= 0 || localAt < p.outBaseline || (has && rec.remoteWatched) {
		return writes, nil
	}
	offsetMS := r.offset.Milliseconds()
	if has && absDuration(time.Duration(offsetMS-rec.offsetMS)*time.Millisecond) < pmsOffsetStep {
		return writes, nil
	}
	ok, err := read()
	if err != nil || !ok {
		return writes, err
	}
	if remote.ViewCount > 0 {
		rec.remoteWatched, rec.localWatched, rec.localAt = true, false, localAt
		return writes, s.saveSent(ctx, p, r.key, rec)
	}
	if absDuration(time.Duration(int64(remote.ViewOffset)-offsetMS)*time.Millisecond) >= pmsOffsetStep {
		if err := p.writeErr(p.client.progress(ctx, p.writeToken, rec.pmsKey, r.offset)); err != nil {
			var st *StatusError
			if errors.As(err, &st) && st.Status == http.StatusBadRequest {
				return writes, nil // a resume point the server won't take; the mark still syncs
			}
			return writes, s.writeRefused(ctx, p, r.key, rec.pmsKey, err)
		}
		writes++
	}
	rec.offsetMS, rec.localAt = offsetMS, localAt
	return writes, s.saveSent(ctx, p, r.key, rec)
}

// errPMSAccountToken is a person's own PMS token refused while the server
// token is still good.
var errPMSAccountToken = errors.New("plexwatch: the server refused this person's token")

// writeErr marks a 401 on a person's own token so it is not reported as the
// server's token failing.
func (p *pmsPair) writeErr(err error) error {
	var st *StatusError
	if !p.owner && errors.As(err, &st) && st.Status == http.StatusUnauthorized {
		return errPMSAccountToken
	}
	return err
}

// writeRefused settles a write the server refused for this item (404 or
// 400): the match is forgotten and the title listed as unmatched, so one
// stale item cannot hold the cursor. Any other error stays retryable.
func (s *Servers) writeRefused(ctx context.Context, p *pmsPair, key core.RatingKey, pmsKey int64, err error) error {
	var st *StatusError
	if !errors.As(err, &st) || (st.Status != http.StatusNotFound && st.Status != http.StatusBadRequest) {
		return err
	}
	if _, e := s.db.ExecContext(ctx, `DELETE FROM plexwatch_pms_item WHERE server_id=? AND pms_key=?`, p.serverID, pmsKey); e != nil {
		return e
	}
	if _, e := s.db.ExecContext(ctx, `UPDATE plexwatch_pms_sent SET pms_key=0 WHERE server_id=? AND user_id=? AND rating_key=?`, p.serverID, p.userID, int64(key)); e != nil {
		return e
	}
	return s.noteUnmatched(ctx, p, "out", key.String(), s.localTitle(ctx, key), "not_on_server")
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func isNotFound(err error) bool {
	var st *StatusError
	return errors.As(err, &st) && st.Status == http.StatusNotFound
}

func (s *Servers) noteUnmatched(ctx context.Context, p *pmsPair, dir, ref, title, reason string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plexwatch_pms_unmatched WHERE server_id=? AND user_id=?`, p.serverID, p.userID).Scan(&n); err != nil {
		return err
	}
	if n >= pmsUnmatchedCap {
		_, err := s.db.ExecContext(ctx, `UPDATE plexwatch_pms_unmatched SET seen_at=?,reason=? WHERE server_id=? AND user_id=? AND dir=? AND ref=?`,
			s.now().Unix(), reason, p.serverID, p.userID, dir, ref)
		return err
	}
	if len(title) > 300 {
		title = title[:300]
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO plexwatch_pms_unmatched(server_id,user_id,dir,ref,title,reason,seen_at) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(server_id,user_id,dir,ref) DO UPDATE SET title=excluded.title,reason=excluded.reason,seen_at=excluded.seen_at`,
		p.serverID, p.userID, dir, ref, title, reason, s.now().Unix())
	return err
}

func (s *Servers) clearUnmatched(ctx context.Context, p *pmsPair, dir, ref string) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM plexwatch_pms_unmatched WHERE server_id=? AND user_id=? AND dir=? AND ref=?`, p.serverID, p.userID, dir, ref)
}

func (s *Servers) localTitle(ctx context.Context, key core.RatingKey) string {
	if s.catalog == nil {
		return key.String()
	}
	t, err := s.catalog.Get(ctx, key)
	if err != nil || t == nil || t.Title == "" {
		return key.String()
	}
	if t.Type == core.TypeEpisode && t.ShowTitle != "" {
		return t.ShowTitle + " S" + strconv.Itoa(t.Season) + "E" + strconv.Itoa(t.Episode) + " " + t.Title
	}
	return t.Title
}

// matchPMS maps a PMS item to an Ether title by identifier only: TMDB first,
// then IMDb and TVDB. Matches and misses are cached per server; a miss is
// stored as unmatched and retried after pmsNoMatchRetry.
func (s *Servers) matchPMS(ctx context.Context, p *pmsPair, pmsKey int64, h pmsMetadata) (core.RatingKey, error) {
	ref := strconv.FormatInt(pmsKey, 10)
	var cached, checked int64
	err := s.db.QueryRowContext(ctx, `SELECT rating_key,checked_at FROM plexwatch_pms_item WHERE server_id=? AND pms_key=?`, p.serverID, pmsKey).Scan(&cached, &checked)
	switch {
	case err == nil && cached > 0:
		return core.RatingKey(cached), nil
	case err == nil && s.now().Sub(time.Unix(checked, 0)) < pmsNoMatchRetry:
		return 0, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}
	md, err := p.client.metadata(ctx, p.serverToken, pmsKey)
	if isNotFound(err) {
		return 0, s.noteUnmatched(ctx, p, "in", ref, h.displayTitle(), "not_on_server")
	}
	if err != nil {
		return 0, err
	}
	key, reason, err := s.keyFromMetadata(ctx, p, md)
	if err != nil {
		return 0, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO plexwatch_pms_item(server_id,pms_key,rating_key,checked_at) VALUES(?,?,?,?)
		ON CONFLICT(server_id,pms_key) DO UPDATE SET rating_key=excluded.rating_key,checked_at=excluded.checked_at`,
		p.serverID, pmsKey, int64(key), s.now().Unix()); err != nil {
		return 0, err
	}
	if key == 0 {
		return 0, s.noteUnmatched(ctx, p, "in", ref, md.displayTitle(), reason)
	}
	s.clearUnmatched(ctx, p, "in", ref)
	return key, nil
}

func episodeNumbersFit(season, episode int) bool {
	return season >= 0 && season < 1000 && episode > 0 && episode < 10_000
}

func (s *Servers) keyFromMetadata(ctx context.Context, p *pmsPair, md pmsMetadata) (core.RatingKey, string, error) {
	switch md.Type {
	case "movie":
		k, err := s.keyFromIDs(ctx, pmsIDs(md), core.TypeMovie)
		if k == 0 && err == nil {
			return 0, "no_match", nil
		}
		return k, "", err
	case "episode":
		season, episode := int(md.ParentIndex), int(md.Index)
		if show := md.showKey(); show > 0 && episodeNumbersFit(season, episode) {
			sk, err := s.showKey(ctx, p, show)
			if err != nil {
				return 0, "", err
			}
			if sk != 0 {
				return core.EpisodeKey(sk.TMDB(), season, episode), "", nil
			}
		}
		// The episode's own IMDb/TVDB identifiers. TMDB episode ids are not
		// Ether keys, so they are not used here.
		for _, id := range pmsIDs(md) {
			if id.source == "tmdb" || s.catalog == nil {
				continue
			}
			t, err := s.catalog.ByExternalIDForType(ctx, id.source, id.value, core.TypeEpisode)
			if err != nil && !errors.Is(err, core.ErrNotFound) {
				return 0, "", err
			}
			if err == nil && t != nil && t.Key.Kind() == core.KindEpisode {
				return t.Key, "", nil
			}
		}
		return 0, "no_match", nil
	default:
		return 0, "kind", nil
	}
}

// showKey maps a PMS show to an Ether show, cached like any item.
func (s *Servers) showKey(ctx context.Context, p *pmsPair, pmsShow int64) (core.RatingKey, error) {
	var cached, checked int64
	err := s.db.QueryRowContext(ctx, `SELECT rating_key,checked_at FROM plexwatch_pms_item WHERE server_id=? AND pms_key=?`, p.serverID, pmsShow).Scan(&cached, &checked)
	switch {
	case err == nil && cached > 0:
		return core.RatingKey(cached), nil
	case err == nil && s.now().Sub(time.Unix(checked, 0)) < pmsNoMatchRetry:
		return 0, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return 0, err
	}
	md, err := p.client.metadata(ctx, p.serverToken, pmsShow)
	if isNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var k core.RatingKey
	if md.Type == "show" {
		if k, err = s.keyFromIDs(ctx, pmsIDs(md), core.TypeShow); err != nil {
			return 0, err
		}
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO plexwatch_pms_item(server_id,pms_key,rating_key,checked_at) VALUES(?,?,?,?)
		ON CONFLICT(server_id,pms_key) DO UPDATE SET rating_key=excluded.rating_key,checked_at=excluded.checked_at`,
		p.serverID, pmsShow, int64(k), s.now().Unix())
	return k, err
}

type pmsID struct{ source, value string }

// pmsIDs lists external identifiers, TMDB first, then IMDb, then TVDB. It
// reads the Guid array and the legacy agent forms of the primary guid.
func pmsIDs(md pmsMetadata) []pmsID {
	raw := []string{md.GUID}
	for _, g := range md.GUIDs {
		raw = append(raw, g.ID)
	}
	var out []pmsID
	for _, want := range []struct{ source, prefix, legacy string }{
		{"tmdb", "tmdb://", "com.plexapp.agents.themoviedb://"},
		{"imdb", "imdb://", "com.plexapp.agents.imdb://"},
		{"tvdb", "tvdb://", "com.plexapp.agents.thetvdb://"},
	} {
		for _, g := range raw {
			v := ""
			switch {
			case strings.HasPrefix(g, want.prefix):
				v = strings.TrimPrefix(g, want.prefix)
			case strings.HasPrefix(g, want.legacy):
				v = strings.TrimPrefix(g, want.legacy)
			default:
				continue
			}
			if i := strings.IndexByte(v, '?'); i >= 0 {
				v = v[:i]
			}
			if v != "" && !strings.Contains(v, "/") {
				out = append(out, pmsID{want.source, v})
			}
		}
	}
	return out
}

func (s *Servers) keyFromIDs(ctx context.Context, ids []pmsID, typ core.MediaType) (core.RatingKey, error) {
	for _, id := range ids {
		if id.source == "tmdb" {
			n, err := strconv.ParseInt(id.value, 10, 64)
			if err != nil || n <= 0 {
				continue
			}
			k := core.MovieKey(n)
			if typ == core.TypeShow {
				k = core.ShowKey(n)
			}
			if s.catalog == nil {
				return k, nil
			}
			t, err := s.catalog.Get(ctx, k)
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			if err != nil {
				return 0, err
			}
			if t != nil && t.Type == typ {
				return k, nil
			}
			continue
		}
		if s.catalog == nil {
			continue
		}
		t, err := s.catalog.ByExternalIDForType(ctx, id.source, id.value, typ)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if t != nil && t.Type == typ {
			return t.Key, nil
		}
	}
	return 0, nil
}

// pmsKeyFor finds the server's item for an Ether title: a stored match
// first, then the title's verified plex:// GUID through the section guid
// filter. ok is false when the server has no such item.
func (s *Servers) pmsKeyFor(ctx context.Context, p *pmsPair, key core.RatingKey) (int64, bool, error) {
	var pmsKey int64
	err := s.db.QueryRowContext(ctx, `SELECT pms_key FROM plexwatch_pms_item WHERE server_id=? AND rating_key=? ORDER BY checked_at DESC LIMIT 1`,
		p.serverID, int64(key)).Scan(&pmsKey)
	if err == nil && pmsKey > 0 {
		return pmsKey, true, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	ref := key.String()
	var seen int64
	err = s.db.QueryRowContext(ctx, `SELECT seen_at FROM plexwatch_pms_unmatched WHERE server_id=? AND user_id=? AND dir='out' AND ref=?`,
		p.serverID, p.userID, ref).Scan(&seen)
	if err == nil && s.now().Sub(time.Unix(seen, 0)) < pmsNoMatchRetry {
		return 0, false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, false, err
	}
	guid := ""
	if s.guid != nil {
		guid, err = s.guid.Resolve(ctx, key)
		if ctx.Err() != nil {
			return 0, false, ctx.Err()
		}
		if err != nil {
			guid = ""
		}
	}
	wantGUID := "plex://movie/"
	section, libType := "movie", 1
	if key.Kind() == core.KindEpisode {
		wantGUID, section, libType = "plex://episode/", "show", 4
	}
	if !strings.HasPrefix(guid, wantGUID) {
		return 0, false, s.noteUnmatched(ctx, p, "out", ref, s.localTitle(ctx, key), "no_plex_id")
	}
	if !p.sectionsLoaded {
		if p.sections, err = p.client.sections(ctx, p.serverToken); err != nil {
			return 0, false, err
		}
		p.sectionsLoaded = true
	}
	for _, sec := range p.sections {
		if sec.Type != section {
			continue
		}
		items, err := p.client.byGUID(ctx, p.serverToken, sec.Key, libType, guid)
		if err != nil {
			return 0, false, err
		}
		for _, it := range items {
			if it.GUID != guid || it.RatingKey <= 0 || !tmdbAgrees(it, key) {
				continue
			}
			if _, err := s.db.ExecContext(ctx, `INSERT INTO plexwatch_pms_item(server_id,pms_key,rating_key,checked_at) VALUES(?,?,?,?)
				ON CONFLICT(server_id,pms_key) DO UPDATE SET rating_key=excluded.rating_key,checked_at=excluded.checked_at`,
				p.serverID, int64(it.RatingKey), int64(key), s.now().Unix()); err != nil {
				return 0, false, err
			}
			s.clearUnmatched(ctx, p, "out", ref)
			return int64(it.RatingKey), true, nil
		}
	}
	return 0, false, s.noteUnmatched(ctx, p, "out", ref, s.localTitle(ctx, key), "not_on_server")
}

// tmdbAgrees rejects a movie whose own TMDB identifier names another title.
func tmdbAgrees(md pmsMetadata, key core.RatingKey) bool {
	if key.Kind() != core.KindMovie {
		return true
	}
	for _, id := range pmsIDs(md) {
		if id.source == "tmdb" {
			return id.value == strconv.FormatInt(key.TMDB(), 10)
		}
	}
	return true
}
