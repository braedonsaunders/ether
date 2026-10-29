package transcode

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

func downloadInfo() core.MediaInfo {
	return core.MediaInfo{
		Container:   "mkv",
		Duration:    100 * time.Second,
		Size:        1000,
		BitrateKbps: 8000,
		Streams: []core.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2},
			{Index: 2, Type: "subtitle", Codec: "hdmv_pgs_subtitle"},
		},
	}
}

// downloadRunner handles the remux Run: it writes the output file (the
// last argument) and emits one progress block, like ffmpeg would.
func downloadRunner(payload string) func(context.Context, Command) error {
	return func(ctx context.Context, cmd Command) error {
		if !hasArg(cmd.Args, "-movflags") {
			return errUnhandled
		}
		out := cmd.Args[len(cmd.Args)-1]
		if cmd.Stdout != nil {
			_, _ = io.WriteString(cmd.Stdout, "frame=10\nout_time_us=50000000\nprogress=continue\n")
		}
		return os.WriteFile(out, []byte(payload), 0o644)
	}
}

func newDownloadRemuxer(t *testing.T, fr *fakeRunner, cfg config.Downloads) *DownloadRemuxer {
	t.Helper()
	m := newTestManager(t, fr, config.Transcode{})
	d, err := m.NewDownloadRemuxer(cfg)
	if err != nil {
		t.Fatalf("NewDownloadRemuxer: %v", err)
	}
	return d
}

func TestRemuxDownloadCopiesToFaststartMP4(t *testing.T) {
	fr := newFakeRunner()
	fr.runFunc = downloadRunner("fake-mp4-bytes")
	d := newDownloadRemuxer(t, fr, config.Downloads{})

	var progress []float64
	path, size, err := d.RemuxDownload(context.Background(), core.DownloadRemuxRequest{
		InputURL: "http://127.0.0.1/source", Info: downloadInfo(), AudioStream: -1,
	}, func(f float64) { progress = append(progress, f) })
	if err != nil {
		t.Fatalf("RemuxDownload: %v", err)
	}
	if size != int64(len("fake-mp4-bytes")) || !strings.HasSuffix(path, ".mp4") {
		t.Fatalf("path=%s size=%d", path, size)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("output missing: %v", err)
	}
	if len(progress) == 0 || progress[len(progress)-1] != 1 {
		t.Fatalf("progress = %v, want ending at 1", progress)
	}
	if progress[0] != 0.5 {
		t.Fatalf("first progress = %v, want 0.5", progress[0])
	}
	// The ffmpeg invocation must be a stream copy with faststart.
	fr.mu.Lock()
	var args []string
	for _, c := range fr.runCalls {
		if hasArg(c, "-movflags") {
			args = c
		}
	}
	fr.mu.Unlock()
	joined := strings.Join(args, " ")
	for _, want := range []string{"-c copy", "-movflags +faststart", "-f mp4", "-map 0:0", "-map 0:1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "0:2") {
		t.Fatalf("args %q must not map the PGS subtitle", joined)
	}
	// Budget reflects the finished file, and release frees it and
	// deletes the temp file.
	if got := d.UsedBytes(); got != size {
		t.Fatalf("UsedBytes = %d, want %d", got, size)
	}
	d.ReleaseDownload(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("released file still exists: %v", err)
	}
	if got := d.UsedBytes(); got != 0 {
		t.Fatalf("UsedBytes after release = %d", got)
	}
	d.ReleaseDownload(path) // unknown path: no-op, must not panic
}

func TestRemuxDownloadHEVCUsesHVC1Tag(t *testing.T) {
	fr := newFakeRunner()
	var got []string
	fr.runFunc = func(ctx context.Context, cmd Command) error {
		if !hasArg(cmd.Args, "-movflags") {
			return errUnhandled
		}
		got = cmd.Args
		return os.WriteFile(cmd.Args[len(cmd.Args)-1], []byte("x"), 0o644)
	}
	d := newDownloadRemuxer(t, fr, config.Downloads{})
	info := downloadInfo()
	info.Streams[0].Codec = "hevc"
	path, _, err := d.RemuxDownload(context.Background(), core.DownloadRemuxRequest{
		InputURL: "http://x", Info: info, AudioStream: -1,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.ReleaseDownload(path)
	if !strings.Contains(strings.Join(got, " "), "-tag:v hvc1") {
		t.Fatalf("args %v missing hvc1 tag", got)
	}
}

func TestRemuxDownloadRejectsNonCopyableCodecs(t *testing.T) {
	fr := newFakeRunner()
	fr.runFunc = downloadRunner("x")
	d := newDownloadRemuxer(t, fr, config.Downloads{})
	for name, mutate := range map[string]func(*core.MediaInfo){
		"av1 video":    func(i *core.MediaInfo) { i.Streams[0].Codec = "av1" },
		"truehd audio": func(i *core.MediaInfo) { i.Streams[1].Codec = "truehd" },
		"no video":     func(i *core.MediaInfo) { i.Streams = i.Streams[1:] },
	} {
		info := downloadInfo()
		mutate(&info)
		_, _, err := d.RemuxDownload(context.Background(), core.DownloadRemuxRequest{
			InputURL: "http://x", Info: info, AudioStream: -1,
		}, nil)
		if !errors.Is(err, core.ErrDownloadNeedsTranscode) {
			t.Fatalf("%s: err = %v, want ErrDownloadNeedsTranscode", name, err)
		}
	}
}

func TestRemuxDownloadBudgetAndFailureCleanup(t *testing.T) {
	fr := newFakeRunner()
	d := newDownloadRemuxer(t, fr, config.Downloads{DiskBudgetBytes: 500})
	// Estimated size (1000) exceeds the whole budget.
	_, _, err := d.RemuxDownload(context.Background(), core.DownloadRemuxRequest{
		InputURL: "http://x", Info: downloadInfo(), AudioStream: -1,
	}, nil)
	if !errors.Is(err, core.ErrDownloadBudget) {
		t.Fatalf("err = %v, want ErrDownloadBudget", err)
	}

	// A failing ffmpeg leaves no file and no budget held.
	fr.runFunc = func(ctx context.Context, cmd Command) error {
		if !hasArg(cmd.Args, "-movflags") {
			return errUnhandled
		}
		_ = os.WriteFile(cmd.Args[len(cmd.Args)-1], []byte("partial"), 0o644)
		return errors.New("exit status 1")
	}
	d2 := newDownloadRemuxer(t, fr, config.Downloads{})
	if _, _, err := d2.RemuxDownload(context.Background(), core.DownloadRemuxRequest{
		InputURL: "http://x", Info: downloadInfo(), AudioStream: -1,
	}, nil); err == nil {
		t.Fatal("want error from failing ffmpeg")
	}
	entries, _ := os.ReadDir(d2.dir)
	if len(entries) != 0 {
		t.Fatalf("leftover files after failure: %v", entries)
	}
	if got := d2.UsedBytes(); got != 0 {
		t.Fatalf("UsedBytes after failure = %d", got)
	}
}

func TestRemuxDownloadCancel(t *testing.T) {
	fr := newFakeRunner()
	fr.runFunc = func(ctx context.Context, cmd Command) error {
		if !hasArg(cmd.Args, "-movflags") {
			return errUnhandled
		}
		<-ctx.Done()
		return ctx.Err()
	}
	d := newDownloadRemuxer(t, fr, config.Downloads{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, _, err := d.RemuxDownload(ctx, core.DownloadRemuxRequest{
		InputURL: "http://x", Info: downloadInfo(), AudioStream: -1,
	}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	entries, _ := os.ReadDir(d.dir)
	if len(entries) != 0 {
		t.Fatalf("leftover files after cancel: %v", entries)
	}
}

func TestNewDownloadRemuxerCleansStaleFiles(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	dir := filepath.Join(m.tempDir, "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "dl-stale.mp4")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Sob a política única de limpeza, só some quem passou de
	// staleDownloadAge: o fixture precisa ser velho de verdade (mtime),
	// não apenas ter "stale" no nome.
	old := time.Now().Add(-2 * staleDownloadAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := m.NewDownloadRemuxer(config.Downloads{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale file survived startup cleanup: %v", err)
	}
}

// Startup cleanup in NewDownloadRemuxer follows the single stale-download
// policy (see staleDownloadAge): it must remove only the files the remuxer
// itself creates (dl-*.mp4 and their .part partials), and only once stale,
// preserving everything else sharing downloads/: unrelated files and other
// apps' subdirs are not Ether's to delete. Fixtures ether-owned são
// envelhecidos via Chtimes porque, frescos, a política manda preservá-los
// (ver TestNewDownloadRemuxerKeepsFreshDownloadFiles).
func TestNewDownloadRemuxerPreservesUnrelatedFiles(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	dir := filepath.Join(m.tempDir, "downloads")
	ether := []string{"dl-ab12cd34.mp4", "dl-ab12cd34.mp4.part"}
	alien := []string{
		"keep.txt",
		"dl-notes.txt",                         // dl-* but not a name RemuxDownload creates
		filepath.Join("other-app", "keep.txt"), // unrelated subdir
	}
	for _, rel := range append(append([]string{}, ether...), alien...) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Envelhece os ether-owned (e o dl-notes.txt, para provar que o predicado
	// isRemuxDownloadFile — e não o prefixo dl-* — decide): sem passar de
	// staleDownloadAge nada disso seria removível pela política única.
	old := time.Now().Add(-2 * staleDownloadAge)
	for _, rel := range append(append([]string{}, ether...), "dl-notes.txt") {
		p := filepath.Join(dir, rel)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.NewDownloadRemuxer(config.Downloads{}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range ether {
		if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Errorf("ether file %s survived startup cleanup: %v", rel, err)
		}
	}
	for _, rel := range alien {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("unrelated %s removed by startup cleanup: %v", rel, err)
		}
	}
}

// Teste-chave da política única: um dl-* fresco (mtime agora, dentro de
// staleDownloadAge) pode ser de outro processo ou de um restart recente e
// sobrevive à criação do remuxer no boot — só o velho (> staleDownloadAge)
// é removido, aqui e em cleanStaleDownloads.
func TestNewDownloadRemuxerKeepsFreshDownloadFiles(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	dir := filepath.Join(m.tempDir, "downloads")
	fresh := []string{"dl-ab12cd34.mp4", "dl-ab12cd34.mp4.part"}
	for _, rel := range fresh {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.NewDownloadRemuxer(config.Downloads{}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range fresh {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("fresh download file %s removed by startup cleanup: %v", rel, err)
		}
	}
}
