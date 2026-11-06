package prerolls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/mediaproc"
)

// Converter probes a source clip and converts it for direct play.
type Converter interface {
	// Probe reads the source's streams and duration.
	Probe(ctx context.Context, path string) (*core.MediaInfo, error)
	// Convert writes an H.264 High / AAC stereo MP4 (at most 1080p,
	// faststart) to out.
	Convert(ctx context.Context, in, out string, info *core.MediaInfo) error
	// Thumbnail writes one JPEG frame taken at offset to out.
	Thumbnail(ctx context.Context, in, out string, at time.Duration) error
}

type job struct {
	seq    int64
	input  string
	upload bool // input is our upload copy; delete it when done
}

// Upload streams a clip from r into the pre-roll folder and queues its
// conversion. It stops reading at the per-file cap.
func (s *Service) Upload(ctx context.Context, id string, r io.Reader) (Preroll, error) {
	cur, err := s.byID(ctx, id)
	if err != nil {
		return Preroll{}, err
	}
	if s.usedBytes() >= s.maxTotal {
		return Preroll{}, ErrStorageFull
	}
	tmp := s.path(cur.seq, "."+strconv.FormatInt(s.now().UnixNano(), 36)+".upload")
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return Preroll{}, err
	}
	n, err := io.Copy(f, io.LimitReader(r, s.maxFile+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > s.maxFile {
		err = ErrTooLarge
	}
	if err == nil && n == 0 {
		err = invalid("file", "the file is empty")
	}
	if err != nil {
		_ = os.Remove(tmp)
		return Preroll{}, err
	}
	if err := s.setVideo(ctx, cur.seq, videoUpdate{state: StateProcessing, source: SourceUpload, sourcePath: tmp}); err != nil {
		_ = os.Remove(tmp)
		return Preroll{}, err
	}
	s.enqueue(job{seq: cur.seq, input: tmp, upload: true})
	return s.Get(ctx, id)
}

// AttachPath uses a file on the server as the clip source. The file is
// only read; Ether converts it into its own folder.
func (s *Service) AttachPath(ctx context.Context, id, path string) (Preroll, error) {
	cur, err := s.byID(ctx, id)
	if err != nil {
		return Preroll{}, err
	}
	path, err = checkSourcePath(path, s.maxFile)
	if err != nil {
		return Preroll{}, err
	}
	if s.usedBytes() >= s.maxTotal {
		return Preroll{}, ErrStorageFull
	}
	if err := s.setVideo(ctx, cur.seq, videoUpdate{state: StateProcessing, source: SourcePath, sourcePath: path}); err != nil {
		return Preroll{}, err
	}
	s.enqueue(job{seq: cur.seq, input: path})
	return s.Get(ctx, id)
}

// checkSourcePath requires an absolute path to a readable regular file
// within the size cap.
func checkSourcePath(path string, maxBytes int64) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" || !filepath.IsAbs(path) {
		return "", invalid("path", "use the full path of a video file on the server")
	}
	path = filepath.Clean(path)
	st, err := os.Stat(path)
	if err != nil {
		return "", invalid("path", "the server cannot find that file")
	}
	if !st.Mode().IsRegular() {
		return "", invalid("path", "that path is not a video file")
	}
	if st.Size() > maxBytes {
		return "", invalid("path", "the file is larger than %d MB", maxBytes>>20)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", invalid("path", "the server cannot read that file")
	}
	_ = f.Close()
	return path, nil
}

func (s *Service) enqueue(j job) {
	select {
	case s.jobs <- j:
	default:
		// The queue is full; resume() picks it up on the next start.
		s.log.Warn("pre-roll conversion queue full", "preroll", j.seq)
	}
}

// usedBytes is the size of the pre-roll folder.
func (s *Service) usedBytes() int64 {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0
	}
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

// resume requeues clips left processing by a restart.
func (s *Service) resume(ctx context.Context) {
	all, err := s.loadAll(ctx)
	if err != nil {
		return
	}
	for _, p := range all {
		if p.Video.State != StateProcessing {
			continue
		}
		switch {
		case p.Video.Source == SourceUpload && filepath.Dir(p.sourcePath) == s.dir && fileExists(p.sourcePath):
			s.enqueue(job{seq: p.seq, input: p.sourcePath, upload: true})
		case p.Video.Source == SourcePath && p.sourcePath != "":
			s.enqueue(job{seq: p.seq, input: p.sourcePath})
		default:
			_ = s.setVideo(ctx, p.seq, videoUpdate{state: StateFailed, problem: ProblemUnreadable, source: p.Video.Source, sourcePath: p.sourcePath})
		}
	}
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

// process converts one queued clip.
func (s *Service) process(ctx context.Context, j job) {
	cur, err := s.bySeq(ctx, j.seq)
	if err != nil || cur.Video.State != StateProcessing || cur.sourcePath != j.input {
		if j.upload {
			_ = os.Remove(j.input)
		}
		return
	}
	if s.conv == nil {
		s.log.Warn("pre-roll waits for FFmpeg", "preroll", cur.ID)
		return
	}
	v := s.convert(ctx, cur.seq, j.input)
	if ctx.Err() != nil {
		return // left processing; resume() retries after restart
	}
	if j.upload {
		_ = os.Remove(j.input)
	}
	v.source, v.sourcePath = cur.Video.Source, cur.sourcePath
	if j.upload {
		v.sourcePath = ""
	}
	// The owner may have attached a newer video meanwhile.
	if now, err := s.bySeq(ctx, j.seq); err != nil || now.Video.Source != cur.Video.Source || now.sourcePath != cur.sourcePath || now.Video.State != StateProcessing {
		return
	}
	if err := s.setVideo(ctx, cur.seq, v); err != nil {
		s.log.Warn("save pre-roll video", "preroll", cur.ID, "err", err)
		return
	}
	s.log.Info("pre-roll video", "preroll", cur.ID, "state", v.state, "problem", v.problem)
}

func (s *Service) convert(ctx context.Context, seq int64, input string) videoUpdate {
	failed := func(problem string) videoUpdate { return videoUpdate{state: StateFailed, problem: problem} }
	pctx, cancel := context.WithTimeout(ctx, time.Minute)
	info, err := s.conv.Probe(pctx, input)
	cancel()
	if err != nil || info == nil || info.Video() == nil {
		return failed(ProblemUnreadable)
	}
	if info.Duration <= 0 {
		return failed(ProblemUnreadable)
	}
	if info.Duration > MaxDuration {
		return failed(ProblemTooLong)
	}
	part := s.path(seq, ".part.mp4")
	cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	err = s.conv.Convert(cctx, input, part, info)
	cancel()
	if err != nil {
		_ = os.Remove(part)
		if ctx.Err() == nil {
			s.log.Warn("convert pre-roll", "preroll", seq, "err", err)
		}
		return failed(ProblemConvertFailed)
	}
	pctx, cancel = context.WithTimeout(ctx, time.Minute)
	out, err := s.conv.Probe(pctx, part)
	cancel()
	st, serr := os.Stat(part)
	if err != nil || out == nil || out.Video() == nil || serr != nil {
		_ = os.Remove(part)
		return failed(ProblemConvertFailed)
	}
	if s.usedBytes() > s.maxTotal {
		_ = os.Remove(part)
		return failed(ProblemConvertFailed)
	}
	if err := os.Rename(part, s.path(seq, ".mp4")); err != nil {
		_ = os.Remove(part)
		return failed(ProblemConvertFailed)
	}
	thumb := s.path(seq, ".part.jpg")
	tctx, cancel := context.WithTimeout(ctx, time.Minute)
	if err := s.conv.Thumbnail(tctx, s.path(seq, ".mp4"), thumb, min(out.Duration/3, 5*time.Second)); err == nil {
		_ = os.Rename(thumb, s.path(seq, ".jpg"))
	} else {
		_ = os.Remove(thumb)
	}
	cancel()
	vid := out.Video()
	bitrate := out.BitrateKbps
	if bitrate <= 0 && out.Duration > 0 {
		bitrate = int(st.Size() * 8 / 1000 / int64(max(out.Duration/time.Second, 1)))
	}
	return videoUpdate{
		state: StateReady, durationMs: out.Duration.Milliseconds(), width: vid.Width, height: vid.Height,
		bitrate: bitrate, size: st.Size(),
	}
}

// FFmpeg converts with the bundled FFmpeg (internal/transcode discovery).
type FFmpeg struct {
	// Path returns the ffmpeg executable; "" means unavailable.
	Path func() string
	// ProbeFunc reads media info (the transcoder's ffprobe).
	ProbeFunc func(ctx context.Context, input string) (*core.MediaInfo, error)
}

// Probe implements Converter.
func (f FFmpeg) Probe(ctx context.Context, path string) (*core.MediaInfo, error) {
	if f.ProbeFunc == nil {
		return nil, errors.New("prerolls: no prober")
	}
	return f.ProbeFunc(ctx, path)
}

func (f FFmpeg) bin() (string, error) {
	if f.Path == nil {
		return "", errors.New("prerolls: ffmpeg not found")
	}
	p := f.Path()
	if p == "" {
		return "", errors.New("prerolls: ffmpeg not found")
	}
	return p, nil
}

// convertArgs builds the conversion command. A clip without audio gets a
// silent stereo track so every client sees the same stream layout.
func convertArgs(in, out string, info *core.MediaInfo) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", in}
	hasAudio := false
	for _, st := range info.Streams {
		if st.Type == "audio" {
			hasAudio = true
			break
		}
	}
	if !hasAudio {
		args = append(args, "-f", "lavfi", "-i", "anullsrc=channel_layout=stereo:sample_rate=48000")
	}
	args = append(args, "-map", "0:v:0")
	if hasAudio {
		args = append(args, "-map", "0:a:0")
	} else {
		args = append(args, "-map", "1:a:0", "-shortest")
	}
	return append(args,
		"-t", strconv.Itoa(int(MaxDuration/time.Second)),
		"-vf", "scale=w='min(1920,iw)':h='min(1080,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2,format=yuv420p",
		"-c:v", "libx264", "-profile:v", "high", "-level:v", "4.1", "-preset", "medium", "-crf", "20",
		"-maxrate", "12M", "-bufsize", "24M",
		"-c:a", "aac", "-ac", "2", "-ar", "48000", "-b:a", "192k",
		"-sn", "-dn", "-map_metadata", "-1", "-map_chapters", "-1",
		"-movflags", "+faststart", "-f", "mp4", out)
}

// Convert implements Converter.
func (f FFmpeg) Convert(ctx context.Context, in, out string, info *core.MediaInfo) error {
	bin, err := f.bin()
	if err != nil {
		return err
	}
	return run(ctx, bin, convertArgs(in, out, info))
}

// Thumbnail implements Converter.
func (f FFmpeg) Thumbnail(ctx context.Context, in, out string, at time.Duration) error {
	bin, err := f.bin()
	if err != nil {
		return err
	}
	return run(ctx, bin, []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-ss", strconv.FormatFloat(at.Seconds(), 'f', 3, 64), "-i", in, "-frames:v", "1",
		"-vf", "scale=w='min(640,iw)':h=-2", "-q:v", "4", "-f", "image2", "-c:v", "mjpeg", out})
}

func run(ctx context.Context, bin string, args []string) error {
	cmd := mediaproc.CommandContext(ctx, mediaproc.EncodeTimeout, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &limitedWriter{w: &stderr, n: 4096}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(len(p), l.n)
		_, _ = l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}
