package plex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

// This provider describes settings independently of its discovery cache.
// The Plex layer must project the supplied identities, not infer a count.
type configuredFakeVariants struct {
	*fakeVariantResolver
	plan []core.ReleaseVariant
}

func (v *configuredFakeVariants) VersionSlots(_ context.Context, req core.ResolveRequest, max int, labels bool) ([]core.ReleaseVariant, error) {
	out := append([]core.ReleaseVariant(nil), v.plan...)
	if labels {
		for i := range out {
			out[i].Label = fmt.Sprintf("Known release %d", i)
		}
	}
	return out, nil
}
func configuredVariantFixture(t *testing.T) (*variantFixture, *fakeVariantPlayback, *fakeVariantResolver) {
	f, p := newVariantPlaybackFixture(t)
	src := f.resolver
	plan := append([]core.ReleaseVariant(nil), testVariantLists()[fightClub.Key]...)
	f.srv.setVariants(&configuredFakeVariants{fakeVariantResolver: src, plan: plan})
	return f, p, src
}
func configuredResponseMedia(t *testing.T, r *httptest.ResponseRecorder, key core.RatingKey) []wireMedia {
	t.Helper()
	if r.Code != 200 {
		t.Fatalf("HTTP %d: %s", r.Code, r.Body.String())
	}
	var root any
	if e := json.Unmarshal(r.Body.Bytes(), &root); e != nil {
		t.Fatal(e)
	}
	var out []wireMedia
	var visit func(any)
	visit = func(x any) {
		switch v := x.(type) {
		case map[string]any:
			if v["ratingKey"] == key.String() {
				b, _ := json.Marshal(v["Media"])
				if e := json.Unmarshal(b, &out); e != nil {
					t.Fatal(e)
				}
				return
			}
			for _, child := range v {
				visit(child)
			}
		case []any:
			for _, child := range v {
				visit(child)
			}
		}
	}
	visit(root)
	if len(out) == 0 {
		t.Fatal("no playable Media")
	}
	return out
}
