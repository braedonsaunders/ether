package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// syncDirAfterRename is the production post-rename step: fsync dir so the
// rename itself is durable. It is a no-op on Windows, where directory fsync
// is unsupported and rename is already journalled by NTFS.
func syncDirAfterRename(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
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
// path, then best-effort fsyncs the directory via syncDirAfterRename — a
// failure there only warns, since the rename already committed the new file.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeFileAtomicWithDirSync(path, data, perm, syncDirAfterRename)
}

// writeFileAtomicWithDirSync is writeFileAtomic with an injected post-rename
// dir-sync step (Store.syncDir). The injected step replaces
// syncDirAfterRename entirely, so tests exercise it on every platform,
// including Windows, without changing production Windows behaviour.
func writeFileAtomicWithDirSync(path string, data []byte, perm os.FileMode, syncDir func(string) error) error {
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
	// The rename already committed the new file, so a dir-sync failure
	// must not be returned: callers would roll back in-memory state to
	// a version the disk no longer holds. Best-effort, with a warning.
	if syncDir != nil {
		if err := syncDir(dir); err != nil {
			slog.Warn("config: fsync directory after rename; new file is live but the rename may not be durable", "path", path, "err", err)
		}
	}
	return nil
}
