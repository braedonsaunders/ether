package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("livetv", 19, `CREATE TABLE livetv_login_slots(source_id TEXT NOT NULL, login_id TEXT NOT NULL, slot INTEGER NOT NULL, holder TEXT NOT NULL, until_ms BIGINT NOT NULL, PRIMARY KEY(source_id,login_id,slot));
 CREATE TABLE livetv_login_streams(source_id TEXT NOT NULL, login_id TEXT NOT NULL, stream_key TEXT NOT NULL, url TEXT NOT NULL, PRIMARY KEY(source_id,login_id,stream_key));`)
}

// Unknown panels and M3U accounts default to one connection, without asking
// the owner to discover or configure a provider's limits.
func loginLimit(a *ProviderAccount) int {
	if a != nil && a.MaxConnections > 0 {
		return min(a.MaxConnections, 100)
	}
	return 1
}

func (s *Service) prepareLogins(ctx context.Context, src *Source) error {
	if len(src.Logins) == 0 {
		if old, err := s.storedSourceByID(ctx, src.ID); err == nil {
			src.LoginPool = old.LoginPool || len(old.Logins) > 0
		}
		return nil
	}
	src.LoginPool = true
	if len(src.Logins) > 15 {
		return fmt.Errorf("a source supports at most 16 logins")
	}
	old, err := s.storedSourceByID(ctx, src.ID)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return err
	}
	seen := map[string]bool{}
	for i := range src.Logins {
		l := &src.Logins[i]
		if l.ID == "" {
			l.ID = randomID()
		}
		if len(l.ID) > 100 || seen[l.ID] || l.ID == "primary" {
			return fmt.Errorf("logins need unique IDs")
		}
		seen[l.ID] = true
		for _, prev := range old.Logins {
			if prev.ID == l.ID {
				if l.Password == redactedSecret {
					if sourceDestination(src) != sourceDestination(&old) {
						return errSourceSecretMoved
					}
					l.Password = prev.Password
				}
				if l.PlaylistURL == redactedSecret {
					l.PlaylistURL = prev.PlaylistURL
				}
			}
		}
		l.Account, l.Error = nil, ""
		if src.Auth != nil && src.Auth.Type == "xtream" {
			l.Username = strings.TrimSpace(l.Username)
			if l.Username == "" || l.Password == "" || l.Password == redactedSecret {
				return fmt.Errorf("login %d needs a username and password", i+2)
			}
			l.PlaylistURL = ""
			if l.Username == src.Auth.Username {
				return fmt.Errorf("login %d repeats the primary username", i+2)
			}
			for j := 0; j < i; j++ {
				if src.Logins[j].Username == l.Username {
					return fmt.Errorf("login %d repeats another username", i+2)
				}
			}
		} else {
			if !validHTTP(l.PlaylistURL) {
				return fmt.Errorf("login %d needs an HTTP(S) playlist URL", i+2)
			}
			if l.PlaylistURL == src.PlaylistURL {
				return fmt.Errorf("login %d repeats the primary playlist", i+2)
			}
			for j := 0; j < i; j++ {
				if src.Logins[j].PlaylistURL == l.PlaylistURL {
					return fmt.Errorf("login %d repeats another playlist", i+2)
				}
			}
			l.Username, l.Password = "", ""
		}
	}
	return nil
}

func (s *Service) refreshLogins(ctx context.Context, src *Source, primary []Channel) error {
	if !src.LoginPool && len(src.Logins) == 0 {
		return nil
	}
	src.loginStreams = map[string]map[string]string{}
	if (src.Auth == nil || src.Auth.Type != "xtream") && src.Account == nil {
		src.Account = &ProviderAccount{Status: "Active", MaxConnections: 1, CheckedAt: s.o.Now()}
	}
	for i := range src.Logins {
		l := &src.Logins[i]
		if src.Auth != nil && src.Auth.Type == "xtream" {
			a := *src.Auth
			a.Username = l.Username
			a.Password = l.Password
			acct, err := newXtreamAPI(s, &a).account(ctx, s.o.Now())
			l.Account = acct
			if err != nil {
				l.Error = "login unavailable"
				if l.Account == nil {
					l.Account = &ProviderAccount{Status: "Disabled", CheckedAt: s.o.Now()}
				}
			}
			continue
		}
		// M3U tokens belong to the URL; primary HTTP credentials are never sent
		// to the additional account's host.
		text, err := s.readURLWith(ctx, l.PlaylistURL, maxPlaylistBytes, nil, "playlist")
		if err != nil {
			l.Error = "could not fetch this login's playlist"
			l.Account = &ProviderAccount{Status: "Disabled", CheckedAt: s.o.Now()}
			continue
		}
		cs, err := ParseM3U(strings.NewReader(text), src.ID)
		if err != nil {
			l.Error = "could not read this login's playlist"
			l.Account = &ProviderAccount{Status: "Disabled", CheckedAt: s.o.Now()}
			continue
		}
		mapped, err := matchLoginLineup(primary, cs)
		if err != nil {
			return fmt.Errorf("login %d: %w", i+2, err)
		}
		src.loginStreams[l.ID] = mapped
		l.Account = &ProviderAccount{Status: "Active", MaxConnections: 1, CheckedAt: s.o.Now().UTC()}
	}
	return nil
}

// Match on stable provider IDs first, then a credential-independent URL path.
// Ambiguous IDs are rejected rather than silently tuning the wrong station.
func matchLoginLineup(primary, extra []Channel) (map[string]string, error) {
	indexes := []map[string][]Channel{{}, {}, {}}
	keys := func(c Channel) []string {
		u, _ := url.Parse(c.StreamURL)
		pattern := ""
		if u != nil {
			pattern = u.Host + u.Path
		}
		return []string{c.GuideID, streamIDOf(c.StreamURL), pattern}
	}
	for _, c := range extra {
		for i, k := range keys(c) {
			if k != "" {
				indexes[i][k] = append(indexes[i][k], c)
			}
		}
	}
	out := map[string]string{}
	used := map[string]bool{}
	for _, c := range primary {
		var match *Channel
		for i, k := range keys(c) {
			if k != "" && len(indexes[i][k]) == 1 {
				v := indexes[i][k][0]
				match = &v
				break
			}
		}
		if match == nil || used[match.ID] {
			return nil, fmt.Errorf("playlist does not match channel %q", c.Name)
		}
		used[match.ID] = true
		out[tapKey(c.StreamURL, nil)] = match.StreamURL
	}
	return out, nil
}

func (s *Service) writeLoginStreams(ctx context.Context, tx *sql.Tx, src Source) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM livetv_login_streams WHERE source_id=?`, src.ID); err != nil {
		return err
	}
	for id, streams := range src.loginStreams {
		for key, raw := range streams {
			sealed, err := config.SealWith(s.o.Sealer, raw, "livetv_login_streams", src.ID, id, key)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_login_streams(source_id,login_id,stream_key,url) VALUES(?,?,?,?)`, src.ID, id, key, sealed); err != nil {
				return err
			}
		}
	}
	return nil
}

type loginPool struct {
	mu     sync.Mutex
	live   map[string]*loginLease
	failed map[string]time.Time
}
type loginLease struct {
	source, login, accountID, key, holder, raw string
	slot, limit, refs                          int
	headers                                    http.Header
	cancel                                     context.CancelFunc
	lost                                       bool
	grants                                     map[*inputGrant]struct{}
	onLost                                     context.CancelFunc
}

// One local shared input holds one database slot, however many viewers join.
// Different nodes atomically compete for the same bounded set of slots.
func (s *Service) leaseLogin(ctx context.Context, source, raw string, headers http.Header) (*loginLease, error) {
	e, err := s.sourceEntry(ctx, source)
	if err != nil {
		return nil, err
	}
	if e.pool == nil || (!e.pool.LoginPool && len(e.pool.Logins) == 0) {
		return nil, nil
	}
	src := e.pool
	p := &s.loginPool
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.live == nil {
		p.live = map[string]*loginLease{}
		p.failed = map[string]time.Time{}
	}
	key := source + "\x00" + tapKey(raw, headers)
	if l := p.live[key]; l != nil && !l.lost {
		l.refs++
		return l, nil
	}
	candidates := append([]ProviderLogin{{ID: "primary", Account: src.Account}}, src.Logins...)
	now := time.Now()
	for key, until := range p.failed {
		if !now.Before(until) {
			delete(p.failed, key)
		}
	}
	for _, candidate := range candidates {
		if strings.HasPrefix(raw, "vod:") && candidate.ID != "primary" {
			continue
		}
		if candidate.Error != "" || now.Before(p.failed[source+"\x00"+candidate.ID]) {
			continue
		}
		a := candidate.Account
		if a != nil && ((a.Status != "" && !strings.EqualFold(a.Status, "active")) || (a.ExpiresAt != nil && !now.Before(*a.ExpiresAt))) {
			continue
		}
		target := raw
		h := headers.Clone()
		if candidate.ID != "primary" {
			if src.Auth != nil && src.Auth.Type == "xtream" {
				// Only the primary login's exact stream path may be rewritten.
				if urlOrigin(raw) != urlOrigin(src.Auth.Server) {
					continue
				}
				prefix := strings.TrimRight(src.Auth.Server, "/") + "/live/" + url.PathEscape(src.Auth.Username) + "/" + url.PathEscape(src.Auth.Password) + "/"
				if !strings.HasPrefix(raw, prefix) {
					continue
				}
				target = strings.TrimRight(src.Auth.Server, "/") + "/live/" + url.PathEscape(candidate.Username) + "/" + url.PathEscape(candidate.Password) + "/" + strings.TrimPrefix(raw, prefix)
			} else {
				streamKey := tapKey(raw, nil)
				var sealed string
				if err := s.o.DB.QueryRowContext(ctx, `SELECT url FROM livetv_login_streams WHERE source_id=? AND login_id=? AND stream_key=?`, source, candidate.ID, streamKey).Scan(&sealed); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						continue
					}
					return nil, err
				}
				var er error
				target, _, er = config.OpenWith(s.o.Sealer, sealed, "livetv_login_streams", source, candidate.ID, streamKey)
				if er != nil {
					return nil, er
				}
				h = http.Header{}
			}
		}
		identity := candidate.PlaylistURL
		if src.Auth != nil && src.Auth.Type == "xtream" {
			identity = candidate.Username
			if candidate.ID == "primary" {
				identity = src.Auth.Username
			}
		} else if candidate.ID == "primary" {
			identity = src.PlaylistURL
			if identity == "" {
				identity = "primary"
			}
		}
		slotLogin := tapKey(identity, nil)
		limit := loginLimit(a)
		// Panel active counts can include streams outside Ether. Reserve that
		// reported occupancy conservatively until the next lineup refresh.
		busy := 0
		if a != nil {
			busy = min(a.ActiveConnections, limit)
		}
		// Do not double-count Ether's own slots in the panel snapshot.
		var ours int
		if err := s.o.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM livetv_login_slots WHERE source_id=? AND login_id=? AND until_ms>?`, source, slotLogin, now.UnixMilli()).Scan(&ours); err != nil {
			return nil, err
		}
		busy = max(0, busy-ours)
		if candidate.ID == "primary" {
			r := &s.inputRelay.taps
			r.mu.Lock()
			busy = max(busy, len(r.bySource[source])+r.external[source])
			r.mu.Unlock()
		}
		for slot := busy; slot < limit; slot++ {
			holder := randomID()
			result, err := s.o.DB.ExecContext(ctx, `INSERT INTO livetv_login_slots(source_id,login_id,slot,holder,until_ms) VALUES(?,?,?,?,?) ON CONFLICT(source_id,login_id,slot) DO UPDATE SET holder=excluded.holder,until_ms=excluded.until_ms WHERE livetv_login_slots.until_ms<=?`, source, slotLogin, slot, holder, now.Add(45*time.Second).UnixMilli(), now.UnixMilli())
			if err != nil {
				return nil, err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return nil, err
			}
			if n == 0 {
				continue
			}
			run, cancel := context.WithCancel(s.serviceContext())
			l := &loginLease{source: source, login: slotLogin, accountID: candidate.ID, key: key, holder: holder, slot: slot, limit: limit, refs: 1, raw: target, headers: h, cancel: cancel, grants: map[*inputGrant]struct{}{}}
			p.live[key] = l
			go s.renewLogin(run, l)
			return l, nil
		}
	}
	return nil, ErrProviderBusy
}

func (s *Service) renewLogin(ctx context.Context, l *loginLease) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.loginPool.mu.Lock()
			if l.onLost != nil {
				l.onLost()
			}
			s.loginPool.mu.Unlock()
			return
		case <-ticker.C:
			check, cancel := context.WithTimeout(ctx, 5*time.Second)
			result, err := s.o.DB.ExecContext(check, `UPDATE livetv_login_slots SET until_ms=? WHERE source_id=? AND login_id=? AND slot=? AND holder=? AND until_ms>?`, time.Now().Add(45*time.Second).UnixMilli(), l.source, l.login, l.slot, l.holder, time.Now().UnixMilli())
			cancel()
			n := int64(0)
			if err == nil {
				n, err = result.RowsAffected()
			}
			if err != nil || n != 1 {
				s.loginPool.mu.Lock()
				l.lost = true
				if l.onLost != nil {
					l.onLost()
				}
				for g := range l.grants {
					g.cancel()
				}
				grants := make([]*inputGrant, 0, len(l.grants))
				for g := range l.grants {
					grants = append(grants, g)
				}
				s.loginPool.mu.Unlock()
				for _, g := range grants {
					s.inputRelay.taps.release(g)
				}
				return
			}
		}
	}
}
func (s *Service) releaseLogin(l *loginLease) {
	if l == nil {
		return
	}
	p := &s.loginPool
	p.mu.Lock()
	defer p.mu.Unlock()
	l.refs--
	if l.refs > 0 {
		return
	}
	l.cancel()
	if p.live[l.key] == l {
		delete(p.live, l.key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.o.DB.ExecContext(ctx, `DELETE FROM livetv_login_slots WHERE source_id=? AND login_id=? AND slot=? AND holder=?`, l.source, l.login, l.slot, l.holder)
}
func (s *Service) failedLogin(l *loginLease) {
	if l == nil {
		return
	}
	p := &s.loginPool
	p.mu.Lock()
	p.failed[l.source+"\x00"+l.accountID] = time.Now().Add(time.Minute)
	l.lost = true
	for g := range l.grants {
		g.cancel()
	}
	p.mu.Unlock()
	// Persist a safe status for the source card. Compare-and-swap preserves a
	// simultaneous owner edit or refresh on another node.
	ctx, cancel := context.WithTimeout(s.serviceContext(), 2*time.Second)
	defer cancel()
	var before []byte
	if err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_sources WHERE id=?`, l.source).Scan(&before); err != nil {
		return
	}
	src, _, err := s.decodeSource(before)
	if err != nil {
		return
	}
	if l.accountID == "primary" {
		if src.Account == nil {
			src.Account = &ProviderAccount{}
		}
		src.Account.Status = "Disabled"
	} else {
		for i := range src.Logins {
			if src.Logins[i].ID == l.accountID {
				src.Logins[i].Error = "login refused"
				if src.Logins[i].Account == nil {
					src.Logins[i].Account = &ProviderAccount{}
				}
				src.Logins[i].Account.Status = "Disabled"
			}
		}
	}
	after, err := s.encodeSource(src)
	if err != nil {
		return
	}
	_, _ = s.o.DB.ExecContext(ctx, `UPDATE livetv_sources SET data=? WHERE id=? AND CAST(data AS TEXT)=?`, after, l.source, string(before))
	s.forgetSourceAuth(l.source)
}

// An expired primary must not disable healthy additional accounts. Keep its
// last successful lineup and guide; never import another login's channels.
func (s *Service) cachedPrimaryLineup(ctx context.Context, src *Source, cause error) (Fetched, error) {
	old, err := s.storedSourceByID(ctx, src.ID)
	same := err == nil && old.PlaylistURL == src.PlaylistURL
	if src.Auth != nil && src.Auth.Type == "xtream" {
		same = err == nil && old.Auth != nil && old.Auth.Username == src.Auth.Username && old.Auth.Password == src.Auth.Password && old.Auth.Server == src.Auth.Server
	}
	if !same {
		return Fetched{}, cause
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT data FROM livetv_channels WHERE `+store.JSONText(store.DialectOf(s.o.DB), "data", "sourceId")+`=?`, src.ID)
	if err != nil {
		return Fetched{}, err
	}
	var cs []Channel
	for rows.Next() {
		var b []byte
		var c Channel
		if err = rows.Scan(&b); err == nil {
			err = json.Unmarshal(b, &c)
		}
		if err != nil {
			rows.Close()
			return Fetched{}, err
		}
		cs = append(cs, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return Fetched{}, err
	}
	if len(cs) == 0 {
		return Fetched{}, cause
	}
	guide := XMLGuide{Channels: map[string]Channel{}}
	for _, c := range cs {
		guide.Channels[c.GuideID] = c
	}
	rows, err = s.o.DB.QueryContext(ctx, `SELECT data FROM livetv_programmes WHERE source_id=?`, src.ID)
	if err != nil {
		return Fetched{}, err
	}
	for rows.Next() {
		var b []byte
		var p Programme
		if err = rows.Scan(&b); err == nil {
			err = json.Unmarshal(b, &p)
		}
		if err != nil {
			rows.Close()
			return Fetched{}, err
		}
		guide.Programmes = append(guide.Programmes, p)
	}
	err = rows.Err()
	rows.Close()
	src.Groups = old.Groups
	return Fetched{Channels: cs, Guide: guide, ProviderNumbers: true}, err
}
