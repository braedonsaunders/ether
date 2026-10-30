package transcode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Saved configs that only set abrLadder:true must keep soft rungs under a
// video-copy primary after upgrade (abrLadderUnderCopy defaults on).
func TestAbrLadderUnderCopyPreservedWhenKeyAbsent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"transcode":{"abrLadder":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := config.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	if !cfg.Transcode.ABRLadder || !cfg.Transcode.ABRLadderUnderCopy {
		t.Fatalf("upgrade defaults: %+v", cfg.Transcode)
	}

	info := ladderInfo(2160, 40000)
	copyPrimary := core.Decision{
		Kind: core.DecisionRemux, VideoCopy: true, AudioCopy: true, AudioStream: 1, Container: ContainerMPEGTS,
	}
	rungs := buildRungs(info, copyPrimary, adaptiveFromConfig(cfg.Transcode))
	if len(rungs) != 5 {
		t.Fatalf("legacy copy ladder after upgrade = %d rungs (%+v), want 5", len(rungs), rungs)
	}

	off := cfg.Transcode
	off.ABRLadderUnderCopy = false
	if got := buildRungs(info, copyPrimary, adaptiveFromConfig(off)); len(got) != 1 {
		t.Fatalf("explicit under-copy off = %d rungs, want 1", len(got))
	}
}
