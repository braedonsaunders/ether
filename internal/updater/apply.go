package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ProgressFunc receives cumulative downloaded bytes (total may be 0 when
// the manifest omits Size).
type ProgressFunc func(downloaded, total int64)

// Download fetches the update's artifact into dir (empty: tempDir()),
// verifying size and SHA-256. The returned file is complete and verified;
// partial files are removed on error. Downloads are capped at
// max(Size, Size*1.1) when Size is known, else 2 GiB.
func (c *Client) Download(ctx context.Context, upd *Update, dir string, progress ProgressFunc) (string, error) {
	if upd == nil || upd.Artifact == nil || upd.Manifest == nil {
		return "", fmt.Errorf("updater: nil update")
	}
	if err := upd.Manifest.ValidateFreshness(time.Now()); err != nil {
		return "", err
	}
	return c.DownloadArtifact(ctx, upd.Artifact, dir, progress)
}

// DownloadArtifact is Download for a bare Artifact.
func (c *Client) DownloadArtifact(ctx context.Context, art *Artifact, dir string, progress ProgressFunc) (string, error) {
	if dir == "" {
		dir = tempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := art.Filename
	if name == "" {
		name = strings.Split(art.URL, "?")[0]
	}
	name = filepath.Base(name) // never trust the feed with a path
	if name == "." || name == ".." || name == string(filepath.Separator) {
		// Base of "", "a/.." or "/" is no file name: joined with dir it
		// would point at dir itself or its parent.
		name = "ether-update"
	}
	dest := filepath.Join(dir, name)
	part := dest + ".part"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, art.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.userAgent())
	// No client-wide timeout on big downloads: rely on ctx. Clone the
	// configured client with the manifest timeout removed.
	dl := *c.http
	dl.Timeout = 0
	resp, err := dl.Do(req)
	if err != nil {
		return "", fmt.Errorf("updater: download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("updater: download %s: %s", art.URL, resp.Status)
	}
	limit := int64(2) << 30
	if art.Size > 0 {
		limit = art.Size + (art.Size / 10) + 4096
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	var w io.Writer = io.MultiWriter(f, h)
	if progress != nil {
		w = io.MultiWriter(f, h, &progressWriter{total: art.Size, fn: progress})
	}
	n, copyErr := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("updater: download: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(part)
		return "", closeErr
	}
	if n > limit {
		_ = os.Remove(part)
		return "", fmt.Errorf("updater: download over size cap (%d bytes)", limit)
	}
	if art.Size > 0 && n != art.Size {
		_ = os.Remove(part)
		return "", fmt.Errorf("%w: size %d, want %d", ErrChecksum, n, art.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, art.SHA256) {
		_ = os.Remove(part)
		return "", fmt.Errorf("%w: sha256 %s, want %s", ErrChecksum, got, art.SHA256)
	}
	if err := os.Rename(part, dest); err != nil {
		_ = os.Remove(part)
		return "", err
	}
	c.log.Info("updater: downloaded", "file", dest, "bytes", n)
	return dest, nil
}

type progressWriter struct {
	n     int64
	total int64
	fn    ProgressFunc
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	p.fn(p.n, p.total)
	return len(b), nil
}

// VerifyFile checks an already-downloaded file against an artifact.
func VerifyFile(path string, art *Artifact) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	if art.Size > 0 && n != art.Size {
		return fmt.Errorf("%w: size %d, want %d", ErrChecksum, n, art.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, art.SHA256) {
		return fmt.Errorf("%w: sha256 %s, want %s", ErrChecksum, got, art.SHA256)
	}
	return nil
}

// ApplyOptions tunes Apply.
type ApplyOptions struct {
	// Target is the installed binary replaced for KindBinary updates.
	// Empty: os.Executable().
	Target string
	// DownloadDir for the artifact. Empty: temp dir.
	DownloadDir string
	// LaunchInstaller starts a downloaded installer/.dmg. Nil: the
	// platform default (exec the installer / `open` on macOS).
	LaunchInstaller func(ctx context.Context, path string) error
	Progress        ProgressFunc
}

// Apply downloads (and verifies) the update, then:
//   - KindBinary: atomically replaces the target binary, keeping
//     "<target>.old" for rollback. The caller must restart the process.
//   - KindInstaller: launches the installer and returns; the installer
//     owns stopping/restarting the service.
//   - KindArchive: returns the downloaded path without applying.
//
// It returns the artifact file path.
func (c *Client) Apply(ctx context.Context, upd *Update, opts ApplyOptions) (string, error) {
	path, err := c.Download(ctx, upd, opts.DownloadDir, opts.Progress)
	if err != nil {
		return "", err
	}
	switch upd.Artifact.Kind {
	case KindBinary:
		target := opts.Target
		if target == "" {
			exe, err := os.Executable()
			if err != nil {
				return path, err
			}
			target = exe
		}
		if err := ApplyBinary(path, target); err != nil {
			return path, err
		}
		c.log.Info("updater: binary replaced; restart to run the new version", "target", target, "version", upd.Version())
		return path, nil
	case KindInstaller:
		launch := opts.LaunchInstaller
		if launch == nil {
			launch = launchInstaller
		}
		if err := launch(ctx, path); err != nil {
			return path, fmt.Errorf("updater: launch installer: %w", err)
		}
		c.log.Info("updater: installer launched", "file", path, "version", upd.Version())
		return path, nil
	default:
		return path, nil
	}
}

// ApplyBinary atomically replaces target with the verified new binary at
// newPath, keeping the previous binary at "<target>.old".
//
// Unix: the new file is staged next to the target and rename(2)d over it —
// running processes keep their inode, so this is safe mid-run. Windows
// cannot overwrite a running executable, so the current binary is first
// renamed aside to .old (allowed while running), then the new one moved
// in; on failure the .old is moved back.
func ApplyBinary(newPath, target string) error {
	if err := refuseBundleTarget(target); err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Errorf("updater: stat target: %w", err)
	}
	mode := info.Mode().Perm()
	if mode&0o111 == 0 {
		mode |= 0o755
	}
	if runtime.GOOS == "windows" {
		return applyBinaryWindows(newPath, target, mode)
	}
	staged := target + ".new"
	if err := copyFile(newPath, staged, mode); err != nil {
		return err
	}
	old := target + ".old"
	// Best effort backup for rollback (rename keeps the running inode too).
	if err := copyFile(target, old, mode); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("updater: backup current binary: %w", err)
	}
	if err := os.Rename(staged, target); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("updater: replace binary: %w", err)
	}
	return nil
}

func applyBinaryWindows(newPath, target string, mode os.FileMode) error {
	old := target + ".old"
	_ = os.Remove(old)
	if err := os.Rename(target, old); err != nil {
		return fmt.Errorf("updater: move current binary aside: %w", err)
	}
	if err := copyFile(newPath, target, mode); err != nil {
		// Roll back.
		if rbErr := os.Rename(old, target); rbErr != nil {
			return fmt.Errorf("updater: install new binary: %w (rollback also failed: %v)", err, rbErr)
		}
		return fmt.Errorf("updater: install new binary: %w", err)
	}
	return nil
}

// Rollback restores "<target>.old" over target. On Windows the caller must
// not be running from target (the service should be stopped first).
func Rollback(target string) error {
	if err := refuseBundleTarget(target); err != nil {
		return err
	}
	old := target + ".old"
	if _, err := os.Stat(old); err != nil {
		return fmt.Errorf("updater: no rollback copy at %s: %w", old, err)
	}
	if runtime.GOOS == "windows" {
		bad := target + ".bad"
		_ = os.Remove(bad)
		if err := os.Rename(target, bad); err != nil {
			return fmt.Errorf("updater rollback: %w", err)
		}
		if err := os.Rename(old, target); err != nil {
			_ = os.Rename(bad, target)
			return fmt.Errorf("updater rollback: %w", err)
		}
		_ = os.Remove(bad)
		return nil
	}
	return os.Rename(old, target)
}

// CleanupOld removes a stale "<target>.old" left by a previous update. The
// server calls this at startup once the new binary is confirmed running
// (on Windows the .old of a *running* binary can only be deleted by the
// next process, which is exactly this).
func CleanupOld(target string) error {
	if err := refuseBundleTarget(target); err != nil {
		return err
	}
	err := os.Remove(target + ".old")
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// refuseBundleTarget stops an update from replacing or deleting an .app
// bundle. The server binary inside Contents/Resources is a file and may be
// replaced; the bundle directory itself is never the target.
func refuseBundleTarget(target string) error {
	clean := filepath.Clean(target)
	if strings.HasSuffix(strings.ToLower(filepath.Base(clean)), ".app") {
		return fmt.Errorf("updater: refusing to replace or delete the app bundle %s", clean)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmp, mode); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, dst)
}

// installerCommand picks the program that opens a downloaded installer.
func installerCommand(goos, path string) (string, []string) {
	switch goos {
	case "darwin":
		return "open", []string{path} // .dmg / .pkg
	case "windows":
		if strings.ToLower(filepath.Ext(path)) == ".msi" {
			return "msiexec", []string{"/i", path}
		}
		return path, nil
	default: // linux etc.: hand to the desktop; package installs need a terminal helper
		return "xdg-open", []string{path}
	}
}

func launchInstaller(ctx context.Context, path string) error {
	name, args := installerCommand(runtime.GOOS, path)
	return startDetached(ctx, name, args...)
}

// startDetached starts a program that must outlive ctx: the caller's
// context typically ends as soon as Apply returns, and binding the process
// to it would kill the installer (or `open`, before it hands the .dmg to
// the desktop) right after launch. ctx only gates the start. The process
// is reaped in the background.
func startDetached(ctx context.Context, name string, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
