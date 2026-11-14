package main

import (
	"github.com/braedonsaunders/ether/internal/plugins/devreference"
	"os"
	"path/filepath"
)

func main() {
	root, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			panic("repository not found")
		}
		root = parent
	}
	b, err := devreference.Generate(root)
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile(filepath.Join(root, "web/src/pages/plugins/developer-reference.json"), append(b, '\n'), 0644); err != nil {
		panic(err)
	}
	short, full, err := devreference.MachineReference(root, b)
	if err != nil {
		panic(err)
	}
	dest := filepath.Join(root, "web/public/plugins/reference")
	if err := os.MkdirAll(dest, 0755); err != nil {
		panic(err)
	}
	for name, content := range map[string][]byte{"llms.txt": short, "llms-full.txt": full} {
		if err := os.WriteFile(filepath.Join(dest, name), content, 0644); err != nil {
			panic(err)
		}
	}
}
