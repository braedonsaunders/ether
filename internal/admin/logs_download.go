package admin

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/logfile"
	"github.com/braedonsaunders/ether/internal/logredact"
)

// handleLogsDownload streams this node's logs as one text file for a bug
// report: the rotated log files oldest first (or the in-memory buffer when
// there are none), every line passed through the credential redactor. It
// never includes config.json, the credential key or the database.
func (h *Handler) handleLogsDownload(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ether-logs-`+now.Format("20060102-150405")+`.txt"`)
	bw := bufio.NewWriterSize(w, 64<<10)
	defer bw.Flush()
	fmt.Fprintf(bw, "# Ether %s on %s/%s, logs captured %s\n", h.version, runtime.GOOS, runtime.GOARCH, now.Format(time.RFC3339))
	fmt.Fprintln(bw, "# Known credential fields are redacted. Review before sharing; unrecognized secrets may remain. Settings and database files are not included.")
	var files []string
	if h.deps.LogDir != "" {
		files = logfile.Files(h.deps.LogDir)
	}
	if len(files) == 0 {
		fmt.Fprintln(bw, "\n===== recent log entries (in memory) =====")
		if h.logs != nil {
			for _, e := range h.logs.Entries(h.logs.Len(), "", "") { // oldest first
				line := e.Time.UTC().Format(time.RFC3339) + " " + e.Level + " " + e.Message
				for _, k := range sortedKeys(e.Attrs) {
					line += " " + k + "=" + fmt.Sprint(e.Attrs[k])
				}
				fmt.Fprintln(bw, logredact.String(line))
			}
		}
		return
	}
	for _, path := range files {
		fmt.Fprintf(bw, "\n===== %s =====\n", filepath.Base(path))
		if err := copyRedacted(bw, path); err != nil {
			fmt.Fprintf(bw, "# could not read %s: %v\n", filepath.Base(path), err)
		}
	}
}

// copyRedacted writes the file at path to w line by line, scrubbed.
func copyRedacted(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	rd := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := rd.ReadString('\n')
		if line != "" {
			if _, werr := io.WriteString(w, logredact.String(strings.TrimSuffix(line, "\n"))+"\n"); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
