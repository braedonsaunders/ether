package personalize

import (
	"context"
	"reflect"
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestPrecomposedRowBehaviorPreservesLegacyClassification(t *testing.T) {
	want := rowBehavior{countsInBudget: false, claimsFirst: true, participatesInDraft: false, requiresMinimum: false, participatesInLead: false, learnsKind: false}
	for _, source := range []string{core.RowSourcePlex, core.RowSourcePrecomposed} {
		if got := behaviorForRowSource(source); got != want {
			t.Fatalf("source=%q got=%+v", source, got)
		}
	}
	for _, source := range []string{core.RowSourceEngine, core.RowSourceOwner, "unknown"} {
		got := behaviorForRowSource(source)
		if !got.countsInBudget || got.claimsFirst || !got.participatesInDraft || !got.requiresMinimum || !got.participatesInLead || !got.learnsKind {
			t.Fatalf("source=%q got=%+v", source, got)
		}
	}
	if core.RowSourcePlex != "plex" {
		t.Fatal("legacy persisted source changed")
	}
}

func TestPrecomposedRowsMatchLegacyAssemblyAndLead(t *testing.T) {
	cat := newFakeCatalog(testNow)
	makeRows := func(source string) []core.HomeRow {
		utility := testRow(KindContinue, "continue", items(cat, movieKey(0, 1), movieKey(0, 2)))
		utility.source, utility.keepOrder, utility.minItems = source, true, 0
		picks := testRow(KindTrending, "trending", items(cat, movieKey(0, 1), movieKey(1, 1), movieKey(1, 2), movieKey(1, 3), movieKey(1, 4), movieKey(1, 5), movieKey(1, 6)))
		return run(emptyModel(utility, picks), func(a *assembly) {
			a.limit = 6
			a.maxRows = 1
			a.cw = items(cat, movieKey(0, 1), movieKey(0, 2))
		})
	}
	legacy := makeRows(core.RowSourcePlex)
	neutral := makeRows(core.RowSourcePrecomposed)
	if len(legacy) != 2 || len(legacy) != len(neutral) {
		t.Fatalf("legacy=%+v neutral=%+v", legacy, neutral)
	}
	continueRow := rowByKind(legacy, KindContinue)
	trending := rowByKind(legacy, KindTrending)
	if continueRow == nil || len(continueRow.Items) != 2 || trending == nil {
		t.Fatalf("precomposed row did not retain items outside budget: %+v", legacy)
	}
	for _, title := range trending.Items {
		if title.Key == movieKey(0, 1) {
			t.Fatal("precomposed item was drafted into trending")
		}
	}
	legacyLead := leadFromRows(legacy, 3)
	neutralLead := leadFromRows(neutral, 3)
	if !reflect.DeepEqual(legacyLead, neutralLead) {
		t.Fatalf("legacy lead=%+v neutral lead=%+v", legacyLead, neutralLead)
	}
	if legacyLead.keys[tasteKey(movieKey(0, 1))] || neutralLead.keys[tasteKey(movieKey(0, 1))] {
		t.Fatal("precomposed continue item entered discovery lead")
	}
	for i := range neutral {
		if neutral[i].Source == core.RowSourcePrecomposed {
			neutral[i].Source = core.RowSourcePlex
		}
	}
	if !reflect.DeepEqual(legacy, neutral) {
		t.Fatalf("legacy rows=%+v neutral rows=%+v", legacy, neutral)
	}
}

func TestPrecomposedRowsMatchLegacyMinimumAndTraining(t *testing.T) {
	cat := newFakeCatalog(testNow)
	for _, source := range []string{core.RowSourcePlex, core.RowSourcePrecomposed} {
		utility := testRow(KindPlaylists, "playlists", items(cat, movieKey(0, 1)))
		utility.source, utility.keepOrder, utility.minItems = source, true, 5
		rows := run(emptyModel(utility), nil)
		if row := rowByKind(rows, KindPlaylists); row == nil || len(row.Items) != 1 {
			t.Fatalf("source=%q short precomposed row dropped: %+v", source, rows)
		}
	}
	for _, source := range []string{core.RowSourcePlex, core.RowSourcePrecomposed} {
		if learnedRowKind(KindPlaylists, source) {
			t.Fatalf("source=%q unexpectedly learned", source)
		}
	}
	if !learnedRowKind(KindPlaylists, core.RowSourceEngine) || learnedRowKind(KindContinue, core.RowSourceEngine) {
		t.Fatal("engine or skipped-kind learning classification changed")
	}
	if kindFeature(KindPlaylists) != "kind:other" || kindFeature(KindContinue) != "kind:other" || kindFeature(KindTrending) != "kind:"+KindTrending {
		t.Fatal("real registry kind features changed")
	}
	names := designNames()
	has := func(name string) bool {
		for _, n := range names {
			if n == name {
				return true
			}
		}
		return false
	}
	if has("kind:"+KindPlaylists) || has("kind:"+KindContinue) || !has("kind:"+KindTrending) || !has("kind:other") {
		t.Fatalf("real registry design features changed: %v", names)
	}

}

func TestNeutralPrecomposedHomeMatchesLegacyConfiguredLayout(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	viewer := &core.User{ID: 1}
	hc := hubContext(testNow, core.DeviceTV)
	_, err := h.e.SetHomeLayout(ctx, core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "home.continue", Status: "always", Slot: 1}, {Row: "movie.trending", Status: "always"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	hubs := []core.Hub{{ID: "movie.trending", Title: "first", ItemCount: 1}, {ID: "home.continue", Title: "Continue", ItemCount: 1}, {ID: "movie.trending", Title: "last", ItemCount: 1}}
	legacy, err := h.e.FinalizeHome(ctx, viewer, hc, "home", hubs)
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]core.NeutralHomeRow, len(hubs))
	for i, h := range hubs {
		candidates[i] = core.NeutralHomeRow{ID: h.ID, Title: h.Title, ItemCount: h.ItemCount}
	}
	got, err := h.e.FinalizeNeutralHome(ctx, viewer, core.HomeSituation{Device: hc.Device, Daypart: hc.Daypart, Weekend: hc.Weekend, Now: hc.Now, LibraryID: hc.SectionID, Product: hc.Product, Platform: hc.Platform, SessionID: hc.SessionID, ClientID: hc.ClientID}, "home", candidates)
	if err != nil {
		t.Fatal(err)
	}
	var oldIDs, newIDs []string
	for _, h := range legacy {
		oldIDs = append(oldIDs, h.ID)
	}
	for _, r := range got {
		newIDs = append(newIDs, r.ID)
	}
	if !reflect.DeepEqual(oldIDs, newIDs) || len(newIDs) != 3 || newIDs[0] != "home.continue" {
		t.Fatalf("legacy=%v neutral=%v", oldIDs, newIDs)
	}
}
