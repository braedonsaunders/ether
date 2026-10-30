package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
	"github.com/braedonsaunders/ether/internal/conform/silo"
)

// drift compares two recordings without discovery, HTTP, redaction or ignored paths.
func drift(args []string) error {
	f := flag.NewFlagSet("drift", flag.ContinueOnError)
	protocol := f.String("protocol", "", "jellyfin|emby|silo")
	profilesDir := f.String("profiles", "", "profile directory")
	before := f.String("fixtures", "", "reviewed recording directory")
	after := f.String("candidate", "", "new recording directory")
	format := f.String("format", "json", "text|json")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *before == "" || *after == "" || (*protocol != "jellyfin" && *protocol != "emby" && *protocol != "silo") || f.NArg() != 0 {
		return fmt.Errorf("drift requires -protocol jellyfin|emby|silo -fixtures DIR -candidate DIR")
	}
	if *format != "json" && *format != "text" {
		return fmt.Errorf("invalid format %q", *format)
	}
	dir := *profilesDir
	if dir == "" {
		dir = filepath.Join("internal", "conform", "profiles", *protocol)
	}
	var p conform.Protocol
	var err error
	if *protocol == "silo" {
		p, err = silo.Load(dir)
	} else {
		p, err = mediabrowser.Load(*protocol, dir)
	}
	if err != nil {
		return err
	}
	report, err := recordingDrift(p, *before, *after)
	if err != nil {
		return err
	}
	emit(os.Stdout, report, *format, false, true)
	if len(report.Differences) != 0 {
		return fmt.Errorf("reference drift: %d differences require review", len(report.Differences))
	}
	return nil
}

func recordingDrift(p conform.Protocol, before, after string) (conform.Report, error) {
	report := conform.Report{}
	count := 0
	for _, profile := range profiles(p, "all") {
		for _, req := range p.Corpus(profile) {
			for _, jm := range p.Formats() {
				name := filepath.Join(profile, p.FixtureName(profile, req, jm)+".json")
				a, err := conform.LoadFixture(filepath.Join(before, name))
				if err != nil {
					return report, fmt.Errorf("reviewed fixture %s: %w", name, err)
				}
				b, err := conform.LoadFixture(filepath.Join(after, name))
				if err != nil {
					return report, fmt.Errorf("candidate fixture %s: %w", name, err)
				}
				if a.Status == 0 || b.Status == 0 {
					return report, fmt.Errorf("invalid fixture status: %s", name)
				}
				var r conform.Report
				if contextual, ok := p.(conform.RequestComparer); ok {
					r = contextual.CompareRequest(req, a, b)
				} else {
					r = p.Compare(a, b)
				}
				r = r.WithRequest(profile, formatName(jm), req.Path)
				report = mergeReports(report, r)
				report.Endpoints = append(report.Endpoints, profile+" "+req.Method+" "+req.Path)
				count++
			}
		}
	}
	if count == 0 {
		return report, fmt.Errorf("empty recording corpus")
	}
	return report, nil
}
