package frontend

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Plex still imports domain packages from the playback files owned by the
// broker cutover. Until those imports leave, the package cannot use the
// frontends-only rule above. This ratchet rejects any new internal import.
func TestPlexProductionImportsStayInsideRecordedBoundary(t *testing.T) {
	allowed := map[string]bool{
		"github.com/braedonsaunders/ether/internal/artwork":         true,
		"github.com/braedonsaunders/ether/internal/artwork/service": true,
		"github.com/braedonsaunders/ether/internal/auth":            true,
		"github.com/braedonsaunders/ether/internal/clusterbus":      true,
		"github.com/braedonsaunders/ether/internal/config":          true,
		"github.com/braedonsaunders/ether/internal/conform":         true,
		"github.com/braedonsaunders/ether/internal/core":            true,
		"github.com/braedonsaunders/ether/internal/diagnostics":     true,
		"github.com/braedonsaunders/ether/internal/discovery":       true,
		"github.com/braedonsaunders/ether/internal/episodeorder":    true,
		"github.com/braedonsaunders/ether/internal/experience":      true,
		"github.com/braedonsaunders/ether/internal/extras":          true,
		"github.com/braedonsaunders/ether/internal/faillog":         true,
		"github.com/braedonsaunders/ether/internal/frontend":        true,
		"github.com/braedonsaunders/ether/internal/identity":        true,
		"github.com/braedonsaunders/ether/internal/langtag":         true,
		"github.com/braedonsaunders/ether/internal/livetv":          true,
		"github.com/braedonsaunders/ether/internal/lru":             true,
		"github.com/braedonsaunders/ether/internal/markers":         true,
		"github.com/braedonsaunders/ether/internal/plex/model":      true,
		"github.com/braedonsaunders/ether/internal/plexguid":        true,
		"github.com/braedonsaunders/ether/internal/sessions":        true,
		"github.com/braedonsaunders/ether/internal/state":           true,
		"github.com/braedonsaunders/ether/internal/store":           true,
		"github.com/braedonsaunders/ether/internal/subtitles":       true,
		"github.com/braedonsaunders/ether/internal/tmdb":            true,
		"github.com/braedonsaunders/ether/internal/trackprefs":      true,
		"github.com/braedonsaunders/ether/internal/transcode":       true,
		"github.com/braedonsaunders/ether/internal/trickplay":       true,
	}
	root := filepath.Join("..", "plex")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		const prefix = "github.com/braedonsaunders/ether/internal/"
		for _, imp := range f.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(name, prefix) && !allowed[name] {
				t.Errorf("%s imports %s, which is outside the recorded Plex boundary", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Frontend implementations use the host and kit boundaries rather than
// reaching into Ether's domain implementation packages. contentrating is
// allowed because it is a pure age-level model (stdlib only), the same
// table the catalogue stores; ids stays under frontend/kit because that
// helper exists for the wire format.
func TestFrontendImports(t *testing.T) {
	root := filepath.Join("..", "frontends")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return
	} else if err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		const prefix = "github.com/braedonsaunders/ether/internal/"
		allowed := map[string]bool{
			prefix + "frontend": true,
			// Pure certification model: no services and no I/O.
			prefix + "contentrating": true,
		}
		for _, imp := range f.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(name, prefix) && !allowed[name] && !strings.HasPrefix(name, prefix+"frontend/kit/") {
				t.Errorf("%s imports forbidden internal package %s", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Cheap adapters prove that new surfaces fit the host boundary without growing
// their own domain orchestration. Count all production lines, including comments.
func TestCheapSurfaceSize(t *testing.T) {
	for _, protocol := range []string{"stremio", "webdav", "m3u", "dlna"} {
		files, err := filepath.Glob(filepath.Join("..", "frontends", protocol, "*.go"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: missing adapter: %v", protocol, err)
		}
		lines := 0
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines += strings.Count(string(data), "\n")
		}
		if lines > 1500 {
			t.Errorf("%s has %d production lines; keep the adapter within 1500", protocol, lines)
		}
	}
}
