package catalog

import (
	"context"
	"strconv"
	"testing"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// Seasons and episodes stored without a rating take their series' rating
// and rank in key-ordered batches that resume after a restart, so a rating
// cap admits a series' seasons as it admits the series.
func TestBackfillChildRatingsGivesSeasonsTheirSeriesRating(t *testing.T) {
	ctx := context.Background()
	c := newChunkTestCatalog(t)
	for id := int64(1); id <= childRatingsBatch+3; id++ {
		rating := "TV-14"
		if id%2 == 0 {
			rating = "TV-MA"
		}
		show := core.Title{Key: core.ShowKey(id), Type: core.TypeShow, Title: "Show", ContentRating: rating, ContentRatingSource: "tmdb:US"}
		if err := c.upsert(ctx, show, upsertOpts{browsable: true}); err != nil {
			t.Fatal(err)
		}
		for _, child := range []core.Title{
			{Key: core.SeasonKey(id, 1), Type: core.TypeSeason, ShowKey: show.Key, Season: 1, Title: "Season 1"},
			{Key: core.EpisodeKey(id, 1, 1), Type: core.TypeEpisode, ShowKey: show.Key, Season: 1, Episode: 1, Title: "Pilot"},
		} {
			if err := c.upsert(ctx, child, upsertOpts{}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE catalog_titles SET content_rating='',content_rating_rank=0 WHERE type IN ('season','episode')`); err != nil {
		t.Fatal(err)
	}
	// One batch, then a "restart" that resumes from the stored cursor.
	n, last, err := c.childRatingsBatch(ctx, 0)
	if err != nil || n != childRatingsBatch {
		t.Fatalf("first batch = %d, %v", n, err)
	}
	if err := c.setMeta(ctx, childRatingsKey, strconv.FormatInt(last, 10)); err != nil {
		t.Fatal(err)
	}
	if err := c.backfillChildRatings(ctx); err != nil {
		t.Fatal(err)
	}
	if got := c.getMeta(ctx, childRatingsKey); got != "done" {
		t.Fatalf("cursor = %q", got)
	}
	var unrated int
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM catalog_titles WHERE type IN ('season','episode') AND (content_rating='' OR content_rating_rank=0)`).Scan(&unrated); err != nil || unrated != 0 {
		t.Fatalf("unrated children = %d, %v", unrated, err)
	}
	var rating string
	var rank int
	if err := c.db.QueryRowContext(ctx, `SELECT content_rating,content_rating_rank FROM catalog_titles WHERE key=?`, int64(core.SeasonKey(2, 1))).Scan(&rating, &rank); err != nil ||
		rating != "TV-MA" || rank != int(contentrating.LevelOf("TV-MA")) {
		t.Fatalf("season 2/1 = %q rank %d, %v", rating, rank, err)
	}
	// Done is done: a later start reads nothing.
	if err := c.backfillChildRatings(ctx); err != nil {
		t.Fatal(err)
	}
}
