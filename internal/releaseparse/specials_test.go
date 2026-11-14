package releaseparse

import "testing"

func TestSpecialTitleIdentityInvariant(t *testing.T) {
	shows := []string{"One Piece"}
	siblings := []string{"Episode of Luffy: Adventure on Hand Island", "Episode of Nami: Tears of a Navigator", "Heart of Gold"}
	want := siblings[0]
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"One.Piece.S00E08.Episode.of.Luffy.1080p.mkv", true},
		{"One.Piece.S00E08.Episode.of.Nami.1080p.mkv", false},
		{"One.Piece.S00E08.1080p.mkv", true},
		{"One Piece OAD 02 [1080p].mkv", false},
		{"One Piece - 05.mkv", false},
		{"One Piece Ep.001-165.1080p", false},
		{"One Piece Specials/Heart of Gold/One Piece S00E08.mkv", false},
	} {
		if got := SpecialMatches(c.name, want, siblings, shows); got != c.ok {
			t.Errorf("%q: %v", c.name, got)
		}
	}
	if EpisodeMatches(Parse("Show.Ep.005.1080p"), 0, 5) {
		t.Error("seasonless number matched special")
	}
	if ClassicPackEpisodeMatches("Specials/05 - Title.mkv", 0, 5) {
		t.Error("pack count matched special")
	}
}
