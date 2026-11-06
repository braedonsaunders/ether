package catalog

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestBrowseStateComparisonSettingsPresence(t *testing.T) {
	c := newChunkTestCatalog(t)
	for i := int64(1); i <= 3; i++ {
		if _, err := c.db.Exec(`INSERT INTO catalog_titles(key,type,title,sort_title,released_ts,browsable,adult) VALUES(?,'movie','x','x',1600000000,1,0)`, int64(core.MovieKey(i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.Exec(`INSERT INTO state_watch(user_id,rating_key,view_count,view_offset_ns,user_rating) VALUES(1,?,0,120000000000,7),(1,?,2,0,0)`, int64(core.MovieKey(1)), int64(core.MovieKey(2))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field, op string
		value     int64
		want      int
	}{{"viewCount", ">=", 0, 2}, {"skipCount", "=", 0, 2}, {"viewCount", "!=", 1, 2}, {"viewCount", "<<", 0, 0}, {"viewOffset", "=", 0, 2}, {"viewOffset", ">>", 60000, 1}} {
		q := core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie, Group: &core.FilterGroup{UserID: 1, StateField: tc.field, StateOperator: tc.op, StateValue: tc.value}}, Limit: 10}
		p, err := c.Browse(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != tc.want {
			t.Errorf("%s %s %d total %d want %d", tc.field, tc.op, tc.value, p.Total, tc.want)
		}
	}
}

func TestBrowseWatchedLeafPredicates(t *testing.T) {
	c := newChunkTestCatalog(t)
	show := core.ShowKey(42)
	season := core.SeasonKey(42, 1)
	for _, row := range []struct {
		k          core.RatingKey
		typ        string
		parent     core.RatingKey
		season, ep int
	}{{show, "show", 0, 0, 0}, {season, "season", show, 1, 0}, {core.EpisodeKey(42, 1, 1), "episode", show, 1, 1}, {core.EpisodeKey(42, 1, 2), "episode", show, 1, 2}} {
		if _, err := c.db.Exec(`INSERT INTO catalog_titles(key,type,title,sort_title,show_key,season,episode,released_ts,browsable,adult) VALUES(?,?,'x','x',?,?,?,1600000000,1,0)`, int64(row.k), row.typ, int64(row.parent), row.season, row.ep); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.db.Exec(`INSERT INTO state_watch(user_id,rating_key,view_count,view_offset_ns) VALUES(1,?,1,0),(1,?,0,120000000000)`, int64(core.EpisodeKey(42, 1, 1)), int64(core.EpisodeKey(42, 1, 2))); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field string
		n     int64
	}{{"viewedLeafCount", 1}, {"viewedOrInProgressLeafCount", 2}, {"unviewedLeafCount", 1}} {
		p, err := c.Browse(context.Background(), core.BrowseQuery{Filter: core.Filter{Type: core.TypeShow, Group: &core.FilterGroup{UserID: 1, StateField: tc.field, StateOperator: "=", StateValue: tc.n}}, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != 1 {
			t.Fatalf("%s total %d want1", tc.field, p.Total)
		}
	}
}
