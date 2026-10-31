package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
)

// A guided restore runs in two halves. The running server validates a
// bundle into a staging folder in the data directory and records a pending
// restore (RequestRestore), then restarts. The next start applies it
// (ApplyPending) before anything opens the database or the settings: the
// current files move into a timestamped rollback folder and the staged ones
// take their place. Any failure puts the original files back, and a report
// tells the owner what happened either way.
const (
	// PendingFile records a requested restore in the data directory.
	PendingFile = "restore-pending.json"
	// ReportFile is the last restore's report, kept until dismissed.
	ReportFile = "restore-report.json"
	// RollbackDir holds the files each restore replaced, one folder per
	// restore (rollbacks/restore-20261031T101500Z).
	RollbackDir = "rollbacks"
	// rollbackInfo describes a rollback folder.
	rollbackInfo = "rollback.json"
	// StagedPrefix starts every staging folder's name.
	StagedPrefix = "restored-"
	// keepRollbacks is how many rollback folders are kept; each holds a
	// whole database.
	keepRollbacks = 3
)

// liveFiles are the data-directory files a restore replaces: the settings,
// the key that unseals them, and the database with its SQLite sidecars (or
// a logical dump waiting to be imported).
var liveFiles = []string{
	"config.json", config.CredentialKeyFile,
	"ether.db", "ether.db-wal", "ether.db-shm", "ether.db-journal",
	LogicalDumpFile,
}

// stagedFiles are the files a staging folder may provide.
var stagedFiles = []string{"config.json", config.CredentialKeyFile, "ether.db", "ether.db-wal", "ether.db-shm", LogicalDumpFile}

// Pending is a restore waiting for the next start.
type Pending struct {
	// Staged is the staging folder, relative to the data directory.
	Staged string `json:"staged"`
	// Kind is "snapshot" (a bundle kept on this server), "upload" (a
	// downloaded bundle) or "undo" (a previous restore's rollback).
	Kind string `json:"kind"`
	// Source names what is restored: a bundle or file name.
	Source        string    `json:"source"`
	BackupCreated time.Time `json:"backupCreated,omitzero"`
	Requested     time.Time `json:"requested"`
	// Rollback is set once the swap has started (the folder the current
	// files are moving to) and Installing once all of them are there, so
	// a start interrupted half way through restores them instead of
	// guessing.
	Rollback   string `json:"rollback,omitempty"`
	Installing bool   `json:"installing,omitempty"`
}

// Report is the outcome of the last restore, shown in the console until the
// owner dismisses it.
type Report struct {
	// Status is "restored" or "failed" (the original files are back).
	Status        string    `json:"status"`
	Kind          string    `json:"kind"`
	Source        string    `json:"source"`
	BackupCreated time.Time `json:"backupCreated,omitzero"`
	RestoredAt    time.Time `json:"restoredAt"`
	// Rollback is the folder (under rollbacks/) holding the files this
	// restore replaced; Undo puts them back.
	Rollback     string `json:"rollback,omitempty"`
	RollbackPath string `json:"rollbackPath,omitempty"`
	// Files are the data-directory files that were replaced.
	Files []string `json:"files,omitempty"`
	// Logical is set for a PostgreSQL-style dump imported at start;
	// Imported once the import finished.
	Logical  bool `json:"logical,omitempty"`
	Imported bool `json:"imported,omitempty"`
	// Integrity is the restored database's check, filled in once it has
	// opened.
	Integrity *store.Integrity `json:"integrity,omitempty"`
	Error     string           `json:"error,omitempty"`
}

// fsOps are the filesystem calls a swap makes, replaceable in tests to
// fail part way through.
type fsOps struct {
	rename func(oldpath, newpath string) error
}

var osOps = fsOps{rename: os.Rename}

// ErrNoStaged means the staging folder named in a request does not exist.
var ErrNoStaged = errors.New("the prepared backup is no longer available; choose it again")

// validStagedName accepts a staging folder name or a rollback folder
// (rollbacks/<name>), never a path that leaves the data directory.
func validStagedName(name string) bool {
	if dir, base, ok := strings.Cut(name, "/"); ok {
		return dir == RollbackDir && base != "" && filepath.Base(base) == base && strings.HasPrefix(base, "restore-")
	}
	return filepath.Base(name) == name && strings.HasPrefix(name, StagedPrefix)
}

// RequestRestore records that the staging folder staged (a name under
// dataDir) is to replace the current data at the next start.
func RequestRestore(dataDir string, p Pending) error {
	if !validStagedName(p.Staged) {
		return ErrNoStaged
	}
	if err := checkStaged(filepath.Join(dataDir, filepath.FromSlash(p.Staged))); err != nil {
		return err
	}
	p.Rollback, p.Installing = "", false
	if p.Requested.IsZero() {
		p.Requested = time.Now().UTC()
	}
	return writeJSON(filepath.Join(dataDir, PendingFile), p)
}

// ReadPending returns the requested restore, or nil.
func ReadPending(dataDir string) (*Pending, error) {
	var p Pending
	if err := readJSON(filepath.Join(dataDir, PendingFile), &p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// CancelPending forgets a requested restore that has not started.
func CancelPending(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, PendingFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// checkStaged requires a folder with settings and a database.
func checkStaged(dir string) error {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return ErrNoStaged
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		return errors.New("the prepared backup has no settings file")
	}
	for _, db := range []string{"ether.db", LogicalDumpFile} {
		if _, err := os.Stat(filepath.Join(dir, db)); err == nil {
			return nil
		}
	}
	return errors.New("the prepared backup has no database")
}

// ApplyPending applies a requested restore. It must run before anything
// opens the settings or the database. It returns nil when nothing was
// pending, and otherwise the report it also saved: "restored", or "failed"
// with every original file back in place. The error is reserved for the
// case where the originals could not be put back; they are then in the
// rollback folder named in the error, and the server must not start.
func ApplyPending(dataDir string, now func() time.Time) (*Report, error) {
	return applyPending(dataDir, now, osOps)
}

func applyPending(dataDir string, now func() time.Time, ops fsOps) (*Report, error) {
	p, err := ReadPending(dataDir)
	if err != nil {
		// An unreadable request is dropped rather than retried forever.
		_ = CancelPending(dataDir)
		rep := &Report{Status: "failed", RestoredAt: now().UTC(), Error: "the restore request could not be read: " + err.Error()}
		return rep, saveReport(dataDir, rep)
	}
	if p == nil {
		return nil, nil
	}
	rep := &Report{Kind: p.Kind, Source: p.Source, BackupCreated: p.BackupCreated, RestoredAt: now().UTC()}
	staged := filepath.Join(dataDir, filepath.FromSlash(p.Staged))
	if p.Rollback != "" {
		// A previous start stopped part way through: put the originals back.
		rollback := filepath.Join(dataDir, RollbackDir, p.Rollback)
		if err := recoverInterrupted(dataDir, staged, rollback, p.Installing, ops); err != nil {
			return rep, fmt.Errorf("an interrupted restore could not be undone; the original files are in %s: %w", rollback, err)
		}
		_ = os.Remove(rollback)
		_ = CancelPending(dataDir)
		rep.Status, rep.Error = "failed", "the restore was interrupted, so the original data was put back"
		return rep, saveReport(dataDir, rep)
	}
	if !validStagedName(p.Staged) {
		_ = CancelPending(dataDir)
		rep.Status, rep.Error = "failed", ErrNoStaged.Error()
		return rep, saveReport(dataDir, rep)
	}
	if err := checkStaged(staged); err != nil {
		_ = CancelPending(dataDir)
		rep.Status, rep.Error = "failed", err.Error()
		return rep, saveReport(dataDir, rep)
	}
	_, rep.Logical = statOK(filepath.Join(staged, LogicalDumpFile))
	name := "restore-" + rep.RestoredAt.Format("20060102T150405.000Z")
	rollback := filepath.Join(dataDir, RollbackDir, name)
	if err := os.MkdirAll(rollback, 0o700); err != nil {
		_ = CancelPending(dataDir)
		rep.Status, rep.Error = "failed", "could not create the rollback folder: "+err.Error()
		return rep, saveReport(dataDir, rep)
	}
	p.Rollback = name
	if err := writeJSON(filepath.Join(dataDir, PendingFile), p); err != nil {
		_ = os.Remove(rollback)
		_ = CancelPending(dataDir)
		rep.Status, rep.Error = "failed", "could not record the restore: "+err.Error()
		return rep, saveReport(dataDir, rep)
	}
	moved, installed, swapErr := swap(dataDir, staged, rollback, ops, func() error {
		p.Installing = true
		return writeJSON(filepath.Join(dataDir, PendingFile), p)
	})
	if swapErr != nil {
		if err := unswap(dataDir, staged, rollback, moved, installed, ops); err != nil {
			return rep, fmt.Errorf("restore failed (%v) and the original files could not be put back; they are in %s: %w", swapErr, rollback, err)
		}
		_ = os.Remove(rollback)
		_ = CancelPending(dataDir)
		rep.Status, rep.Error = "failed", swapErr.Error()
		return rep, saveReport(dataDir, rep)
	}
	rep.Status, rep.Rollback, rep.RollbackPath, rep.Files = "restored", name, rollback, moved
	_ = writeJSON(filepath.Join(rollback, rollbackInfo), Rollback{
		Name: name, Created: rep.RestoredAt, ReplacedBy: p.Source, Files: moved,
	})
	_ = os.RemoveAll(staged)
	if err := CancelPending(dataDir); err != nil {
		return rep, err
	}
	pruneRollbacks(dataDir, name)
	return rep, saveReport(dataDir, rep)
}

// swap moves the current files into rollback and the staged ones into
// dataDir. It returns the names it moved out and the names it installed,
// for unswap. installing runs between the two halves (to record that every
// original has moved).
func swap(dataDir, staged, rollback string, ops fsOps, installing func() error) (moved, installed []string, err error) {
	for _, f := range liveFiles {
		if _, ok := statOK(filepath.Join(dataDir, f)); !ok {
			continue
		}
		if err := ops.rename(filepath.Join(dataDir, f), filepath.Join(rollback, f)); err != nil {
			return moved, installed, fmt.Errorf("move %s aside: %w", f, err)
		}
		moved = append(moved, f)
	}
	if err := installing(); err != nil {
		return moved, installed, fmt.Errorf("record the restore: %w", err)
	}
	for _, f := range stagedFiles {
		if _, ok := statOK(filepath.Join(staged, f)); !ok {
			continue
		}
		if err := ops.rename(filepath.Join(staged, f), filepath.Join(dataDir, f)); err != nil {
			return moved, installed, fmt.Errorf("install %s: %w", f, err)
		}
		installed = append(installed, f)
	}
	return moved, installed, nil
}

// unswap returns the installed files to staged and the moved originals
// from rollback to dataDir.
func unswap(dataDir, staged, rollback string, moved, installed []string, ops fsOps) error {
	var errs []error
	for _, f := range installed {
		if err := ops.rename(filepath.Join(dataDir, f), filepath.Join(staged, f)); err != nil {
			errs = append(errs, err)
		}
	}
	for _, f := range moved {
		if err := ops.rename(filepath.Join(rollback, f), filepath.Join(dataDir, f)); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// recoverInterrupted undoes a swap a crash interrupted, from what is on
// disk: every original in rollback goes back, and once installing had
// begun (all originals had moved) every live restorable file is one the
// swap installed.
func recoverInterrupted(dataDir, staged, rollback string, installing bool, ops fsOps) error {
	var moved, installed []string
	for _, f := range liveFiles {
		if _, ok := statOK(filepath.Join(rollback, f)); ok {
			moved = append(moved, f)
		}
	}
	if installing {
		if err := os.MkdirAll(staged, 0o700); err != nil {
			return err
		}
		for _, f := range stagedFiles {
			if _, ok := statOK(filepath.Join(dataDir, f)); ok {
				installed = append(installed, f)
			}
		}
	}
	return unswap(dataDir, staged, rollback, moved, installed, ops)
}

// Rollback is a folder of files a restore replaced.
type Rollback struct {
	Name       string    `json:"name"`
	Created    time.Time `json:"created"`
	ReplacedBy string    `json:"replacedBy,omitempty"`
	Files      []string  `json:"files,omitempty"`
}

// pruneRollbacks keeps the newest keepRollbacks rollback folders (and
// always keep).
func pruneRollbacks(dataDir, keep string) {
	entries, err := os.ReadDir(filepath.Join(dataDir, RollbackDir))
	if err != nil {
		return
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "restore-") {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for i, n := range names {
		if i >= keepRollbacks && n != keep {
			_ = os.RemoveAll(filepath.Join(dataDir, RollbackDir, n))
		}
	}
}

// ReadReport returns the last restore's report, or nil.
func ReadReport(dataDir string) (*Report, error) {
	var r Report
	if err := readJSON(filepath.Join(dataDir, ReportFile), &r); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// UpdateReport changes the saved report, if there is one.
func UpdateReport(dataDir string, fn func(*Report)) error {
	r, err := ReadReport(dataDir)
	if err != nil || r == nil {
		return err
	}
	fn(r)
	return saveReport(dataDir, r)
}

// DismissReport removes the report; the rollback folder stays.
func DismissReport(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, ReportFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// RequestUndo asks the next start to put back the files the last restore
// replaced (its rollback folder), as a restore of their own: the restored
// data moves into a new rollback folder, so an undo can itself be undone.
func RequestUndo(dataDir string) error {
	r, err := ReadReport(dataDir)
	if err != nil {
		return err
	}
	if r == nil || r.Status != "restored" || r.Rollback == "" {
		return errors.New("there is no restore to undo")
	}
	return RequestRestore(dataDir, Pending{
		Staged: RollbackDir + "/" + r.Rollback, Kind: "undo",
		Source: r.Source, BackupCreated: r.RestoredAt,
	})
}

func saveReport(dataDir string, r *Report) error {
	return writeJSON(filepath.Join(dataDir, ReportFile), r)
}

// writeJSON writes v to path atomically, owner-only.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func statOK(path string) (fs.FileInfo, bool) {
	st, err := os.Stat(path)
	return st, err == nil
}
