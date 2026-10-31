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

func TestApplyPendingSwapsAndKeepsRollback(t *testing.T) {
	dataDir, staged := restoreFixture(t)
	rep, err := ApplyPending(dataDir, fixedNow)
	if err != nil || rep == nil || rep.Status != "restored" {
		t.Fatalf("ApplyPending = %+v, %v", rep, err)
	}
	assertLive(t, dataDir, "staged")
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
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatal("staging folder left behind")
	}
	if p, _ := ReadPending(dataDir); p != nil {
		t.Fatal("pending restore still recorded")
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
	for _, n := range []int{2, 4, 5, 6} {
		t.Run("rename"+string(rune('0'+n)), func(t *testing.T) {
			dataDir, staged := restoreFixture(t)
			rep, err := applyPending(dataDir, fixedNow, failingOps(n))
			if err != nil {
				t.Fatalf("applyPending: %v", err)
			}
			if rep.Status != "failed" || !strings.Contains(rep.Error, "disk on fire") {
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

func TestApplyPendingRecoversAnInterruptedSwap(t *testing.T) {
	for _, installing := range []bool{false, true} {
		dataDir, staged := restoreFixture(t)
		// Simulate a crash: the swap got part way and the process died.
		p, _ := ReadPending(dataDir)
		p.Rollback, p.Installing = "restore-crashed", installing
		if err := writeJSON(filepath.Join(dataDir, PendingFile), p); err != nil {
			t.Fatal(err)
		}
		rollback := filepath.Join(dataDir, RollbackDir, "restore-crashed")
		writeFiles(t, rollback, nil)
		for _, f := range []string{"config.json", "ether.db", "ether.db-wal"} {
			if err := os.Rename(filepath.Join(dataDir, f), filepath.Join(rollback, f)); err != nil {
				t.Fatal(err)
			}
		}
		if installing {
			for _, f := range []string{config.CredentialKeyFile} {
				if err := os.Rename(filepath.Join(dataDir, f), filepath.Join(rollback, f)); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range []string{"config.json", "ether.db"} {
				if err := os.Rename(filepath.Join(staged, f), filepath.Join(dataDir, f)); err != nil {
					t.Fatal(err)
				}
			}
		}
		rep, err := ApplyPending(dataDir, fixedNow)
		if err != nil || rep.Status != "failed" || !strings.Contains(rep.Error, "interrupted") {
			t.Fatalf("installing=%v: ApplyPending = %+v, %v", installing, rep, err)
		}
		assertLive(t, dataDir, "live")
		if got := readFile(t, filepath.Join(staged, "ether.db")); got != "staged-db" {
			t.Fatalf("installing=%v: staged database = %q", installing, got)
		}
	}
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
	if _, err := ApplyPending(dataDir, fixedNow); err != nil {
		t.Fatal(err)
	}
	if err := RequestUndo(dataDir); err != nil {
		t.Fatal(err)
	}
	later := func() time.Time { return fixedNow().Add(time.Hour) }
	rep, err := ApplyPending(dataDir, later)
	if err != nil || rep.Status != "restored" || rep.Kind != "undo" {
		t.Fatalf("undo = %+v, %v", rep, err)
	}
	assertLive(t, dataDir, "live")
	if got := readFile(t, filepath.Join(dataDir, "ether.db-wal")); got != "live-wal" {
		t.Fatalf("wal = %q", got)
	}
	// The undone restore's data is itself kept for an undo.
	if got := readFile(t, filepath.Join(rep.RollbackPath, "ether.db")); got != "staged-db" {
		t.Fatalf("undo rollback database = %q", got)
	}
}

func TestReportUpdateAndDismiss(t *testing.T) {
	dataDir, _ := restoreFixture(t)
	if _, err := ApplyPending(dataDir, fixedNow); err != nil {
		t.Fatal(err)
	}
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
