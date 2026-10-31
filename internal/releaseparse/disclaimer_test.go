package releaseparse

import "testing"

// The disclaimer rule is general: it compares against whichever director
// the catalogue lists for the title, never a built-in name.
func TestDisclaimsDirector(t *testing.T) {
	for _, tc := range []struct {
		name, director string
		want           bool
	}{
		{"Dune.2021.NOT.The.Denis.Villeneuve.FILM.1080p.WEB-DL.x264-GRP", "Denis Villeneuve", true},
		{"Magnolia 1999 Not the Paul Thomas Anderson Film 720p BluRay", "Paul Thomas Anderson", true},
		{"Dune.2021.NOT.The.Denis.Villeneuve.FILM.1080p.WEB-DL.x264-GRP", "David Lynch", false},
		{"Dune.2021.NOT.The.Villeneuve.FILM.1080p.WEB-DL.x264-GRP", "Denis Villeneuve", false},
		{"Dune.2021.NOT.The.Denis.Villeneuve.1080p.WEB-DL.x264-GRP", "Denis Villeneuve", false},
		{"Dune.2021.1080p.WEB-DL.x264-GRP", "Denis Villeneuve", false},
		{"Dune.NOT.The.Denis.Villeneuve.FILM.1080p", "Denis Villeneuve", false},
		{"Dune.2021.NOT.The.Denis.Villeneuve.FILM.1080p", "Villeneuve", false},
	} {
		if got := DisclaimsDirector(tc.name, tc.director); got != tc.want {
			t.Errorf("DisclaimsDirector(%q, %q)=%v want %v", tc.name, tc.director, got, tc.want)
		}
	}
}
