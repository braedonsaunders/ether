package core

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecoveryStartCloneAndAbsentWire(t *testing.T) {
	if CloneRecoveryStart(nil) != nil {
		t.Fatal("nil became an instruction")
	}
	r := &RecoveryStart{AdmittedDecision: Decision{AudioFilters: []string{"original"}}, Audio: &RecoveryAACCursor{NextSample: 1024, NextRawAUSHA256: [32]byte{1}}}
	c := CloneRecoveryStart(r)
	r.Audio.NextSample = 0
	r.Audio.NextRawAUSHA256[0] = 2
	r.AdmittedDecision.AudioFilters[0] = "changed"
	if c.Audio.NextSample != 1024 || c.Audio.NextRawAUSHA256[0] != 1 || c.AdmittedDecision.AudioFilters[0] != "original" {
		t.Fatal("clone shared mutable state")
	}
	b, err := json.Marshal(HLSRequest{})
	if err != nil || strings.Contains(string(b), "RecoveryStart") {
		t.Fatalf("nil changes legacy wire: %s %v", b, err)
	}
}
