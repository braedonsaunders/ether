package transcode

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
)

func TestRetainedDownloadSurvivesBootAndRemainsBudgeted(t *testing.T) {
	root := t.TempDir()
	m := &Manager{tempDir: root}
	d, err := m.NewDownloadRemuxer(config.Downloads{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "downloads", "dl-aabbcc.mp4")
	if err := os.WriteFile(path, []byte("video"), 0600); err != nil {
		t.Fatal(err)
	}
	d.adoptOrphans()
	if err := d.RetainDownload(path, time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleDownloadAge)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	cleanStaleDownloads(filepath.Dir(path))
	restored, err := m.NewDownloadRemuxer(config.Downloads{})
	if err != nil {
		t.Fatal(err)
	}
	restored.expireStaleAdopted(time.Now())
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("retained output lost: %v", err)
	}
	if got := restored.UsedBytes(); got != 5 {
		t.Fatalf("restored budget: %d", got)
	}
	restored.ReleaseDownload(path)
	if _, err := os.Stat(path + ".retain"); !os.IsNotExist(err) {
		t.Fatalf("retention marker survived delete: %v", err)
	}
	if got := restored.UsedBytes(); got != 0 {
		t.Fatalf("release budget: %d", got)
	}
}
