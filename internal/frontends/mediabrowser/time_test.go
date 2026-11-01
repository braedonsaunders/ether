package mediabrowser

import (
	"testing"
	"time"
)

func TestWireTimeUsesSevenFractionalDigits(t *testing.T) {
	cases := []struct {
		value time.Time
		want  string
	}{
		{time.Time{}, "0001-01-01T00:00:00.0000000Z"},
		{time.Date(2026, 4, 5, 8, 9, 10, 123456789, time.FixedZone("test", 3600)), "2026-04-05T07:09:10.1234567Z"},
		{time.Date(2026, 4, 5, 7, 9, 10, 0, time.UTC), "2026-04-05T07:09:10.0000000Z"},
	}
	for _, tc := range cases {
		if got := wireTime(tc.value); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}
