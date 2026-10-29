package libpath

import (
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestLibraryPathsAndSafeNames(t *testing.T) {
	if got := Movie(core.Title{Title: "The Film", Year: 2024}); got != "Movies/The Film (2024)/The Film (2024).mkv" {
		t.Fatal(got)
	}
	if got := Movie(core.Title{Title: "Film"}); got != "Movies/Film/Film.mkv" {
		t.Fatal(got)
	}
	if got := Episode(core.Title{ShowTitle: "Show", Season: 1, Episode: 2}); got != "TV/Show/Season 01/Show - S01E02.mkv" {
		t.Fatal(got)
	}
	if got := Show(core.Title{Title: "../A/B: C"}); got != "TV/A B C" {
		t.Fatal(got)
	}
	if got := Collection("..", "../trailer.mkv"); got != "Collections/Untitled/trailer.mkv" {
		t.Fatal(got)
	}
	for _, name := range []string{"", ".", "..", "/", "\x00"} {
		if got := Safe(name); got == "" || got == "." || got == ".." || strings.ContainsAny(got, "/\\") {
			t.Fatal(name, got)
		}
	}
}
