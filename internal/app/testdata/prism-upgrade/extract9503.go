// Command extract9503 writes the SQLite migrations registered at delivered
// commit 9503ece9. It parses that commit's non-test Go sources; it does not
// compile Ether and it does not label the current binary's schema as old.
//
//	go run ./internal/app/testdata/prism-upgrade/extract9503.go
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const delivered9503 = "9503ece9d544e5f111f2afa531e87a2c3ad0e6c1"

const migrationEnd = "-- prism-upgrade-migration-end"

type step struct {
	Component string `json:"component"`
	Version   int    `json:"version"`
	SQL       string `json:"-"`
}

func main() {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		fatal(err)
	}
	repo := strings.TrimSpace(string(root))
	files := listGo(repo)
	fset := token.NewFileSet()
	parsed := make([]*ast.File, 0, len(files))
	dirs := make([]string, 0, len(files))
	consts := map[string]map[string]string{}
	for _, name := range files {
		src, err := gitShow(repo, name)
		if err != nil {
			fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			fatal(fmt.Errorf("%s: %w", name, err))
		}
		dir := filepath.ToSlash(filepath.Dir(name))
		if consts[dir] == nil {
			consts[dir] = map[string]string{}
		}
		collectConsts(f, consts[dir])
		parsed = append(parsed, f)
		dirs = append(dirs, dir)
	}
	var steps []step
	for i, f := range parsed {
		steps = append(steps, registers(f, dirs[i], consts)...)
	}
	sort.Slice(steps, func(i, j int) bool {
		if steps[i].Component != steps[j].Component {
			return steps[i].Component < steps[j].Component
		}
		return steps[i].Version < steps[j].Version
	})
	for i, s := range steps {
		if strings.Contains(s.SQL, migrationEnd) {
			fatal(fmt.Errorf("%s/%d contains the fixture sentinel", s.Component, s.Version))
		}
		if i > 0 && steps[i-1].Component == s.Component && steps[i-1].Version == s.Version {
			fatal(fmt.Errorf("duplicate %s/%d", s.Component, s.Version))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "-- source %s\n", delivered9503)
	fmt.Fprintf(&b, "-- order component then version, matching 9503 store.registered (alphabetical; RegisterAfter is absent)\n")
	for _, s := range steps {
		fmt.Fprintf(&b, "-- migration %s %d\n%s\n%s\n", s.Component, s.Version, strings.TrimSpace(s.SQL), migrationEnd)
	}
	out := b.String()
	sum := sha256.Sum256([]byte(out))
	dir := filepath.Join(repo, "internal", "app", "testdata", "prism-upgrade")
	if err := os.WriteFile(filepath.Join(dir, "schema.sql"), []byte(out), 0o644); err != nil {
		fatal(err)
	}
	manifest := struct {
		Commit     string `json:"commit"`
		Order      string `json:"order"`
		SHA256     string `json:"sha256"`
		Migrations []step `json:"migrations"`
	}{
		Commit:     delivered9503,
		Order:      "alphabetical component, then version; 9503 has no RegisterAfter",
		SHA256:     hex.EncodeToString(sum[:]),
		Migrations: steps,
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		fatal(err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), raw, 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %d migrations from %s\n", len(steps), delivered9503)
}

func listGo(repo string) []string {
	cmd := exec.Command("git", "grep", "-l", "store.Register(", delivered9503, "--", "*.go")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		fatal(err)
	}
	var files []string
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if i := strings.IndexByte(name, ':'); i >= 0 {
			name = name[i+1:]
		}
		if strings.HasSuffix(name, "_test.go") || name == "" {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)
	return files
}

func gitShow(repo, name string) ([]byte, error) {
	cmd := exec.Command("git", "show", delivered9503+":"+name)
	cmd.Dir = repo
	return cmd.Output()
}

func collectConsts(f *ast.File, dst map[string]string) {
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || (g.Tok != token.CONST && g.Tok != token.VAR) {
			continue
		}
		for _, spec := range g.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			if s, ok := stringLit(vs.Values[0]); ok {
				dst[vs.Names[0].Name] = s
			}
		}
	}
}

func registers(f *ast.File, dir string, consts map[string]map[string]string) []step {
	var out []step
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 3 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Register" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "store" {
			return true
		}
		comp, ok := stringLit(call.Args[0])
		if !ok {
			fatal(fmt.Errorf("%s: non-literal component", f.Name.Name))
		}
		ver, ok := intLit(call.Args[1])
		if !ok {
			fatal(fmt.Errorf("%s: non-literal version for %s", f.Name.Name, comp))
		}
		sql, ok := stringLit(call.Args[2])
		if !ok {
			id, isIdent := call.Args[2].(*ast.Ident)
			if !isIdent {
				fatal(fmt.Errorf("%s: %s/%d sql is not a string", f.Name.Name, comp, ver))
			}
			sql, ok = consts[dir][id.Name]
			if !ok {
				fatal(fmt.Errorf("%s: unresolved %s for %s/%d", f.Name.Name, id.Name, comp, ver))
			}
		}
		out = append(out, step{Component: comp, Version: ver, SQL: sql})
		return true
	})
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		fatal(err)
	}
	return s, true
}

func intLit(e ast.Expr) (int, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.INT {
		return 0, false
	}
	n, err := strconv.Atoi(lit.Value)
	if err != nil {
		fatal(err)
	}
	return n, true
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
