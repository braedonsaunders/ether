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
	info := downloadInfo()
	info.Streams = info.Streams[1:]
	_, _, err := d.RemuxDownload(context.Background(), core.DownloadRemuxRequest{
		InputURL: "http://x", Info: info, AudioStream: -1,
	}, nil)
	if !errors.Is(err, core.ErrDownloadNeedsTranscode) {
		t.Fatalf("no video: err = %v, want ErrDownloadNeedsTranscode", err)
	}
}

func TestRemuxDownloadConvertsIncompatibleCodecs(t *testing.T) {
	fr := newFakeRunner()
	fr.runFunc = downloadRunner("converted-mp4")
	d := newDownloadRemuxer(t, fr, config.Downloads{})
	for name, mutate := range map[string]func(*core.MediaInfo){
		"av1 video":    func(i *core.MediaInfo) { i.Streams[0].Codec = "av1" },
		"truehd audio": func(i *core.MediaInfo) { i.Streams[1].Codec = "truehd" },
	} {
		info := downloadInfo()
		mutate(&info)
		path, size, err := d.RemuxDownload(context.Background(), core.DownloadRemuxRequest{
			InputURL: "http://x", Info: info, AudioStream: -1, ViewerID: 7,
		}, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if size != int64(len("converted-mp4")) {
			t.Fatalf("%s: size = %d", name, size)
		}
		d.ReleaseDownload(path)
	}
	fr.mu.Lock()
	var convert []string
	for _, c := range fr.runCalls {
		if hasArg(c, "-movflags") && !strings.Contains(strings.Join(c, " "), "-c copy") {
			convert = c
			break
		}
	}
	fr.mu.Unlock()
	if len(convert) == 0 {
		t.Fatal("convert did not encode")
	}
	joined := strings.Join(convert, " ")
	if !strings.Contains(joined, "-f mp4") || !strings.Contains(joined, "-movflags +faststart") {
		t.Fatalf("convert args %q missing mp4 faststart", joined)
	}
}

func TestRemuxDownloadConvertOneEncodePerViewer(t *testing.T) {
	fr := newFakeRunner()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	fr.runFunc = func(ctx context.Context, cmd Command) error {
		if !hasArg(cmd.Args, "-movflags") {
			return errUnhandled
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return os.WriteFile(cmd.Args[len(cmd.Args)-1], []byte("x"), 0o644)
	}
	d := newDownloadRemuxer(t, fr, config.Downloads{MaxConcurrent: 2})
	info := downloadInfo()
	info.Streams[0].Codec = "av1"
	req := core.DownloadRemuxRequest{InputURL: "http://x", Info: info, AudioStream: -1, ViewerID: 3}
	errc := make(chan error, 2)
	go func() {
		path, _, err := d.RemuxDownload(context.Background(), req, nil)
		if err == nil {
			d.ReleaseDownload(path)
		}
		errc <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first convert did not start")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	go func() {
		_, _, err := d.RemuxDownload(ctx, req, nil)
		errc <- err
	}()
	select {
	case <-started:
		t.Fatal("second convert started while the first still held the viewer encode")
	case err := <-errc:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("second convert err = %v, want deadline", err)
		}
	}
	close(release)
	if err := <-errc; err != nil {
		t.Fatalf("first convert: %v", err)
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
	// não apenas ter "stale" no nome — fresco, ele seria adotado.
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
// apps' subdirs are not Ether's to delete.
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
	old := time.Now().Add(-2 * staleDownloadAge)
	for _, rel := range append(append([]string{}, ether...), "dl-notes.txt") {
		p := filepath.Join(dir, rel)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	d, err := m.NewDownloadRemuxer(config.Downloads{})
	if err != nil {
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
	if got := d.UsedBytes(); got != 0 {
		t.Errorf("UsedBytes = %d, want 0 (nothing retained, nothing adopted)", got)
	}
}

// Teste do dono: um dl-* fresco retido pela política (16B, budget de 8B)
// NÃO pode ficar invisível — NewDownloadRemuxer o ADOTA na contabilidade
// (UsedBytes conta os 16B, não 0), o arquivo sobrevive no disco, e a
// admissão enxerga esses bytes (um remux novo estoura o budget).
func TestNewDownloadRemuxerAdoptsFreshOrphan(t *testing.T) {
	fr := newFakeRunner()
	m := newTestManager(t, fr, config.Transcode{})
	dir := filepath.Join(m.tempDir, "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(dir, "dl-ab12cd34.mp4")
	payload := []byte("0123456789abcdef") // exactly 16B
	if err := os.WriteFile(fresh, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	part := fresh + ".part"
	if err := os.WriteFile(part, []byte("part"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := m.NewDownloadRemuxer(config.Downloads{DiskBudgetBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fresh, part} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("fresh download file %s removed by startup cleanup: %v", p, err)
		}
	}
	want := int64(len(payload) + len("part"))
	if got := d.UsedBytes(); got != want {
		t.Fatalf("UsedBytes = %d, want %d (adopted orphans counted, not 0)", got, want)
	}
	// Admission sees the adopted bytes: any new estimate busts budget=8.
	if _, _, err := d.RemuxDownload(context.Background(), core.DownloadRemuxRequest{
		InputURL: "http://x", Info: downloadInfo(), AudioStream: -1,
	}, nil); !errors.Is(err, core.ErrDownloadBudget) {
		t.Fatalf("RemuxDownload err = %v, want ErrDownloadBudget (adopted bytes occupy the budget)", err)
	}
	// Release of an adopted file frees it like any owned file.
	d.ReleaseDownload(fresh)
	if _, err := os.Stat(fresh); !os.IsNotExist(err) {
		t.Errorf("released adopted file still exists: %v", err)
	}
	if got := d.UsedBytes(); got != int64(len("part")) {
		t.Errorf("UsedBytes after release = %d, want %d", got, len("part"))
	}
}

// Runtime expiry: an adopted orphan that crosses staleDownloadAge is
// removed (and its budget freed) on the next admission, via
// expireStaleAdopted — not just at boot. Files remuxed by this process
// never expire by age.
func TestDownloadRemuxerAdoptedOrphanExpiresAtRuntime(t *testing.T) {
	fr := newFakeRunner()
	fr.runFunc = downloadRunner("new-file")
	m := newTestManager(t, fr, config.Transcode{})
	dir := filepath.Join(m.tempDir, "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "dl-ab12cd34.mp4")
	if err := os.WriteFile(orphan, []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := m.NewDownloadRemuxer(config.Downloads{})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.UsedBytes(); got != 16 {
		t.Fatalf("UsedBytes after adoption = %d, want 16", got)
	}
	// Simulate the passage of time: the orphan stops advancing (real
	// mtime aged on disk and in the adopted registry).
	old := time.Now().Add(-2 * staleDownloadAge)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	d.adopted[orphan] = old
	d.mu.Unlock()

	path, size, err := d.RemuxDownload(context.Background(), core.DownloadRemuxRequest{
		InputURL: "http://x", Info: downloadInfo(), AudioStream: -1,
	}, nil)
	if err != nil {
		t.Fatalf("RemuxDownload: %v", err)
	}
	defer d.ReleaseDownload(path)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("stale adopted orphan not expired at runtime: %v", err)
	}
	if got := d.UsedBytes(); got != size {
		t.Errorf("UsedBytes = %d, want %d (only the new file; adopted bytes freed)", got, size)
	}
}

// cleanStale (boot only, see New) must remove Ether-owned orphans —
// session dirs with their nested DASH run-*/chunk and chunk c-* dirs, and
// downloads/dl-* files older than staleDownloadAge — and nothing else in
// a shared temp dir: fresh dl-* files (a live remux keeps writing its
// .part, advancing mtime) and unrelated files survive. Fresh dl-* files
// kept here are adopted later by NewDownloadRemuxer (same policy, no
// contradiction: cleanStaleDownloads só apaga os velhos; a adoção conta
// os frescos retidos).
func TestCleanStaleRemovesOnlyEtherOrphans(t *testing.T) {
	dir := t.TempDir()
	ether := []string{
		"s-old-1234/run-3/chunk-0-00001.m4s",
		"s-old-1234/run-3/init-0.m4s",
		"s-old-1234/c-1/00000.ts",
		"s-old-1234/rung-720p/00000.ts",
		"s-old-1234/seg-0-00001.m4s",
		"http-old-1234/pipe.mkv",
		"downloads/dl-ab12cd34.mp4",
		"downloads/dl-ab12cd34.mp4.part",
	}
	alien := []string{
		"other/keep.txt",
		"keep.txt",
		"downloads/keep.txt",
		"downloads/dl-notas.txt",
		"downloads/dl-fresh99.mp4",
		"downloads/dl-fresh99.mp4.part",
		"s-notes.txt",
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
	old := time.Now().Add(-2 * time.Hour)
	for _, rel := range ether[5:] {
		p := filepath.Join(dir, rel)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(filepath.Join(dir, "downloads/dl-notas.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	(&Manager{tempDir: dir}).cleanStale()
	if _, err := os.Stat(filepath.Join(dir, "s-old-1234")); !os.IsNotExist(err) {
		t.Errorf("stale session dir kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "http-old-1234")); !os.IsNotExist(err) {
		t.Errorf("stale http session dir kept: %v", err)
	}
	for _, rel := range ether[6:] {
		if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
			t.Errorf("ether orphan %s kept: %v", rel, err)
		}
	}
	for _, rel := range alien {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("unrelated/fresh %s removed: %v", rel, err)
		}
	}
}
