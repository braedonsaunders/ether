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

// Frontend implementations use the host and kit boundaries rather than
// reaching into Ether's domain implementation packages.
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
		for _, imp := range f.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(name, prefix) && name != prefix+"frontend" && !strings.HasPrefix(name, prefix+"frontend/kit/") {
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
