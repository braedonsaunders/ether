package personalize

import (
	"context"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPolicyHomeCandidatesFilterBeforeLimitsAndIsolateCache(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	sciFiFan(h, 1)
	h.rebuild(*viewer)
	hc := hubContext(testNow, core.DeviceTV)
	original, err := h.e.HomeCandidatesFor(ctx, viewer, hc, 2)
	if err != nil {
		t.Fatal(err)
	}
	allowed, err := h.e.HomeCandidatesWithPolicy(ctx, viewer, hc, 2, func(context.Context, *core.User, *core.Title) bool { return true })
	if err != nil || !reflect.DeepEqual(original, allowed) {
		t.Fatalf("allow-all differs: %v", err)
	}
	var blocked core.RatingKey
	for _, row := range original {
		if len(row.Items) > 0 {
			blocked = row.Items[0].Key
			break
		}
	}
	if blocked == 0 {
		t.Fatal("fixture has no candidate")
	}
	filtered, err := h.e.HomeCandidatesWithPolicy(ctx, viewer, hc, 2, func(_ context.Context, _ *core.User, title *core.Title) bool { return title.Key != blocked })
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range filtered {
		for _, title := range row.Items {
			if title.Key == blocked {
				t.Fatal("restricted item leaked")
			}
		}
	}
	restored, err := h.e.HomeCandidatesWithPolicy(ctx, viewer, hc, 2, func(context.Context, *core.User, *core.Title) bool { return true })
	if err != nil || !reflect.DeepEqual(original, restored) {
		t.Fatal("restricted policy polluted allow-all cache")
	}
	if _, err := h.e.HomeCandidatesWithPolicy(ctx, viewer, hc, 2, nil); err == nil {
		t.Fatal("nil policy accepted")
	}
}

func TestPolicyHiddenCandidatesBackfillBeforeRowCap(t *testing.T) {
	cat := newFakeCatalog(testNow)
	keys := []core.RatingKey{movieKey(0, 1), movieKey(0, 2), movieKey(0, 3), movieKey(0, 4), movieKey(0, 5)}
	row := testRow(KindTop10, "top", items(cat, keys...))
	row.keepOrder = true
	a := &assembly{model: emptyModel(row), user: core.User{ID: 1}, hc: hubContext(testNow, core.DeviceTV), limit: 3, hidden: map[core.RatingKey]bool{keys[0]: true}}
	got := a.contextItems(row)
	if len(got) != 4 || got[0].t.Key != keys[1] || got[1].t.Key != keys[2] || got[2].t.Key != keys[3] {
		t.Fatalf("filtered ranking: %+v", got)
	}
	a.section = core.TypeMovie
	got = a.contextItems(row)
	if len(got) != 4 || got[0].t.Key != keys[1] || got[2].t.Key != keys[3] {
		t.Fatalf("section backfill: %+v", got)
	}
	for _, key := range keys {
		a.hidden[key] = true
	}
	if got := a.contextItems(row); len(got) != 0 {
		t.Fatalf("exhausted pool returned candidates: %+v", got)
	}
}

func TestSectionCandidatesPolicyMatchesAndIsolatesViewer(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	sciFiFan(h, 1)
	h.rebuild(*viewer)
	hc := hubContext(testNow, core.DeviceTV)
	original, err := h.e.SectionCandidatesFor(ctx, viewer, core.TypeMovie, hc, 12)
	if err != nil {
		t.Fatal(err)
	}
	allow := func(context.Context, *core.User, *core.Title) bool { return true }
	same, err := h.e.SectionCandidatesWithPolicy(ctx, viewer, core.TypeMovie, hc, 12, allow)
	if err != nil || !reflect.DeepEqual(original, same) {
		t.Fatalf("section allow-all differs: %v", err)
	}
	var denied core.RatingKey
	for _, row := range original {
		if len(row.Items) > 0 {
			denied = row.Items[0].Key
			break
		}
	}
	if denied == 0 {
		t.Fatal("section has no candidates")
	}
	restricted, err := h.e.SectionCandidatesWithPolicy(ctx, viewer, core.TypeMovie, hc, 12, func(_ context.Context, _ *core.User, t *core.Title) bool { return t.Key != denied })
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range restricted {
		for _, title := range row.Items {
			if title.Key == denied {
				t.Fatal("section policy leaked")
			}
		}
	}
	restored, err := h.e.SectionCandidatesFor(ctx, viewer, core.TypeMovie, hc, 12)
	if err != nil || !reflect.DeepEqual(original, restored) {
		t.Fatal("section policy changed unrestricted results")
	}
}

func TestPolicyHomeChecksLiveContinue(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	sciFiFan(h, 1)
	h.start(1, movieKey(0, 10), 0.5, testNow)
	h.rebuild(*viewer)
	target := movieKey(0, 10)
	called := false
	_, err := h.e.HomeCandidatesWithPolicy(ctx, viewer, hubContext(testNow, core.DeviceTV), 6, func(_ context.Context, _ *core.User, title *core.Title) bool {
		if title.Key == target {
			called = true
			return false
		}
		return true
	})
	if err != nil || !called {
		t.Fatalf("live continue candidate not checked: %v", err)
	}
	h.e.presentationMu.Lock()
	defer h.e.presentationMu.Unlock()
	for _, p := range h.e.presentations {
		for _, row := range p.rows {
			for _, key := range row.keys {
				if key == target {
					t.Fatal("denied continue candidate entered presentation")
				}
			}
		}
	}
}
