package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/braedonsaunders/ether/internal/conform"
	"github.com/braedonsaunders/ether/internal/conform/mediabrowser"
	"github.com/braedonsaunders/ether/internal/conform/silo"
)

func TestRecordingDriftOffline(t *testing.T) {
	p, err := mediabrowser.Load("emby", "../../internal/conform/profiles/emby")
	if err != nil {
		t.Fatal(err)
	}
	a, b := t.TempDir(), t.TempDir()
	for _, profile := range profiles(p, "all") {
		for _, req := range p.Corpus(profile) {
			for _, jm := range p.Formats() {
				r := conform.Response{Status: 200, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"Value":1}`)}
				for _, dir := range []string{a, b} {
					if err := conform.SaveFixture(filepath.Join(dir, profile), p.FixtureName(profile, req, jm), r); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	report, err := recordingDrift(p, a, b)
	if err != nil || len(report.Differences) != 0 {
		t.Fatalf("identical: %+v %v", report, err)
	}
	profile := profiles(p, "all")[0]
	req := p.Corpus(profile)[0]
	name := p.FixtureName(profile, req, p.Formats()[0])
	if err := conform.SaveFixture(filepath.Join(b, profile), name, conform.Response{Status: 403, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"Value":"wrong type"}`)}); err != nil {
		t.Fatal(err)
	}
	report, err = recordingDrift(p, a, b)
	if err != nil || report.Blocking() == 0 {
		t.Fatalf("drift: %+v %v", report, err)
	}
	if err := os.Remove(filepath.Join(b, profile, name+".json")); err != nil {
		t.Fatal(err)
	}
	if _, err := recordingDrift(p, a, b); err == nil {
		t.Fatal("missing fixture accepted")
	}
}

func TestDriftRequiresOfflineDirectories(t *testing.T) {
	if err := drift([]string{"-protocol", "emby", "-fixtures", "http://example.invalid", "-candidate", t.TempDir()}); err == nil {
		t.Fatal("URL accepted as recording")
	}
}

func TestDriftCheckedInCorpora(t *testing.T) {
	for _, protocol := range []string{"jellyfin", "emby", "silo"} {
		t.Run(protocol, func(t *testing.T) {
			var p conform.Protocol
			var err error
			dir := filepath.Join("../../internal/conform/profiles", protocol)
			if protocol == "silo" {
				p, err = silo.Load(dir)
			} else {
				p, err = mediabrowser.Load(protocol, dir)
			}
			if err != nil {
				t.Fatal(err)
			}
			recordings := filepath.Join("../../testdata/conform", protocol)
			report, err := recordingDrift(p, recordings, recordings)
			if err != nil || len(report.Differences) != 0 {
				t.Fatalf("self drift: %+v %v", report, err)
			}
		})
	}
}
