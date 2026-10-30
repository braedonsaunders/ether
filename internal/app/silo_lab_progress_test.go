//go:build plexparity && conformlab

package app

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestSiloLabHomeProgressUsesOnlyObservedMatchingTitles(t *testing.T) {
	home := map[string]any{"sections": []any{map[string]any{"items": []any{
		map[string]any{"type": "episode", "series_id": "series-tvdb-78927", "season_number": float64(1), "episode_number": float64(2), "series_title": "Fixture series", "item_source": "in_progress", "position_seconds": float64(30), "duration_seconds": float64(600)},
		map[string]any{"type": "episode", "series_id": "another-series", "season_number": float64(1), "episode_number": float64(2), "item_source": "in_progress", "position_seconds": float64(30), "duration_seconds": float64(600)},
		map[string]any{"type": "movie", "content_id": "movie-tmdb-10378", "item_source": "next_up", "position_seconds": float64(30), "duration_seconds": float64(600)},
		map[string]any{"type": "movie", "content_id": "movie-tmdb-45745", "item_source": "in_progress", "position_seconds": float64(60), "duration_seconds": float64(888)},
	}}}}
	entries := siloLabHomeProgress(home)
	if len(entries) != 2 || entries[0].key != core.EpisodeKey(91461, 1, 2) || entries[0].seriesTitle != "Fixture series" || entries[0].position != 30*time.Second || entries[1].key != core.MovieKey(45745) {
		t.Fatalf("unexpected reference-matched progress: %+v", entries)
	}
}

func TestSiloLabNextUpRequiresMatchingReferenceIdentity(t *testing.T) {
	home := map[string]any{"sections": []any{map[string]any{"items": []any{
		map[string]any{"type": "episode", "series_id": "series-tvdb-78927", "season_number": float64(1), "episode_number": float64(2), "series_title": "Fixture series", "item_source": "next_up"},
		map[string]any{"type": "episode", "series_id": "another-series", "season_number": float64(1), "episode_number": float64(2), "item_source": "next_up"},
		map[string]any{"type": "episode", "series_id": "series-tvdb-78927", "season_number": float64(1), "episode_number": float64(2), "item_source": "in_progress"},
	}}}}
	entries := siloLabNextUp(home)
	if len(entries) != 1 || entries[0].key != core.EpisodeKey(91461, 1, 2) || entries[0].seriesTitle != "Fixture series" {
		t.Fatalf("unexpected next-up candidates: %+v", entries)
	}
}
