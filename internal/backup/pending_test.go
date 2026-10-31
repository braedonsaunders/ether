package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

func fixedNow() time.Time { return time.Date(2026, 10, 31, 10, 15, 0, 0, time.UTC) }

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// restoreFixture is a data directory holding "live" files and a staging
// folder holding "staged" ones, with a pending restore requested.
func restoreFixture(t *testing.T) (dataDir, staged string) {
	t.Helper()
	dataDir = t.TempDir()
	writeFiles(t, dataDir, map[string]string{
		"config.json": "live-config", config.CredentialKeyFile: "live-key",
		"ether.db": "live-db", "ether.db-wal": "live-wal",
	})
	staged = filepath.Join(dataDir, "restored-1")
	writeFiles(t, staged, map[string]string{
		"config.json": "staged-config", config.CredentialKeyFile: "staged-key",
		"ether.db": "staged-db", "manifest.json": "{}",
	})
	if err := RequestRestore(dataDir, Pending{Staged: "restored-1", Kind: "snapshot", Source: "ether-x.tar.gz"}); err != nil {
		t.Fatal(err)
	}
	return dataDir, staged
}

func assertLive(t *testing.T, dataDir, prefix string) {
	t.Helper()
	for _, name := range []string{"config.json", config.CredentialKeyFile, "ether.db"} {
		want := prefix + "-" + map[string]string{"config.json": "config", config.CredentialKeyFile: "key", "ether.db": "db"}[name]
		if got := readFile(t, filepath.Join(dataDir, name)); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
}

// applyAndConfirm applies the pending restore and confirms it, as a start
// that reaches ready does.
func applyAndConfirm(t *testing.T, dataDir string, now func() time.Time) *Report {
	t.Helper()
	rep, err := ApplyPending(dataDir, now)
	if err != nil || rep == nil || rep.Status != "applied" {
		t.Fatalf("ApplyPending = %+v, %v", rep, err)
	}
	if err := ConfirmRestore(dataDir, rep); err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestApplyPendingSwapsAndKeepsRollback(t *testing.T) {
	dataDir, staged := restoreFixture(t)
	rep, err := ApplyPending(dataDir, fixedNow)
	if err != nil || rep == nil || rep.Status != "applied" {
		t.Fatalf("ApplyPending = %+v, %v", rep, err)
	}
	assertLive(t, dataDir, "staged")
	// Applied is not restored: no report until the server is ready.
	if r, _ := ReadReport(dataDir); r != nil {
		t.Fatalf("report saved before the restored server started: %+v", r)
	}
	// The old WAL belongs to the old database: it must not sit next to the
	// restored one.
	if _, err := os.Stat(filepath.Join(dataDir, "ether.db-wal")); err == nil {
		t.Fatal("the replaced database's WAL was left live")
	}
	rollback := filepath.Join(dataDir, RollbackDir, rep.Rollback)
	if rep.RollbackPath != rollback {
		t.Fatalf("rollback path = %q, want %q", rep.RollbackPath, rollback)
	}
	for name, want := range map[string]string{"config.json": "live-config", "ether.db": "live-db", "ether.db-wal": "live-wal", config.CredentialKeyFile: "live-key"} {
		if got := readFile(t, filepath.Join(rollback, name)); got != want {
			t.Fatalf("rollback %s = %q, want %q", name, got, want)
		}
	}
	if err := ConfirmRestore(dataDir, rep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("staging folder left behind")
	}
	if p, _ := ReadPending(dataDir); p != nil {
		t.Fatal("pending restore still recorded")
	}
	if _, err := os.Stat(filepath.Join(rollback, rollbackInfo)); err != nil {
		t.Fatal("confirmed rollback folder has no rollback.json")
	}
	saved, err := ReadReport(dataDir)
	if err != nil || saved == nil || saved.Status != "restored" || saved.Source != "ether-x.tar.gz" || len(saved.Files) != 4 {
		t.Fatalf("saved report = %+v, %v", saved, err)
	}
	// Nothing pending: a second start does nothing.
	if rep, err := ApplyPending(dataDir, fixedNow); rep != nil || err != nil {
		t.Fatalf("second ApplyPending = %+v, %v", rep, err)
	}
}

// failingOps fails the nth rename (1-based).
func failingOps(n int) fsOps {
	calls := 0
	return fsOps{rename: func(a, b string) error {
		calls++
		if calls == n {
			return errors.New("disk on fire")
		}
		return os.Rename(a, b)
	}}
}

func TestApplyPendingRollsBackAFailureMidSwap(t *testing.T) {
	// Renames 1-4 move the live files aside, 5-7 install the staged ones:
	// fail while moving aside and while installing.
	for _, n := range []int{2, 4, 5, 6, 7} {
		t.Run("rename"+string(rune('0'+n)), func(t *testing.T) {
			dataDir, staged := restoreFixture(t)
			rep, err := applyPending(dataDir, fixedNow, failingOps(n))
			if err != nil {
				t.Fatalf("applyPending: %v", err)
			}
			if rep.Status != "failed" || rep.RolledBack || !strings.Contains(rep.Error, "disk on fire") {
				t.Fatalf("report = %+v", rep)
			}
			assertLive(t, dataDir, "live")
			if got := readFile(t, filepath.Join(dataDir, "ether.db-wal")); got != "live-wal" {
				t.Fatalf("wal = %q", got)
			}
			if got := readFile(t, filepath.Join(staged, "ether.db")); got != "staged-db" {
				t.Fatalf("staged database not returned to the staging folder: %q", got)
			}
			if p, _ := ReadPending(dataDir); p != nil {
				t.Fatal("a failed restore stays pending (it would retry on every start)")
			}
			if entries, _ := os.ReadDir(filepath.Join(dataDir, RollbackDir)); len(entries) != 0 {
				t.Fatalf("empty rollback folder left: %v", entries)
			}
		})
	}
}

// Finding 1: installing ether.db fails, then putting the original ether.db
// back fails too. The retry at the next start must move only what is left:
// the original settings and key, already back in place, must stay live and
// never land in the staging folder (which is deleted after a day).
func TestRetriedUndoAfterAPartialUndoKeepsTheOriginals(t *testing.T) {
	dataDir, staged := restoreFixture(t)
	liveDB := filepath.Join(dataDir, "ether.db")
	broken := fsOps{rename: func(a, b string) error {
		if b == liveDB {
			return errors.New("disk on fire")
		}
		return os.Rename(a, b)
	}}
	if _, err := applyPending(dataDir, fixedNow, broken); err == nil {
		t.Fatal("applyPending succeeded although the original database could not be put back")
	}
	// The next start (or Serve's retry) finishes the undo.
	rep, err := ApplyPending(dataDir, fixedNow)
	if err != nil || rep == nil || rep.Status != "failed" {
		t.Fatalf("retry = %+v, %v", rep, err)
	}
	assertLive(t, dataDir, "live")
	for _, f := range []string{"config.json", config.CredentialKeyFile} {
		if got := readFile(t, filepath.Join(staged, f)); !strings.HasPrefix(got, "staged-") {
			t.Fatalf("staging %s = %q: an original was moved into the staging folder", f, got)
		}
	}
	CleanStaged(dataDir, -time.Second) // what the next day does
	assertLive(t, dataDir, "live")
}

// crashOps simulates a power loss at the nth rename: before it happens, or
// right after it (before the journal records it).
type crash struct{}

func crashOps(n int, after bool) fsOps {
	calls := 0
	return fsOps{rename: func(a, b string) error {
		calls++
		if calls == n {
			if after {
				_ = os.Rename(a, b)
			}
			panic(crash{})
		}
		return os.Rename(a, b)
	}}
}

func applyUntilCrash(dataDir string, ops fsOps) (crashed bool) {
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(crash); !ok {
				panic(r)
			}
			crashed = true
		}
	}()
	_, _ = applyPending(dataDir, fixedNow, ops)
	return false
}

// A crash at any move, before or after the rename, is undone exactly at the
// next start.
func TestApplyPendingRecoversACrashAtEveryMove(t *testing.T) {
	for n := 1; n <= 7; n++ {
		for _, after := range []bool{false, true} {
			dataDir, staged := restoreFixture(t)
			if !applyUntilCrash(dataDir, crashOps(n, after)) {
				t.Fatalf("rename %d: no crash", n)
			}
			rep, err := ApplyPending(dataDir, fixedNow)
			if err != nil || rep.Status != "failed" || !strings.Contains(rep.Error, "interrupted") {
				t.Fatalf("rename %d after=%v: ApplyPending = %+v, %v", n, after, rep, err)
			}
			assertLive(t, dataDir, "live")
			if got := readFile(t, filepath.Join(dataDir, "ether.db-wal")); got != "live-wal" {
				t.Fatalf("rename %d after=%v: wal = %q", n, after, got)
			}
			for _, f := range []string{"config.json", config.CredentialKeyFile, "ether.db"} {
				if got := readFile(t, filepath.Join(staged, f)); !strings.HasPrefix(got, "staged-") {
					t.Fatalf("rename %d after=%v: staged %s = %q", n, after, f, got)
				}
			}
		}
	}
}

// Finding 2: a damaged record while files were half moved must never be
// dropped (the originals would be orphaned): the start fails closed.
func TestDamagedRecordMidSwapFailsClosed(t *testing.T) {
	dataDir, _ := restoreFixture(t)
	if !applyUntilCrash(dataDir, crashOps(3, true)) {
		t.Fatal("no crash")
	}
	if err := os.WriteFile(filepath.Join(dataDir, PendingFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := ApplyPending(dataDir, fixedNow)
	if err == nil || rep != nil || !strings.Contains(err.Error(), RollbackDir) {
		t.Fatalf("ApplyPending = %+v, %v; want a refusal naming the rollback folder", rep, err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, PendingFile)); err != nil {
		t.Fatal("the damaged record was dropped")
	}
	if err := RequestRestore(dataDir, Pending{Staged: "restored-1"}); !errors.Is(err, ErrRestoreInProgress) {
		t.Fatalf("RequestRestore over a damaged record = %v", err)
	}
	// Nothing moved yet: the damaged record can safely go.
	clean, _ := restoreFixture(t)
	if err := os.WriteFile(filepath.Join(clean, PendingFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err = ApplyPending(clean, fixedNow)
	if err != nil || rep.Status != "failed" {
		t.Fatalf("damaged record before the swap = %+v, %v", rep, err)
	}
	assertLive(t, clean, "live")
}

func TestWriteJSONLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(dir, "x.json"), map[string]int{"a": 1}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}
	if st, _ := os.Stat(filepath.Join(dir, "x.json")); st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode = %v", st.Mode())
	}
}

// Finding 6: a new request must not overwrite the record of a restore
// being applied.
func TestRequestRestoreRefusesWhileARestoreIsApplied(t *testing.T) {
	dataDir, _ := restoreFixture(t)
	if _, err := ApplyPending(dataDir, fixedNow); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, filepath.Join(dataDir, "restored-2"), map[string]string{"config.json": "c", "ether.db": "d"})
	if err := RequestRestore(dataDir, Pending{Staged: "restored-2"}); !errors.Is(err, ErrRestoreInProgress) {
		t.Fatalf("RequestRestore = %v", err)
	}
	if err := CancelPending(dataDir); !errors.Is(err, ErrRestoreInProgress) {
		t.Fatalf("CancelPending = %v", err)
	}
	if p, _ := ReadPending(dataDir); p == nil || !p.Applied || p.Staged != "restored-1" {
		t.Fatalf("record overwritten: %+v", p)
	}
}

// Finding 7: a restored server that cannot start puts the originals back,
// at once (RollbackApplied) or at the next start after a crash.
func TestAppliedRestoreThatCannotStartRollsBack(t *testing.T) {
	dataDir, staged := restoreFixture(t)
	if _, err := ApplyPending(dataDir, fixedNow); err != nil {
		t.Fatal(err)
	}
	rep, err := RollbackApplied(dataDir, fixedNow, "migration catalog/9: no such column")
	if err != nil || rep.Status != "failed" || !rep.RolledBack || !strings.Contains(rep.Error, "no such column") {
		t.Fatalf("RollbackApplied = %+v, %v", rep, err)
	}
	assertLive(t, dataDir, "live")
	if got := readFile(t, filepath.Join(staged, "ether.db")); got != "staged-db" {
		t.Fatalf("staged database = %q", got)
	}
	if saved, _ := ReadReport(dataDir); saved == nil || !saved.RolledBack {
		t.Fatalf("saved report = %+v", saved)
	}
	if p, _ := ReadPending(dataDir); p != nil {
		t.Fatal("record left after the rollback")
	}

	// The restored server made its own WAL and shared memory before it
	// failed: they leave with the restored files, never sitting next to
	// the original database (which has a WAL of its own, but no -shm).
	dataDir, staged = restoreFixture(t)
	if _, err := ApplyPending(dataDir, fixedNow); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dataDir, map[string]string{"ether.db-wal": "restored-wal", "ether.db-shm": "restored-shm"})
	if _, err := RollbackApplied(dataDir, fixedNow, "boom"); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(dataDir, "ether.db-wal")); got != "live-wal" {
		t.Fatalf("wal after rollback = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "ether.db-shm")); err == nil {
		t.Fatal("the restored server's shared memory was left next to the original database")
	}
	if got := readFile(t, filepath.Join(staged, "ether.db-wal")); got != "restored-wal" {
		t.Fatalf("staged wal = %q", got)
	}

	// A crash before the restored server was ready: the next start rolls
	// back by itself.
	dataDir, _ = restoreFixture(t)
	if _, err := ApplyPending(dataDir, fixedNow); err != nil {
		t.Fatal(err)
	}
	rep, err = ApplyPending(dataDir, fixedNow)
	if err != nil || rep.Status != "failed" || !rep.RolledBack {
		t.Fatalf("next start = %+v, %v", rep, err)
	}
	assertLive(t, dataDir, "live")
}

func TestApplyPendingRejectsMissingOrUnsafeStaging(t *testing.T) {
	dataDir := t.TempDir()
	writeFiles(t, dataDir, map[string]string{"config.json": "live-config", "ether.db": "live-db"})
	for _, name := range []string{"../elsewhere", "restored-1/../../x", "config.json", "rollbacks/../restored-1"} {
		if err := RequestRestore(dataDir, Pending{Staged: name}); err == nil {
			t.Fatalf("RequestRestore(%q) accepted", name)
		}
	}
	// A request whose folder vanished before the restart fails cleanly.
	writeFiles(t, filepath.Join(dataDir, "restored-2"), map[string]string{"config.json": "c", "ether.db": "d"})
	if err := RequestRestore(dataDir, Pending{Staged: "restored-2"}); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(dataDir, "restored-2"))
	rep, err := ApplyPending(dataDir, fixedNow)
	if err != nil || rep.Status != "failed" {
		t.Fatalf("ApplyPending = %+v, %v", rep, err)
	}
	if got := readFile(t, filepath.Join(dataDir, "ether.db")); got != "live-db" {
		t.Fatalf("database = %q", got)
	}
}

func TestUndoPutsTheReplacedDataBack(t *testing.T) {
	dataDir, _ := restoreFixture(t)
	// A hot journal next to the replaced database is part of it.
	writeFiles(t, dataDir, map[string]string{"ether.db-journal": "live-journal"})
	applyAndConfirm(t, dataDir, fixedNow)
	if err := RequestUndo(dataDir); err != nil {
		t.Fatal(err)
	}
	later := func() time.Time { return fixedNow().Add(time.Hour) }
	rep := applyAndConfirm(t, dataDir, later)
	if rep.Kind != "undo" {
		t.Fatalf("undo = %+v", rep)
	}
	assertLive(t, dataDir, "live")
	for f, want := range map[string]string{"ether.db-wal": "live-wal", "ether.db-journal": "live-journal"} {
		if got := readFile(t, filepath.Join(dataDir, f)); got != want {
			t.Fatalf("%s = %q", f, got)
		}
	}
	// The undone restore's data is itself kept for an undo.
	if got := readFile(t, filepath.Join(rep.RollbackPath, "ether.db")); got != "staged-db" {
		t.Fatalf("undo rollback database = %q", got)
	}
}

func TestReportUpdateAndDismiss(t *testing.T) {
	dataDir, _ := restoreFixture(t)
	applyAndConfirm(t, dataDir, fixedNow)
	if err := UpdateReport(dataDir, func(r *Report) { r.Imported = true }); err != nil {
		t.Fatal(err)
	}
	if r, _ := ReadReport(dataDir); r == nil || !r.Imported {
		t.Fatalf("report = %+v", r)
	}
	if err := DismissReport(dataDir); err != nil {
		t.Fatal(err)
	}
	if r, _ := ReadReport(dataDir); r != nil {
		t.Fatal("report still present after dismiss")
	}
	if err := RequestUndo(dataDir); err == nil {
		t.Fatal("undo without a report accepted")
	}
	// The rollback folder outlives the report.
	if entries, _ := os.ReadDir(filepath.Join(dataDir, RollbackDir)); len(entries) != 1 {
		t.Fatalf("rollbacks = %v", entries)
	}
}

func TestPruneRollbacksKeepsTheNewest(t *testing.T) {
	dataDir := t.TempDir()
	for _, n := range []string{"restore-1", "restore-2", "restore-3", "restore-4", "restore-5"} {
		writeFiles(t, filepath.Join(dataDir, RollbackDir, n), map[string]string{"ether.db": n})
	}
	pruneRollbacks(dataDir, "restore-1")
	entries, _ := os.ReadDir(filepath.Join(dataDir, RollbackDir))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "restore-1,restore-3,restore-4,restore-5" {
		t.Fatalf("kept %v", names)
	}
}
