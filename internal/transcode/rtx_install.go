package transcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/egress"
)

// Pinned upstream release and GitHub asset digest, not a mutable 'latest' URL.
// https://github.com/rigaya/NVEnc/releases/tag/9.38
const rtxPackageURL = "https://github.com/rigaya/NVEnc/releases/download/9.38/NVEncC_9.38_x64.7z"
const rtxPackageSHA256 = "f4ec81cd2bd4acae35415dc539c7a439e45667b4398e6209f3aa16ac21cf55e5"
const rtxPackageSize int64 = 246707699

func windowsTool(name string) string { return filepath.Join(os.Getenv("SystemRoot"), "System32", name) }

func checkRTXGPU(ctx context.Context, runner Runner) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out boundedRTXOutput
	// Driver installs nvidia-smi in System32. Do not run a downloaded executable
	// merely to find out whether this host needs the download.
	err := runner.Run(ctx, Command{Path: windowsTool("nvidia-smi.exe"), Args: []string{"--query-gpu=name", "--format=csv,noheader"}, Stdout: &out, Stderr: io.Discard})
	if err != nil {
		return errors.New("cannot detect an NVIDIA RTX GPU; install a working NVIDIA driver before enabling RTX Video")
	}
	first := strings.Split(strings.TrimSpace(out.String()), "\n")[0]
	if !strings.Contains(strings.ToUpper(first), "RTX") {
		return errors.New("RTX Video requires an NVIDIA RTX GPU as device 0; no runtime was downloaded")
	}
	return nil
}

type rtxInstaller struct {
	root, url, digest string
	size              int64
	client            *http.Client
	extract           func(context.Context, string, string) error
}

func newRTXInstaller(root string, runner Runner) rtxInstaller {
	client := egress.Client(egress.CapDirect, 12*time.Minute)
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != "https" {
			return errors.New("unsafe runtime download redirect")
		}
		return nil
	}
	return rtxInstaller{root: root, url: rtxPackageURL, digest: rtxPackageSHA256, size: rtxPackageSize,
		client: client, extract: func(ctx context.Context, archive, dir string) error {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			// Windows 10/11/Server includes bsdtar with 7z support. The archive must
			// match the embedded SHA-256 before any archive parser or tool sees it.
			var out boundedRTXOutput
			if err := runner.Run(ctx, Command{Path: windowsTool("tar.exe"), Args: []string{"-xf", archive, "-C", dir}, Stdout: &out, Stderr: &out}); err != nil {
				return errors.New("Windows archive extraction failed (System32/tar.exe is required)")
			}
			return nil
		}}
}

func (i rtxInstaller) ensure(ctx context.Context) (string, error) {
	final := filepath.Join(i.root, "nvenc-9.38-windows-amd64")
	exe := filepath.Join(final, "NVEncC64.exe")
	if b, err := os.ReadFile(filepath.Join(final, ".complete")); err == nil && strings.TrimSpace(string(b)) == i.digest {
		if err = validateRTXFiles(final); err == nil {
			return exe, nil
		}
	}
	// Never overwrite a partial or owner-modified installed directory. Staging
	// is unique, completion is atomic, and a failed attempt removes only its own files.
	if _, err := os.Lstat(final); err == nil {
		return "", errors.New("managed runtime directory is incomplete; move it aside before retrying")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(i.root, 0o700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(i.root, ".nvenc-install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	archive := filepath.Join(stage, "runtime.7z")
	if err = i.download(ctx, archive); err != nil {
		return "", err
	}
	unpack := filepath.Join(stage, "runtime")
	if err = os.Mkdir(unpack, 0o700); err != nil {
		return "", err
	}
	if err = i.extract(ctx, archive, unpack); err != nil {
		return "", err
	}
	if err = validateRTXFiles(unpack); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = os.WriteFile(filepath.Join(unpack, ".complete"), []byte(i.digest+"\n"), 0o600); err != nil {
		return "", err
	}
	if err = os.Rename(unpack, final); err != nil {
		return "", err
	}
	return exe, nil
}

func (i rtxInstaller) download(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, i.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Ether-RTX-Runtime/1")
	res, err := i.client.Do(req)
	if err != nil {
		return errors.New("runtime download failed")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("runtime download returned HTTP %d", res.StatusCode)
	}
	if res.ContentLength >= 0 && res.ContentLength != i.size {
		return errors.New("runtime download size mismatch")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(f, h), io.LimitReader(res.Body, i.size+1))
	closeErr := f.Close()
	if copyErr != nil {
		return errors.New("runtime download interrupted")
	}
	if closeErr != nil {
		return closeErr
	}
	if n != i.size || hex.EncodeToString(h.Sum(nil)) != i.digest {
		return errors.New("runtime checksum verification failed; package was not extracted")
	}
	return ctx.Err()
}

func validateRTXFiles(dir string) error {
	for _, name := range []string{"NVEncC64.exe", "nvngx_vsr.dll", "NVEncNVSDKNGX.dll"} {
		st, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
			return fmt.Errorf("runtime package is missing %s", name)
		}
	}
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("runtime package contains a symbolic link")
		}
		return nil
	})
}
