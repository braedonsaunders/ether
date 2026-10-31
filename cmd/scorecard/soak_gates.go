package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/braedonsaunders/ether/internal/hoststat"
)

// SoakSample is one telemetry point from the Ether process and its encoder children.
type SoakSample = hoststat.Telemetry

// SoakOp is one offered client operation.
type SoakOp struct {
	At              time.Time `json:"at"`
	Protocol        string    `json:"protocol"`
	Kind            string    `json:"kind"`
	Delivery        string    `json:"delivery,omitempty"`
	Client          string    `json:"client"`
	Success         bool      `json:"success"`
	ExpectedFailure bool      `json:"expected_failure"`
	Verified        bool      `json:"verified"`
	Error           string    `json:"error,omitempty"`
}

// SoakFault records one injected fault.
type SoakFault struct {
	Action   string    `json:"action"`
	At       time.Time `json:"at"`
	OK       bool      `json:"ok"`
	Signaled int       `json:"signaled,omitempty"`
}

// SoakManifest is the evidence a soak run hands to the gates.
type SoakManifest struct {
	Profile     string       `json:"profile"`
	Build       string       `json:"build"`
	Started     time.Time    `json:"started"`
	FaultEnd    time.Time    `json:"fault_end"`
	FollowupEnd time.Time    `json:"followup_end"`
	LoadStopped time.Time    `json:"load_stopped"`
	Baseline    SoakSample   `json:"baseline"`
	Samples     []SoakSample `json:"samples"`
	Ops         []SoakOp     `json:"ops"`
	Faults      []SoakFault  `json:"faults"`
	GPU         string       `json:"gpu"`
}

// SoakGate is one executable pass, fail, inconclusive or not-run result.
type SoakGate struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// SoakReport is the machine-readable pass/fail result.
type SoakReport struct {
	Status           string     `json:"status"`
	Profile          string     `json:"profile"`
	Build            string     `json:"build"`
	Gates            []SoakGate `json:"gates"`
	Offered          int        `json:"offered"`
	Succeeded        int        `json:"succeeded"`
	Failed           int        `json:"failed"`
	ExpectedFailures int        `json:"expected_failures"`
	SustainedClaimed bool       `json:"sustained_claimed"`
}

func soakEval(args []string) error {
	fs := flag.NewFlagSet("soak-eval", flag.ContinueOnError)
	path := fs.String("input", "", "soak manifest JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return fmt.Errorf("input is required")
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return err
	}
	var manifest SoakManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return err
	}
	report := EvaluateSoak(manifest)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return err
	}
	if report.Status != "pass" {
		return fmt.Errorf("soak %s", report.Status)
	}
	return nil
}

// EvaluateSoak applies the sustained-pressure gates. A shakedown run leaves
// the two-hour RSS gate not_run; it does not count as a sustained pass.
func EvaluateSoak(m SoakManifest) SoakReport {
	report := SoakReport{Profile: m.Profile, Build: m.Build, Status: "pass"}
	for _, op := range m.Ops {
		report.Offered++
		if op.Success {
			report.Succeeded++
		} else if op.ExpectedFailure {
			report.ExpectedFailures++
		} else {
			report.Failed++
		}
	}
	gates := []SoakGate{
		gateFaults(m),
		gateMix(m),
		gateFaultFree(m),
		gateRecovery(m),
		gateCleanup(m),
		gateTelemetry(m),
		gateGoroutines(m),
		gateLeak(m),
		gateSustained(m),
	}
	requiredFail := false
	requiredInconclusive := false
	for _, gate := range gates {
		report.Gates = append(report.Gates, gate)
		if gate.Status == "not_run" || gate.Status == "not_applicable" {
			continue
		}
		if gate.Status == "fail" {
			requiredFail = true
		}
		if gate.Status == "inconclusive" {
			requiredInconclusive = true
		}
	}
	switch {
	case requiredFail:
		report.Status = "fail"
	case requiredInconclusive:
		report.Status = "inconclusive"
	}
	for _, gate := range report.Gates {
		if gate.Name == "rss_sustained" && gate.Status == "pass" {
			report.SustainedClaimed = true
		}
	}
	return report
}

func gateFaults(m SoakManifest) SoakGate {
	need := []string{"source_outage", "slow_upstream", "encoder_crash", "encoder_kill", "db_stall"}
	got := map[string]SoakFault{}
	for _, fault := range m.Faults {
		got[fault.Action] = fault
	}
	for _, action := range need {
		fault, ok := got[action]
		if !ok || !fault.OK {
			return SoakGate{Name: "faults_injected", Status: "fail", Reason: action + " was not injected"}
		}
		if (action == "encoder_crash" || action == "encoder_kill") && fault.Signaled < 1 {
			return SoakGate{Name: "faults_injected", Status: "fail", Reason: action + " signaled no process"}
		}
	}
	return SoakGate{Name: "faults_injected", Status: "pass"}
}

func gateMix(m SoakManifest) SoakGate {
	ok := map[string]int{}
	protocols := map[string]int{}
	remuxDelivery := ""
	for _, op := range m.Ops {
		if !op.Success {
			continue
		}
		protocols[op.Protocol]++
		ok[op.Kind]++
		if op.Kind == "remux" {
			remuxDelivery = op.Delivery
		}
	}
	for _, kind := range []string{"browse", "direct", "remux", "transcode"} {
		if ok[kind] < 1 {
			return SoakGate{Name: "mix", Status: "fail", Reason: "no successful " + kind}
		}
	}
	if remuxDelivery != "hls" && remuxDelivery != "remux" {
		return SoakGate{Name: "mix", Status: "fail", Reason: "remux request was served as " + remuxDelivery}
	}
	if protocols["plex"] <= protocols["jellyfin"]+protocols["emby"] {
		return SoakGate{Name: "mix", Status: "fail", Reason: "plex was not the majority of successful operations"}
	}
	return SoakGate{Name: "mix", Status: "pass"}
}

func gateFaultFree(m SoakManifest) SoakGate {
	for _, op := range m.Ops {
		if !op.Success && !op.ExpectedFailure {
			return SoakGate{Name: "fault_free_errors", Status: "fail", Reason: "unexpected " + op.Protocol + " " + op.Kind + " failure"}
		}
	}
	if len(m.Ops) == 0 {
		return SoakGate{Name: "fault_free_errors", Status: "fail", Reason: "no operations were offered"}
	}
	return SoakGate{Name: "fault_free_errors", Status: "pass"}
}

func gateRecovery(m SoakManifest) SoakGate {
	if m.FaultEnd.IsZero() || m.FollowupEnd.IsZero() {
		return SoakGate{Name: "recovery", Status: "inconclusive", Reason: "fault window was not closed"}
	}
	var observations []recoveryObservation
	for _, op := range m.Ops {
		if op.At.Before(m.FaultEnd) || op.At.After(m.FollowupEnd) {
			continue
		}
		observations = append(observations, recoveryObservation{
			At: op.At, Client: op.Client, Success: op.Success, Verified: op.Verified, ExpectedFailure: op.ExpectedFailure,
		})
	}
	result := evaluateRecovery(recoveryTrial{
		ID: "soak", Complete: true, Offered: len(observations), FaultEnd: m.FaultEnd, ObservedUntil: m.FollowupEnd, Observations: observations,
	})
	return SoakGate{Name: "recovery", Status: result.Status, Reason: result.Reason}
}

func gateCleanup(m SoakManifest) SoakGate {
	if !m.Baseline.Valid || m.LoadStopped.IsZero() {
		return SoakGate{Name: "cleanup", Status: "inconclusive", Reason: "cleanup baseline was not recorded"}
	}
	baseline := cleanupObservation{
		At: m.Baseline.At, ActiveSessions: m.Baseline.Sessions, Children: m.Baseline.Children,
		FDs: m.Baseline.FDs, Encoders: m.Baseline.FFmpeg, RetryQueue: m.Baseline.RetryQueue, Valid: true,
	}
	var samples []cleanupObservation
	for _, sample := range m.Samples {
		if sample.At.Before(m.LoadStopped) {
			continue
		}
		samples = append(samples, cleanupObservation{
			At: sample.At, ActiveSessions: sample.Sessions, Children: sample.Children,
			FDs: sample.FDs, Encoders: sample.FFmpeg, RetryQueue: sample.RetryQueue, Valid: sample.Valid,
		})
	}
	result := evaluateCleanup(m.LoadStopped, baseline, samples)
	return SoakGate{Name: "cleanup", Status: result.Status, Reason: result.Reason}
}

func gateTelemetry(m SoakManifest) SoakGate {
	switch m.GPU {
	case "no_device", "dri_present", "nvidia_present", "sampled":
	default:
		return SoakGate{Name: "telemetry", Status: "fail", Reason: "gpu presence was not scoped"}
	}
	if len(m.Samples) < 2 {
		return SoakGate{Name: "telemetry", Status: "fail", Reason: "telemetry series is missing"}
	}
	var prev *SoakSample
	for i := range m.Samples {
		sample := &m.Samples[i]
		if !sample.Valid || sample.Goroutines < 1 || sample.GPU == "" || sample.FDs < 0 {
			return SoakGate{Name: "telemetry", Status: "inconclusive", Reason: "telemetry sample incomplete"}
		}
		if sample.GPU != m.GPU {
			return SoakGate{Name: "telemetry", Status: "fail", Reason: "gpu scope changed during the run"}
		}
		if prev != nil {
			if sample.At.Sub(prev.At) > 30*time.Second || !sample.At.After(prev.At) {
				return SoakGate{Name: "telemetry", Status: "inconclusive", Reason: "telemetry sample gap"}
			}
			if sample.NetRx < prev.NetRx || sample.NetTx < prev.NetTx {
				return SoakGate{Name: "telemetry", Status: "fail", Reason: "network counters moved backwards"}
			}
		}
		prev = sample
	}
	return SoakGate{Name: "telemetry", Status: "pass"}
}

func gateGoroutines(m SoakManifest) SoakGate {
	cleanup := gateCleanup(m)
	if cleanup.Status != "pass" {
		return SoakGate{Name: "goroutine_cleanup", Status: "inconclusive", Reason: "cleanup did not pass"}
	}
	if m.Baseline.Goroutines < 1 {
		return SoakGate{Name: "goroutine_cleanup", Status: "inconclusive", Reason: "baseline goroutines missing"}
	}
	var last *SoakSample
	for i := range m.Samples {
		sample := &m.Samples[i]
		if !sample.At.Before(m.LoadStopped) && sample.Valid && sample.Sessions == m.Baseline.Sessions && sample.FFmpeg == m.Baseline.FFmpeg && sample.RetryQueue <= m.Baseline.RetryQueue {
			last = sample
		}
	}
	if last == nil {
		return SoakGate{Name: "goroutine_cleanup", Status: "inconclusive", Reason: "no stable cleanup sample"}
	}
	if last.Goroutines > m.Baseline.Goroutines+50 {
		return SoakGate{Name: "goroutine_cleanup", Status: "fail", Reason: "goroutines did not return to the baseline"}
	}
	return SoakGate{Name: "goroutine_cleanup", Status: "pass"}
}

func gateLeak(m SoakManifest) SoakGate {
	if m.Profile == "sustained" {
		return SoakGate{Name: "rss_leak", Status: "not_applicable", Reason: "covered by the two-hour gate"}
	}
	if m.FaultEnd.IsZero() || m.FollowupEnd.IsZero() {
		return SoakGate{Name: "rss_leak", Status: "inconclusive", Reason: "follow-up window missing"}
	}
	var window []SoakSample
	for _, sample := range m.Samples {
		if !sample.At.Before(m.FaultEnd) && !sample.At.After(m.FollowupEnd) && sample.Valid {
			window = append(window, sample)
		}
	}
	if len(window) < 3 || window[len(window)-1].At.Sub(window[0].At) < 4*time.Minute {
		return SoakGate{Name: "rss_leak", Status: "inconclusive", Reason: "follow-up RSS window is shorter than 4 minutes"}
	}
	for i := 1; i < len(window); i++ {
		if window[i].At.Sub(window[i-1].At) > 30*time.Second {
			return SoakGate{Name: "rss_leak", Status: "inconclusive", Reason: "follow-up RSS sample gap"}
		}
	}
	drift, slope, ok := rssSlope(window)
	if !ok {
		return SoakGate{Name: "rss_leak", Status: "inconclusive", Reason: "RSS slope is undefined"}
	}
	limit := math.Max(32*1024*1024, 0.05*float64(window[0].SelfRSS))
	if drift > limit || slope > 1024*1024 {
		return SoakGate{Name: "rss_leak", Status: "fail", Reason: "follow-up RSS growth exceeds the declared bound"}
	}
	if child := encoderChildRSS(window); child > encoderRSSCap {
		return SoakGate{Name: "rss_leak", Status: "fail", Reason: "encoder RSS exceeded the 256 MiB cap for 2 transcode slots"}
	}
	return SoakGate{Name: "rss_leak", Status: "pass"}
}

// encoderRSSCap is the plateau of ffmpeg plus its guardian, measured on the
// lab file: about 105 MiB for one live transcode and about 210 MiB for the
// two slots the manager allows. Tree RSS swings with those processes and is
// not the leak signal. 128 MiB per slot is that plateau plus headroom; a
// third encoder does not fit.
const encoderRSSCap = 2 * 128 * 1024 * 1024

func encoderChildRSS(window []SoakSample) uint64 {
	var child uint64
	for _, sample := range window {
		if sample.RSSBytes > sample.SelfRSS && sample.RSSBytes-sample.SelfRSS > child {
			child = sample.RSSBytes - sample.SelfRSS
		}
	}
	return child
}

func gateSustained(m SoakManifest) SoakGate {
	if m.Profile != "sustained" {
		return SoakGate{Name: "rss_sustained", Status: "not_run", Reason: "shakedown does not claim the two-hour gate"}
	}
	start := m.Started.Add(15 * time.Minute)
	var rows []resourceSample
	for _, sample := range m.Samples {
		if sample.At.Before(start) || !sample.At.Before(m.FaultEnd) || !sample.Valid {
			continue
		}
		rows = append(rows, resourceSample{Time: sample.At, RSSSum: sample.SelfRSS, ProcessCount: 1})
	}
	result := evaluateGrowth(rows)
	return SoakGate{Name: "rss_sustained", Status: result.Status, Reason: result.Reason}
}

func rssSlope(window []SoakSample) (drift, slope float64, ok bool) {
	base := window[0].SelfRSS
	last := window[len(window)-1].SelfRSS
	if base > math.MaxInt64 || last > math.MaxInt64 {
		return 0, 0, false
	}
	drift = float64(int64(last) - int64(base))
	var sx, sy, sxx, sxy float64
	for _, sample := range window {
		x := sample.At.Sub(window[0].At).Minutes()
		y := float64(sample.SelfRSS)
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	n := float64(len(window))
	den := n*sxx - sx*sx
	if den <= 0 {
		return drift, 0, false
	}
	return drift, (n*sxy - sx*sy) / den, true
}
