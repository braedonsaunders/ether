// Package logfile provides Ether's on-disk logs: a size-rotated log file
// (stderr is discarded under a Windows service and unrotated under
// launchd) and the crash log that receives fatal runtime traces.
package logfile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Defaults for the main log file.
const (
	DefaultMaxSize int64 = 10 << 20 // rotate at 10 MiB
	DefaultKeep          = 5        // ether.log.1 … ether.log.5
)

const (
	dirMode  = 0o750
	fileMode = 0o640
)

// Writer is a concurrency-safe io.Writer that appends to a file and
// rotates it by size: path -> path.1 -> … -> path.<keep>, dropping the
// oldest. A failed open or write closes the file and the next Write
// reopens it, so a transient error (disk full, file removed) does not
// disable logging for the rest of the process.
type Writer struct {
	path    string
	maxSize int64
	keep    int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open creates the directory if needed and opens path for appending.
// maxSize <= 0 uses DefaultMaxSize; keep < 0 uses DefaultKeep (0 keeps no
// rotated files: the log is truncated at maxSize).
func Open(path string, maxSize int64, keep int) (*Writer, error) {
	if maxSize <= 0 {
		maxSize = DefaultMaxSize
	}
	if keep < 0 {
		keep = DefaultKeep
	}
	w := &Writer{path: path, maxSize: maxSize, keep: keep}
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return nil, fmt.Errorf("logfile: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.openLocked(); err != nil {
		return nil, err
	}
	return w, nil
}

// Path is the active log file.
func (w *Writer) Path() string { return w.path }

func (w *Writer) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, fileMode)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// The directory was removed under us; recreate it.
			if mkErr := os.MkdirAll(filepath.Dir(w.path), dirMode); mkErr == nil {
				f, err = os.OpenFile(w.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, fileMode)
			}
		}
		if err != nil {
			return fmt.Errorf("logfile: %w", err)
		}
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("logfile: %w", err)
	}
	w.f, w.size = f, st.Size()
	return nil
}

// Write appends p, rotating first when p would push the file past
// maxSize. A single write is never split across files.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}
	if w.size > 0 && w.size+int64(len(p)) > w.maxSize {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	if err != nil {
		w.f.Close()
		w.f = nil
		return n, fmt.Errorf("logfile: %w", err)
	}
	return n, nil
}

// rotateLocked shifts path.N to path.N+1 (dropping path.keep) and starts
// a fresh path.
func (w *Writer) rotateLocked() error {
	w.f.Close()
	w.f = nil
	if w.keep == 0 {
		_ = os.Remove(w.path)
	} else {
		_ = os.Remove(w.rotated(w.keep))
		for i := w.keep - 1; i >= 1; i-- {
			_ = os.Rename(w.rotated(i), w.rotated(i+1))
		}
		if err := os.Rename(w.path, w.rotated(1)); err != nil && !errors.Is(err, os.ErrNotExist) {
			// Could not move it aside (e.g. held open elsewhere on
			// Windows): truncate rather than grow without bound.
			_ = os.Truncate(w.path, 0)
		}
	}
	return w.openLocked()
}

func (w *Writer) rotated(i int) string { return fmt.Sprintf("%s.%d", w.path, i) }

// Close closes the current file. Later writes reopen it.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Tee returns a writer that writes to every w, ignoring individual
// failures (unlike io.MultiWriter, which stops at the first). Under a
// Windows service stderr is invalid; the file must still get every line.
func Tee(ws ...io.Writer) io.Writer { return tee(ws) }

type tee []io.Writer

func (t tee) Write(p []byte) (int, error) {
	var firstErr error
	ok := false
	for _, w := range t {
		if _, err := w.Write(p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok = true
	}
	if ok || len(t) == 0 {
		return len(p), nil
	}
	return 0, firstErr
}

// MainLogName is the main log file inside the logs directory.
const MainLogName = "ether.log"

// Files lists the log files present in dir, oldest first: the crash log
// (rotated, then current), then the main log's rotations from the highest
// number down, then the active main log.
func Files(dir string) []string {
	var names []string
	names = append(names, CrashLogName+".1", CrashLogName)
	for i := 64; i >= 1; i-- {
		names = append(names, fmt.Sprintf("%s.%d", MainLogName, i))
	}
	names = append(names, MainLogName)
	var out []string
	for _, n := range names {
		p := filepath.Join(dir, n)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			out = append(out, p)
		}
	}
	return out
}
