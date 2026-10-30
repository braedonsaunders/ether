package subtitles

import (
	"strings"
	"testing"
)

// A release that already carries an SDH token must not gain a second
// " (SDH)" marker; one without it still gets exactly one.
func TestSidecarStreamSDHMarkerNotDuplicated(t *testing.T) {
	for _, release := range []string{
		"Movie.2024.SDH.1080p.WEB-DL",
		"Movie.2024.sdh.WEB-DL",
		"Movie 2024 SDH",
	} {
		st := sidecarStream(0, "en", selection{ID: "x", Release: release, SDH: true})
		if strings.Contains(st.Title, " (SDH)") || strings.Count(strings.ToUpper(st.Title), "SDH") != 1 {
			t.Fatalf("release %q: title %q duplicates the SDH marker", release, st.Title)
		}
	}
	st := sidecarStream(0, "en", selection{ID: "x", Release: "Movie.2024.1080p.WEB-DL", SDH: true})
	if strings.Count(st.Title, " (SDH)") != 1 {
		t.Fatalf("plain release: title %q, want exactly one (SDH) marker", st.Title)
	}
	// Non-SDH tracks never gain the marker, SDH token or not.
	st = sidecarStream(0, "en", selection{ID: "x", Release: "Movie.2024.SDH.1080p", SDH: false})
	if strings.Contains(st.Title, " (SDH)") {
		t.Fatalf("non-SDH title %q gained a marker", st.Title)
	}
}
