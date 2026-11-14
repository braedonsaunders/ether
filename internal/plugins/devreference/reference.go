// Package devreference generates the console's plugin API reference from source.
package devreference

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

//go:generate go run ./cmd
type Section struct {
	Path string `json:"path"`
	Code string `json:"code"`
}

// Generate preserves source comments, signatures and JSON tags rather than
// maintaining a second description of the protocol.
func Generate(root string) ([]byte, error) {
	paths := []string{"schemas/plugin-manifest.v1.json", "sdk/typescript/README.md", "internal/plugins/contract.go", "internal/core/kinditem.go", "internal/plugins/hosted.go", "internal/plugins/manifest.go", "internal/plugins/manifest_kinds.go", "internal/plugins/descriptors.go", "internal/plugins/host.go", "internal/plugins/host_kinds.go", "internal/plugins/hash_jobs.go", "internal/plugins/events.go", "internal/plugins/settings_public.go", "internal/plugins/setup.go", "internal/plugins/wasm/js.go", "internal/plugins/wasm/wasm.go", "internal/plugins/remote/host.go", "internal/plugins/studio/templates.go", "internal/plugins/studio/mcp_tools.go", "docs/plugin-kinds.md", "docs/plugin-debrid.md", "sdk/catalogue-template/README.md"}
	// Discover SDK additions automatically. Exclude build products and tests: this
	// corpus is the shipped authoring contract, not a copy of the repository.
	for _, base := range []string{"sdk/go", "sdk/typescript/src", "sdk/python", "sdk/examples", "internal/plugins/kit"} {
		err := filepath.WalkDir(filepath.Join(root, base), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "node_modules" || d.Name() == "dist" || d.Name() == "__pycache__" || d.Name() == "tests" {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if strings.Contains(name, "test") || strings.HasSuffix(name, ".d.ts") {
				return nil
			}
			ext := filepath.Ext(name)
			if ext == ".go" || ext == ".ts" || ext == ".py" || ext == ".md" || name == "manifest.json" {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				paths = append(paths, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(paths)

	var sections []Section
	for _, path := range paths {
		b, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return nil, err
		}
		if path == "schemas/plugin-manifest.v1.json" {
			table, err := schemaFields(b)
			if err != nil {
				return nil, err
			}
			sections = append(sections, Section{path + ".fields.md", table})
		}
		// Go contract declarations are extracted with the Go parser. Include all
		// declarations (including validators) so permission and contribution rules
		// remain inspectable alongside the public types.
		if filepath.Ext(path) == ".go" {
			fs := token.NewFileSet()
			f, err := parser.ParseFile(fs, path, b, parser.ParseComments)
			if err != nil {
				return nil, err
			}
			index := 0
			for _, decl := range f.Decls {
				if _, ok := decl.(*ast.GenDecl); ok {
					g := decl.(*ast.GenDecl)
					if g.Tok == token.IMPORT {
						continue
					}
				}
				start := decl.Pos()
				switch d := decl.(type) {
				case *ast.GenDecl:
					if d.Doc != nil {
						start = d.Doc.Pos()
					}
				case *ast.FuncDecl:
					if d.Doc != nil {
						start = d.Doc.Pos()
					}
				}
				name := fmt.Sprint(index)
				switch d := decl.(type) {
				case *ast.FuncDecl:
					name = d.Name.Name
					if d.Recv != nil && len(d.Recv.List) > 0 {
						r := d.Recv.List[0].Type
						name = string(b[fs.Position(r.Pos()).Offset:fs.Position(r.End()).Offset]) + "." + name
					}
				case *ast.GenDecl:
					if len(d.Specs) > 0 {
						switch spec := d.Specs[0].(type) {
						case *ast.TypeSpec:
							name = spec.Name.Name
						case *ast.ValueSpec:
							if len(spec.Names) > 0 {
								name = spec.Names[0].Name
							}
						}
					}
				}
				// Present JSON field names and optionality alongside exact Go types.
				if g, ok := decl.(*ast.GenDecl); ok {
					for _, spec := range g.Specs {
						if ts, ok := spec.(*ast.TypeSpec); ok {
							if st, ok := ts.Type.(*ast.StructType); ok {
								var table strings.Builder
								table.WriteString("| Field | Go type | Wire presence | Description |\n|---|---|---|---|\n")
								for _, f := range st.Fields.List {
									if f.Tag == nil {
										continue
									}
									tag, err := strconv.Unquote(f.Tag.Value)
									if err != nil {
										return nil, err
									}
									jt := reflect.StructTag(tag).Get("json")
									if jt == "" || jt == "-" {
										continue
									}
									parts := strings.Split(jt, ",")
									presence := "emitted (validation may require a value)"
									if len(parts) > 1 {
										presence = "may be omitted (see validation for conditions)"
									}
									doc := strings.TrimSpace(f.Doc.Text() + f.Comment.Text())
									doc = strings.ReplaceAll(strings.ReplaceAll(doc, "\n", " "), "|", "\\|")
									fmt.Fprintf(&table, "| `%s` | `%s` | %s | %s |\n", parts[0], string(b[fs.Position(f.Type.Pos()).Offset:fs.Position(f.Type.End()).Offset]), presence, doc)
								}
								sections = append(sections, Section{path + "#" + ts.Name.Name + ".fields.md", table.String()})
							}
						}
					}
				}
				sections = append(sections, Section{path + "#" + name, string(b[fs.Position(start).Offset:fs.Position(decl.End()).Offset])})
				index++
			}
			continue
		}
		sections = append(sections, Section{path, string(b)})
	}
	return json.MarshalIndent(sections, "", "  ")
}
