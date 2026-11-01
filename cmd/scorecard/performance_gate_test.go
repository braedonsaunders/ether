package main

import "testing"

func TestTTFFAbsoluteTargetsAndMissingEvidence(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	o := TTFFObservation{ID: "cold", CacheState: "unresolved", FirstFrameMS: f(5000), Completed: true, SourceVerified: true, Delivery: "direct"}
	if g := evaluateTTFF(o); g.Status != "pass" || g.TargetMS != 5000 || g.StretchTargetMS != 4000 || g.PhaseStatus != "incomplete" {
		t.Fatalf("cold: %+v", g)
	}
	o.FirstFrameMS = f(5000.1)
	if g := evaluateTTFF(o); g.Status != "fail" {
		t.Fatalf("cold over: %+v", g)
	}
	o.CacheState = "cached"
	o.FirstFrameMS = f(1000)
	o.ResolveMS = f(0)
	o.ProbeMS = f(0)
	o.DecisionMS = f(1)
	o.FirstByteMS = f(2)
	if g := evaluateTTFF(o); g.Status != "pass" || g.PhaseStatus != "complete" {
		t.Fatalf("warm: %+v", g)
	}
	o.FirstFrameMS = f(1001)
	if g := evaluateTTFF(o); g.Status != "fail" {
		t.Fatalf("warm over: %+v", g)
	}
	o.SourceVerified = false
	if g := evaluateTTFF(o); g.Status != "inconclusive" {
		t.Fatalf("unverified: %+v", g)
	}
	o.SourceVerified = true
	o.Delivery = "hls"
	o.FirstFrameMS = f(500)
	if g := evaluateTTFF(o); g.PhaseStatus != "incomplete" {
		t.Fatalf("missing segment: %+v", g)
	}
}

func TestPerformancePairRequiresEquivalentSuccessfulQuality(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	p := PerformancePair{ID: "seek-1", Metric: "seek_to_picture", Unit: "ms", Reference: f(100), Ether: f(101), MatchedWorkload: true, SourceVerified: true, Completed: true, QualityVerified: true}
	if g := evaluatePerformancePair(p); g.Status != "fail" || *g.Difference != 1 {
		t.Fatalf("loss: %+v", g)
	}
	p.Ether = f(100)
	if g := evaluatePerformancePair(p); g.Status != "pass" {
		t.Fatalf("tie: %+v", g)
	}
	p.QualityVerified = false
	if g := evaluatePerformancePair(p); g.Status != "inconclusive" {
		t.Fatalf("unverified: %+v", g)
	}
}
