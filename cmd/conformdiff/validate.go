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
	spec := f.String("spec", "", "cached OpenAPI or Swagger JSON file")
	fixture := f.String("fixture", "", "recorded response fixture")
	method := f.String("method", "GET", "HTTP method")
	path := f.String("path", "", "request path")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *spec == "" || *fixture == "" || *path == "" {
		return fmt.Errorf("validate requires -spec, -fixture and -path")
	}
	b, err := os.ReadFile(*spec)
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
