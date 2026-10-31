package plex

import (
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/store"
)

// TestFirstCharacterJumpbarOffsetsMatchTitleSortEveryLetter drives the section
// handler this server actually serves: /library/sections/{id}/firstCharacter
// and the title-sorted library list behind each jump.
func TestFirstCharacterJumpbarOffsetsMatchTitleSortEveryLetter(t *testing.T) {
	cfg := config.Catalog{
		Exclusions: []core.LibraryExclusion{{
			Name:      "English/French only",
			MediaType: "movie",
			Match:     "all",
			Rules: []core.CollectionRule{
				{Field: "language", Op: "notIn", Value: json.RawMessage(`["en","fr"]`)},
			},
		}},
	}
	c, err := catalog.New(store.OpenTest(t), nil, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	rel := time.Now().Add(-time.Hour)
	var nextID int64 = 1
	add := func(title, lang string) {
		id := nextID
		nextID++
		ti := core.Title{
			Key:      core.MovieKey(id),
			Type:     core.TypeMovie,
			Title:    title,
			Language: lang,
			Released: rel,
		}
		if err := c.UpsertTitle(ctx, ti, true); err != nil {
			t.Fatal(err)
		}
	}
	add("!Three Amigos!", "en")
	add("1917", "en")
	add("2001: A Space Odyssey", "en")
	add("The 39 Steps", "en")
	for ch := byte('A'); ch <= 'Z'; ch++ {
		letter := string(ch)
		add(letter+"lpha "+letter, "en")
		add("The "+letter+"ravo "+letter, "en")
		add("A "+letter+"harlie "+letter, "en")
		add("An "+letter+"elta "+letter, "en")
		add(letter+" Hidden Hindi", "hi")
	}
	add("Été 85", "fr")
	add("七人の侍", "ja")
	add("Солярис", "ru")

	f := newFixture(func(d *Deps) { d.Catalog = c })
	defer f.srv.Close()
	decode := func(t *testing.T, rec *httptest.ResponseRecorder) model.MediaContainer {
		t.Helper()
		var body struct {
			MediaContainer model.MediaContainer
		}
		if rec.Code != 200 {
			t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.MediaContainer
	}
	page := func(t *testing.T, path string, start, size int) model.MediaContainer {
		t.Helper()
		return decode(t, f.get(t, path,
			jsonAccept[0], jsonAccept[1],
			"X-Plex-Container-Start", strconv.Itoa(start),
			"X-Plex-Container-Size", strconv.Itoa(size),
		))
	}

	dirs := decode(t, f.get(t, "/library/sections/1/firstCharacter", jsonAccept...)).Directory
	// One "#" (digits and symbols that sort before "a") plus A–Z. The
	// non-ASCII title sorts after Z and is not a second "#".
	if len(dirs) != 27 {
		t.Fatalf("expected 27 firstCharacter buckets, got %d", len(dirs))
	}
	hashTitles := 0
	for _, d := range dirs {
		if d.Title == "#" {
			hashTitles++
		}
	}
	if hashTitles != 1 {
		t.Fatalf("jump bar titled %d entries #; tvOS adds every one into the A offset", hashTitles)
	}
	if dirs[0].Key != "%23" && dirs[0].Key != "#" || dirs[0].Title != "#" || dirs[0].Size == nil || *dirs[0].Size != 4 {
		t.Fatalf("unexpected leading # bucket: %+v", dirs[0])
	}
	last := dirs[len(dirs)-1]
	if last.Key != "Z" || last.Title != "Z" {
		t.Fatalf("jump bar should end on Z, got %+v", last)
	}
	reversed := decode(t, f.get(t, "/library/sections/1/firstCharacter?sort=titleSort:desc", jsonAccept...)).Directory
	if len(reversed) != len(dirs) || reversed[0].Key != last.Key || reversed[len(reversed)-1].Key != dirs[0].Key {
		t.Fatalf("descending jumpbar did not reverse the letter order")
	}

	all := page(t, "/library/sections/1/all?type=1&sort=titleSort:asc", 0, 500)
	if all.TotalSize == nil {
		t.Fatal("title sort omitted the total")
	}
	offset := 0
	for _, d := range dirs {
		if d.Size == nil || d.FastKey == "" {
			t.Fatalf("bucket %s missing size or fastKey", d.Key)
		}
		window := page(t, "/library/sections/1/all?type=1&sort=titleSort:asc", offset, *d.Size)
		filtered := page(t, d.FastKey+"&type=1&sort=titleSort:asc", 0, 500)
		keyed := page(t, "/library/sections/1/firstCharacter/"+d.Key+"?type=1&sort=titleSort:asc", 0, 500)
		if len(window.Metadata) != *d.Size || len(filtered.Metadata) != *d.Size || len(keyed.Metadata) != *d.Size {
			t.Fatalf("bucket %s: window=%d filtered=%d keyed=%d want %d", d.Key, len(window.Metadata), len(filtered.Metadata), len(keyed.Metadata), *d.Size)
		}
		for i := range window.Metadata {
			if filtered.Metadata[i].RatingKey != window.Metadata[i].RatingKey || keyed.Metadata[i].RatingKey != window.Metadata[i].RatingKey {
				t.Fatalf("bucket %s item %d mismatch: jumpbar=%s filtered=%s keyed=%s", d.Key, i, window.Metadata[i].Title, filtered.Metadata[i].Title, keyed.Metadata[i].Title)
			}
			sortTitle := strings.ToLower(window.Metadata[i].TitleSort)
			switch d.Title {
			case "#":
				if d.Key == "~" {
					if sortTitle < "{" {
						t.Fatalf("bucket ~ contained %q (sort %q)", window.Metadata[i].Title, sortTitle)
					}
				} else if sortTitle >= "a" {
					t.Fatalf("bucket # contained %q (sort %q)", window.Metadata[i].Title, sortTitle)
				}
			default:
				if !strings.HasPrefix(sortTitle, strings.ToLower(d.Title)) {
					t.Fatalf("bucket %s landed on %q (sort %q)", d.Title, window.Metadata[i].Title, sortTitle)
				}
			}
		}
		offset += *d.Size
	}
	// Non-ASCII sort titles sit after Z. They stay in the library total and
	// off the jump bar, so they cannot inflate the offset of A.
	if offset != *all.TotalSize-1 {
		t.Fatalf("sum of jump-bar counts (%d) != title-sorted total minus the trailing title (%d)", offset, *all.TotalSize-1)
	}
	if got := all.Metadata[len(all.Metadata)-1].Title; got != "Été 85" {
		t.Fatalf("expected the non-ASCII title after Z, last item %q", got)
	}
}
