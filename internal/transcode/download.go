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

// Offline downloads: a resolved source is copied, or converted when a
// copy cannot produce an MP4 the requesting client's download profile
// plays, into a standalone faststart MP4 under the transcode temp dir.
// faststart moves the moov atom to the front, which needs a seekable
// output, so downloads cannot be a pure pipe: the file lands in a temp
// first and is then served with Range + exact Content-Length (which is
// also what lets clients resume). Files are bounded by a disk budget
// and deleted on release, expiry or startup — never permanent storage.
// Converting downloads reuse the streaming hardware pipeline and take
// one encode slot per viewer (in addition to the remuxer's global cap).

// mp4CopyVideo / mp4CopyAudio are the codecs that copy into an MP4 every
// current mobile Plex client plays. Anything else (AV1, VP9, TrueHD,
// DTS) is converted to the profile's H.264/HEVC + AAC/AC3 target.
// Image subtitles are burned when the request asks, otherwise dropped.
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
	// adopted holds the real mtime of fresh dl-* orphans adopted at
	// startup (see adoptOrphans): a subset of files. Only adopted files
	// expire by age at runtime (expireStaleAdopted); files this process
	// remuxed itself are freed only by ReleaseDownload.
	adopted   map[string]time.Time
	viewerEnc map[int64]chan struct{}
}

var _ core.DownloadRemuxer = (*DownloadRemuxer)(nil)

// NewDownloadRemuxer prepares the download temp dir under the transcode
// temp dir. Boot cleanup follows the single stale-download policy (see
// staleDownloadAge in manager.go for who deletes what): stale dl-*
// orphans are removed by cleanStaleDownloads, while fresh dl-* orphans
// it keeps are ADOPTED by the new remuxer — registered in files/used at
// their real size, with their real mtime, so they count against the
// disk budget from the start and expire at runtime via
// expireStaleAdopted once they cross staleDownloadAge.
func (m *Manager) NewDownloadRemuxer(cfg config.Downloads) (*DownloadRemuxer, error) {
	dir := filepath.Join(m.tempDir, "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("transcode: download dir: %w", err)
	}
	cleanStaleDownloads(dir)
	budget := cfg.DiskBudgetBytes
	if budget <= 0 {
		budget = 20 << 30
	}
	n := cfg.MaxConcurrent
	if n <= 0 {
		n = 2
	}
	d := &DownloadRemuxer{
		m: m, dir: dir, budget: budget,
		sem:       make(chan struct{}, n),
		files:     map[string]int64{},
		adopted:   map[string]time.Time{},
		viewerEnc: map[int64]chan struct{}{},
	}
	d.adoptOrphans()
	return d, nil
}

// isRemuxDownloadFile reports whether name is one of the only files
// RemuxDownload creates in the downloads dir: the finished
// dl-<hex>.mp4 (see RemuxDownload) and the dl-<hex>.mp4.part partial it
// renames into place. It is the predicate of the single stale-download
// policy (cleanStaleDownloads) and of orphan adoption (adoptOrphans).
func isRemuxDownloadFile(name string) bool {
	if !strings.HasPrefix(name, "dl-") {
		return false
	}
	name = strings.TrimSuffix(name, ".part")
	return len(name) > len("dl-") && strings.HasSuffix(name, ".mp4")
}

// adoptOrphans registers fresh dl-* files left by a previous process in
// files/used at their real size, with their real mtime in adopted.
// cleanStaleDownloads has already removed the stale ones, so everything
// matching isRemuxDownloadFile here is fresh and retained — but retained
// must not mean invisible: without adoption a retained orphan would sit
// on disk outside the budget (UsedBytes would report 0 for it and
// admission would hand its bytes out again). Adopted files expire at
// runtime in expireStaleAdopted; unrelated files are never adopted.
func (d *DownloadRemuxer) adoptOrphans() {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !isRemuxDownloadFile(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		path := filepath.Join(d.dir, e.Name())
		d.files[path] = info.Size()
		d.adopted[path] = info.ModTime()
		d.used += info.Size()
	}
}

// expireStaleAdopted removes adopted orphans whose real mtime has
// crossed staleDownloadAge, freeing their budget. It is the runtime
// counterpart of cleanStaleDownloads (boot): same predicate, same age.
// Only adopted files are eligible — a file this process remuxed may be
// held for a caller far longer than staleDownloadAge and is freed only
// by ReleaseDownload. Called on the admission path (RemuxDownload), so
// stale adopted bytes are reclaimed before new budget is handed out.
func (d *DownloadRemuxer) expireStaleAdopted(now time.Time) {
	cutoff := now.Add(-staleDownloadAge)
	d.mu.Lock()
	var candidates []string
	for path, mtime := range d.adopted {
		if mtime.Before(cutoff) {
			candidates = append(candidates, path)
		}
	}
	d.mu.Unlock()
	for _, path := range candidates {
		// Re-stat: a .part another process is still writing keeps
		// advancing its mtime and must not expire under us.
		if info, err := os.Stat(path); err == nil && !info.ModTime().Before(cutoff) {
			d.mu.Lock()
			if _, ok := d.adopted[path]; ok {
				d.adopted[path] = info.ModTime()
			}
			d.mu.Unlock()
			continue
		} else if err == nil {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				// Keep the bytes accounted for and retry on later admission.
				continue
			}
		} else if !os.IsNotExist(err) {
			continue // erro transitório: mantém a contabilidade adotada
		}
		d.mu.Lock()
		if size, ok := d.files[path]; ok {
			if _, adopted := d.adopted[path]; adopted {
				delete(d.files, path)
				delete(d.adopted, path)
				d.used -= size
			}
		}
		d.mu.Unlock()
	}
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

// downloadProfile constrains a client's caps to the MP4 download target:
// H.264/HEVC + AAC/AC3. Empty codecs use that full set.
func downloadProfile(caps core.ClientCaps) core.ClientCaps {
	out := caps
	out.Download = true
	var v []string
	for _, c := range caps.VideoCodecs {
		switch strings.ToLower(c) {
		case "h264", "hevc":
			v = append(v, strings.ToLower(c))
		}
	}
	if len(v) == 0 {
		v = []string{"h264", "hevc"}
	}
	out.VideoCodecs = v
	audio := out.TranscodeAudioCodecs
	if len(audio) == 0 {
		audio = out.AudioCodecs
	}
	var a []string
	for _, c := range audio {
		switch strings.ToLower(c) {
		case "aac", "ac3":
			a = append(a, strings.ToLower(c))
		}
	}
	if len(a) == 0 {
		a = []string{"aac", "ac3"}
	}
	out.AudioCodecs = a
	out.TranscodeAudioCodecs = a
	if len(out.Containers) == 0 {
		out.Containers = []string{"mp4"}
	}
	return out
}

func downloadConvertArgs(input string, info core.MediaInfo, dec core.Decision, accel core.HWAccel, feat features, out string) []string {
	dec.Container = ContainerFMP4
	spec := runSpec{Input: input, Info: info, Decision: dec, Accel: accel, Features: feat}
	args := buildCodecArgs(spec)
	codec := dec.VideoCodec
	if dec.VideoCopy {
		if v := info.Video(); v != nil {
			codec = v.Codec
		}
	}
	if strings.EqualFold(codec, "hevc") {
		tagged := false
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "-tag:v" {
				tagged = true
				break
			}
		}
		if !tagged {
			args = append(args, "-tag:v", "hvc1")
		}
	}
	return append(args, "-movflags", "+faststart", "-f", "mp4", out)
}

func estimateConvertBytes(info core.MediaInfo, d core.Decision) int64 {
	if d.BitrateKbps > 0 && info.Duration > 0 {
		return int64(d.BitrateKbps) * 1000 / 8 * int64(info.Duration.Seconds())
	}
	return estimateBytes(info)
}

func (d *DownloadRemuxer) acquireViewerEncode(ctx context.Context, viewerID int64) (func(), error) {
	if viewerID == 0 {
		return func() {}, nil
	}
	d.mu.Lock()
	ch, ok := d.viewerEnc[viewerID]
	if !ok {
		ch = make(chan struct{}, 1)
		d.viewerEnc[viewerID] = ch
	}
	d.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
	if ok && !downloadBurnsImage(req) {
		return d.runDownload(ctx, req, progress, estimateBytes(req.Info), "remux",
			func(part string) []string { return downloadArgs(req.InputURL, video, audio, part) })
	}
	return d.convertDownload(ctx, req, progress)
}

func firstImageSubtitle(info core.MediaInfo) *core.StreamInfo {
	for i := range info.Streams {
		s := &info.Streams[i]
		if s.Type == "subtitle" && IsImageSubtitle(s.Codec) {
			return s
		}
	}
	return nil
}

// downloadBurnsImage is true when the request asked to burn and the
// source has a bitmap subtitle. Those cannot ride along in the MP4
// remux, so the download converts and burns (or, without this flag,
// remux/convert drop them).
func downloadBurnsImage(req core.DownloadRemuxRequest) bool {
	sub := pickSubtitle(req.Info, req.Options.SubtitleStream)
	if sub == nil && req.Options.BurnSubtitles {
		sub = firstImageSubtitle(req.Info)
	}
	return sub != nil && IsImageSubtitle(sub.Codec) &&
		(req.Options.BurnSubtitles || req.Options.SubtitleStream >= 0)
}

func (d *DownloadRemuxer) convertDownload(ctx context.Context, req core.DownloadRemuxRequest, progress func(float64)) (string, int64, error) {
	if req.Info.Video() == nil {
		return "", 0, core.ErrDownloadNeedsTranscode
	}
	release, err := d.acquireViewerEncode(ctx, req.ViewerID)
	if err != nil {
		return "", 0, err
	}
	defer release()

	opts := req.Options
	opts.DirectPlay = false
	opts.DirectStream = true
	opts.AudioStream = req.AudioStream
	if downloadBurnsImage(req) {
		opts.BurnSubtitles = true
		if pickSubtitle(req.Info, opts.SubtitleStream) == nil {
			if sub := firstImageSubtitle(req.Info); sub != nil {
				opts.SubtitleStream = sub.Index
			}
		}
	} else if opts.SubtitleStream == 0 {
		opts.SubtitleStream = -1
	}
	dec := d.m.Decide(req.Info, downloadProfile(req.Caps), opts)
	if dec.Kind == core.DecisionDirectPlay {
		dec.Kind = core.DecisionRemux
		dec.VideoCopy, dec.AudioCopy = true, true
	}
	dec.Container = ContainerFMP4
	return d.runDownload(ctx, req, progress, estimateConvertBytes(req.Info, dec), "convert",
		func(part string) []string {
			return downloadConvertArgs(req.InputURL, req.Info, dec, d.m.hw, d.m.feat, part)
		})
}

func (d *DownloadRemuxer) runDownload(ctx context.Context, req core.DownloadRemuxRequest, progress func(float64), est int64, kind string, argsFn func(part string) []string) (string, int64, error) {
	// Reclaim adopted orphans that crossed staleDownloadAge before
	// handing out more budget (runtime expiry; see expireStaleAdopted).
	d.expireStaleAdopted(time.Now())
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}

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
		Args:   argsFn(part),
		Stderr: &stderr,
	}
	if progress != nil && req.Info.Duration > 0 {
		cmd.Stdout = &progressWriter{dur: req.Info.Duration, fn: progress}
	}
	d.m.log.Info("download "+kind+" starting", "input", req.InputURL, "estimateBytes", est)
	if err := d.m.runner.Run(ctx, cmd); err != nil {
		_ = os.Remove(part)
		releaseEstimate()
		if ctx.Err() != nil {
			return "", 0, ctx.Err()
		}
		return "", 0, fmt.Errorf("transcode: download %s: %w: %s",
			kind, err, strings.TrimSpace(stderr.String()))
	}
	st, err := os.Stat(part)
	if err != nil {
		releaseEstimate()
		return "", 0, fmt.Errorf("transcode: download %s output: %w", kind, err)
	}
	if err := os.Rename(part, final); err != nil {
		_ = os.Remove(part)
		releaseEstimate()
		return "", 0, fmt.Errorf("transcode: download %s rename: %w", kind, err)
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
		delete(d.adopted, path)
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
