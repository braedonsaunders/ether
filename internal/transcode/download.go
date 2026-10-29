package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Offline downloads: a resolved source is stream-copied into a standalone
// faststart MP4 under the transcode temp dir. faststart moves the moov
// atom to the front, which needs a seekable output, so downloads cannot
// be a pure pipe: the remux lands in a temp file first and is then served
// with Range + exact Content-Length (which is also what lets clients
// resume). Files are bounded by a disk budget and deleted on release,
// expiry or startup — never permanent storage.

// mp4CopyVideo / mp4CopyAudio are the codecs that copy into an MP4 every
// current mobile Plex client plays. Anything else (AV1, VP9, TrueHD,
// DTS, image subtitles) makes the download decision fail honestly rather
// than trigger an offline transcode Ether does not do.
var (
	mp4CopyVideo = map[string]bool{"h264": true, "hevc": true}
	mp4CopyAudio = map[string]bool{"aac": true, "ac3": true, "eac3": true, "mp3": true, "alac": true}
)

// DownloadCopyable reports whether info's video (and chosen audio) can be
// stream-copied to MP4, returning the streams a remux would keep.
func DownloadCopyable(info core.MediaInfo, audioStream int) (video, audio *core.StreamInfo, ok bool) {
	v := info.Video()
	if v == nil || !mp4CopyVideo[strings.ToLower(v.Codec)] {
		return nil, nil, false
	}
	a := pickAudio(info, audioStream)
	if a != nil && !mp4CopyAudio[strings.ToLower(a.Codec)] {
		return nil, nil, false
	}
	return v, a, true
}

// DownloadRemuxer remuxes resolved sources to temporary MP4s. It
// implements core.DownloadRemuxer.
type DownloadRemuxer struct {
	m      *Manager
	dir    string
	budget int64
	sem    chan struct{}

	mu    sync.Mutex
	used  int64
	files map[string]int64
}

var _ core.DownloadRemuxer = (*DownloadRemuxer)(nil)

// NewDownloadRemuxer prepares the download temp dir under the transcode
// temp dir and removes stale files a previous process left behind.
func (m *Manager) NewDownloadRemuxer(cfg config.Downloads) (*DownloadRemuxer, error) {
	dir := filepath.Join(m.tempDir, "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("transcode: download dir: %w", err)
	}
	// Aplica a política única de limpeza de downloads,
	// cleanStaleDownloads (manager.go) — a mesma do cleanStale no boot:
	// só arquivos que RemuxDownload cria (isRemuxDownloadFile) e só após
	// staleDownloadAge. Um dl-* fresco pode ser de outro processo ou de
	// um restart recente e sobrevive; qualquer outra coisa no dir não é
	// nossa e é mantida.
	cleanStaleDownloads(dir)
	budget := cfg.DiskBudgetBytes
	if budget <= 0 {
		budget = 20 << 30
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = 2
	}
	return &DownloadRemuxer{
		m: m, dir: dir, budget: budget,
		sem: make(chan struct{}, n), files: map[string]int64{},
	}, nil
}

// isRemuxDownloadFile reports whether name is one of the only files
// RemuxDownload creates in the downloads dir: the finished
// dl-<hex>.mp4 (see RemuxDownload) and the dl-<hex>.mp4.part partial it
// renames into place.
func isRemuxDownloadFile(name string) bool {
	if !strings.HasPrefix(name, "dl-") {
		return false
	}
	name = strings.TrimSuffix(name, ".part")
	return len(name) > len("dl-") && strings.HasSuffix(name, ".mp4")
}

// downloadArgs builds the ffmpeg argument list for a copy remux.
func downloadArgs(input string, video, audio *core.StreamInfo, out string) []string {
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-nostats",
		"-progress", "pipe:1", "-y"}
	args = append(args, inputSafetyArgs(input, false)...)
	args = append(args, "-i", input, "-map", fmt.Sprintf("0:%d", video.Index))
	if audio != nil {
		args = append(args, "-map", fmt.Sprintf("0:%d", audio.Index))
	}
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1", "-sn", "-dn",
		"-c", "copy")
	if strings.EqualFold(video.Codec, "hevc") {
		// Apple players want the hvc1 sample entry, not hev1.
		args = append(args, "-tag:v", "hvc1")
	}
	return append(args, "-movflags", "+faststart", "-f", "mp4", out)
}

func estimateBytes(info core.MediaInfo) int64 {
	if info.Size > 0 {
		return info.Size
	}
	if info.BitrateKbps > 0 && info.Duration > 0 {
		return int64(info.BitrateKbps) * 1000 / 8 * int64(info.Duration.Seconds())
	}
	return 0
}

// RemuxDownload implements core.DownloadRemuxer.
func (d *DownloadRemuxer) RemuxDownload(ctx context.Context, req core.DownloadRemuxRequest, progress func(float64)) (string, int64, error) {
	if req.InputURL == "" {
		return "", 0, errors.New("transcode: download: empty input URL")
	}
	video, audio, ok := DownloadCopyable(req.Info, req.AudioStream)
	if !ok {
		return "", 0, core.ErrDownloadNeedsTranscode
	}
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}

	est := estimateBytes(req.Info)
	d.mu.Lock()
	if est > 0 && d.used+est > d.budget {
		d.mu.Unlock()
		return "", 0, core.ErrDownloadBudget
	}
	d.used += est
	d.mu.Unlock()
	releaseEstimate := func() {
		d.mu.Lock()
		d.used -= est
		d.mu.Unlock()
	}

	final := filepath.Join(d.dir, "dl-"+randomHex(8)+".mp4")
	part := final + ".part"
	var stderr bytes.Buffer
	cmd := Command{
		Path:   d.m.ffmpeg,
		Args:   downloadArgs(req.InputURL, video, audio, part),
		Stderr: &stderr,
	}
	if progress != nil && req.Info.Duration > 0 {
		cmd.Stdout = &progressWriter{dur: req.Info.Duration, fn: progress}
	}
	d.m.log.Info("download remux starting", "input", req.InputURL,
		"video", video.Codec, "estimateBytes", est)
	if err := d.m.runner.Run(ctx, cmd); err != nil {
		_ = os.Remove(part)
		releaseEstimate()
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		return "", 0, fmt.Errorf("transcode: download remux: %w: %s",
			err, strings.TrimSpace(stderr.String()))
	}
	st, err := os.Stat(part)
	if err != nil {
		releaseEstimate()
		return "", 0, fmt.Errorf("transcode: download remux output: %w", err)
	}
	if err := os.Rename(part, final); err != nil {
		_ = os.Remove(part)
		releaseEstimate()
		return "", 0, fmt.Errorf("transcode: download remux rename: %w", err)
	}
	d.mu.Lock()
	d.used += st.Size() - est
	d.files[final] = st.Size()
	d.mu.Unlock()
	if progress != nil {
		progress(1)
	}
	return final, st.Size(), nil
}

// ReleaseDownload implements core.DownloadRemuxer.
func (d *DownloadRemuxer) ReleaseDownload(path string) {
	d.mu.Lock()
	size, ok := d.files[path]
	if ok {
		delete(d.files, path)
		d.used -= size
	}
	d.mu.Unlock()
	if ok {
		_ = os.Remove(path)
	}
}

// UsedBytes is the budget currently held by finished and in-flight
// downloads (tests and diagnostics).
func (d *DownloadRemuxer) UsedBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.used
}

// progressWriter turns ffmpeg -progress output into fractions.
type progressWriter struct {
	buf    []byte
	parser progressParser
	dur    time.Duration
	fn     func(float64)
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := string(w.buf[:i])
		w.buf = w.buf[i+1:]
		if pr, done := w.parser.line(line); done && w.dur.Seconds() > 0 {
			f := pr.OutTime.Seconds() / w.dur.Seconds()
			if f < 0 {
				f = 0
			}
			if f > 1 {
				f = 1
			}
			w.fn(f)
		}
	}
	return len(p), nil
}
