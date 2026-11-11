package plex

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// accessorFuncs are the only functions allowed to read the raw Deps fields.
// Production mounts the Plex server with NewServer(Deps{Host: host}) so
// Playback, Transcoder and DB are nil; every handler must reach them through
// these accessors, which fall back to the session runtime on Host.
var accessorFuncs = map[string]bool{
	"playback":   true,
	"transcoder": true,
	"runtimeDB":  true,
}

// TestProductionWiringHostIsTheOnlyPath fails if any non-test file in the Plex
// package reads s.d.Playback, s.d.Transcoder or s.d.DB outside the accessor
// functions. Such a read passes against the old direct-Deps test fixture but
// breaks every real client, because production wires only Deps.Host (#432).
func TestProductionWiringHostIsTheOnlyPath(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || accessorFuncs[fn.Name.Name] {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name != "Playback" && sel.Sel.Name != "Transcoder" && sel.Sel.Name != "DB" {
					return true
				}
				inner, ok := sel.X.(*ast.SelectorExpr)
				if !ok || inner.Sel.Name != "d" {
					return true
				}
				pos := fset.Position(sel.Pos())
				t.Errorf("%s:%d: %s reads %s.%s directly; use playback(), transcoder() or runtimeDB() so production wiring (Deps{Host}) is tested",
					name, pos.Line, fn.Name.Name, exprString(inner.X), sel.Sel.Name)
				return true
			})
		}
	}
}

func exprString(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return exprString(v.X) + "." + v.Sel.Name
	default:
		return "?"
	}
}

// TestFixtureUsesProductionWiring asserts the shared fixture does not populate
// Deps.Playback, Deps.Transcoder or Deps.DB: the server must reach playback,
// transcoding and the store through the session runtime on Host.
func TestFixtureUsesProductionWiring(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	if f.srv.d.Playback != nil || f.srv.d.Transcoder != nil || f.srv.d.DB != nil {
		t.Fatalf("fixture wired direct Deps: playback=%v transcoder=%v db=%v",
			f.srv.d.Playback != nil, f.srv.d.Transcoder != nil, f.srv.d.DB != nil)
	}
	if f.srv.playback() == nil {
		t.Fatal("playback not reachable through the session runtime")
	}
	if f.srv.transcoder() == nil {
		t.Fatal("transcoder not reachable through the session runtime")
	}
	if f.srv.playback() != f.srv.testRuntime().currentPlayback() {
		t.Fatal("playback does not resolve to the runtime's value")
	}
	if f.srv.transcoder() != f.srv.testRuntime().currentTranscoder() {
		t.Fatal("transcoder does not resolve to the runtime's value")
	}
}
