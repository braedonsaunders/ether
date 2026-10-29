package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/braedonsaunders/ether/internal/conform"
)

// checkDefects replays annotated reference probes without changing committed fixtures.
func checkDefects(p conform.Protocol, vars map[string]string, base, token, client string, timeout time.Duration, concurrency int) error {
	dir, err := os.MkdirTemp("", "conformdiff-defects-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	report, err := record(p, vars, base, token, dir, client, "/stream", timeout, concurrency)
	if err != nil {
		return err
	}
	if report.Blocking() > 0 {
		return fmt.Errorf("defect probes failed: %d request errors", report.Blocking())
	}
	checked, open := 0, 0
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() || filepath.Ext(path) != ".json" || d.Name() == "manifest.json" || d.Name() == "_discovery.json" {
			return nil
		}
		r, e := conform.LoadFixture(path)
		if e != nil {
			return e
		}
		if r.RangeKind != "suffix" {
			return nil
		}
		checked++
		if r.ReferenceDefect != nil {
			open++
			fmt.Printf("OPEN %s: %s (%s)\n", r.ReferenceDefect.ID, r.ReferenceDefect.Detail, r.ReferenceDefect.Observed)
			return nil
		}
		if e = conform.ValidateBinary(r); e != nil {
			return fmt.Errorf("unexpected invalid reference suffix range: %w", e)
		}
		fmt.Println("RESOLVED emby-suffix-range-ignored: suffix range is valid")
		return nil
	})
	if err != nil {
		return err
	}
	if checked == 0 {
		return fmt.Errorf("no annotated suffix probes were exercised")
	}
	fmt.Printf("Checked %d suffix probe(s); %d reference defect(s) open.\n", checked, open)
	return nil
}
