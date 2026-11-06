package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

// Also run unchanged at 7330b3ced (v0.3.4). The capture contains summaries,
// not raw XML or image stamps; paths are reconstructed from authored art facts.
func TestIssue341V034ArtworkProjection(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	title := fightClub
	title.Key = core.MovieKey(718789)
	title.PosterPath = "/poster.jpg"
	title.BackdropPath = "/backdrop.jpg"
	for _, product := range []string{"Plex for Android (Mobile)", "Plex for Android (TV)", "Plex Web"} {
		for _, detail := range []bool{false, true} {
			md := f.srv.metadata(context.Background(), &Request{User: &ownerUser, Product: product, Platform: "Android"}, &title, renderOpts{detail: detail})
			wantThumb := "/library/metadata/1000000000718789/thumb/" + imageStamp(&title)
			wantArt := "/library/metadata/1000000000718789/art/" + imageStamp(&title)
			if md.Thumb != wantThumb || md.Art != wantArt {
				t.Fatalf("%s detail=%v: thumb=%q art=%q", product, detail, md.Thumb, md.Art)
			}
			t.Logf("%s detail=%v thumb=%s art=%s", product, detail, md.Thumb, md.Art)
		}
	}
}
