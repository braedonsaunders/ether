package app

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestFrontendVersionExposesOnlyReleaseSourceFacts(t *testing.T) {
	titleDate := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
	published := time.Date(2024, 4, 5, 0, 0, 0, 0, time.UTC)
	title := &core.Title{UpdatedAt: titleDate}
	v := core.ReleaseVariant{ID: "release-id", Label: "1080p", Release: core.Release{Name: "/private/folder/movie.mkv?token=secret", PubDate: published}}
	got := frontendVersion(title, v)
	if got.DateModified == nil || !got.DateModified.Equal(published) || !got.AddedAt.Equal(published) {
		t.Fatalf("publication fact lost: %+v", got)
	}
	if got.FileName != "" || got.PartCount != nil || got.ETag != "" {
		t.Fatalf("unverified source facts exposed: %+v", got)
	}
	v.Release.PubDate = time.Time{}
	got = frontendVersion(title, v)
	if got.DateModified != nil || !got.AddedAt.Equal(titleDate) {
		t.Fatalf("catalogue date treated as release metadata: %+v", got)
	}
}
