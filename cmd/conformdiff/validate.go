package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/openapi"
)

// validateFixture validates one recorded response against a locally cached spec.
func validateFixture(args []string) error {
	f := flag.NewFlagSet("validate", flag.ContinueOnError)
	spec := f.String("spec", "", "OpenAPI or Swagger JSON file")
	protocol := f.String("protocol", "", "protocol whose pinned cached spec should be used")
	cache := f.String("cache", "", "spec cache directory")
	manifest := f.String("manifest", "internal/conform/specs/manifest.json", "pinned spec manifest")
	fixture := f.String("fixture", "", "recorded response fixture")
	method := f.String("method", "GET", "HTTP method")
	path := f.String("path", "", "request path")
	if err := f.Parse(args); err != nil {
		return err
	}
	if (*spec == "" && *protocol == "") || *fixture == "" || *path == "" {
		return fmt.Errorf("validate requires -protocol or -spec, plus -fixture and -path")
	}
	var b []byte
	var err error
	if *protocol != "" {
		if *spec != "" {
			return fmt.Errorf("choose -protocol or -spec")
		}
		b, err = openapi.CachedSpec(*manifest, *protocol, *cache)
	} else {
		b, err = os.ReadFile(*spec)
	}
	if err != nil {
		return err
	}
	d, err := openapi.Parse(b)
	if err != nil {
		return err
	}
	r, err := conform.LoadFixture(*fixture)
	if err != nil {
		return err
	}
	return d.Validate(*method, *path, r.Status, r.Headers.Get("Content-Type"), r.Body)
}
