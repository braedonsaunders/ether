package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	sqlite "modernc.org/sqlite"

	"github.com/braedonsaunders/ether/internal/perfharness"
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

// mintCopy opens the copied database (applying this tree's migrations, as
// the server would) and mints viewer credentials into it.
func mintCopy(ctx context.Context, dataDir string, max int) ([]perfharness.Viewer, error) {
	db, err := store.Open(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return perfharness.MintCredentials(ctx, db, max)
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
