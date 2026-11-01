package config

import "testing"

func TestMarkerButtonsDefaultOn(t *testing.T) {
	var m Markers
	if !m.OfferIntro() || !m.OfferCredits() {
		t.Fatalf("missing switches should offer both buttons: %+v", m)
	}
	off := false
	m.SkipIntro = &off
	if m.OfferIntro() || !m.OfferCredits() {
		t.Fatalf("intro off: %+v", m)
	}
	on := true
	m.SkipCredits = &on
	if !m.OfferCredits() {
		t.Fatal("explicit on")
	}
}
