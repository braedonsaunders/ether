// Package backup creates integrity-checked, portable snapshots without copying live WAL files.
//
// On SQLite a snapshot holds a VACUUM INTO copy (ether.db). On PostgreSQL,
// where file copies do not apply, it holds a logical dump of every table
// (database.jsonl, see store.ExportLogical) and the manifest records
// dialect "postgres"; a restored dump is imported into an empty database
// on the next start (ImportPending).
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/schedule"
	"github.com/braedonsaunders/ether/internal/store"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func init() {
	store.Register("backup", 1, `CREATE TABLE backup_settings(id INTEGER PRIMARY KEY CHECK(id=1),interval_hours INTEGER NOT NULL DEFAULT 24,retain INTEGER NOT NULL DEFAULT 7,last_run INTEGER NOT NULL DEFAULT 0); INSERT INTO backup_settings(id)VALUES(1);`)
	// Clustered servers: an optional backup directory every node can reach,
	// and a cluster-visible record of which node holds which bundle.
	store.Register("backup", 2, `
ALTER TABLE backup_settings ADD COLUMN shared_dir TEXT NOT NULL DEFAULT '';
CREATE TABLE backup_files(
	node_id TEXT NOT NULL,
	name TEXT NOT NULL,
	node_name TEXT NOT NULL DEFAULT '',
	dir TEXT NOT NULL DEFAULT '',
	shared INTEGER NOT NULL DEFAULT 0,
	size INTEGER NOT NULL DEFAULT 0,
	created INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(node_id, name)
);
CREATE INDEX backup_files_created ON backup_files(created);`)
}

const maxBundleFile int64 = 8 << 30

// LogicalDumpFile is the PostgreSQL bundle's database entry. Left in a data
// directory by a restore, it is imported into the (empty) database at start.
const LogicalDumpFile = "database.jsonl"

type Service struct {
	db              *sql.DB
	configPath, dir string
	retain          int
	mu              sync.Mutex
	// node is set on a clustered server (SetCluster).
	node atomic.Pointer[Node]
	// restoring serialises uploaded restores (Handler).
	restoring sync.Mutex
}

// Node identifies the cluster node a Service runs on.
type Node struct {
	ID, Name string
}

// SetCluster marks the server as clustered: every bundle is recorded in
// the shared database with the node that holds it (so any node lists the
// whole cluster's backups), and the shared backup directory setting is
// honoured. A standalone server never calls it and keeps its bundles in
// its data directory only.
func (s *Service) SetCluster(n Node) { s.node.Store(&n) }

func (s *Service) clusterNode() *Node { return s.node.Load() }

func New(db *sql.DB, configPath, dir string, retain int) *Service {
	if retain < 1 {
		retain = 7
	}
	return &Service{db: db, configPath: configPath, dir: dir, retain: retain}
}

type Snapshot struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
	// Clustered servers only: the node that wrote the bundle and holds it
	// (unless Shared), whether it is in the shared backup directory, and
	// Remote when it is on another node's disk (download it there).
	Node   string `json:"node,omitempty"`
	NodeID string `json:"nodeId,omitempty"`
	Shared bool   `json:"shared,omitempty"`
	Remote bool   `json:"remote,omitempty"`
}
type Settings struct {
	IntervalHours int   `json:"intervalHours"`
	Retain        int   `json:"retain"`
	LastRun       int64 `json:"lastRun"`
	// SharedDir (clustered servers) is a directory every node can reach,
	// at the same path, where scheduled and manual backups are written
	// instead of the writing node's data directory. Empty keeps them on
	// the node that made them.
	SharedDir string `json:"sharedDir,omitempty"`
}
type Manifest struct {
	Version int               `json:"version"`
	Created time.Time         `json:"created"`
	SHA256  map[string]string `json:"sha256"`
	// Dialect is "postgres" for a logical dump; empty means SQLite.
	Dialect string `json:"dialect,omitempty"`
}

// dbEntry is the bundle entry holding the database for a dialect.
func dbEntry(dialect string) string {
	if dialect == string(store.Postgres) {
		return LogicalDumpFile
	}
	return "ether.db"
}

func validateLogical(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = store.ValidateLogical(f)
	return e
}

func exportLogical(ctx context.Context, db *sql.DB, path string) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	e = store.ExportLogical(ctx, db, f)
	if se := f.Sync(); e == nil {
		e = se
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	return validateLogical(path)
}

func (s *Service) Settings(ctx context.Context) (Settings, error) {
	var v Settings
	e := s.db.QueryRowContext(ctx, `SELECT interval_hours,retain,last_run,shared_dir FROM backup_settings WHERE id=1`).Scan(&v.IntervalHours, &v.Retain, &v.LastRun, &v.SharedDir)
	return v, e
}
func (s *Service) Configure(ctx context.Context, v Settings) error {
	if v.IntervalHours < 0 || v.IntervalHours > 8760 || v.Retain < 1 || v.Retain > 365 {
		return errors.New("intervalHours must be 0..8760 and retain 1..365")
	}
	v.SharedDir = strings.TrimSpace(v.SharedDir)
	if v.SharedDir != "" && (!filepath.IsAbs(v.SharedDir) || len(v.SharedDir) > 4096) {
		return errors.New("sharedDir must be an absolute path")
	}
	if v.SharedDir != "" && s.clusterNode() == nil {
		return errors.New("a shared backup directory applies only to clustered servers")
	}
	if v.SharedDir != "" {
		v.SharedDir = filepath.Clean(v.SharedDir)
	}
	_, e := s.db.ExecContext(ctx, `UPDATE backup_settings SET interval_hours=?,retain=?,shared_dir=? WHERE id=1`, v.IntervalHours, v.Retain, v.SharedDir)
	return e
}

// List is this node's own bundles (its data directory's backups).
func (s *Service) List() ([]Snapshot, error) { return listDir(s.dir) }

func listDir(dir string) ([]Snapshot, error) {
	entries, e := os.ReadDir(dir)
	if os.IsNotExist(e) {
		return []Snapshot{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []Snapshot{}
	for _, d := range entries {
		if !d.Type().IsRegular() || !strings.HasPrefix(d.Name(), "ether-") || !strings.HasSuffix(d.Name(), ".tar.gz") {
			continue
		}
		st, e := d.Info()
		if e != nil {
			return nil, e
		}
		out = append(out, Snapshot{Name: d.Name(), Size: st.Size(), Created: st.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}
func validateDB(ctx context.Context, path string) error {
	abs, e := filepath.Abs(path)
	if e != nil {
		return e
	}
	// A path is data, not a URI: escape literal ?, # and % before adding
	// the read-only option. Keep drive-letter paths absolute on Windows.
	uriPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro"}
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return e
	}
	defer db.Close()
	var status string
	if e = db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&status); e != nil {
		return e
	}
	if status != "ok" {
		return fmt.Errorf("SQLite integrity check: %s", status)
	}
	var n int
	if e = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); e != nil {
		return errors.New("bundle is not an Ether database")
	}
	return nil
}
func validateConfig(b []byte) error {
	var c map[string]json.RawMessage
	if e := json.Unmarshal(b, &c); e != nil {
		return e
	}
	if c == nil || c["server"] == nil {
		return errors.New("bundle config has no server settings")
	}
	return nil
}
func hashFile(path string) (string, error) {
	f, e := os.Open(path)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (s *Service) Create(ctx context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Snapshot
	settings, se := s.Settings(ctx)
	dir, shared := s.dir, false
	node := s.clusterNode()
	if node != nil && se == nil && settings.SharedDir != "" {
		dir, shared = settings.SharedDir, true
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return out, e
	}
	stage, e := os.MkdirTemp(dir, ".snapshot-")
	if e != nil {
		return out, e
	}
	defer os.RemoveAll(stage)
	conf, e := os.ReadFile(s.configPath)
	if e != nil {
		return out, e
	}
	if e = validateConfig(conf); e != nil {
		return out, e
	}
	if e = os.WriteFile(filepath.Join(stage, "config.json"), conf, 0600); e != nil {
		return out, e
	}
	dialect := ""
	if store.DialectOf(s.db) == store.Postgres {
		dialect = string(store.Postgres)
	}
	dbPath := filepath.Join(stage, dbEntry(dialect))
	if dialect != "" {
		if e = exportLogical(ctx, s.db, dbPath); e != nil {
			return out, e
		}
	} else {
		if _, e = s.db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(dbPath, "'", "''")+`'`); e != nil {
			return out, e
		}
		if e = validateDB(ctx, dbPath); e != nil {
			return out, e
		}
	}
	names := []string{"config.json", dbEntry(dialect)}
	if key, keyErr := os.ReadFile(filepath.Join(filepath.Dir(s.configPath), config.CredentialKeyFile)); keyErr == nil {
		if len(key) != 32 {
			return out, errors.New("invalid credential key")
		}
		if e = os.WriteFile(filepath.Join(stage, config.CredentialKeyFile), key, 0600); e != nil {
			return out, e
		}
		names = append(names, config.CredentialKeyFile)
	} else if !os.IsNotExist(keyErr) {
		return out, keyErr
	} else if strings.Contains(string(conf), sealedPrefix) {
		return out, errors.New("encrypted config has no credential key")
	}
	manifest := Manifest{Version: 1, Created: time.Now().UTC(), SHA256: map[string]string{}, Dialect: dialect}
	if len(names) == 3 {
		manifest.Version = 2
	}
	for _, name := range names {
		h, e := hashFile(filepath.Join(stage, name))
		if e != nil {
			return out, e
		}
		manifest.SHA256[name] = h
	}
	b, e := json.Marshal(manifest)
	if e != nil {
		return out, e
	}
	if e = os.WriteFile(filepath.Join(stage, "manifest.json"), b, 0600); e != nil {
		return out, e
	}
	name := "ether-" + manifest.Created.Format("20060102T150405.000000000Z") + ".tar.gz"
	temp := filepath.Join(stage, "bundle")
	f, e := os.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return out, e
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, name := range append([]string{"manifest.json"}, names...) {
		path := filepath.Join(stage, name)
		st, se := os.Stat(path)
		if se != nil {
			e = se
			break
		}
		if e = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: st.Size(), ModTime: manifest.Created}); e != nil {
			break
		}
		src, se := os.Open(path)
		if se != nil {
			e = se
			break
		}
		_, e = io.Copy(tw, src)
		src.Close()
		if e != nil {
			break
		}
	}
	for _, ce := range []error{tw.Close(), gz.Close(), f.Sync(), f.Close()} {
		if e == nil {
			e = ce
		}
	}
	if e != nil {
		return out, e
	}
	target := filepath.Join(dir, name)
	if e = os.Rename(temp, target); e != nil {
		return out, e
	}
	retain := s.retain
	if se == nil {
		retain = settings.Retain
	}
	if _, e = s.db.ExecContext(ctx, `UPDATE backup_settings SET last_run=? WHERE id=1`, manifest.Created.Unix()); e != nil {
		return out, e
	}
	snaps, e := listDir(dir)
	if e != nil {
		return out, e
	}
	for i, snap := range snaps {
		if snap.Name == name {
			out = snap
		}
		if i >= retain {
			if e = os.Remove(filepath.Join(dir, snap.Name)); e != nil {
				return out, e
			}
		}
	}
	if node != nil {
		if e = s.record(ctx, *node, dir, shared); e != nil {
			return out, e
		}
		out.Node, out.NodeID, out.Shared = node.Name, node.ID, shared
	}
	return out, nil
}

// sealedPrefix starts every value sealed with the credential key, in
// config.json and in database columns, whatever its format version.
const sealedPrefix = "ether-sealed:"

// fileContains reports whether the file at path contains needle, reading
// it as a stream.
func fileContains(path, needle string) (bool, error) {
	f, e := os.Open(path)
	if e != nil {
		return false, e
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	carry := 0
	for {
		n, e := f.Read(buf[carry:])
		if strings.Contains(string(buf[:carry+n]), needle) {
			return true, nil
		}
		if e == io.EOF {
			return false, nil
		}
		if e != nil {
			return false, e
		}
		// Keep a needle-length tail so a match across reads is found.
		keep := min(len(needle)-1, carry+n)
		copy(buf, buf[carry+n-keep:carry+n])
		carry = keep
	}
}

// RestoreBundle validates all content before atomically installing into a new directory.
// It deliberately refuses existing destinations and never replaces a running database.
func RestoreBundle(ctx context.Context, r io.Reader, destination string) error {
	return restore(ctx, r, destination, nil)
}

// RestoreEncrypted is RestoreBundle for a password-encrypted bundle
// (NewEncryptWriter). Nothing is installed unless the whole file
// authenticates, including its final chunk and the absence of anything
// after it.
func RestoreEncrypted(ctx context.Context, r io.Reader, password, destination string) error {
	if _, e := os.Lstat(destination); e == nil {
		return errors.New("restore destination already exists")
	}
	dec, e := NewDecryptReader(r, password)
	if e != nil {
		return e
	}
	return restore(ctx, dec, destination, func() error {
		_, e := io.Copy(io.Discard, dec)
		return e
	})
}

// restore validates a plaintext bundle from r into a staging directory and
// renames it to destination. finish, when set, runs before the rename and
// must succeed for anything to be installed.
func restore(ctx context.Context, r io.Reader, destination string, finish func() error) error {
	if _, e := os.Lstat(destination); e == nil {
		return errors.New("restore destination already exists")
	} else if !os.IsNotExist(e) {
		return e
	}
	parent := filepath.Dir(destination)
	if e := os.MkdirAll(parent, 0700); e != nil {
		return e
	}
	stage, e := os.MkdirTemp(parent, ".restore-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	gz, e := gzip.NewReader(r)
	if e != nil {
		return e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		isDB := h.Name == "ether.db" || h.Name == LogicalDumpFile
		if h.Typeflag != tar.TypeReg || (h.Name != "manifest.json" && h.Name != "config.json" && !isDB && h.Name != config.CredentialKeyFile) || seen[h.Name] || h.Size < 0 || h.Size > maxBundleFile {
			return errors.New("invalid or unsafe bundle entry")
		}
		if !isDB && h.Size > 16<<20 {
			return errors.New("bundle metadata too large")
		}
		seen[h.Name] = true
		f, e := os.OpenFile(filepath.Join(stage, h.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = io.CopyN(f, tr, h.Size)
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	}
	tail, te := io.ReadAll(io.LimitReader(gz, (1<<20)+1))
	if te != nil {
		return te
	}
	if len(tail) > 1<<20 || strings.Trim(string(tail), "\x00") != "" {
		return errors.New("unexpected trailing bundle content")
	}
	if len(seen) != 3 && len(seen) != 4 {
		return errors.New("incomplete backup bundle")
	}
	raw, e := os.ReadFile(filepath.Join(stage, "manifest.json"))
	if e != nil {
		return e
	}
	var m Manifest
	if e = json.Unmarshal(raw, &m); e != nil {
		return e
	}
	if m.Dialect != "" && m.Dialect != string(store.Postgres) {
		return errors.New("unsupported backup manifest")
	}
	names := []string{"config.json", dbEntry(m.Dialect)}
	if m.Version == 2 {
		names = append(names, config.CredentialKeyFile)
	}
	if (m.Version != 1 && m.Version != 2) || len(m.SHA256) != len(names) || len(seen) != len(names)+1 {
		return errors.New("unsupported backup manifest")
	}
	for _, name := range names {
		sum, e := hashFile(filepath.Join(stage, name))
		if e != nil {
			return e
		}
		if sum != m.SHA256[name] {
			return fmt.Errorf("checksum mismatch for %s", name)
		}
	}
	raw, e = os.ReadFile(filepath.Join(stage, "config.json"))
	if e != nil {
		return e
	}
	if e = validateConfig(raw); e != nil {
		return e
	}
	hasKey := m.Version == 2
	if strings.Contains(string(raw), sealedPrefix) {
		if !hasKey {
			return errors.New("bundle has encrypted settings but no credential key")
		}
		if _, e = config.Open(stage); e != nil {
			return e
		}
	}
	dbPath := filepath.Join(stage, dbEntry(m.Dialect))
	if m.Dialect != "" {
		e = validateLogical(dbPath)
	} else {
		e = validateDB(ctx, dbPath)
	}
	if e != nil {
		return e
	}
	if !hasKey {
		// Sealed database columns are unreadable without the key.
		sealed, e := fileContains(dbPath, sealedPrefix)
		if e != nil {
			return e
		}
		if sealed {
			return errors.New("bundle has encrypted data but no credential key")
		}
	}
	if finish != nil {
		if e = finish(); e != nil {
			return e
		}
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	return os.Rename(stage, destination)
}

// Run takes scheduled backups until ctx ends. onResult hears every
// scheduled attempt: its error, or nil once one succeeds. A failing backup
// is retried with backoff (15 minutes, doubling, at most the interval), not
// every minute.
func (s *Service) Run(ctx context.Context, onResult func(error)) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	var r retryState
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if attempted, e := s.runScheduled(ctx, time.Now(), &r); attempted && onResult != nil {
				onResult(e)
			}
		}
	}
}

// retryState spaces out attempts after scheduled backups fail.
type retryState struct {
	failures int
	next     time.Time
}

const firstBackupRetry = 15 * time.Minute

// runScheduled takes a backup if one is due and reports whether it tried.
func (s *Service) runScheduled(ctx context.Context, now time.Time, r *retryState) (bool, error) {
	if now.Before(r.next) {
		return false, nil
	}
	limit := 24 * time.Hour
	v, e := s.Settings(ctx)
	if e == nil {
		interval := time.Duration(v.IntervalHours) * time.Hour
		if v.IntervalHours <= 0 || !schedule.Due(now, time.Unix(v.LastRun, 0), interval) {
			return false, nil
		}
		limit = interval
		_, e = s.Create(ctx)
	}
	if e == nil {
		*r = retryState{}
		return true, nil
	}
	wait := firstBackupRetry << min(r.failures, 8)
	if wait > limit {
		wait = limit
	}
	r.failures++
	r.next = now.Add(wait)
	return true, e
}

// FilesystemWarning detects the mount containing dataDir on Linux and macOS.
func FilesystemWarning(ctx context.Context, dataDir string) string {
	out, e := exec.CommandContext(ctx, "mount").Output()
	if e != nil {
		return ""
	}
	abs, e := filepath.Abs(dataDir)
	if e != nil {
		return ""
	}
	bestLen := 0
	kind := ""
	for _, line := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(line, " on ", 2)
		if len(parts) != 2 {
			continue
		}
		rest := parts[1]
		cut := strings.Index(rest, " type ")
		mount := ""
		typ := ""
		if cut >= 0 {
			mount = rest[:cut]
			typ = strings.Fields(rest[cut+6:])[0]
		} else if cut = strings.Index(rest, " ("); cut >= 0 {
			mount = rest[:cut]
			typ = strings.Trim(strings.Split(rest[cut+2:], ",")[0], ") ")
		}
		mount = strings.ReplaceAll(mount, `\040`, " ")
		if mount != "" && (abs == mount || strings.HasPrefix(abs, strings.TrimRight(mount, "/")+"/")) && len(mount) > bestLen {
			bestLen = len(mount)
			kind = strings.ToLower(typ)
		}
	}
	if strings.Contains(kind, "nfs") || strings.Contains(kind, "smb") || strings.Contains(kind, "cifs") {
		return "Data directory is on " + kind + "; keep the live SQLite database on a local filesystem and copy backup bundles to network storage."
	}
	return ""
}

// ImportPending imports a restored logical dump (LogicalDumpFile in
// dataDir) into db, which must be freshly migrated and empty, then renames
// the dump so it is imported only once. It reports whether it imported.
func ImportPending(ctx context.Context, db *sql.DB, dataDir string) (bool, error) {
	path := filepath.Join(dataDir, LogicalDumpFile)
	f, e := os.Open(path)
	if os.IsNotExist(e) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	e = store.ImportLogical(ctx, db, f)
	f.Close()
	if e != nil {
		if errors.Is(e, store.ErrNotEmpty) {
			return false, fmt.Errorf("restored %s cannot be imported: %w; point the server at an empty database or remove the file", LogicalDumpFile, e)
		}
		return false, fmt.Errorf("import %s: %w", LogicalDumpFile, e)
	}
	if e = os.Rename(path, path+".imported"); e != nil {
		return true, e
	}
	return true, nil
}
