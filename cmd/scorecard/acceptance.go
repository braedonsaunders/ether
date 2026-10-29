package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type recoveryObservation struct {
	At              time.Time `json:"at"`
	Client          string    `json:"client"`
	Success         bool      `json:"success"`
	Verified        bool      `json:"verified"`
	ExpectedFailure bool      `json:"expected_failure"`
}
type recoveryTrial struct {
	ID            string                `json:"id"`
	Complete      bool                  `json:"complete"`
	Offered       int                   `json:"offered"`
	FaultEnd      time.Time             `json:"fault_end"`
	ObservedUntil time.Time             `json:"observed_until"`
	Observations  []recoveryObservation `json:"observations"`
}
type recoveryResult struct {
	Status     string  `json:"status"`
	RecoveryMS float64 `json:"recovery_ms,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

// evaluateRecovery requires three consecutive verified successes by distinct
// clients, a <=30s bound and a fully observed five-minute fault-free follow-up.
func evaluateRecovery(t recoveryTrial) recoveryResult {
	if t.ID == "" || !t.Complete || t.Offered < 1 || t.Offered != len(t.Observations) || t.FaultEnd.IsZero() || t.ObservedUntil.Before(t.FaultEnd.Add(30*time.Second)) {
		return recoveryResult{Status: "inconclusive", Reason: "recovery observation incomplete"}
	}
	rows := append([]recoveryObservation(nil), t.Observations...)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].At.Before(rows[j].At) })
	streak := map[string]bool{}
	var recovered time.Time
	for _, o := range rows {
		if o.At.Before(t.FaultEnd) || o.At.After(t.ObservedUntil) {
			continue
		}
		if !o.Success || !o.Verified || o.Client == "" {
			streak = map[string]bool{}
			continue
		}
		if streak[o.Client] {
			streak = map[string]bool{o.Client: true}
		} else {
			streak[o.Client] = true
		}
		if len(streak) == 3 {
			recovered = o.At
			break
		}
	}
	if recovered.IsZero() {
		return recoveryResult{Status: "fail", Reason: "no three consecutive verified successes"}
	}
	elapsed := recovered.Sub(t.FaultEnd)
	if elapsed > 30*time.Second {
		return recoveryResult{Status: "fail", RecoveryMS: float64(elapsed) / float64(time.Millisecond), Reason: "recovery exceeds 30 seconds"}
	}
	if t.ObservedUntil.Before(recovered.Add(5 * time.Minute)) {
		return recoveryResult{Status: "inconclusive", RecoveryMS: float64(elapsed) / float64(time.Millisecond), Reason: "five-minute follow-up incomplete"}
	}
	last := recovered
	followEnd := recovered.Add(5 * time.Minute)
	for _, o := range rows {
		if !o.At.After(recovered) || o.At.After(followEnd) {
			continue
		}
		if o.At.Sub(last) > 30*time.Second {
			return recoveryResult{Status: "inconclusive", RecoveryMS: float64(elapsed) / float64(time.Millisecond), Reason: "follow-up observation gap"}
		}
		last = o.At
		if !o.Success {
			if o.ExpectedFailure {
				return recoveryResult{Status: "inconclusive", RecoveryMS: float64(elapsed) / float64(time.Millisecond), Reason: "fault-free follow-up contains an expected failure"}
			}
			return recoveryResult{Status: "fail", RecoveryMS: float64(elapsed) / float64(time.Millisecond), Reason: "unexpected failure after recovery"}
		}
		if !o.Verified || o.Client == "" {
			return recoveryResult{Status: "inconclusive", RecoveryMS: float64(elapsed) / float64(time.Millisecond), Reason: "follow-up success lacks source verification or client identity"}
		}
	}
	if followEnd.Sub(last) > 30*time.Second {
		return recoveryResult{Status: "inconclusive", RecoveryMS: float64(elapsed) / float64(time.Millisecond), Reason: "follow-up observation gap"}
	}
	return recoveryResult{Status: "pass", RecoveryMS: float64(elapsed) / float64(time.Millisecond)}
}

type recoveryComparison struct {
	Status       string            `json:"status"`
	ReferenceP95 float64           `json:"reference_p95_ms,omitempty"`
	EtherP95     float64           `json:"ether_p95_ms,omitempty"`
	ReferenceCI  *quantileInterval `json:"reference_ci95,omitempty"`
	EtherCI      *quantileInterval `json:"ether_ci95,omitempty"`
	Reason       string            `json:"reason,omitempty"`
}

func compareRecovery(reference, ether []recoveryTrial, toleranceMS float64) recoveryComparison {
	if toleranceMS < 0 || math.IsNaN(toleranceMS) || math.IsInf(toleranceMS, 0) {
		return recoveryComparison{Status: "inconclusive", Reason: "invalid predeclared tolerance"}
	}
	if len(reference) != len(ether) || len(reference) < 211 {
		return recoveryComparison{Status: "inconclusive", Reason: "matched recovery trials require at least 211 observations for p95 uncertainty"}
	}
	ref, eth := make([]float64, 0, len(reference)), make([]float64, 0, len(ether))
	seen := map[string]bool{}
	refTimes, ethTimes := map[time.Time]bool{}, map[time.Time]bool{}
	for i := range reference {
		if reference[i].ID == "" || reference[i].ID != ether[i].ID || seen[reference[i].ID] || refTimes[reference[i].FaultEnd] || ethTimes[ether[i].FaultEnd] {
			return recoveryComparison{Status: "inconclusive", Reason: "trial identities or fault times do not establish distinct matched trials"}
		}
		seen[reference[i].ID] = true
		refTimes[reference[i].FaultEnd] = true
		ethTimes[ether[i].FaultEnd] = true
		a, b := evaluateRecovery(reference[i]), evaluateRecovery(ether[i])
		if a.Status != "pass" || b.Status != "pass" {
			status := "inconclusive"
			if a.Status == "pass" && b.Status == "fail" {
				status = "reference"
			} else if a.Status == "fail" && b.Status == "pass" {
				status = "ether"
			}
			return recoveryComparison{Status: status, Reason: fmt.Sprintf("trial %d: reference=%s ether=%s", i, a.Status, b.Status)}
		}
		ref = append(ref, a.RecoveryMS)
		eth = append(eth, b.RecoveryMS)
	}
	sort.Float64s(ref)
	sort.Float64s(eth)
	a, b := quantileCI(ref, .95), quantileCI(eth, .95)
	r := recoveryComparison{ReferenceP95: percentile(ref, 95), EtherP95: percentile(eth, 95), ReferenceCI: a, EtherCI: b}
	if a == nil || b == nil {
		r.Status = "inconclusive"
		r.Reason = "insufficient tail observations"
		return r
	}
	lower, upper := b.Lower-a.Upper, b.Upper-a.Lower
	switch {
	case lower > toleranceMS:
		r.Status = "reference"
	case upper < 0:
		r.Status = "ether"
	case toleranceMS > 0 && lower >= -toleranceMS && upper <= toleranceMS:
		r.Status = "tie"
	default:
		r.Status = "inconclusive"
		r.Reason = "uncertainty overlaps without demonstrated equivalence"
	}
	return r
}

type growthResult struct {
	Status              string  `json:"status"`
	DriftBytes          int64   `json:"drift_bytes,omitempty"`
	SlopeBytesPerMinute float64 `json:"slope_bytes_per_minute,omitempty"`
	Reason              string  `json:"reason,omitempty"`
}

// evaluateGrowth uses the final 30 minutes of an at-least-two-hour trace.
// Incomplete process snapshots or sparse data must not count as a pass.
func evaluateGrowth(samples []resourceSample) growthResult {
	if len(samples) < 3 {
		return growthResult{Status: "inconclusive", Reason: "insufficient samples"}
	}
	rows := append([]resourceSample(nil), samples...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Time.Before(rows[j].Time) })
	if rows[0].Time.IsZero() || rows[len(rows)-1].Time.Sub(rows[0].Time) < 2*time.Hour {
		return growthResult{Status: "inconclusive", Reason: "less than two hours observed"}
	}
	for i, s := range rows {
		if s.ProcessReadErrors > 0 || s.ProcessCount < 1 {
			return growthResult{Status: "inconclusive", Reason: "incomplete or empty process snapshot"}
		}
		if i > 0 && (s.Time.Sub(rows[i-1].Time) > 30*time.Second || !s.Time.After(rows[i-1].Time)) {
			return growthResult{Status: "inconclusive", Reason: "resource sampling gap or duplicate time"}
		}
	}
	end := rows[len(rows)-1].Time
	start := end.Add(-30 * time.Minute)
	var window []resourceSample
	for _, s := range rows {
		if !s.Time.Before(start) {
			window = append(window, s)
		}
	}
	if len(window) < 3 || window[0].Time.After(start.Add(30*time.Second)) {
		return growthResult{Status: "inconclusive", Reason: "final 30-minute window incomplete"}
	}
	for i, s := range window {
		if s.ProcessReadErrors > 0 {
			return growthResult{Status: "inconclusive", Reason: "incomplete process snapshot"}
		}
		if i > 0 && (s.Time.Sub(window[i-1].Time) > 30*time.Second || !s.Time.After(window[i-1].Time)) {
			return growthResult{Status: "inconclusive", Reason: "resource sampling gap or duplicate time"}
		}
	}
	base := window[0].RSSSum
	last := window[len(window)-1].RSSSum
	if base > math.MaxInt64 || last > math.MaxInt64 {
		return growthResult{Status: "inconclusive", Reason: "RSS value out of range"}
	}
	drift := int64(last) - int64(base)
	var sx, sy, sxx, sxy float64
	for _, s := range window {
		x := s.Time.Sub(window[0].Time).Minutes()
		y := float64(s.RSSSum)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	n := float64(len(window))
	den := n*sxx - sx*sx
	if den <= 0 {
		return growthResult{Status: "inconclusive", Reason: "invalid sample times"}
	}
	slope := (n*sxy - sx*sy) / den
	limit := math.Max(32*1024*1024, .05*float64(base))
	status := "pass"
	reason := ""
	if float64(drift) > limit || slope > 1024*1024 {
		status = "fail"
		reason = "RSS growth exceeds declared bound"
	}
	return growthResult{Status: status, DriftBytes: drift, SlopeBytesPerMinute: slope, Reason: reason}
}

type cleanupObservation struct {
	At             time.Time `json:"at"`
	ActiveSessions int       `json:"active_sessions"`
	Children       int       `json:"children"`
	FDs            int       `json:"fds"`
	Encoders       int       `json:"encoders"`
	RetryQueue     int       `json:"retry_queue"`
	Valid          bool      `json:"valid"`
}
type cleanupResult struct {
	Status     string  `json:"status"`
	RecoveryMS float64 `json:"recovery_ms,omitempty"`
	Reason     string  `json:"reason,omitempty"`
}

func evaluateCleanup(stop time.Time, baseline cleanupObservation, samples []cleanupObservation) cleanupResult {
	if stop.IsZero() || !baseline.Valid || baseline.ActiveSessions < 0 || baseline.Children < 0 || baseline.FDs < 0 || baseline.Encoders != 0 || baseline.RetryQueue < 0 || len(samples) < 2 {
		return cleanupResult{Status: "inconclusive", Reason: "missing cleanup baseline or samples"}
	}
	rows := append([]cleanupObservation(nil), samples...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].At.Before(rows[j].At) })
	deadline := stop.Add(5 * time.Minute)
	if rows[0].At.After(stop.Add(30*time.Second)) || rows[len(rows)-1].At.Before(deadline) {
		return cleanupResult{Status: "inconclusive", Reason: "cleanup observation window incomplete"}
	}
	var stable time.Time
	lastRetry := -1
	lastAt := stop
	covered := false
	for _, sample := range rows {
		if sample.At.Before(stop) {
			continue
		}
		if sample.At.Sub(lastAt) > 30*time.Second || !sample.Valid || sample.ActiveSessions < 0 || sample.Children < 0 || sample.FDs < 0 || sample.Encoders < 0 || sample.RetryQueue < 0 {
			return cleanupResult{Status: "inconclusive", Reason: "invalid or sparse cleanup samples"}
		}
		lastAt = sample.At
		good := sample.ActiveSessions == baseline.ActiveSessions && sample.Children == baseline.Children && absInt(sample.FDs-baseline.FDs) <= 5 && sample.Encoders == 0 && (lastRetry < 0 || sample.RetryQueue <= lastRetry)
		if good {
			if stable.IsZero() {
				stable = sample.At
			}
		} else {
			stable = time.Time{}
		}
		lastRetry = sample.RetryQueue
		if !sample.At.Before(deadline) {
			covered = true
			break
		}
	}
	if !covered {
		return cleanupResult{Status: "inconclusive", Reason: "cleanup observation window incomplete"}
	}
	if stable.IsZero() {
		return cleanupResult{Status: "fail", Reason: "cleanup did not reach stable baseline in five minutes"}
	}
	if stable.After(deadline) {
		return cleanupResult{Status: "fail", Reason: "cleanup exceeded five minutes"}
	}
	return cleanupResult{Status: "pass", RecoveryMS: float64(stable.Sub(stop)) / float64(time.Millisecond)}
}
func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

type gateComparison struct {
	Status                    string `json:"status"`
	Reason                    string `json:"reason"`
	EtherPointWithinReference *bool  `json:"ether_point_within_reference,omitempty"`
}

func compareGrowth(reference, ether growthResult) gateComparison {
	if reference.Status == "pass" && ether.Status == "fail" {
		return gateComparison{Status: "reference", Reason: "ether exceeded an absolute RSS bound"}
	}
	if reference.Status == "fail" && ether.Status == "pass" {
		return gateComparison{Status: "ether", Reason: "reference exceeded an absolute RSS bound"}
	}
	if reference.Status != "pass" || ether.Status != "pass" {
		return gateComparison{Status: "inconclusive", Reason: "growth gate incomplete or failed on both sides"}
	}
	within := ether.DriftBytes <= reference.DriftBytes && ether.SlopeBytesPerMinute <= reference.SlopeBytesPerMinute
	return gateComparison{Status: "inconclusive", Reason: "one growth trace per side does not establish comparative uncertainty", EtherPointWithinReference: &within}
}
func compareCleanup(reference, ether cleanupResult) gateComparison {
	if reference.Status == "pass" && ether.Status == "fail" {
		return gateComparison{Status: "reference", Reason: "ether failed the absolute cleanup gate"}
	}
	if reference.Status == "fail" && ether.Status == "pass" {
		return gateComparison{Status: "ether", Reason: "reference failed the absolute cleanup gate"}
	}
	if reference.Status != "pass" || ether.Status != "pass" {
		return gateComparison{Status: "inconclusive", Reason: "cleanup gate incomplete or failed on both sides"}
	}
	within := ether.RecoveryMS <= reference.RecoveryMS
	return gateComparison{Status: "inconclusive", Reason: "one cleanup observation per side does not establish comparative uncertainty", EtherPointWithinReference: &within}
}

type acceptanceProvenance struct {
	Host              string    `json:"host"`
	CPUMax            string    `json:"cpu_max"`
	MemoryMax         string    `json:"memory_max"`
	CPUSet            string    `json:"cpu_set"`
	GPULimit          string    `json:"gpu_limit"`
	CacheState        string    `json:"cache_state"`
	TraceSHA256       string    `json:"trace_sha256"`
	ProfileSHA256     string    `json:"profile_sha256"`
	MediaSHA256       string    `json:"media_sha256"`
	QualitySHA256     string    `json:"quality_sha256"`
	ImageSHA256       string    `json:"image_sha256"`
	RunStarted        time.Time `json:"run_started"`
	RunEnded          time.Time `json:"run_ended"`
	Offered           int       `json:"offered"`
	Completed         int       `json:"completed"`
	Cancelled         int       `json:"cancelled"`
	Failed            int       `json:"failed"`
	Dropped           int       `json:"dropped"`
	VerifiedCompleted int       `json:"verified_completed"`
	UnexpectedErrors  int       `json:"unexpected_errors"`
	Timeouts          int       `json:"timeouts"`
	ExpectedTimeouts  int       `json:"expected_timeouts"`
	QualityErrors     int       `json:"quality_errors"`
}

func validSHA(v string) bool { b, e := hex.DecodeString(v); return e == nil && len(b) == 32 }
func validateAcceptanceProvenance(in acceptanceInput) error {
	a, b := in.ReferenceProvenance, in.EtherProvenance
	if !validSHA(in.ManifestSHA256) || in.DeclaredAt.IsZero() || a.RunStarted.IsZero() || b.RunStarted.IsZero() || !in.DeclaredAt.Before(a.RunStarted) || !in.DeclaredAt.Before(b.RunStarted) {
		return errors.New("missing predeclared manifest evidence")
	}
	if !a.RunEnded.After(a.RunStarted) || !b.RunEnded.After(b.RunStarted) || !(a.RunEnded.Before(b.RunStarted) || b.RunEnded.Before(a.RunStarted)) {
		return errors.New("comparison run windows must be complete and non-overlapping")
	}
	if a.Host == "" || a.Host != b.Host || a.CPUMax != b.CPUMax || a.MemoryMax != b.MemoryMax || a.CPUSet == "" || a.CPUSet != b.CPUSet || a.GPULimit == "" || a.GPULimit != b.GPULimit || a.CacheState == "" || a.CacheState != b.CacheState {
		return errors.New("host, limits or cache state are not matched")
	}
	parts := strings.Fields(a.CPUMax)
	if len(parts) != 2 {
		return errors.New("finite CPU limit required")
	}
	for _, p := range parts {
		n, e := strconv.ParseUint(p, 10, 64)
		if e != nil || n == 0 {
			return errors.New("finite CPU limit required")
		}
	}
	mem, e := strconv.ParseUint(a.MemoryMax, 10, 64)
	if e != nil || mem == 0 {
		return errors.New("finite memory limit required")
	}
	for _, pair := range [][2]string{{a.TraceSHA256, b.TraceSHA256}, {a.ProfileSHA256, b.ProfileSHA256}, {a.MediaSHA256, b.MediaSHA256}, {a.QualitySHA256, b.QualitySHA256}} {
		if !validSHA(pair[0]) || pair[0] != pair[1] {
			return errors.New("trace, profile, media or quality evidence is not matched")
		}
	}
	if !validSHA(a.ImageSHA256) || !validSHA(b.ImageSHA256) {
		return errors.New("image digests are required")
	}
	for _, r := range []acceptanceProvenance{a, b} {
		if r.Offered <= 0 || r.Completed < 0 || r.Cancelled < 0 || r.Failed < 0 || r.Dropped < 0 || r.Offered != r.Completed+r.Cancelled+r.Failed+r.Dropped || r.VerifiedCompleted != r.Completed || r.UnexpectedErrors != 0 || r.Timeouts < 0 || r.ExpectedTimeouts < 0 || r.Timeouts != r.ExpectedTimeouts || r.QualityErrors != 0 || r.Dropped != 0 {
			return errors.New("work accounting or verified quality gate failed")
		}
	}
	if a.Failed != b.Failed || a.Timeouts != b.Timeouts || a.Cancelled != b.Cancelled || a.Offered != b.Offered {
		return errors.New("error or offered work counts differ")
	}
	return nil
}

type acceptanceInput struct {
	ManifestSHA256      string               `json:"manifest_sha256"`
	DeclaredAt          time.Time            `json:"declared_at"`
	ReferenceProvenance acceptanceProvenance `json:"reference_provenance"`
	EtherProvenance     acceptanceProvenance `json:"ether_provenance"`
	ToleranceMS         float64              `json:"predeclared_tolerance_ms"`
	ReferenceTrials     []recoveryTrial      `json:"reference_trials"`
	EtherTrials         []recoveryTrial      `json:"ether_trials"`
	ReferenceResources  []resourceSample     `json:"reference_resources"`
	EtherResources      []resourceSample     `json:"ether_resources"`
	ReferenceCleanup    struct {
		Stop     time.Time            `json:"stop"`
		Baseline cleanupObservation   `json:"baseline"`
		Samples  []cleanupObservation `json:"samples"`
	} `json:"reference_cleanup"`
	EtherCleanup struct {
		Stop     time.Time            `json:"stop"`
		Baseline cleanupObservation   `json:"baseline"`
		Samples  []cleanupObservation `json:"samples"`
	} `json:"ether_cleanup"`
}

type recoveryPair struct {
	ID        string         `json:"id"`
	Reference recoveryResult `json:"reference"`
	Ether     recoveryResult `json:"ether"`
}

func recoveryPairs(reference, ether []recoveryTrial) []recoveryPair {
	n := len(reference)
	if len(ether) < n {
		n = len(ether)
	}
	out := make([]recoveryPair, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, recoveryPair{ID: reference[i].ID, Reference: evaluateRecovery(reference[i]), Ether: evaluateRecovery(ether[i])})
	}
	return out
}

type acceptanceReport struct {
	ProvenanceStatus  string             `json:"provenance_status"`
	ProvenanceReason  string             `json:"provenance_reason,omitempty"`
	Recovery          recoveryComparison `json:"recovery"`
	RecoveryTrials    []recoveryPair     `json:"recovery_trials"`
	GrowthComparison  gateComparison     `json:"growth_comparison"`
	CleanupComparison gateComparison     `json:"cleanup_comparison"`
	ReferenceGrowth   growthResult       `json:"reference_growth"`
	EtherGrowth       growthResult       `json:"ether_growth"`
	ReferenceCleanup  cleanupResult      `json:"reference_cleanup"`
	EtherCleanup      cleanupResult      `json:"ether_cleanup"`
	Caveat            string             `json:"caveat"`
}

func evaluateAcceptance(in acceptanceInput) acceptanceReport {
	recovery := compareRecovery(in.ReferenceTrials, in.EtherTrials, in.ToleranceMS)
	refGrowth, ethGrowth := evaluateGrowth(in.ReferenceResources), evaluateGrowth(in.EtherResources)
	refCleanup, ethCleanup := evaluateCleanup(in.ReferenceCleanup.Stop, in.ReferenceCleanup.Baseline, in.ReferenceCleanup.Samples), evaluateCleanup(in.EtherCleanup.Stop, in.EtherCleanup.Baseline, in.EtherCleanup.Samples)
	if refCleanup.Status != "pass" || ethCleanup.Status != "pass" {
		recovery.Status = "inconclusive"
		recovery.Reason = "cleanup gate did not pass on both sides"
	}
	growthComparison, cleanupComparison := compareGrowth(refGrowth, ethGrowth), compareCleanup(refCleanup, ethCleanup)
	provenanceStatus := "valid"
	provenanceReason := ""
	if err := validateAcceptanceProvenance(in); err != nil {
		provenanceStatus = "inconclusive"
		provenanceReason = err.Error()
		recovery.Status = "inconclusive"
		recovery.Reason = "provenance or correctness gate failed"
		growthComparison = gateComparison{Status: "inconclusive", Reason: "provenance or correctness gate failed"}
		cleanupComparison = gateComparison{Status: "inconclusive", Reason: "provenance or correctness gate failed"}
	}
	return acceptanceReport{provenanceStatus, provenanceReason, recovery, recoveryPairs(in.ReferenceTrials, in.EtherTrials), growthComparison, cleanupComparison, refGrowth, ethGrowth, refCleanup, ethCleanup, "Manifest metadata must be independently collected and signed off before runs; equality checks alone do not authenticate provenance. Growth and cleanup results are individual thresholds, not comparative wins."}
}

func acceptance(args []string) error {
	fs := flag.NewFlagSet("acceptance", flag.ContinueOnError)
	path := fs.String("input", "", "predeclared measurement evidence JSON")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if *path == "" {
		return errors.New("input is required")
	}
	b, e := os.ReadFile(*path)
	if e != nil {
		return e
	}
	var in acceptanceInput
	if e = json.Unmarshal(b, &in); e != nil {
		return e
	}
	out := evaluateAcceptance(in)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
