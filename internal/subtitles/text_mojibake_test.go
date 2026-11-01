package subtitles

import (
	"strings"
	"testing"
)

func TestRepairMojibake(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"â™ª", "♪"},
		{"CafÃ©", "Café"},
		{"naÃ¯ve", "naïve"},
		{"hello", "hello"},
	}
	for _, tc := range cases {
		if got := RepairMojibake(tc.in); got != tc.want {
			t.Fatalf("%q -> %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestNormalizeRepairsMojibake(t *testing.T) {
	raw := []byte("1\n00:00:01,000 --> 00:00:02,000\nCafÃ© â™ª\n\n")
	srt, _, err := Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(srt, "Café") || !strings.Contains(srt, "♪") {
		t.Fatalf("not repaired: %q", srt)
	}
}

func TestRankPenalisesMojibake(t *testing.T) {
	cands := []Candidate{
		{FileID: 1, FileName: "Movie.2024.CafÃ©.srt", DownloadCount: 5000},
		{FileID: 2, FileName: "Movie.2024.srt", DownloadCount: 5000},
	}
	Rank(cands, "Movie.2024")
	if cands[0].FileID != 2 {
		t.Fatalf("mojibake not penalised: %+v", cands)
	}
}

func TestRankKeepsNordicAndGermanNames(t *testing.T) {
	for _, name := range []string{"Movie.2024.Åsberg.srt", "Movie.2024.Ärzte.srt", "Movie.2024.Øresund.srt", "Movie.2024.Mädchen.srt"} {
		cands := []Candidate{
			{FileID: 1, FileName: name, DownloadCount: 5000},
			{FileID: 2, FileName: "Movie.2024.srt", DownloadCount: 5000},
		}
		Rank(cands, "Movie.2024")
		if cands[0].FileID != 1 {
			t.Fatalf("%q lost ranking: %+v", name, cands)
		}
		if HasMojibake(name) {
			t.Fatalf("HasMojibake(%q) = true", name)
		}
	}
}
