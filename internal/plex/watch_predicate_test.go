package plex

import (
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestUnwatchedSeasonChildrenUseLeaves(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.state.set(ownerUser.ID, core.WatchStatus{Key: bbS1.Key, ViewCount: 7})
	f.state.set(ownerUser.ID, core.WatchStatus{Key: bbE1.Key, ViewCount: 1})
	path := "/library/metadata/" + breakingBad.Key.String() + "/children"
	for _, tc := range []struct {
		query string
		want  int
	}{{"unwatched=1", 1}, {"unwatched=0", 0}, {"unwatched=2", 0}, {"unwatched!=0", 1}} {
		rec := f.get(t, path+"?"+tc.query, jsonAccept...)
		var out struct{ MediaContainer struct{ Metadata []any } }
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.MediaContainer.Metadata) != tc.want {
			t.Fatalf("%s got %s", tc.query, rec.Body.String())
		}
	}
}
func TestWatchFilterBooleanGrammar(t *testing.T) {
	for _, tc := range []struct {
		op, v string
		want  bool
	}{{"=", "1", true}, {"!=", "0", true}, {"=", "true", false}, {"=", "01", false}, {"==", "1", false}, {">>", "0", false}} {
		got, _ := parseBoolFilter(tc.op, tc.v)
		if got != tc.want {
			t.Fatalf("%s %s =%t want%t", tc.op, tc.v, got, tc.want)
		}
	}
}

func TestChildWatchQueryBeforePageAndFlagValidation(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	f.state.set(ownerUser.ID, core.WatchStatus{Key: bbS1.Key, ViewCount: 7})
	f.state.set(ownerUser.ID, core.WatchStatus{Key: bbE1.Key, ViewCount: 1})
	f.state.set(ownerUser.ID, core.WatchStatus{Key: bbE2.Key, UserRating: 7})
	base := "/library/metadata/" + breakingBad.Key.String()
	for _, tc := range []struct {
		path         string
		status, size int
		key          string
	}{
		{base + "/children?skipCount=0", 200, 1, bbS1.Key.String()},
		{base + "/children?viewCount=0", 200, 0, ""},
		{base + "/children?sort=invalid", 500, 0, ""},
		{base + "/allLeaves?unwatched=01", 200, 1, bbE2.Key.String()},
		{base + "/allLeaves?unwatched=abc", 400, 0, ""},
		{base + "/allLeaves?viewCount=0", 200, 1, bbE2.Key.String()},
	} {
		r := f.get(t, tc.path, "Accept", "application/json", "X-Plex-Container-Start", "0", "X-Plex-Container-Size", "1")
		if r.Code != tc.status {
			t.Fatalf("%s status%d want%d: %s", tc.path, r.Code, tc.status, r.Body.String())
		}
		if tc.status != 200 {
			continue
		}
		var out struct {
			MediaContainer struct{ Metadata []struct{ RatingKey string } }
		}
		if err := json.Unmarshal(r.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.MediaContainer.Metadata) != tc.size {
			t.Fatalf("%s got%s", tc.path, r.Body.String())
		}
		if tc.size > 0 && out.MediaContainer.Metadata[0].RatingKey != tc.key {
			t.Fatalf("%s selected wrong scoped row: %s", tc.path, r.Body.String())
		}
	}
}
