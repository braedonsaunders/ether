package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func makeRecoveryTrial(id string, base time.Time, thirdSeconds int) recoveryTrial {
	observations := []recoveryObservation{{At: base.Add(time.Second), Client: "a", Success: true, Verified: true}, {At: base.Add(2 * time.Second), Client: "b", Success: true, Verified: true}, {At: base.Add(time.Duration(thirdSeconds) * time.Second), Client: "c", Success: true, Verified: true}}
	for i := 1; i <= 10; i++ {
		observations = append(observations, recoveryObservation{At: base.Add(time.Duration(thirdSeconds+i*30) * time.Second), Client: "follow", Success: true, Verified: true})
	}
	return recoveryTrial{ID: id, Complete: true, Offered: len(observations), FaultEnd: base, ObservedUntil: base.Add(time.Duration(thirdSeconds+300) * time.Second), Observations: observations}
}
func TestRecoveryRequiresDistinctVerifiedClientsAndFollowup(t *testing.T) {
	base := time.Unix(1000, 0)
	trial := makeRecoveryTrial("trial", base, 3)
	if got := evaluateRecovery(trial); got.Status != "pass" || got.RecoveryMS != 3000 {
		t.Fatalf("%+v", got)
	}
	missing := trial
	missing.Offered++
	if got := evaluateRecovery(missing); got.Status != "inconclusive" {
		t.Fatalf("missing work: %+v", got)
	}
	late := makeRecoveryTrial("late", base, 31)
	if got := evaluateRecovery(late); got.Status != "fail" {
		t.Fatalf("late: %+v", got)
	}
	gap := trial
	gap.Observations = append([]recoveryObservation(nil), trial.Observations[:3]...)
	gap.Offered = len(gap.Observations)
	if got := evaluateRecovery(gap); got.Status != "inconclusive" {
		t.Fatalf("followup gap: %+v", got)
	}
	unverified := trial
	unverified.Observations = append([]recoveryObservation(nil), trial.Observations...)
	unverified.Observations[4].Verified = false
	if got := evaluateRecovery(unverified); got.Status != "inconclusive" {
		t.Fatalf("unverified followup: %+v", got)
	}
	expectedFailure := trial
	expectedFailure.Observations = append([]recoveryObservation(nil), trial.Observations...)
	expectedFailure.Observations[4].Success = false
	expectedFailure.Observations[4].ExpectedFailure = true
	if got := evaluateRecovery(expectedFailure); got.Status != "inconclusive" {
		t.Fatalf("fault in fault-free followup: %+v", got)
	}
	failure := trial
	failure.Observations = append([]recoveryObservation(nil), trial.Observations...)
	failure.Observations[4].Success = false
	if got := evaluateRecovery(failure); got.Status != "fail" {
		t.Fatalf("post-recovery failure: %+v", got)
	}
}
func TestRecoveryComparisonRequiresIndependentMatchedTrials(t *testing.T) {
	base := time.Unix(1000, 0)
	ref, ether := []recoveryTrial{}, []recoveryTrial{}
	for i := 0; i < 211; i++ {
		id := fmt.Sprintf("trial-%d", i)
		at := base.Add(time.Duration(i) * 10 * time.Minute)
		ref = append(ref, makeRecoveryTrial(id, at, 3))
		ether = append(ether, makeRecoveryTrial(id, at.Add(time.Hour), 4))
	}
	if got := compareRecovery(ref[:100], ether[:100], 0); got.Status != "inconclusive" {
		t.Fatalf("too few trials: %+v", got)
	}
	if got := compareRecovery(ref, ether, 0); got.Status != "reference" {
		t.Fatalf("slower recovery: %+v", got)
	}
	ether[1].ID = ether[0].ID
	if got := compareRecovery(ref, ether, 0); got.Status != "inconclusive" {
		t.Fatalf("duplicate trial: %+v", got)
	}
}
func TestGrowthRejectsGapsAndExcessiveDrift(t *testing.T) {
	base := time.Unix(1000, 0)
	rows := []resourceSample{}
	for i := 0; i <= 240; i++ {
		rows = append(rows, resourceSample{Time: base.Add(time.Duration(i) * 30 * time.Second), ProcessCount: 1, RSSSum: 100 * 1024 * 1024})
	}
	if got := evaluateGrowth(rows); got.Status != "pass" {
		t.Fatalf("stable: %+v", got)
	}
	bad := append([]resourceSample(nil), rows...)
	bad[len(bad)-1].RSSSum += 100 * 1024 * 1024
	if got := evaluateGrowth(bad); got.Status != "fail" {
		t.Fatalf("growth: %+v", got)
	}
	gap := append([]resourceSample(nil), rows[:100]...)
	gap = append(gap, rows[102:]...)
	if got := evaluateGrowth(gap); got.Status != "inconclusive" {
		t.Fatalf("gap: %+v", got)
	}
}
func TestCleanupRequiresFullStableWindow(t *testing.T) {
	base := time.Unix(1000, 0)
	baseline := cleanupObservation{Valid: true, FDs: 10}
	rows := []cleanupObservation{}
	for i := 0; i <= 10; i++ {
		rows = append(rows, cleanupObservation{At: base.Add(time.Duration(i) * 30 * time.Second), Valid: true, FDs: 10})
	}
	if got := evaluateCleanup(base, baseline, rows); got.Status != "pass" {
		t.Fatalf("clean: %+v", got)
	}
	rows[len(rows)-1].Encoders = 1
	if got := evaluateCleanup(base, baseline, rows); got.Status != "fail" {
		t.Fatalf("lingering encoder: %+v", got)
	}
	rows = rows[:len(rows)-1]
	if got := evaluateCleanup(base, baseline, rows); got.Status != "inconclusive" {
		t.Fatalf("missing end: %+v", got)
	}
}

func TestCleanupExcludesWarmSQLitePool(t *testing.T) {
	base := time.Unix(1000, 0)
	// 30-minute soak: baseline 7 sqlite fds, cleanup 18, other descriptors -4.
	baseline := cleanupObservation{Valid: true, FDs: 27, SQLiteFDs: 7}
	rows := []cleanupObservation{}
	for i := 0; i <= 10; i++ {
		rows = append(rows, cleanupObservation{At: base.Add(time.Duration(i) * 30 * time.Second), Valid: true, FDs: 34, SQLiteFDs: 18})
	}
	if got := evaluateCleanup(base, baseline, rows); got.Status != "pass" {
		t.Fatalf("warm pool: %+v", got)
	}
	for i := range rows {
		rows[i].SQLiteFDs = sqliteFDCap + 1
		rows[i].FDs = 27 - 7 + rows[i].SQLiteFDs
	}
	if got := evaluateCleanup(base, baseline, rows); got.Status != "fail" || got.Reason != "sqlite file descriptors exceeded the warm-pool cap" {
		t.Fatalf("cap: %+v", got)
	}
	for i := range rows {
		rows[i].SQLiteFDs = 18
		rows[i].FDs = 46
	}
	if got := evaluateCleanup(base, baseline, rows); got.Status != "fail" || got.Reason == "sqlite file descriptors exceeded the warm-pool cap" {
		t.Fatalf("unnamed growth: %+v", got)
	}
}

func TestAcceptanceProvenanceDisqualifiesMismatchAndErrors(t *testing.T) {
	hash := strings.Repeat("a", 64)
	declared := time.Unix(1000, 0)
	ref := acceptanceProvenance{Host: "worker", CPUMax: "200000 100000", MemoryMax: "1073741824", CPUSet: "0-1", GPULimit: "none", CacheState: "warm", TraceSHA256: hash, ProfileSHA256: hash, MediaSHA256: hash, QualitySHA256: hash, ImageSHA256: hash, RunStarted: declared.Add(time.Hour), RunEnded: declared.Add(2 * time.Hour), Offered: 10, Completed: 10, VerifiedCompleted: 10}
	ether := ref
	ether.RunStarted = declared.Add(3 * time.Hour)
	ether.RunEnded = declared.Add(4 * time.Hour)
	in := acceptanceInput{ManifestSHA256: hash, DeclaredAt: declared, ReferenceProvenance: ref, EtherProvenance: ether}
	if e := validateAcceptanceProvenance(in); e != nil {
		t.Fatal(e)
	}
	faultA, faultB := ref, ether
	faultA.Offered = 11
	faultB.Offered = 11
	faultA.Failed = 1
	faultB.Failed = 1
	faultA.Timeouts = 1
	faultB.Timeouts = 1
	faultA.ExpectedTimeouts = 1
	faultB.ExpectedTimeouts = 1
	in.ReferenceProvenance = faultA
	in.EtherProvenance = faultB
	if e := validateAcceptanceProvenance(in); e != nil {
		t.Fatalf("matched intentional fault timeout rejected: %v", e)
	}
	in.ReferenceProvenance = ref
	in.EtherProvenance = ether
	in.EtherProvenance.Timeouts = 1
	if e := validateAcceptanceProvenance(in); e == nil {
		t.Fatal("timeout accepted")
	}
	in.EtherProvenance = ether
	in.EtherProvenance.MediaSHA256 = strings.Repeat("b", 64)
	if e := validateAcceptanceProvenance(in); e == nil {
		t.Fatal("mismatched media accepted")
	}
	in.EtherProvenance = ether
	in.EtherProvenance.RunStarted = ref.RunStarted.Add(time.Minute)
	if e := validateAcceptanceProvenance(in); e == nil {
		t.Fatal("overlapping workload accepted")
	}
}

func TestGrowthAndCleanupDoNotDeclareWinFromOnePassingTrace(t *testing.T) {
	a := growthResult{Status: "pass", DriftBytes: 100, SlopeBytesPerMinute: 20}
	b := growthResult{Status: "pass", DriftBytes: 90, SlopeBytesPerMinute: 10}
	got := compareGrowth(a, b)
	if got.Status != "inconclusive" || got.EtherPointWithinReference == nil || !*got.EtherPointWithinReference {
		t.Fatalf("unjustified growth win: %+v", got)
	}
	if got := compareGrowth(a, growthResult{Status: "fail"}); got.Status != "reference" {
		t.Fatalf("absolute failure not surfaced: %+v", got)
	}
	c := cleanupResult{Status: "pass", RecoveryMS: 100}
	d := cleanupResult{Status: "pass", RecoveryMS: 90}
	if got := compareCleanup(c, d); got.Status != "inconclusive" {
		t.Fatalf("unjustified cleanup win: %+v", got)
	}
}

func TestAcceptanceNeverClaimsWinWithoutProvenanceAndCleanup(t *testing.T) {
	base := time.Unix(1000, 0)
	in := acceptanceInput{}
	for i := 0; i < 211; i++ {
		id := fmt.Sprintf("trial-%d", i)
		at := base.Add(time.Duration(i) * 10 * time.Minute)
		in.ReferenceTrials = append(in.ReferenceTrials, makeRecoveryTrial(id, at, 4))
		in.EtherTrials = append(in.EtherTrials, makeRecoveryTrial(id, at.Add(time.Hour), 3))
	}
	r := evaluateAcceptance(in)
	if r.ProvenanceStatus != "inconclusive" || r.Recovery.Status != "inconclusive" || r.GrowthComparison.Status != "inconclusive" || r.CleanupComparison.Status != "inconclusive" || len(r.RecoveryTrials) != 211 {
		t.Fatalf("unverified evidence produced winner: %+v", r)
	}
}
