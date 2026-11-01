package main

import (
	"math"
)

// TTFFObservation is a measured client Play-to-picture event. Phase intervals
// are independent evidence and can overlap; absence is not treated as zero.
type TTFFObservation struct {
	ID             string   `json:"id"`
	CacheState     string   `json:"cache_state"` // unresolved or cached
	FirstFrameMS   *float64 `json:"first_frame_ms"`
	ResolveMS      *float64 `json:"resolve_ms,omitempty"`
	ProbeMS        *float64 `json:"probe_ms,omitempty"`
	DecisionMS     *float64 `json:"decision_ms,omitempty"`
	FirstByteMS    *float64 `json:"first_byte_ms,omitempty"`
	FirstSegmentMS *float64 `json:"first_segment_ms,omitempty"`
	Delivery       string   `json:"delivery"` // direct or hls
	SourceVerified bool     `json:"source_verified"`
	Completed      bool     `json:"completed"`
}
type TTFFGate struct {
	ID              string   `json:"id"`
	CacheState      string   `json:"cache_state"`
	Status          string   `json:"status"`
	TargetMS        float64  `json:"target_ms,omitempty"`
	StretchTargetMS float64  `json:"stretch_target_ms,omitempty"`
	FirstFrameMS    *float64 `json:"first_frame_ms,omitempty"`
	PhaseStatus     string   `json:"phase_status"`
	ResolveMS       *float64 `json:"resolve_ms,omitempty"`
	ProbeMS         *float64 `json:"probe_ms,omitempty"`
	DecisionMS      *float64 `json:"decision_ms,omitempty"`
	FirstByteMS     *float64 `json:"first_byte_ms,omitempty"`
	FirstSegmentMS  *float64 `json:"first_segment_ms,omitempty"`
	Reason          string   `json:"reason,omitempty"`
}

func validDuration(p *float64) bool {
	return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) && *p >= 0
}
func evaluateTTFF(o TTFFObservation) TTFFGate {
	r := TTFFGate{ID: o.ID, CacheState: o.CacheState, Status: "inconclusive", PhaseStatus: "incomplete", FirstFrameMS: o.FirstFrameMS, ResolveMS: o.ResolveMS, ProbeMS: o.ProbeMS, DecisionMS: o.DecisionMS, FirstByteMS: o.FirstByteMS, FirstSegmentMS: o.FirstSegmentMS}
	switch o.CacheState {
	case "unresolved":
		r.TargetMS = 5000
		r.StretchTargetMS = 4000
	case "cached":
		r.TargetMS = 1000
	default:
		r.Reason = "cache state must be evidenced as unresolved or cached"
		return r
	}
	if o.ID == "" || !o.Completed || !o.SourceVerified || !validDuration(o.FirstFrameMS) {
		r.Reason = "completed playback, verified source and first-frame timing are required"
		return r
	}
	if o.Delivery != "direct" && o.Delivery != "hls" {
		r.Reason = "delivery method is unknown"
		return r
	}
	for _, p := range []*float64{o.ResolveMS, o.ProbeMS, o.DecisionMS, o.FirstByteMS, o.FirstSegmentMS} {
		if p != nil && !validDuration(p) {
			r.Reason = "invalid phase duration"
			return r
		}
	}
	if validDuration(o.ResolveMS) && validDuration(o.ProbeMS) && validDuration(o.DecisionMS) && validDuration(o.FirstByteMS) && (o.Delivery == "direct" || validDuration(o.FirstSegmentMS)) {
		r.PhaseStatus = "complete"
	}
	if *o.FirstFrameMS > r.TargetMS {
		r.Status = "fail"
		r.Reason = "first frame exceeds absolute target"
	} else {
		r.Status = "pass"
	}
	return r
}

type PerformancePair struct {
	ID              string   `json:"id"`
	Metric          string   `json:"metric"`
	Unit            string   `json:"unit"`
	Reference       *float64 `json:"reference"`
	Ether           *float64 `json:"ether"`
	MatchedWorkload bool     `json:"matched_workload"`
	SourceVerified  bool     `json:"source_verified"`
	Completed       bool     `json:"completed"`
	QualityVerified bool     `json:"quality_verified"`
}
type PerformanceGate struct {
	ID         string   `json:"id"`
	Metric     string   `json:"metric"`
	Status     string   `json:"status"`
	Difference *float64 `json:"ether_minus_reference,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

func evaluatePerformancePair(p PerformancePair) PerformanceGate {
	r := PerformanceGate{ID: p.ID, Metric: p.Metric, Status: "inconclusive"}
	units := map[string]string{"transcode_start": "ms", "seek_to_picture": "ms", "resume": "ms", "subtitle_switch": "ms", "audio_switch": "ms", "browse_latency": "ms", "cpu_per_stream_minute": "cpu_seconds"}
	if p.ID == "" || units[p.Metric] == "" || p.Unit != units[p.Metric] || !p.MatchedWorkload || !p.SourceVerified || !p.Completed || !p.QualityVerified || !validDuration(p.Reference) || !validDuration(p.Ether) {
		r.Reason = "matched completed and quality-verified measurement is required"
		return r
	}
	diff := *p.Ether - *p.Reference
	r.Difference = &diff
	if diff > 0 {
		r.Status = "fail"
		r.Reason = "Ether exceeds reference in this matched observation"
	} else {
		r.Status = "pass"
		r.Reason = "this observation meets the reference; repetitions and uncertainty still required"
	}
	return r
}
