package ratings

import "testing"

func TestPresentUsesConfiguredSourcesAndSkipsMissing(t *testing.T) {
	cfg := Settings{Enabled: true, Critic: "tomatoes", Audience: "imdb"}
	got := Present(cfg, []Badge{
		{Source: "imdb", Value: 8.2, Scale: 10, Votes: 1000},
		{Source: "tomatoes", Value: 94, Scale: 100},
		{Source: "letterboxd", Value: 4.1, Scale: 5, Votes: 20},
		{Source: "popcorn", Value: 88, Scale: 100},
	})
	if got.Community == nil || *got.Community != 8.2 || got.Critic == nil || *got.Critic != 94 {
		t.Fatalf("scales: community=%v critic=%v", got.Community, got.Critic)
	}
	if len(got.Sources) != 4 || got.Sources[2].Source != "letterboxd" || got.Sources[2].Name != "Letterboxd" || got.Sources[2].Display != "4.1" || got.Sources[2].Score != 82 {
		t.Fatalf("sources: %+v", got.Sources)
	}
	if got.Sources[1].Display != "94%" || got.Sources[1].Source != "rt_critic" {
		t.Fatalf("critic source: %+v", got.Sources[1])
	}
	absent := Present(cfg, []Badge{{Source: "imdb", Value: 8.2, Scale: 10}})
	if absent.Critic != nil || absent.Community == nil {
		t.Fatalf("missing critic must stay absent: %+v", absent)
	}
	off := Present(Settings{Critic: "tomatoes", Audience: "imdb"}, []Badge{{Source: "tomatoes", Value: 94, Scale: 100}})
	if off.Critic != nil || off.Community != nil || len(off.Sources) != 0 {
		t.Fatalf("disabled provider must show nothing: %+v", off)
	}
}
