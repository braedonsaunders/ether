package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/backup"
	"github.com/braedonsaunders/ether/internal/identity"
)

// ownerRecoveryFile mirrors the owner's local sign-in (username, Argon2id
// hash, identity) in the data directory, owner-only, so a failed start's
// recovery page can check it while the database is down or damaged. It is
// rewritten from the database on every successful owner sign-in, password
// change and start, and removed when the owner has no local password. Each
// cluster node keeps its own; until the next sign-in on a node, its copy
// may hold a previous password.
const ownerRecoveryFile = "owner-recovery.json"

type ownerMirror struct {
	identity.OwnerCredential
	Updated time.Time `json:"updated"`
}

func ownerMirrorPath(dataDir string) string { return filepath.Join(dataDir, ownerRecoveryFile) }

// readOwnerMirror returns the mirrored owner credential, if there is one.
func readOwnerMirror(dataDir string) (identity.OwnerCredential, bool) {
	raw, err := os.ReadFile(ownerMirrorPath(dataDir))
	if err != nil {
		return identity.OwnerCredential{}, false
	}
	var m ownerMirror
	if err := json.Unmarshal(raw, &m); err != nil || m.Username == "" || m.Hash == "" {
		return identity.OwnerCredential{}, false
	}
	return m.OwnerCredential, true
}

// syncOwnerRecovery rewrites (or removes) the mirror from the database.
func (a *App) syncOwnerRecovery(ctx context.Context) {
	if a.db == nil {
		return
	}
	cred, ok, err := identity.OwnerLocalCredential(ctx, a.db)
	if err != nil {
		a.log.Warn("owner recovery sign-in not refreshed", "err", err)
		return
	}
	if !ok {
		if err := os.Remove(ownerMirrorPath(a.opts.DataDir)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			a.log.Warn("owner recovery sign-in not removed", "err", err)
		}
		return
	}
	if cur, ok := readOwnerMirror(a.opts.DataDir); ok && cur == cred {
		return
	}
	if err := backup.WriteJSONDurable(ownerMirrorPath(a.opts.DataDir), ownerMirror{OwnerCredential: cred, Updated: time.Now().UTC()}); err != nil {
		a.log.Warn("owner recovery sign-in not saved", "err", err)
	}
}

// ownerCredentialFromDB reads the owner's local credential from a standalone
// server's SQLite database, read-only (no migration, no write, no WAL
// checkpoint), for when the mirror is missing.
func ownerCredentialFromDB(ctx context.Context, dataDir string) (identity.OwnerCredential, bool, error) {
	path := filepath.Join(dataDir, "ether.db")
	if _, err := os.Stat(path); err != nil {
		return identity.OwnerCredential{}, false, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return identity.OwnerCredential{}, false, err
	}
	uriPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(2000)"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return identity.OwnerCredential{}, false, err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return identity.OwnerLocalCredential(ctx, db)
}
