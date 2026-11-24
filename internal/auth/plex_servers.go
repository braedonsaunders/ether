package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

// Extra Plex servers (config.PlexExtraServer) share this viewer table with
// the main server. Each server's access tokens carry its own source
// ("friend:p2", "server:p2"), and auth_server_members records which server
// shares with which viewer, so one server's friend sync never takes access
// away that another server still grants.
func init() {
	// Its own component, without a foreign key, so the auth table rebuilds
	// (auth/4) and the delivered auth version stay as they are; viewer
	// purge and merge drop the rows (the next friend sync rebuilds them).
	store.Register("plex_server_members", 1, `CREATE TABLE auth_server_members (
	server_id TEXT NOT NULL,
	user_id INTEGER NOT NULL,
	PRIMARY KEY (server_id, user_id)
);`)
	store.RegisterViewerTable("plex_server_members", "auth_server_members", "user_id", store.ViewerMergeDrop)
}

// mainServerMember is the main server's key in auth_server_members.
const mainServerMember = "main"

func friendSource(serverID string) string {
	if serverID == "" {
		return sourceFriend
	}
	return sourceFriend + ":" + serverID
}

func memberServer(serverID string) string {
	if serverID == "" {
		return mainServerMember
	}
	return serverID
}

// SetServerToken records the extra server id's own plex.tv token as an owner
// credential: Plex apps signed in to that server's account present it, and it
// belongs to the Ether owner, who added the account. The main owner is not
// replaced (SetOwner is for the main server only).
func (s *Service) SetServerToken(ctx context.Context, id, token string) error {
	if id == "" {
		return errors.New("auth: extra plex server without id")
	}
	src := sourceServer + ":" + id
	now := s.o.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE source=?`, src); err != nil {
		return err
	}
	if token != "" {
		var owner int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM auth_users WHERE is_owner=1`).Scan(&owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("auth: no owner yet")
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_tokens (token_hash, user_id, source, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(token_hash) DO UPDATE SET user_id=excluded.user_id, source=excluded.source, created_at=excluded.created_at`,
			HashToken(token), owner, src, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	s.pos = map[string]cacheEntry{}
	s.neg = map[string]negEntry{}
	s.mu.Unlock()
	err = s.reload(ctx)
	s.notify("caches")
	return err
}

// RemoveServer forgets an extra server's tokens and members. Viewers it alone
// shared with lose access at the main server's next friend sync.
func (s *Service) RemoveServer(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("auth: extra plex server without id")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_tokens WHERE source=? OR source=?`, friendSource(id), sourceServer+":"+id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_server_members WHERE server_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_server_moves WHERE from_server=? OR to_server=?`, id, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.mu.Lock()
	s.pos = map[string]cacheEntry{}
	s.neg = map[string]negEntry{}
	s.mu.Unlock()
	err = s.reload(ctx)
	s.notify("caches")
	return err
}

// TokenServer reports which Plex server a token was issued for: "" for the
// main server, an extra server's id, or ok=false for an unknown token.
func (s *Service) TokenServer(ctx context.Context, token string) (string, bool) {
	if token == "" || s.db == nil {
		return "", false
	}
	var src string
	if err := s.db.QueryRowContext(ctx, `SELECT source FROM auth_tokens WHERE token_hash=?`, HashToken(token)).Scan(&src); err != nil {
		return "", false
	}
	if _, id, ok := strings.Cut(src, ":"); ok && (strings.HasPrefix(src, sourceFriend+":") || strings.HasPrefix(src, sourceServer+":")) {
		return id, true
	}
	return "", true
}

// ServerMembers counts the viewers each Plex server shares with ("" is the
// main server).
func (s *Service) ServerMembers(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.server_id, COUNT(*) FROM auth_server_members m JOIN auth_users u ON u.id=m.user_id WHERE u.active=1 GROUP BY m.server_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		if id == mainServerMember {
			id = ""
		}
		out[id] = n
	}
	return out, rows.Err()
}

// ServerMemberships lists, per viewer, the Plex servers that share with them
// ("" is the main server), as the last friend syncs recorded.
func (s *Service) ServerMemberships(ctx context.Context) (map[int64][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, server_id FROM auth_server_members ORDER BY server_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var uid int64
		var id string
		if err := rows.Scan(&uid, &id); err != nil {
			return nil, err
		}
		if id == mainServerMember {
			id = ""
		}
		out[uid] = append(out[uid], id)
	}
	return out, rows.Err()
}

// localSignIns is the set of viewers with a local username and password
// (identity_links). Empty when the identity tables are not installed.
func (s *Service) localSignIns(ctx context.Context) map[int64]bool {
	out := map[int64]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT viewer_id FROM identity_links WHERE provider='local' AND password_hash<>''`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

func init() {
	// A move waiting for the viewer to accept the new server's invite: the
	// old server keeps sharing until then (from/to "main" is the main server).
	store.Register("plex_server_members", 2, `CREATE TABLE auth_server_moves (
	user_id INTEGER NOT NULL,
	from_server TEXT NOT NULL,
	to_server TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, from_server)
);`)
	store.RegisterViewerTable("plex_server_members", "auth_server_moves", "user_id", store.ViewerMergeDrop)
}

// PendingMove is a viewer invited on To who still has From's share.
type PendingMove struct {
	UserID int64
	From   string
	To     string
}

// AddPendingMove records that uid's share on from ends once they accept to.
func (s *Service) AddPendingMove(ctx context.Context, uid int64, from, to string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO auth_server_moves (user_id, from_server, to_server, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, from_server) DO UPDATE SET to_server=excluded.to_server, created_at=excluded.created_at`,
		uid, memberServer(from), memberServer(to), s.o.Now().Unix())
	return err
}

// PendingMoves lists the moves waiting for an accepted invite.
func (s *Service) PendingMoves(ctx context.Context) ([]PendingMove, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, from_server, to_server FROM auth_server_moves ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingMove
	for rows.Next() {
		var m PendingMove
		if err := rows.Scan(&m.UserID, &m.From, &m.To); err != nil {
			return nil, err
		}
		if m.From == mainServerMember {
			m.From = ""
		}
		if m.To == mainServerMember {
			m.To = ""
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DonePendingMove forgets a finished move and records the viewer off from.
func (s *Service) DonePendingMove(ctx context.Context, uid int64, from string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_server_moves WHERE user_id=? AND from_server=?`, uid, memberServer(from)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM auth_server_members WHERE user_id=? AND server_id=?`, uid, memberServer(from)); err != nil {
		return err
	}
	return tx.Commit()
}
