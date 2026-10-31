package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/store"
)

// A guided restore runs in three steps. The running server validates a
// bundle into a staging folder in the data directory and records a pending
// restore (RequestRestore), then restarts. The next start applies it
// (ApplyPending) before anything opens the database or the settings: the
// current files move into a timestamped rollback folder and the staged ones
// take their place. The restore only counts once the restored server has
// opened its database and is about to serve (ConfirmRestore); a start that
// fails before then puts the original files back (RollbackApplied, or
// ApplyPending at the next start), and a report tells the owner what
// happened either way.
//
// Every file move is journalled in the pending marker before and after it
// happens, and the marker, the files and their directories are fsynced, so
// a crash or power loss at any point leaves enough on disk to put exactly
// the original files back.
const (
	// PendingFile records a requested restore in the data directory.
	PendingFile = "restore-pending.json"
	// ReportFile is the last restore's report, kept until dismissed.
	ReportFile = "restore-report.json"
	// RollbackDir holds the files each restore replaced, one folder per
	// restore (rollbacks/restore-20261031T101500Z).
	RollbackDir = "rollbacks"
	// rollbackInfo describes a confirmed rollback folder; a folder without
	// it belongs to a restore that never finished.
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

// stagedFiles are the files a staging (or rollback) folder may provide;
// an undo puts back everything the restore moved aside, a hot journal
// included.
var stagedFiles = liveFiles

// Pending is a restore waiting for (or in the middle of) a start.
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
	// Rollback is set once the swap has started: the folder (under
	// rollbacks/) the current files move to.
	Rollback string `json:"rollback,omitempty"`
	// Moved are the original files now in the rollback folder, Installed
	// the staged files now in the data directory, and Step the move in
	// flight ("aside:", "install:", "return:" or "restore:" and a file
	// name), all written before and after each move.
	Moved     []string `json:"moved,omitempty"`
	Installed []string `json:"installed,omitempty"`
	Step      string   `json:"step,omitempty"`
	// Applied is set once every file is in place: the restored server is
	// starting and the restore is confirmed when it is ready.
	Applied bool `json:"applied,omitempty"`
	// Failure is why an applied restore is being rolled back.
	Failure string `json:"failure,omitempty"`
}

// Report is the outcome of the last restore, shown in the console until the
// owner dismisses it.
type Report struct {
	// Status is "restored", or "failed" when the original files are back
	// (the restore could not be applied, or the restored server could not
	// start: RolledBack).
	Status        string    `json:"status"`
	Kind          string    `json:"kind"`
	Source        string    `json:"source"`
	BackupCreated time.Time `json:"backupCreated,omitzero"`
	RestoredAt    time.Time `json:"restoredAt"`
	// RolledBack is set when the backup was in place but the server could
	// not start on it, so the original data was put back automatically.
	RolledBack bool `json:"rolledBack,omitempty"`
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
	// Integrity is the restored database's check.
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

// ErrRestoreInProgress means a restore has started (or its record is
// damaged): nothing else may be requested until the next start has
// finished or undone it.
var ErrRestoreInProgress = errors.New("a restore is already being applied; wait for Ether to start")

// validStagedName accepts a staging folder name or a rollback folder
// (rollbacks/<name>), never a path that leaves the data directory.
func validStagedName(name string) bool {
	if dir, base, ok := strings.Cut(name, "/"); ok {
		return dir == RollbackDir && base != "" && filepath.Base(base) == base && strings.HasPrefix(base, "restore-")
	}
	return filepath.Base(name) == name && strings.HasPrefix(name, StagedPrefix)
}

// RequestRestore records that the staging folder staged (a name under
// dataDir) is to replace the current data at the next start. It refuses
// while a restore is being applied: its record must stay until that
// restore is finished or undone.
func RequestRestore(dataDir string, p Pending) error {
	if !validStagedName(p.Staged) {
		return ErrNoStaged
	}
	if err := checkStaged(filepath.Join(dataDir, filepath.FromSlash(p.Staged))); err != nil {
		return err
	}
	if cur, err := ReadPending(dataDir); err != nil || (cur != nil && cur.Rollback != "") {
		return ErrRestoreInProgress
	}
	p.Rollback, p.Moved, p.Installed, p.Step, p.Applied, p.Failure = "", nil, nil, "", false, ""
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

// CancelPending forgets a requested restore that has not started. A
// restore being applied (or a damaged record) is left alone.
func CancelPending(dataDir string) error {
	if p, err := ReadPending(dataDir); err != nil || (p != nil && p.Rollback != "") {
		return ErrRestoreInProgress
	}
	return removeFile(filepath.Join(dataDir, PendingFile))
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

// ApplyPending applies a requested restore, or finishes undoing one a
// previous start left behind. It must run before anything opens the
// settings or the database. It returns nil when nothing was pending, a
// report with Status "applied" when the backup is now in place (call
// ConfirmRestore once the server is ready, or RollbackApplied if it cannot
// start), and otherwise the "failed" report it also saved, with every
// original file back in place. The error is reserved for the cases where
// the originals cannot be put back or the restore record is damaged; the
// server must then not start.
func ApplyPending(dataDir string, now func() time.Time) (*Report, error) {
	return applyPending(dataDir, now, osOps)
}

func applyPending(dataDir string, now func() time.Time, ops fsOps) (*Report, error) {
	p, err := ReadPending(dataDir)
	if err != nil {
		return damagedRecord(dataDir, now, err)
	}
	if p == nil {
		return nil, nil
	}
	j := journal{dataDir: dataDir, p: p, ops: ops}
	rep := &Report{Kind: p.Kind, Source: p.Source, BackupCreated: p.BackupCreated, RestoredAt: now().UTC()}
	if p.Rollback != "" {
		// A previous start stopped part way through the swap, or never
		// got the restored server ready: put the originals back.
		applied, failure := p.Applied, p.Failure
		if err := j.undo(); err != nil {
			return rep, fmt.Errorf("a restore could not be undone; the original files are in %s: %w", j.rollback(), err)
		}
		rep.Status = "failed"
		switch {
		case failure != "":
			rep.RolledBack, rep.Error = true, failure
		case applied:
			rep.RolledBack, rep.Error = true, "Ether stopped before the restored backup finished starting"
		default:
			rep.Error = "the restore was interrupted, so the original data was put back"
		}
		return rep, saveReport(dataDir, rep)
	}
	fail := func(msg string) (*Report, error) {
		_ = removeFile(filepath.Join(dataDir, PendingFile))
		rep.Status, rep.Error = "failed", msg
		return rep, saveReport(dataDir, rep)
	}
	if !validStagedName(p.Staged) {
		return fail(ErrNoStaged.Error())
	}
	if err := checkStaged(j.staged()); err != nil {
		return fail(err.Error())
	}
	_, rep.Logical = statOK(filepath.Join(j.staged(), LogicalDumpFile))
	name := "restore-" + rep.RestoredAt.Format("20060102T150405.000Z")
	if err := os.MkdirAll(filepath.Join(dataDir, RollbackDir, name), 0o700); err != nil {
		return fail("could not create the rollback folder: " + err.Error())
	}
	syncDir(filepath.Join(dataDir, RollbackDir))
	p.Rollback = name
	if err := j.save(); err != nil {
		_ = os.Remove(j.rollback())
		return fail("could not record the restore: " + err.Error())
	}
	if swapErr := j.swap(); swapErr != nil {
		if err := j.undo(); err != nil {
			return rep, fmt.Errorf("restore failed (%v) and the original files could not be put back; they are in %s: %w", swapErr, j.rollback(), err)
		}
		rep.Status, rep.Error = "failed", swapErr.Error()
		return rep, saveReport(dataDir, rep)
	}
	p.Applied = true
	if err := j.save(); err != nil {
		if uerr := j.undo(); uerr != nil {
			return rep, fmt.Errorf("could not record the restore (%v) and the original files could not be put back; they are in %s: %w", err, j.rollback(), uerr)
		}
		rep.Status, rep.Error = "failed", "could not record the restore: "+err.Error()
		return rep, saveReport(dataDir, rep)
	}
	// The previous restore's report no longer describes this server.
	_ = removeFile(filepath.Join(dataDir, ReportFile))
	rep.Status, rep.Rollback, rep.RollbackPath, rep.Files = "applied", name, j.rollback(), slices.Clone(p.Moved)
	return rep, nil
}

// ConfirmRestore records that the server started on the applied restore:
// it saves the "restored" report (with the integrity check and import
// result filled in by the caller) and keeps the rollback folder for undo.
func ConfirmRestore(dataDir string, rep *Report) error {
	p, err := ReadPending(dataDir)
	if err != nil {
		return err
	}
	if p == nil || !p.Applied {
		return errors.New("backup: no applied restore to confirm")
	}
	j := journal{dataDir: dataDir, p: p, ops: osOps}
	_ = writeJSON(filepath.Join(j.rollback(), rollbackInfo), Rollback{
		Name: p.Rollback, Created: rep.RestoredAt, ReplacedBy: p.Source, Files: p.Moved,
	})
	rep.Status = "restored"
	if err := saveReport(dataDir, rep); err != nil {
		return err
	}
	if err := removeFile(filepath.Join(dataDir, PendingFile)); err != nil {
		return err
	}
	_ = os.RemoveAll(j.staged())
	pruneRollbacks(dataDir, p.Rollback)
	return nil
}

// RollbackApplied puts the original files back after the server could not
// start on an applied restore (reason says why), and saves the report. The
// settings and database must be closed.
func RollbackApplied(dataDir string, now func() time.Time, reason string) (*Report, error) {
	p, err := ReadPending(dataDir)
	if err != nil {
		return nil, err
	}
	if p == nil || !p.Applied {
		return nil, errors.New("backup: no applied restore to roll back")
	}
	p.Failure = "the restored backup could not start: " + reason
	j := journal{dataDir: dataDir, p: p, ops: osOps}
	if err := j.save(); err != nil {
		return nil, err
	}
	rep := &Report{Kind: p.Kind, Source: p.Source, BackupCreated: p.BackupCreated, RestoredAt: now().UTC(),
		Status: "failed", RolledBack: true, Error: p.Failure}
	if err := j.undo(); err != nil {
		return rep, fmt.Errorf("the restored backup could not start and the original files could not be put back; they are in %s: %w", j.rollback(), err)
	}
	return rep, saveReport(dataDir, rep)
}

// damagedRecord handles a restore record that cannot be read. Nothing has
// moved when no unfinished rollback folder exists, so the record is
// dropped; otherwise the server must not start on files it cannot account
// for, and the error names the folder holding the originals.
func damagedRecord(dataDir string, now func() time.Time, cause error) (*Report, error) {
	if dir := unfinishedRollback(dataDir); dir != "" {
		return nil, fmt.Errorf("the restore record %s is damaged (%v) while a restore was being applied; Ether will not start until it is checked. The original files are in %s", PendingFile, cause, dir)
	}
	if err := removeFile(filepath.Join(dataDir, PendingFile)); err != nil {
		return nil, err
	}
	rep := &Report{Status: "failed", RestoredAt: now().UTC(), Error: "the restore request could not be read, so nothing was changed"}
	return rep, saveReport(dataDir, rep)
}

// unfinishedRollback is the newest rollback folder of a restore that never
// finished (one without rollback.json that still holds files), or "".
func unfinishedRollback(dataDir string) string {
	entries, err := os.ReadDir(filepath.Join(dataDir, RollbackDir))
	if err != nil {
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		dir := filepath.Join(dataDir, RollbackDir, e.Name())
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "restore-") {
			continue
		}
		if _, ok := statOK(filepath.Join(dir, rollbackInfo)); ok {
			continue
		}
		if files, _ := os.ReadDir(dir); len(files) > 0 {
			return dir
		}
	}
	return ""
}

// journal moves files for one restore, recording each move in the
// pending marker before and after it happens.
type journal struct {
	dataDir string
	p       *Pending
	ops     fsOps
}

func (j *journal) staged() string { return filepath.Join(j.dataDir, filepath.FromSlash(j.p.Staged)) }
func (j *journal) rollback() string {
	return filepath.Join(j.dataDir, RollbackDir, j.p.Rollback)
}
func (j *journal) save() error { return writeJSON(filepath.Join(j.dataDir, PendingFile), j.p) }

// move renames from to to as step, journalled on both sides.
func (j *journal) move(step, from, to string, done func()) error {
	j.p.Step = step
	if err := j.save(); err != nil {
		return err
	}
	if err := j.ops.rename(from, to); err != nil {
		return err
	}
	syncDir(filepath.Dir(from))
	syncDir(filepath.Dir(to))
	done()
	j.p.Step = ""
	return j.save()
}

// swap moves the current files into the rollback folder, then the staged
// ones into the data directory.
func (j *journal) swap() error {
	for _, f := range liveFiles {
		if _, ok := statOK(filepath.Join(j.dataDir, f)); !ok {
			continue
		}
		if err := j.move("aside:"+f, filepath.Join(j.dataDir, f), filepath.Join(j.rollback(), f), func() {
			j.p.Moved = append(j.p.Moved, f)
		}); err != nil {
			return fmt.Errorf("move %s aside: %w", f, err)
		}
	}
	for _, f := range stagedFiles {
		if _, ok := statOK(filepath.Join(j.staged(), f)); !ok {
			continue
		}
		if err := j.move("install:"+f, filepath.Join(j.staged(), f), filepath.Join(j.dataDir, f), func() {
			j.p.Installed = append(j.p.Installed, f)
		}); err != nil {
			return fmt.Errorf("install %s: %w", f, err)
		}
	}
	return nil
}

// settle resolves a move a crash interrupted, from where the file is now.
func (j *journal) settle() {
	op, f, ok := strings.Cut(j.p.Step, ":")
	j.p.Step = ""
	if !ok || !slices.Contains(liveFiles, f) {
		return
	}
	_, inLive := statOK(filepath.Join(j.dataDir, f))
	_, inRollback := statOK(filepath.Join(j.rollback(), f))
	_, inStaged := statOK(filepath.Join(j.staged(), f))
	switch op {
	case "aside": // data dir → rollback
		if inRollback && !inLive && !slices.Contains(j.p.Moved, f) {
			j.p.Moved = append(j.p.Moved, f)
		}
	case "install": // staging → data dir
		if inLive && !inStaged && !slices.Contains(j.p.Installed, f) {
			j.p.Installed = append(j.p.Installed, f)
		}
	case "return": // data dir → staging
		if inStaged && !inLive {
			j.p.Installed = slices.DeleteFunc(j.p.Installed, func(s string) bool { return s == f })
		}
	case "restore": // rollback → data dir
		if inLive && !inRollback {
			j.p.Moved = slices.DeleteFunc(j.p.Moved, func(s string) bool { return s == f })
		}
	}
}

// undo returns every installed file to the staging folder, then every
// original to the data directory, journalling each move so a retry after a
// partial undo only moves what is left. On success the marker is removed.
func (j *journal) undo() error {
	j.settle()
	if j.p.Applied {
		// Every original had moved aside before anything was installed, so
		// any other live file was made by the restored server (its WAL and
		// shared memory, or a database created to import a dump). It goes
		// with the restored files: a foreign WAL must never sit next to
		// the original database.
		for _, f := range liveFiles {
			if _, ok := statOK(filepath.Join(j.dataDir, f)); ok && !slices.Contains(j.p.Installed, f) {
				j.p.Installed = append(j.p.Installed, f)
			}
		}
	}
	if err := j.save(); err != nil {
		return err
	}
	if len(j.p.Installed) > 0 {
		if err := os.MkdirAll(j.staged(), 0o700); err != nil {
			return err
		}
	}
	for len(j.p.Installed) > 0 {
		f := j.p.Installed[len(j.p.Installed)-1]
		if _, ok := statOK(filepath.Join(j.dataDir, f)); !ok {
			// Consumed by the restored server (an imported dump is renamed).
			j.p.Installed = j.p.Installed[:len(j.p.Installed)-1]
			if err := j.save(); err != nil {
				return err
			}
			continue
		}
		if err := j.move("return:"+f, filepath.Join(j.dataDir, f), filepath.Join(j.staged(), f), func() {
			j.p.Installed = j.p.Installed[:len(j.p.Installed)-1]
		}); err != nil {
			return fmt.Errorf("return %s: %w", f, err)
		}
	}
	for len(j.p.Moved) > 0 {
		f := j.p.Moved[0]
		if err := j.move("restore:"+f, filepath.Join(j.rollback(), f), filepath.Join(j.dataDir, f), func() {
			j.p.Moved = j.p.Moved[1:]
		}); err != nil {
			return fmt.Errorf("put back %s: %w", f, err)
		}
	}
	_ = os.Remove(j.rollback())
	return removeFile(filepath.Join(j.dataDir, PendingFile))
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
	return removeFile(filepath.Join(dataDir, ReportFile))
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

// writeJSON writes v to path atomically and durably, owner-only: the
// temporary file is fsynced before the rename and the directory after it.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// removeFile removes path if it exists and makes the removal durable.
func removeFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir makes renames and removals in dir durable. Windows cannot open a
// directory for syncing; NTFS journals its metadata itself.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
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

// WriteJSONDurable writes v as JSON to path atomically and durably,
// owner-only (the same write the restore journal uses).
func WriteJSONDurable(path string, v any) error { return writeJSON(path, v) }
