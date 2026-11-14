package stremio

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// Exercises the numbering boundary as one invariant: remote numbering can
// change a route and a target, but never become proof of filename identity.
func TestSpecialIdentityMapping(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/meta/series/tt123.json" {
			t.Errorf("metadata route %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"meta":{"videos":[{"season":0,"episode":9,"name":"The Path of the Ball"},{"season":0,"episode":8,"name":"Land vs Air"},{"season":0,"episode":11,"name":"Quest Picture Drama 1"},{"season":0,"episode":13,"name":"Quest Picture Drama 3"}]}}`)
	}))
	defer srv.Close()
	mapper := NewSpecialMapper(srv.Client(), srv.URL)
	req := core.SearchRequest{Type: core.TypeEpisode, IDs: core.IDs{IMDB: "tt123"}, Title: "Haikyu!!", Season: 0, Episode: 5, EpisodeTitle: "The Path of the Ball"}
	m := Manifest{Resources: Resources{{Name: ResourceStream, Types: []string{TypeSeries}, IDPrefixes: []string{"tt"}}}}
	look, ok := LookupStreams(context.Background(), m, req, nil, mapper)
	if !ok || look.ID != "tt123:0:9" {
		t.Fatalf("lookup %+v %v", look, ok)
	}
	if _, ok := LookupStreams(context.Background(), m, req, nil, nil); ok {
		t.Fatal("unmapped special asked")
	}
	for _, c := range []struct {
		title   string
		episode int
		ok      bool
	}{
		{"Path of Ball", 9, true}, {"Quest Picture Drama 3", 13, true}, {"Quest Picture Drama", 0, false}, {"Unknown", 0, false},
	} {
		r := req
		r.EpisodeTitle = c.title
		ep, ok := mapper.LookupSpecial(context.Background(), r)
		if ok != c.ok || ok && ep != c.episode {
			t.Errorf("%q: %d %v", c.title, ep, ok)
		}
	}
	if hits != 1 {
		t.Errorf("metadata fetched %d times", hits)
	}
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Haikyu!!.S00E09.The.Path.of.the.Ball.1080p.mkv", true},
		{"Haikyu!!.S00E05.1080p.mkv", false},
		{"Haikyu!!.S00E09.Land.vs.Air.1080p.mkv", false},
		{"Haikyu!!.S01E05.1080p.mkv", false},
		{"Haikyu!!.6x00.1080p.mkv", false},
		{"Haikyu!! - 590 [1080p].mkv", false},
		{"Haikyu!! OAD 02 [1080p].mkv", false},
		{"Haikyu!! Special The Path of the Ball [1080p].mkv", true},
	} {
		out := Convert([]Stream{{Title: c.name, InfoHash: strings.Repeat("a", 40)}}, req, look, ConvertOptions{})
		if (len(out.Releases) == 1) != c.ok {
			t.Errorf("%q releases %d", c.name, len(out.Releases))
			continue
		}
		if c.ok {
			r := out.Releases[0]
			if r.Target == nil || r.Target.Episode != 5 || r.Target.AddonSpecialEpisode != 9 || len(r.Info.Episodes) != 1 || r.Info.Episodes[0] != 5 {
				t.Errorf("identity %+v", r)
			}
		}
	}
	req.EpisodeTitle = "same"
	if _, ok := matchSpecialTitle(req, []specialVideo{{Episode: 1, Name: "same"}, {Episode: 2, Name: "same"}}); ok {
		t.Error("ambiguous exact title mapped")
	}
}
