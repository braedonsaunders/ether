package frontend

import "testing"

func TestApplyDetailHeaderMatchesPlexPlacement(t *testing.T) {
	tag, overview := ApplyDetailHeader("", "A linguist.", "96% Match · Directed by Denis Villeneuve")
	if tag != "96% Match · Directed by Denis Villeneuve" || overview != "A linguist." {
		t.Fatalf("empty tagline: %q %q", tag, overview)
	}
	tag, overview = ApplyDetailHeader("Fear is the mind-killer.", "A linguist.", "96% Match")
	if tag != "Fear is the mind-killer." || overview != "96% Match\n\nA linguist." {
		t.Fatalf("existing tagline: %q %q", tag, overview)
	}
	tag, overview = ApplyDetailHeader("Fear is the mind-killer.", "  ", "96% Match")
	if tag != "Fear is the mind-killer." || overview != "96% Match" {
		t.Fatalf("empty overview: %q %q", tag, overview)
	}
	tag, overview = ApplyDetailHeader("Keep", "Plot", "  ")
	if tag != "Keep" || overview != "Plot" {
		t.Fatalf("blank header must not invent a match: %q %q", tag, overview)
	}
}
