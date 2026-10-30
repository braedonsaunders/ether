package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

type revisionTestFile struct{ *os.File }

func (f revisionTestFile) Size() int64         { st, _ := f.Stat(); return st.Size() }
func (revisionTestFile) ReleaseID() string     { return "local:test" }
func (f revisionTestFile) LocalFile() *os.File { return f.File }

func TestLocalFileRevisionDetectsTouchReplaceAndMove(t *testing.T) {
	open := func(t *testing.T) (string, *os.File, *PlaySession) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "video.mp4")
		if err := os.WriteFile(path, []byte("bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return path, f, &PlaySession{Key: MovieKey(1), Source: revisionTestFile{f}}
	}
	t.Run("touch", func(t *testing.T) {
		path, _, ps := open(t)
		first := SourceRevision(ps)
		if first == "" || first != SourceRevision(ps) {
			t.Fatalf("unstable revision: %q", first)
		}
		reopened, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		if got := SourceRevision(&PlaySession{Key: ps.Key, Source: revisionTestFile{reopened}}); got != first {
			t.Fatalf("reopened same file changed revision: %q", got)
		}
		future := time.Now().Add(2 * time.Second)
		if err := os.Chtimes(path, future, future); err != nil {
			t.Fatal(err)
		}
		if next := SourceRevision(ps); next == "" || next == first {
			t.Fatalf("touch not detected: %q", next)
		}
	})
	t.Run("replace", func(t *testing.T) {
		path, _, ps := open(t)
		if SourceRevision(ps) == "" {
			t.Fatal("missing original revision")
		}
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("bytes"), 0600); err != nil {
			t.Fatal(err)
		}
		if got := SourceRevision(ps); got != "" {
			t.Fatalf("replacement accepted: %q", got)
		}
	})
	t.Run("move", func(t *testing.T) {
		path, _, ps := open(t)
		if SourceRevision(ps) == "" {
			t.Fatal("missing original revision")
		}
		if err := os.Rename(path, path+".moved"); err != nil {
			t.Fatal(err)
		}
		if got := SourceRevision(ps); got != "" {
			t.Fatalf("moved file accepted: %q", got)
		}
	})
}
