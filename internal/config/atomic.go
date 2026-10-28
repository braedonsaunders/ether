package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// syncDir fsyncs dir; a var so tests can inject a post-rename dir-sync failure.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeFileAtomic replaces path with data so that a crash or power loss
// leaves either the old or the new file, never a truncated one: it writes a
// uniquely named temp file in the same directory (concurrent writers or a
// stale temp from a crash cannot collide), fsyncs it and renames it over
// path, then best-effort fsyncs the directory — a failure there only warns,
// since the rename already committed the new file. Directory fsync is
// skipped on Windows, where it is unsupported and rename is already
// journalled by NTFS.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil && runtime.GOOS != "windows" {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if runtime.GOOS != "windows" {
		// The rename already committed the new file, so a dir-sync failure
		// must not be returned: callers would roll back in-memory state to
		// a version the disk no longer holds. Best-effort, with a warning.
		if err := syncDir(dir); err != nil {
			slog.Warn("config: fsync directory after rename; new file is live but the rename may not be durable", "path", path, "err", err)
		}
	}
	return nil
}
