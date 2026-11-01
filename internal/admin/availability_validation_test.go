package admin_test

import (
	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"testing"
)

func TestMovieEvidenceDaysValidation(t *testing.T) {
	for _, tc := range []struct {
		days    int
		invalid bool
	}{{-1, true}, {0, false}, {1, false}, {366, false}, {367, true}} {
		c := config.Defaults(t.TempDir())
		c.Availability.MovieEvidenceDays = tc.days
		found := false
		for _, e := range admin.ValidateConfig(c) {
			if e.Field == "availability.movieEvidenceDays" {
				found = true
			}
		}
		if found != tc.invalid {
			t.Fatalf("days %d: invalid=%v", tc.days, found)
		}
	}
}

func TestAvailabilityModeValidation(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		invalid bool
	}{
		{"", false},
		{config.ModePlayable, false},
		{config.ModeDigital, false},
		{config.ModeImmediate, false},
		{"feeds", true},
	} {
		c := config.Defaults(t.TempDir())
		c.Availability.Mode = tc.mode
		found := false
		for _, e := range admin.ValidateConfig(c) {
			if e.Field == "availability.mode" {
				found = true
			}
		}
		if found != tc.invalid {
			t.Fatalf("mode %q: invalid=%v", tc.mode, found)
		}
	}
}
