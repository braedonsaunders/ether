package transcode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	slashpath "path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// ErrFFmpegNotFound means no usable ffmpeg/ffprobe executable was found in
// the configured path, the bundled locations or PATH.
var ErrFFmpegNotFound = errors.New("transcode: ffmpeg not found")

// LookPath finds bundled or PATH ffmpeg the same way Manager does.
func LookPath() string {
	p, err := defaultFinder().find("ffmpeg", "", "")
	if err != nil {
		return ""
	}
	return p
}

// finder locates bundled executables. Fields are injectable for tests.
type finder struct {
	exeDir   string // directory of the running executable
	goos     string
	lookPath func(string) (string, error)
	isExec   func(string) bool
}

func defaultFinder() finder {
	f := finder{goos: runtime.GOOS, lookPath: exec.LookPath, isExec: isExecutable}
	if exe, err := os.Executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		f.exeDir = filepath.Dir(exe)
	}
	return f
}

func isExecutable(path string) bool {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return st.Mode().Perm()&0o111 != 0
}

// candidates lists the bundled locations for a tool name, in search order.
func (f finder) candidates(name string) []string {
	bin := name
	if f.goos == "windows" {
		bin += ".exe"
	}
	var out []string
	if f.exeDir != "" {
		out = append(out,
			filepath.Join(f.exeDir, bin),
			filepath.Join(f.exeDir, "bin", bin),
			filepath.Join(f.exeDir, "ffmpeg", bin),
		)
		if f.goos == "darwin" {
			// Ether.app/Contents/MacOS/ether -> Ether.app/Contents/Resources/
			res := filepath.Join(filepath.Dir(f.exeDir), "Resources")
			out = append(out, filepath.Join(res, bin), filepath.Join(res, "bin", bin))
		}
	}
	if f.goos == "linux" {
		// Linux package locations: always slash paths, whatever the host OS.
		out = append(out, slashpath.Join("/usr/lib/ether", bin), slashpath.Join("/usr/lib/ether/bin", bin))
	}
	return out
}

// find resolves one tool. configured wins when set; sibling is a directory
// to try first (where ffmpeg was found, for ffprobe).
func (f finder) find(name, configured, sibling string) (string, error) {
	if configured != "" {
		if st, err := os.Stat(configured); err == nil && st.IsDir() {
			configured = filepath.Join(configured, name+exeSuffixFor(f.goos))
		}
		if f.isExec(configured) {
			return configured, nil
		}
		return "", fmt.Errorf("%w: configured %s %q is not an executable file", ErrFFmpegNotFound, name, configured)
	}
	var tried []string
	if sibling != "" {
		p := filepath.Join(sibling, name+exeSuffixFor(f.goos))
		if f.isExec(p) {
			return p, nil
		}
		tried = append(tried, p)
	}
	for _, p := range f.candidates(name) {
		if f.isExec(p) {
			return p, nil
		}
		tried = append(tried, p)
	}
	if f.lookPath != nil {
		if p, err := f.lookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("%w: %s not in %s or PATH", ErrFFmpegNotFound, name, strings.Join(tried, ", "))
}

func exeSuffixFor(goos string) string {
	if goos == "windows" {
		return ".exe"
	}
	return ""
}

// discover finds ffmpeg and ffprobe. ffprobe is looked for next to ffmpeg
// first so a bundled pair is never mixed with a system one.
func (f finder) discover(ffmpegCfg, ffprobeCfg string) (ffmpeg, ffprobe string, err error) {
	ffmpeg, err = f.find("ffmpeg", ffmpegCfg, "")
	if err != nil {
		return "", "", err
	}
	sibling := ""
	if ffprobeCfg == "" {
		sibling = filepath.Dir(ffmpeg)
	}
	ffprobe, err = f.find("ffprobe", ffprobeCfg, sibling)
	if err != nil {
		return "", "", err
	}
	return ffmpeg, ffprobe, nil
}

var versionRE = regexp.MustCompile(`(?m)^ff(?:mpeg|probe) version (\S+)`)

// parseVersion extracts the version from `ffmpeg -version` output.
func parseVersion(out []byte) string {
	if m := versionRE.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// toolVersion runs `<path> -version`.
func toolVersion(ctx context.Context, r Runner, path string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := r.Run(ctx, Command{Path: path, Args: []string{"-hide_banner", "-version"}, Stdout: &out, Stderr: &out}); err != nil {
		return "", fmt.Errorf("transcode: %s -version: %w", path, err)
	}
	// -hide_banner suppresses nothing for -version, but be lenient either way.
	v := parseVersion(out.Bytes())
	if v == "" {
		v = "unknown"
	}
	return v, nil
}
