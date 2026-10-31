package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/store"
)

// copyDataDir copies what a server needs from src into dst: a consistent
// online backup of ether.db (the source may be a running server) plus the
// settings, credential key, node identity and certificates. Caches, logs,
// backups and NZBs are left behind.
func copyDataDir(ctx context.Context, src, dst string) error {
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if err := backupSQLite(ctx, filepath.Join(src, "ether.db"), filepath.Join(dst, "ether.db")); err != nil {
		return fmt.Errorf("copy database: %w", err)
	}
	for _, name := range []string{"config.json", ".credentials-key", "node.json"} {
		if err := copyFile(filepath.Join(src, name), filepath.Join(dst, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return copyTree(filepath.Join(src, "certs"), filepath.Join(dst, "certs"))
}

// backupSQLite uses SQLite's online backup, which is consistent while
// another process writes the source.
func backupSQLite(ctx context.Context, src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	_ = os.Remove(dst)
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(raw any) error {
		b, ok := raw.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("sqlite driver has no backup API")
		}
		bk, err := b.NewBackup(dst)
		if err != nil {
			return err
		}
		for {
			more, err := bk.Step(4096)
			if err != nil {
				_ = bk.Finish()
				return err
			}
			if !more {
				break
			}
		}
		return bk.Finish()
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if errors.Is(err, os.ErrNotExist) && path == src {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target)
	})
}

// sanitizeConfig rewrites the copied settings so the server cannot reach
// anything real: no Usenet, debrid or indexer providers, no LLM, subtitle,
// Trakt or Simkl accounts, no update feed, no UPnP or remote access, and
// every listener on loopback ports derived from port. Unknown fields are
// kept as they are.
func sanitizeConfig(path string, port int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var c map[string]any
	if err := json.Unmarshal(b, &c); err != nil {
		return err
	}
	section := func(name string) map[string]any {
		m, _ := c[name].(map[string]any)
		if m == nil {
			m = map[string]any{}
			c[name] = m
		}
		return m
	}
	server := section("server")
	server["port"] = port
	server["bindAddress"] = "127.0.0.1"
	server["webFrameOrigins"] = []any{}
	c["frontendPorts"] = map[string]any{"jellyfin": port + 1, "emby": port + 2, "silo": port + 3}
	plex := section("plex")
	plex["remoteAccess"] = false
	plex["useUpnp"] = false
	updates := section("updates")
	updates["autoUpdate"] = false
	updates["feedUrl"] = "http://127.0.0.1:1/feed"
	section("usenet")["providers"] = []any{}
	section("debrid")["providers"] = []any{}
	c["indexers"] = []any{}
	for _, name := range []string{"llm", "openSubtitles", "trakt", "simkl", "posters"} {
		section(name)["enabled"] = false
	}
	section("markers")["theIntroDb"] = false
	out, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// Credentials minted into the throwaway copy. Every viewer signs in with
// perfPassword through the real protocol sign-in routes; Plex and the owner
// console use a minted Plex token; Stremio uses a minted surface grant.
const perfPassword = "etherperf-password"

type viewer struct {
	ID         int64
	Username   string
	Owner      bool
	Restricted bool
	PlexToken  string
	Stremio    string
}

type activeViewers struct{ db *sql.DB }

func (a activeViewers) ActiveUser(ctx context.Context, id int64) (*core.User, error) {
	var u core.User
	var owner int
	err := a.db.QueryRowContext(ctx, `SELECT id, username, is_owner FROM auth_users WHERE id=? AND active=1`, id).Scan(&u.ID, &u.Username, &owner)
	if err != nil {
		return nil, core.ErrUnauthorized
	}
	u.IsOwner = owner == 1
	return &u, nil
}

// mintCredentials opens the copied database (applying this tree's
// migrations, as the server would) and gives up to max active viewers a
// known password, a Plex token and a Stremio grant. The owner comes first.
func mintCredentials(ctx context.Context, dataDir string, max int) ([]viewer, error) {
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT u.id, u.is_owner, u.restricted, COALESCE((SELECT subject FROM identity_links l WHERE l.viewer_id=u.id AND l.provider='local' LIMIT 1), u.username)
		FROM auth_users u WHERE u.active=1 AND u.owner_enabled=1 ORDER BY u.is_owner DESC, u.id LIMIT ?`, max)
	if err != nil {
		return nil, err
	}
	var viewers []viewer
	for rows.Next() {
		var v viewer
		var owner, restricted int
		if err := rows.Scan(&v.ID, &owner, &restricted, &v.Username); err != nil {
			rows.Close()
			return nil, err
		}
		v.Owner, v.Restricted = owner == 1, restricted == 1
		viewers = append(viewers, v)
	}
	rows.Close()
	if len(viewers) == 0 {
		return nil, errors.New("no active viewers in the copied database")
	}
	local, err := identity.NewLocal(db)
	if err != nil {
		return nil, err
	}
	grants, err := identity.NewGrants(db, activeViewers{db}, 30*24*time.Hour, nil)
	if err != nil {
		return nil, err
	}
	for i := range viewers {
		v := &viewers[i]
		if err := local.UpsertPassword(ctx, v.ID, v.Username, perfPassword); err != nil {
			return nil, fmt.Errorf("password for %s: %w", v.Username, err)
		}
		var raw [20]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return nil, err
		}
		v.PlexToken = "perf" + hex.EncodeToString(raw[:])
		if _, err := db.ExecContext(ctx, `INSERT INTO auth_tokens(token_hash, user_id, source, created_at) VALUES(?,?,?,?)`,
			auth.HashToken(v.PlexToken), v.ID, "etherperf", time.Now().Unix()); err != nil {
			return nil, fmt.Errorf("plex token for %s: %w", v.Username, err)
		}
		v.Stremio, err = grants.Issue(ctx, v.ID, nil, "stremio", core.Device{ID: "etherperf-stremio-" + v.Username, Client: "stremio", Product: "stremio"})
		if err != nil {
			return nil, fmt.Errorf("stremio grant for %s: %w", v.Username, err)
		}
	}
	return viewers, nil
}
