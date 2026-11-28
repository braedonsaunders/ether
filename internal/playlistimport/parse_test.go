package playlistimport

import "testing"

func TestParseM3U(t *testing.T) {
	name, tracks, err := Parse("m3u", "\uFEFF#EXTM3U\n#PLAYLIST:Road Trip\n#EXTINF:320,Daft Punk - One More Time\n/music/a.flac\n03 - Massive Attack - Teardrop.mp3\n")
	if err != nil || name != "Road Trip" || len(tracks) != 2 {
		t.Fatalf("%q %+v %v", name, tracks, err)
	}
	if tracks[0].Artist != "Daft Punk" || tracks[0].Title != "One More Time" || tracks[0].DurationSec != 320 {
		t.Fatalf("extinf: %+v", tracks[0])
	}
	if tracks[1].Artist != "Massive Attack" || tracks[1].Title != "Teardrop" {
		t.Fatalf("filename: %+v", tracks[1])
	}
}

func TestParseCSVExport(t *testing.T) {
	csv := "Track Name,Artist Name(s),Album Name,ISRC,Duration (ms)\n\"One More Time\",\"Daft Punk\",Discovery,gbduw0000053,320357\nTeardrop,\"Massive Attack, Elizabeth Fraser\",Mezzanine,,\n"
	_, tracks, err := Parse("csv", csv)
	if err != nil || len(tracks) != 2 {
		t.Fatalf("%+v %v", tracks, err)
	}
	if tracks[0].ISRC != "GBDUW0000053" || tracks[0].DurationSec < 320 || tracks[0].DurationSec > 321 || tracks[0].Album != "Discovery" {
		t.Fatalf("row 1: %+v", tracks[0])
	}
	if tracks[1].Artist != "Massive Attack" {
		t.Fatalf("row 2 artist: %+v", tracks[1])
	}
	if _, _, err := Parse("csv", "foo,bar\n1,2\n"); err == nil {
		t.Fatal("a CSV without a title column was read")
	}
	if _, _, err := Parse("xspf", "x"); err == nil {
		t.Fatal("unknown format read")
	}
}

func TestParseM3UQuotesBeforeDisplayName(t *testing.T) {
	_, tracks, err := Parse("m3u", `#EXTM3U
#EXTINF:320 tvg-name="One, Two" album='Three, Four',Earth, Wind & Fire - September, live
/music/a.flac
`)
	if err != nil || len(tracks) != 1 || tracks[0].Artist != "Earth, Wind & Fire" || tracks[0].Title != "September, live" || tracks[0].DurationSec != 320 {
		t.Fatalf("tracks = %+v, err = %v", tracks, err)
	}
}

func TestParseCSVPreservesNamedArtist(t *testing.T) {
	_, tracks, err := Parse("csv", "Title,Artist Name\nSeptember,\"Earth, Wind & Fire\"\n")
	if err != nil || len(tracks) != 1 || tracks[0].Artist != "Earth, Wind & Fire" {
		t.Fatalf("tracks = %+v, err = %v", tracks, err)
	}
}
