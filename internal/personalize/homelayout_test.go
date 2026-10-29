package personalize

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func sampleRows(ids ...string) []core.HomeRow {
	out := []core.HomeRow{}
	for _, id := range ids {
		out = append(out, core.HomeRow{HubID: id, Kind: id, Title: id, Hub: core.Hub{ID: id, Title: id}})
	}
	return out
}
func visibleIDs(rows []core.HomeRow) []string {
	out := []string{}
	for _, r := range rows {
		if !r.Hidden {
			out = append(out, r.HubID)
		}
	}
	return out
}
func TestLayeredConstraintsAndProvenance(t *testing.T) {
	hc := hubContext(testNow, core.DeviceTV)
	d := core.HomeLayout{Global: []core.LayoutPage{{Section: "home", MaxRows: 4, Rows: []core.RowPolicy{{Row: "anchor", Status: "always", Slot: 3}, {Row: "bounded", Status: "eligible", MinSlot: 2, MaxSlot: 2}, {Row: "off", Status: "off"}, {Row: "seasonal", Status: "always", When: &core.LayoutWhen{Months: []int{12}}}}}}, Viewers: []core.ViewerLayout{{UserID: 7, Pages: []core.LayoutPage{{Section: "home", DeviceMaxRows: map[string]int{"tv": 3}, Rows: []core.RowPolicy{{Row: "off", Status: "always", Slot: 1}}}}}}}
	got := arrangeLayout(sampleRows("off", "bounded", "plain", "anchor", "seasonal"), d, 7, hc, "home", 14)
	if ids := visibleIDs(got); !reflect.DeepEqual(ids, []string{"off", "bounded", "anchor"}) {
		t.Fatalf("slots = %v", ids)
	}
	if got[0].Layer != "override" || got[0].AnchorSlot != 1 || got[2].Layer != "global" || got[2].Slot != 3 {
		t.Fatalf("provenance = %+v", got)
	}
	for _, r := range got {
		if r.HubID == "seasonal" && (!r.Hidden || r.HiddenReason != "Outside schedule") {
			t.Fatalf("seasonal = %+v", r)
		}
	}
	d.Viewers = nil
	reset := arrangeLayout(sampleRows("off", "bounded", "plain", "anchor"), d, 7, hc, "home", 14)
	for _, r := range reset {
		if r.HubID == "off" && !r.Hidden {
			t.Fatal("reset failed to inherit hidden global policy")
		}
	}
}
func TestLayeredCapsAndConflictingAnchors(t *testing.T) {
	hc := hubContext(testNow, core.DeviceTV)
	d := core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "*", Status: "eligible", MaxPerPage: 2}}}}}
	if got := visibleIDs(arrangeLayout(sampleRows("a", "b", "c"), d, 4, hc, "home", 14)); len(got) != 2 {
		t.Fatalf("cap = %v", got)
	}
	d.Global[0].Rows = []core.RowPolicy{{Row: "global", Status: "always", Slot: 1}}
	d.Viewers = []core.ViewerLayout{{UserID: 4, Pages: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "mine", Status: "always", Slot: 1}}}}}}
	if got := visibleIDs(arrangeLayout(sampleRows("global", "mine", "free"), d, 4, hc, "home", 14)); got[0] != "mine" {
		t.Fatalf("viewer anchor precedence = %v", got)
	}
	for n := 1; n < 7; n++ {
		days := 0
		for day := 0; day < 7; day++ {
			hc.Now = time.Date(2026, 10, 12+day, 20, 0, 0, 0, time.UTC)
			if weekEligible(4, "seasonal", hc, n) {
				days++
			}
		}
		if days != n {
			t.Fatalf("weekly cap %d produced %d days", n, days)
		}
	}
}
func TestLayeredValidation(t *testing.T) {
	_, err := normalizeHomeLayout(core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "a", Status: "always", Slot: 1}, {Row: "b", Status: "always", Slot: 1}, {Row: "x", Status: "eligible", MinSlot: 5, MaxSlot: 2, MaxDaysPerWeek: 9, When: &core.LayoutWhen{Months: []int{13}, Dayparts: []string{"noon"}, StartDate: "2026-12-31", EndDate: "2026-01-01"}}}}}})
	ve, ok := err.(*ValidationError)
	if !ok || len(ve.Problems) < 5 {
		t.Fatalf("validation = %v", err)
	}
}
func TestEmptyLayoutByDefaultAndDraftDoesNotWrite(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	empty, err := h.e.store.homeLayout(ctx)
	if err != nil || len(empty.Global) != 0 || len(empty.Viewers) != 0 || empty.Global == nil || empty.Viewers == nil {
		t.Fatalf("unsaved layout = %+v %v", empty, err)
	}
	var stored int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM personalize_home_layout`).Scan(&stored); err != nil || stored != 0 {
		t.Fatal("reading the default layout wrote a document", stored, err)
	}
	doc := core.HomeLayout{
		Global:  []core.LayoutPage{{Section: "home", MaxRows: 8, Rows: []core.RowPolicy{{Row: KindTrending, Status: "always", Slot: 3}}}},
		Viewers: []core.ViewerLayout{{UserID: 1, Pages: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: KindTop10, Status: "off"}}}}}},
	}
	if _, err := h.e.SetHomeLayout(ctx, doc); err != nil {
		t.Fatal(err)
	}
	d, err := h.e.store.homeLayout(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if d.Global[0].Rows[0].Slot != 3 || d.Viewers[0].Pages[0].Rows[0].Status != "off" {
		t.Fatalf("saved layout = %+v", d)
	}
	var rawBefore string
	if err := h.e.store.db.QueryRow(`SELECT document FROM personalize_home_layout WHERE id=1`).Scan(&rawBefore); err != nil {
		t.Fatal(err)
	}
	d.Global[0].Rows[0].Status = "off"
	u := core.User{ID: 1}
	sciFiFan(h, 1)
	h.rebuild(u)
	if _, err := h.e.PreviewLayout(ctx, &u, hubContext(testNow, core.DeviceTV), "home", 12, d); err != nil {
		t.Fatal(err)
	}
	var rawAfter string
	_ = h.e.store.db.QueryRow(`SELECT document FROM personalize_home_layout WHERE id=1`).Scan(&rawAfter)
	if rawBefore != rawAfter {
		t.Fatal("draft preview wrote the saved layout")
	}
	saved, _ := h.e.HomeLayout(ctx)
	b, _ := json.Marshal(saved)
	if len(b) == 0 || saved.Global[0].Rows[0].Status != "always" {
		t.Fatal("draft leaked into saved in-memory layout")
	}
	// A mutation of the returned document cannot mutate the shared engine.
	saved.Global[0].Rows[0].Status = "off"
	again, _ := h.e.HomeLayout(ctx)
	if again.Global[0].Rows[0].Status != "always" {
		t.Fatal("document alias escaped")
	}
}
func TestProductionFinalizerAndDraftPreviewShareSlots(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	u := core.User{ID: 1}
	sciFiFan(h, 1)
	h.rebuild(u)
	hc := hubContext(testNow, core.DeviceTV)
	h.e.SetExtraHomeRows(func(context.Context, *core.User, core.HubContext, string, int) []core.Hub {
		return []core.Hub{{ID: "live.onnow", Title: "Live now"}}
	})
	d := core.HomeLayout{Global: []core.LayoutPage{{Section: "home", MaxRows: 4, Rows: []core.RowPolicy{{Row: "live.onnow", Status: "always", Slot: 2}, {Row: KindContinue, Status: "always", Slot: 1}}}}}
	if _, err := h.e.SetHomeLayout(ctx, d); err != nil {
		t.Fatal(err)
	}
	p, err := h.e.PreviewHome(ctx, &u, hc, "home", 12)
	if err != nil {
		t.Fatal(err)
	}
	// Build the complete front-end row list from candidates, keeping CW's
	// production placeholder and a custom non-catalogue LiveTV hub.
	hubs, err := h.e.HomeCandidatesFor(ctx, &u, hc, 12)
	if err != nil {
		t.Fatal(err)
	}
	hubs = append(hubs, core.Hub{ID: "live.onnow", Title: "Live now"})
	got, err := h.e.FinalizeHome(ctx, &u, hc, "home", hubs)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hubIDsOf(got), visibleIDs(p.Rows)) {
		t.Fatalf("production %v != preview %v", hubIDsOf(got), visibleIDs(p.Rows))
	}
	if len(got) < 2 || got[1].ID != "live.onnow" {
		t.Fatalf("custom hub lost: %+v", got)
	}
}

type selectedCollectionSource struct {
	fakeCollections
	picks         []core.Title
	selectedCalls int
}

func (s *selectedCollectionSource) ListSpecs(context.Context) ([]core.CollectionSpec, error) {
	return []core.CollectionSpec{{ID: "select", Name: "Just for this viewer", Enabled: true}}, nil
}
func (s *selectedCollectionSource) LayoutRow(ctx context.Context, u *core.User, hc core.HubContext, id string) (*core.CollectionRow, error) {
	s.mu.Lock()
	s.selectedCalls++
	s.mu.Unlock()
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &core.CollectionRow{CollectionID: id, HomeRow: true, SectionRow: true, Hub: core.Hub{ID: "collection." + id, Title: "Just for this viewer", Items: s.picks}}, nil
}
func TestViewerAddsCollectionWithoutChangingPublicationTargets(t *testing.T) {
	source := &selectedCollectionSource{}
	h := newHarness(t, func(o *Options) { o.Collections = source; o.OwnerRowsWait = time.Millisecond })
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		source.picks = append(source.picks, h.cat.titles[movieKey(8, i)])
	}
	u := core.User{ID: 1}
	h.rebuild(u)
	hc := hubContext(testNow, core.DeviceTV)
	d := core.HomeLayout{Viewers: []core.ViewerLayout{{UserID: 1, Pages: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "collection:select", Status: "always", Slot: 2}}}}}}}
	rows, err := h.e.AvailableRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range rows {
		if r.HubID == "collection:select" {
			found = true
		}
	}
	if !found {
		t.Fatal("unpromoted collection missing from row catalogue")
	}
	for i := 0; i < 2; i++ {
		p, err := h.e.PreviewLayout(ctx, &u, hc, "home", 12, d)
		if err != nil {
			t.Fatal(err)
		}
		r := rowByKind(p.Rows, "collection:select")
		if r == nil || r.Hidden || r.Slot != 2 || r.Layer != "override" {
			t.Fatalf("selected collection = %+v", r)
		}
	}
	source.mu.Lock()
	calls := source.selectedCalls
	source.mu.Unlock()
	if calls != 1 {
		t.Fatalf("selected collection was not cached: %d calls", calls)
	}
	other := core.User{ID: 2}
	h.rebuild(other)
	p, err := h.e.PreviewLayout(ctx, &other, hc, "home", 12, d)
	if err != nil {
		t.Fatal(err)
	}
	if rowByKind(p.Rows, "collection:select") != nil {
		t.Fatal("viewer-only collection leaked to another viewer")
	}
	saved, err := h.e.PreviewHome(ctx, &u, hc, "home", 12)
	if err != nil {
		t.Fatal(err)
	}
	if rowByKind(saved.Rows, "collection:select") != nil {
		t.Fatal("draft-only collection leaked to saved Home")
	}
}

func TestHiddenExplanationsDoNotChangePriorityPlacement(t *testing.T) {
	d := core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "hidden", Status: "off"}, {Row: "low", Status: "eligible", Priority: 1.6}}}}}
	hc := hubContext(testNow, core.DeviceTV)
	production := arrangeLayout(sampleRows("high", "low"), d, 1, hc, "home", 14)
	preview := arrangeLayout(sampleRows("high", "low", "hidden"), d, 1, hc, "home", 14)
	if !reflect.DeepEqual(visibleIDs(production), visibleIDs(preview)) {
		t.Fatalf("hidden row altered priority: production=%v preview=%v", visibleIDs(production), visibleIDs(preview))
	}
}

func TestPreviewRechecksCurrentViewerPolicyForEveryProducer(t *testing.T) {
	blocked := false
	h := newHarness(t, func(o *Options) {
		o.UserPolicy = func(u *core.User) *core.User { v := *u; v.MaxContentRating = "PG"; return &v }
		o.Allowed = func(_ *core.User, _ *core.Title) bool { return !blocked }
	})
	u := core.User{ID: 1}
	sciFiFan(h, 1)
	h.rebuild(u)
	h.e.SetExtraHomeRows(func(_ context.Context, u *core.User, _ core.HubContext, _ string, _ int) []core.Hub {
		if u.MaxContentRating != "PG" {
			t.Error("effective policy not applied to preview")
		}
		return []core.Hub{{ID: "extra", Title: "Extra", Items: []core.Title{{Key: core.MovieKey(99999), Title: "Extra title", Type: core.TypeMovie}}}}
	})
	ctx := context.Background()
	hc := hubContext(testNow, core.DeviceTV)
	for _, deny := range []bool{false, true} {
		blocked = deny
		p, err := h.e.PreviewHome(ctx, &u, hc, "home", 12)
		if err != nil {
			t.Fatal(err)
		}
		total := 0
		for _, row := range p.Rows {
			total += len(row.Items)
			if deny && (len(row.Hub.Items) > 0 || len(row.ItemReasons) > 0) {
				t.Fatal("cached title details escaped current policy")
			}
		}
		if deny && total != 0 || !deny && total == 0 {
			t.Fatalf("deny=%v titles=%d", deny, total)
		}
	}
}

func TestDesignerHasOnePersistentLayout(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	d, _ := h.e.HomeLayout(ctx)
	d.Viewers = []core.ViewerLayout{{UserID: 1, Pages: []core.LayoutPage{{Section: "home", DeviceMaxRows: map[string]int{"tv": 8}, Rows: []core.RowPolicy{{Row: KindTrending, Status: "always", Slot: 2, MaxDaysPerWeek: 4}}}}}}
	if _, err := h.e.SetHomeLayout(ctx, d); err != nil {
		t.Fatal(err)
	}
	e2, err := New(Options{DB: h.db, Catalog: h.cat, Logger: quietLogger(), Now: h.clock.Now, Location: time.UTC, RefreshEvery: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	loaded, err := e2.HomeLayout(ctx)
	if err != nil || len(loaded.Viewers) != 1 || loaded.Viewers[0].Pages[0].Rows[0].MaxDaysPerWeek != 4 || loaded.Viewers[0].Pages[0].DeviceMaxRows["tv"] != 8 {
		t.Fatal("canonical layout lost advanced viewer constraints", loaded, err)
	}
}

func TestNeutralHomeLayoutMatchesLegacy(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	user := &core.User{ID: 1}
	hc := hubContext(testNow, core.DeviceTV)
	hubs := []core.Hub{
		{ID: "home.continue", Title: "Continue", ItemCount: 1},
		{ID: "movie.trending", Title: "Trending", ItemCount: 4},
		{ID: "live.onnow", Title: "On Now", ItemCount: 2},
	}
	old, err := h.e.FinalizeHome(ctx, user, hc, "home", hubs)
	if err != nil {
		t.Fatal(err)
	}
	candidates := make([]core.NeutralHomeRow, len(hubs))
	for i, hub := range hubs {
		candidates[i] = core.NeutralHomeRow{ID: hub.ID, Title: hub.Title, Type: hub.Type, Items: hub.Items, ItemCount: hub.ItemCount}
	}
	got, err := h.e.FinalizeNeutralHome(ctx, user, core.HomeSituation{Device: hc.Device, Daypart: hc.Daypart, Weekend: hc.Weekend, Now: hc.Now, LibraryID: hc.SectionID, Product: hc.Product, Platform: hc.Platform, SessionID: hc.SessionID, ClientID: hc.ClientID}, "home", candidates)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(got))
	for i, row := range got {
		ids[i] = row.ID
	}
	if !reflect.DeepEqual(hubIDsOf(old), ids) {
		t.Fatalf("legacy %v != neutral %v", hubIDsOf(old), ids)
	}
}
