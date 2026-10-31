package main

import (
	"testing"
	"time"
)

func passManifest(t *testing.T) SoakManifest {
	t.Helper()
	start := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	faultEnd := start.Add(time.Minute)
	recovered := faultEnd.Add(3 * time.Second)
	followEnd := recovered.Add(5*time.Minute + 10*time.Second)
	m := SoakManifest{
		Profile: "shakedown", Build: "abc", Started: start, FaultEnd: faultEnd, FollowupEnd: followEnd, LoadStopped: followEnd,
		GPU:      "no_device",
		Baseline: SoakSample{At: start, RSSBytes: 100 << 20, FDs: 10, Goroutines: 8, GPU: "no_device", Valid: true},
		Faults: []SoakFault{
			{Action: "source_outage", At: start, OK: true},
			{Action: "slow_upstream", At: start, OK: true},
			{Action: "encoder_crash", At: start, OK: true, Signaled: 1},
			{Action: "encoder_kill", At: start, OK: true, Signaled: 1},
			{Action: "db_stall", At: start, OK: true},
		},
	}
	for _, op := range []SoakOp{
		{Protocol: "plex", Kind: "browse", Delivery: "browse", Client: "w0", Success: true, Verified: true},
		{Protocol: "plex", Kind: "direct", Delivery: "direct", Client: "w0", Success: true, Verified: true},
		{Protocol: "plex", Kind: "remux", Delivery: "hls", Client: "w0", Success: true, Verified: true},
		{Protocol: "plex", Kind: "transcode", Delivery: "hls", Client: "w0", Success: true, Verified: true},
		{Protocol: "plex", Kind: "browse", Delivery: "browse", Client: "w1", Success: true, Verified: true},
		{Protocol: "jellyfin", Kind: "browse", Delivery: "browse", Client: "w1", Success: true, Verified: true},
		{Protocol: "emby", Kind: "browse", Delivery: "browse", Client: "w2", Success: true, Verified: true},
	} {
		op.At = start.Add(10 * time.Second)
		m.Ops = append(m.Ops, op)
	}
	m.Ops = append(m.Ops,
		SoakOp{At: faultEnd.Add(time.Second), Protocol: "plex", Kind: "direct", Delivery: "direct", Client: "recover-a", Success: true, Verified: true},
		SoakOp{At: faultEnd.Add(2 * time.Second), Protocol: "plex", Kind: "direct", Delivery: "direct", Client: "recover-b", Success: true, Verified: true},
		SoakOp{At: recovered, Protocol: "plex", Kind: "direct", Delivery: "direct", Client: "recover-c", Success: true, Verified: true},
	)
	for at := recovered.Add(20 * time.Second); !at.After(followEnd); at = at.Add(20 * time.Second) {
		m.Ops = append(m.Ops, SoakOp{At: at, Protocol: "plex", Kind: "browse", Delivery: "browse", Client: "follow", Success: true, Verified: true})
	}
	for at := start; !at.After(followEnd.Add(5*time.Minute + 10*time.Second)); at = at.Add(20 * time.Second) {
		m.Samples = append(m.Samples, SoakSample{At: at, RSSBytes: 100 << 20, FDs: 10, Goroutines: 8, GPU: "no_device", Valid: true, NetRx: uint64(at.Sub(start) / time.Second)})
	}
	return m
}

func TestEvaluateSoakShakedownPass(t *testing.T) {
	report := EvaluateSoak(passManifest(t))
	if report.Status != "pass" || report.SustainedClaimed {
		t.Fatalf("status %s claimed %v gates %+v", report.Status, report.SustainedClaimed, report.Gates)
	}
	for _, gate := range report.Gates {
		if gate.Name == "rss_sustained" && gate.Status != "not_run" {
			t.Fatalf("sustained gate: %+v", gate)
		}
		if gate.Status == "fail" || gate.Status == "inconclusive" {
			t.Fatalf("gate: %+v", gate)
		}
	}
}

func TestEvaluateSoakUnexpectedError(t *testing.T) {
	m := passManifest(t)
	m.Ops = append(m.Ops, SoakOp{At: m.Started.Add(time.Second), Protocol: "plex", Kind: "browse", Client: "w0"})
	report := EvaluateSoak(m)
	if report.Status != "fail" {
		t.Fatalf("status %s", report.Status)
	}
}

func TestEvaluateSoakLeak(t *testing.T) {
	m := passManifest(t)
	for i := range m.Samples {
		if !m.Samples[i].At.Before(m.FaultEnd) {
			m.Samples[i].RSSBytes += uint64(m.Samples[i].At.Sub(m.FaultEnd)/time.Minute) * 2 << 20
		}
	}
	report := EvaluateSoak(m)
	found := false
	for _, gate := range report.Gates {
		if gate.Name == "rss_leak" {
			found = gate.Status == "fail"
		}
	}
	if report.Status != "fail" || !found {
		t.Fatalf("leak not failed: %+v", report.Gates)
	}
}
