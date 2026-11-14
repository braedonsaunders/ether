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
}
