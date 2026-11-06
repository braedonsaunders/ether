package core

import "testing"

func TestLogicalPreviewIdentityStillIncludesItem(t *testing.T) {
	a := &PlaySession{Key: 1, Resolution: &Resolution{Release: Release{ID: "shared-pack"}}}
	b := &PlaySession{Key: 2, Resolution: a.Resolution}
	if TrickplayCacheKey(a) == TrickplayCacheKey(b) {
		t.Fatal("logical descriptors for different items share previews")
	}
	if TrickplayCacheKey(a) != TrickplayCacheKey(a) {
		t.Fatal("identity is unstable")
	}
}
