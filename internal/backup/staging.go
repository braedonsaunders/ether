package backup

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
)

// Staged is a validated bundle waiting in a staging folder.
type Staged struct {
	// ID is the staging folder's name in the data directory.
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	// Created is when the backup was taken.
	Created time.Time `json:"created"`
	// Logical is set for a PostgreSQL-style dump: it is imported into
	// this server's own database when it starts.
	Logical bool  `json:"logical,omitempty"`
	Size    int64 `json:"size"`
	// Path is the staging folder, for the guided restore on a cluster.
	Path string `json:"path"`
}

// errNotStaged means a staging folder named in a request does not exist.
var errNotStaged = errors.New("no such prepared backup")

// newStagingDir names a fresh staging folder in dataDir.
func newStagingDir(dataDir string) string {
	return filepath.Join(dataDir, StagedPrefix+time.Now().UTC().Format("20060102T150405.000000000"))
}

// StageUpload validates an uploaded bundle (encrypted with password, or a
// legacy plaintext bundle when password is empty) into a new staging folder
// in dataDir.
func StageUpload(ctx context.Context, dataDir string, r io.Reader, password, source string) (Staged, error) {
	body := bufio.NewReaderSize(r, 64<<10)
	head, _ := body.Peek(len(encMagic))
	dest := newStagingDir(dataDir)
	var err error
	if IsEncrypted(head) {
		err = RestoreEncrypted(ctx, body, password, dest)
	} else {
		err = RestoreBundle(ctx, body, dest)
	}
	if err != nil {
		return Staged{}, err
	}
	return describeStaged(dataDir, filepath.Base(dest), "upload", source)
}

// StageSnapshot validates a bundle kept on this server into a staging
// folder.
func (s *Service) StageSnapshot(ctx context.Context, name string) (Staged, error) {
	if filepath.Base(name) != name || !strings.HasPrefix(name, "ether-") || !strings.HasSuffix(name, ".tar.gz") {
		return Staged{}, os.ErrNotExist
	}
	path, err := s.locate(ctx, name)
	if err != nil {
		return Staged{}, err
	}
	return stageFile(ctx, filepath.Dir(s.configPath), path)
}

// LocalSnapshots lists the bundles kept in a standalone server's data
// directory, read without its database (a start that failed).
func LocalSnapshots(dataDir string) ([]Snapshot, error) {
	return listDir(filepath.Join(dataDir, "backups"))
}

// StageLocal validates a bundle kept in a standalone server's data
// directory (LocalSnapshots) into a staging folder.
func StageLocal(ctx context.Context, dataDir, name string) (Staged, error) {
	if filepath.Base(name) != name || !strings.HasPrefix(name, "ether-") || !strings.HasSuffix(name, ".tar.gz") {
		return Staged{}, os.ErrNotExist
	}
	return stageFile(ctx, dataDir, filepath.Join(dataDir, "backups", name))
}

func stageFile(ctx context.Context, dataDir, path string) (Staged, error) {
	f, err := os.Open(path)
	if err != nil {
		return Staged{}, err
	}
	defer f.Close()
	dest := newStagingDir(dataDir)
	if err := RestoreBundle(ctx, f, dest); err != nil {
		return Staged{}, err
	}
	return describeStaged(dataDir, filepath.Base(dest), "snapshot", filepath.Base(path))
}

// describeStaged summarises a staging folder from its manifest.
func describeStaged(dataDir, id, kind, source string) (Staged, error) {
	dir := filepath.Join(dataDir, id)
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return Staged{}, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return Staged{}, err
	}
	st := Staged{ID: id, Kind: kind, Source: source, Created: m.Created, Logical: m.Dialect != "", Path: dir}
	if fi, ok := statOK(filepath.Join(dir, dbEntry(m.Dialect))); ok {
		st.Size = fi.Size()
	}
	// Remember where the bundle came from for the confirmation and report.
	_ = writeJSON(filepath.Join(dir, "staged.json"), st)
	return st, nil
}

// StagedInfo returns the summary of a staging folder.
func StagedInfo(dataDir, id string) (Staged, error) {
	if filepath.Base(id) != id || !strings.HasPrefix(id, StagedPrefix) {
		return Staged{}, errNotStaged
	}
	var st Staged
	if err := readJSON(filepath.Join(dataDir, id, "staged.json"), &st); err != nil {
		return Staged{}, errNotStaged
	}
	return st, nil
}

// DiscardStaged removes a staging folder the owner decided not to restore.
func DiscardStaged(dataDir, id string) error {
	if filepath.Base(id) != id || !strings.HasPrefix(id, StagedPrefix) {
		return errNotStaged
	}
	if p, _ := ReadPending(dataDir); p != nil && p.Staged == id {
		return errors.New("this backup is about to be restored")
	}
	return os.RemoveAll(filepath.Join(dataDir, id))
}

// CleanStaged removes staging folders older than age that no restore is
// waiting for: each holds a whole database.
func CleanStaged(dataDir string, age time.Duration) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return
	}
	p, _ := ReadPending(dataDir)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), StagedPrefix) || (p != nil && p.Staged == e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > age {
			_ = os.RemoveAll(filepath.Join(dataDir, e.Name()))
		}
	}
}

// PrepareStaged makes a staged bundle's settings fit this server before it
// is restored here: the listening port and address stay this server's (so
// the console reconnects after the restart), and the database setting is
// cleared, because a guided restore only replaces a standalone server's own
// database (a cluster's dump is imported into it).
func PrepareStaged(dataDir, id string, current config.Server) error {
	if _, err := StagedInfo(dataDir, id); err != nil {
		return err
	}
	cfg, err := config.Open(filepath.Join(dataDir, id))
	if err != nil {
		return err
	}
	return cfg.Update(func(c *config.Config) error {
		c.Server.Port, c.Server.BindAddress = current.Port, current.BindAddress
		c.Database = config.Database{}
		return nil
	})
}

// RequestStaged records a pending restore of a staging folder.
func RequestStaged(dataDir, id string) error {
	st, err := StagedInfo(dataDir, id)
	if err != nil {
		return err
	}
	return RequestRestore(dataDir, Pending{Staged: id, Kind: st.Kind, Source: st.Source, BackupCreated: st.Created})
}

// OneClick reports whether this server can restore in place: only a
// standalone server, whose database is a file in its own data directory. A
// cluster node shares a PostgreSQL database with the other nodes, so
// replacing it is a guided, cluster-wide procedure.
func (s *Service) OneClick() bool {
	return s.clusterNode() == nil && store.DialectOf(s.db) != store.Postgres
}
