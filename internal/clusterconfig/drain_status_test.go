package clusterconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDrainDiagnosticDocumentRemainsBoundedAndCompatible(t *testing.T) {
	dir := t.TempDir()
	n := 1
	st := DrainControl{Boot: "boot", Operation: "operation", State: "draining", Pending: 2, PendingRuntime: &n, PendingSessions: &n, PendingCategories: &DrainPendingCategories{RemoteLive: 1}}
	if err := WriteDrainControl(dir, DrainStatusFile, st); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, DrainStatusFile))
	if err != nil || len(raw) > 4096 {
		t.Fatal("diagnostics exceed protected file bound")
	}
	got, err := ReadDrainControl(dir, DrainStatusFile)
	if err != nil || got.Pending != 2 || got.PendingCategories == nil || got.PendingCategories.RemoteLive != 1 {
		t.Fatalf("diagnostics roundtrip: %+v %v", got, err)
	}
	if err := WriteDrainControl(dir, DrainStatusFile, DrainControl{Boot: "boot", Operation: "legacy", State: "draining", Pending: 1}); err != nil {
		t.Fatal(err)
	}
	got, err = ReadDrainControl(dir, DrainStatusFile)
	if err != nil || got.PendingRuntime != nil || got.PendingSessions != nil || got.PendingCategories != nil {
		t.Fatal("legacy absent diagnostics became zero")
	}
	if err := WriteDrainControl(dir, DrainStatusFile, DrainControl{Boot: "boot", State: strings.Repeat("x", 4097)}); err == nil {
		t.Fatal("oversized status was persisted")
	}
	got, err = ReadDrainControl(dir, DrainStatusFile)
	if err != nil || got.Operation != "legacy" {
		t.Fatal("oversized write damaged existing status")
	}
}
