package config

import "testing"

func TestAvailabilityModeMigration(t *testing.T) {
	if got := DefaultAvailability().EffectiveMode(); got != ModePlayable {
		t.Fatalf("default mode = %q", got)
	}
	legacy := Availability{DisableMovieEvidenceGate: true}
	if got := legacy.EffectiveMode(); got != ModeImmediate {
		t.Fatalf("gate off = %q", got)
	}
	legacy.ApplyMode()
	if legacy.Mode != ModeImmediate || legacy.Monitor || !legacy.DisableMovieEvidenceGate || legacy.DigitalDateEvidence {
		t.Fatalf("immediate flags %+v", legacy)
	}
	digital := Availability{DigitalDateEvidence: true}
	if got := digital.EffectiveMode(); got != ModeDigital {
		t.Fatalf("digital date = %q", got)
	}
	digital.ApplyMode()
	if digital.Mode != ModeDigital || digital.Monitor || digital.DisableMovieEvidenceGate || !digital.DigitalDateEvidence {
		t.Fatalf("digital flags %+v", digital)
	}
	playable := Availability{Monitor: false, Effort: "gentle"}
	playable.ApplyMode()
	if playable.Mode != ModePlayable || !playable.Monitor || playable.DisableMovieEvidenceGate || playable.DigitalDateEvidence || playable.Effort != "normal" {
		t.Fatalf("playable flags %+v", playable)
	}
	named := Availability{Mode: ModePlayable, DisableMovieEvidenceGate: true}
	if named.EffectiveMode() != ModePlayable {
		t.Fatal("explicit mode lost to flags")
	}
}

func TestAvailabilityBudgetPresets(t *testing.T) {
	base := DefaultAvailability()
	if !base.LegacyDefaultBudgets() {
		t.Fatal("v0.2.57 defaults should move to the scaled preset")
	}
	unlimited := []IndexerCap{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	got := base.Resolved(unlimited)
	if got.Effort != "normal" || got.FeedRequests != 8000 || got.MaxPages != 8 || got.PollMinutes != 15 {
		t.Fatalf("%+v", got)
	}
	capped := base.Resolved([]IndexerCap{{ID: "a", DailyLimit: 100}})
	if capped.FeedRequests != 20 {
		t.Fatalf("background share = %d", capped.FeedRequests)
	}
	custom := base
	custom.FeedRequests = 428
	kept := custom.Resolved(unlimited)
	if kept.Effort != "custom" || kept.FeedRequests != 428 {
		t.Fatalf("custom preset %+v", kept)
	}
}
