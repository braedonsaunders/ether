package personalize

import (
	"context"
	"encoding/json"
	"errors"
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
	schedule      *core.CollectionSchedule
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
	return &core.CollectionRow{CollectionID: id, HomeRow: true, SectionRow: true, Schedule: s.schedule, Hub: core.Hub{ID: "collection." + id, Title: "Just for this viewer", Items: s.picks}}, nil
}

// A collection row follows its collection's schedule, wherever the layout
// places it; a designer draft previews an edited schedule, and only a
// viewer override with its own When replaces it.
func TestCollectionRowFollowsCollectionSchedule(t *testing.T) {
	today := int(testNow.Weekday())
	source := &selectedCollectionSource{schedule: &core.CollectionSchedule{Days: []int{(today + 1) % 7}}}
	h := newHarness(t, func(o *Options) { o.Collections = source; o.OwnerRowsWait = time.Millisecond })
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		source.picks = append(source.picks, h.cat.titles[movieKey(8, i)])
	}
	u := core.User{ID: 1}
	h.rebuild(u)
	hc := hubContext(testNow, core.DeviceTV)
	placed := core.RowPolicy{Row: "collection:select", Status: "always", Slot: 2}
	base := core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{placed}}}}
	shown := func(d core.HomeLayout) *core.HomeRow {
		t.Helper()
		p, err := h.e.PreviewLayout(ctx, &u, hc, "home", 12, d)
		if err != nil {
			t.Fatal(err)
		}
		return rowByKind(p.Rows, "collection:select")
	}
	if r := shown(base); r == nil || !r.Hidden || r.HiddenCode != "outside_collection_schedule" {
		t.Fatalf("placed row ignored its collection's schedule: %+v", r)
	}
	draft := base
	draft.CollectionSchedules = map[string]*core.CollectionSchedule{"select": {Days: []int{today}}}
	if r := shown(draft); r == nil || r.Hidden {
		t.Fatalf("draft schedule not previewed: %+v", r)
	}
	cleared := base
	cleared.CollectionSchedules = map[string]*core.CollectionSchedule{"select": nil}
	if r := shown(cleared); r == nil || r.Hidden {
		t.Fatalf("cleared draft schedule not previewed: %+v", r)
	}
	override := base
	override.Viewers = []core.ViewerLayout{{UserID: 1, Pages: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{
		{Row: "collection:select", Status: "always", Slot: 2, When: &core.LayoutWhen{Days: []int{today}}}}}}}}
	if r := shown(override); r == nil || r.Hidden {
		t.Fatalf("viewer override did not replace the schedule: %+v", r)
	}
	override.Viewers[0].Pages[0].Rows[0].When = &core.LayoutWhen{Days: []int{(today + 2) % 7}}
	if r := shown(override); r != nil && !r.Hidden {
		t.Fatalf("viewer override schedule not applied: %+v", r)
	}
}

func TestEveryonePolicyCannotScheduleACollectionRow(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	when := &core.LayoutWhen{Days: []int{1}}
	bad := core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "collection:7", Status: "eligible", When: when}}}}}
	var ve *ValidationError
	if _, err := h.e.SetHomeLayout(ctx, bad); !errors.As(err, &ve) || ve.Problems[0].Field != "layout.global[0].rows[0].when" {
		t.Fatalf("Everyone When on a collection row: %v", err)
	}
	ok := core.HomeLayout{
		Global:              []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "collection:*", Status: "eligible", When: when}}}},
		Viewers:             []core.ViewerLayout{{UserID: 2, Pages: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: "collection:7", Status: "eligible", When: when}}}}}},
		CollectionSchedules: map[string]*core.CollectionSchedule{"7": {Days: []int{2}}},
	}
	saved, err := h.e.SetHomeLayout(ctx, ok)
	if err != nil {
		t.Fatal(err)
	}
	if saved.CollectionSchedules != nil {
		t.Fatalf("collection schedules stored with the layout: %+v", saved.CollectionSchedules)
	}
	if got, _ := h.e.HomeLayout(ctx); got.CollectionSchedules != nil {
		t.Fatalf("collection schedules kept by the engine: %+v", got.CollectionSchedules)
	}
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

func TestNeutralHomeKeepsFullLibraryBackfill(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	newer := core.Title{Key: core.MovieKey(11), Title: "Newer", Released: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)}
	older := core.Title{Key: core.MovieKey(12), Title: "Older", Released: time.Date(1990, 1, 1, 0, 0, 0, 0, time.UTC)}
	candidates := []core.NeutralHomeRow{
		{ID: "home.continue", Title: "Continue Watching", Kind: "continue-watching", Purpose: "continue", Items: []core.Title{{Key: core.MovieKey(1)}}},
		{ID: "library.recent.1", Title: "Recently Added Movies", Kind: "recentlyAdded", Purpose: "recentlyAdded", Items: []core.Title{newer, older}},
	}
	got, err := h.e.FinalizeNeutralHome(ctx, &core.User{ID: 1}, core.HomeSituation{Now: testNow, Device: core.DeviceTV}, "home", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].ID != "library.recent.1" || len(got[1].Items) != 2 || got[1].Items[0].Key != newer.Key || got[1].Items[1].Key != older.Key {
		t.Fatalf("backfill = %+v", got)
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

func TestPreviewLayoutTypicalViewerUsesGenericModel(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	hc := hubContext(testNow, core.DeviceTV)
	p, err := h.e.PreviewLayout(ctx, &core.User{IsOwner: true}, hc, "home", 12, core.HomeLayout{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rows) == 0 {
		t.Fatal("typical viewer preview returned no rows")
	}
	h.e.mu.Lock()
	hasZero := h.e.models[0] != nil
	hasGeneric := h.e.models[genericID] != nil
	h.e.mu.Unlock()
	if hasZero || !hasGeneric {
		t.Fatalf("models[0]=%v models[genericID]=%v, expected generic model reuse", hasZero, hasGeneric)
	}
}

func TestMatchingPolicyRanksLikeTheConsole(t *testing.T) {
	page := &core.LayoutPage{Section: "home", Rows: []core.RowPolicy{
		{Row: "because.*", Status: "off"},
		{Row: "because-you-watched", Status: "eligible"},
		{Row: "because.42", Status: "always"},
		{Row: "collection:*", Status: "off"},
		{Row: "collection:c1*", Status: "always"},
	}}
	cases := []struct{ hubID, kind, want string }{
		// A screen-less stem must not change existing Plex policy precedence.
		{"home.because.42", "because-you-watched", "because-you-watched"},
		{"home.because.7", "because-you-watched", "because-you-watched"},
		{"home.because.7", "because-you-watched.x", "because.*"},
		{"home.because.42", "other", "because.42"},
		// Equally specific rules: the later one wins.
		{"collection.c10", "collection:c10", "collection:c1*"},
		{"collection.c2", "collection:c2", "collection:*"},
	}
	for _, c := range cases {
		got := matchingPolicy(page, core.HomeRow{HubID: c.hubID, Kind: c.kind})
		if got == nil || got.Row != c.want {
			t.Errorf("matchingPolicy(%s, %s) = %v, want %s", c.hubID, c.kind, got, c.want)
		}
	}
}

func TestAlwaysRowsNeverDroppedByCapsThresholdsOrDedupe(t *testing.T) {
	hc := hubContext(testNow, core.DeviceTV)
	// 1. Displaced anchor, sparse page (Slot > len(rows)), Slot > MaxRows, and count(Always) > MaxRows.
	d := core.HomeLayout{
		Global: []core.LayoutPage{{
			Section: "home",
			MaxRows: 1,
			Rows: []core.RowPolicy{
				{Row: "global_s1", Status: "always", Slot: 1},
				{Row: "beyond_cap", Status: "always", Slot: 9},
			},
		}},
		Viewers: []core.ViewerLayout{{
			UserID: 4,
			Pages: []core.LayoutPage{{
				Section: "home",
				Rows: []core.RowPolicy{
					{Row: "viewer_s1", Status: "always", Slot: 1},
					{Row: "min_beyond", Status: "always", MinSlot: 5},
				},
			}},
		}},
	}
	got := arrangeLayout(sampleRows("opt", "global_s1", "viewer_s1", "beyond_cap", "min_beyond"), d, 4, hc, "home", 14)
	ids := visibleIDs(got)
	want := []string{"viewer_s1", "global_s1", "min_beyond", "beyond_cap"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("visible Always rows = %v, want %v", ids, want)
	}
	for _, r := range got {
		if r.HubID == "opt" && (!r.Hidden || r.HiddenCode != "page_constraints") {
			t.Fatalf("optional row should be hidden with page_constraints: %+v", r)
		}
	}

	// 2. Thin row (< MinRowItems=12) and cold-only row for a warm viewer survive when Always, not when Eligible.
	h := newHarness(t, func(o *Options) { o.MinRowItems = 12 })
	ctx := context.Background()
	u := core.User{ID: 1}
	sciFiFan(h, 1)
	h.rebuild(u)
	h.e.mu.Lock()
	m := h.e.models[u.ID]
	if m == nil || m.taste == nil || m.taste.Confidence < coldStartConfidence {
		h.e.mu.Unlock()
		t.Fatalf("expected warm model for sciFiFan: %+v", m)
	}
	thinItems := m.rows[0].items
	if len(thinItems) > 2 {
		thinItems = thinItems[:2]
	}
	m.rows = append(m.rows,
		&candRow{id: "thin.custom", kind: KindHiddenGems, title: "Thin Gems", reason: "Two picks", scale: 0.7, items: thinItems, minItems: 6},
		&candRow{id: "cold.only", kind: KindNewReleases, title: "New Releases", reason: "Cold row", scale: 0.6, coldOnly: true, items: thinItems},
		&candRow{id: "dedupe.twin", kind: KindTopRated, title: "Dedupe Twin", reason: "Same two picks", scale: 0.5, items: thinItems},
	)
	h.e.mu.Unlock()

	eligLayout := core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{
		{Row: "thin.custom", Status: "eligible"},
	}}}}
	eligPrev, err := h.e.PreviewLayout(ctx, &u, hc, "home", 12, eligLayout)
	if err != nil {
		t.Fatal(err)
	}
	if r := rowByKind(eligPrev.Rows, KindHiddenGems); r != nil && r.HubID == "home.thin.custom" && !r.Hidden {
		t.Fatalf("2-item Eligible row should not pass MinRowItems=12: %+v", r)
	}

	alwaysLayout := core.HomeLayout{Global: []core.LayoutPage{{Section: "home", MaxRows: 1, Rows: []core.RowPolicy{
		{Row: "thin.custom", Status: "always"},
		{Row: KindNewReleases, Status: "always"},
		{Row: "dedupe.twin", Status: "always"},
	}}}}
	if _, err := h.e.SetHomeLayout(ctx, alwaysLayout); err != nil {
		t.Fatal(err)
	}
	liveHubs, err := h.e.HomeHubsFor(ctx, &u, hc, 12)
	if err != nil {
		t.Fatal(err)
	}
	gotHubIDs := hubIDsOf(liveHubs)
	for _, wantID := range []string{"home.thin.custom", "home.cold.only", "home.dedupe.twin"} {
		found := false
		for _, id := range gotHubIDs {
			if id == wantID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Always row %s missing from HomeHubsFor: %v", wantID, gotHubIDs)
		}
	}
}

type specCollectionSource struct {
	selectedCollectionSource
	specs []core.CollectionSpec
}

func (s *specCollectionSource) ListSpecs(context.Context) ([]core.CollectionSpec, error) {
	return s.specs, nil
}

func TestPreviewLayoutExplainsAbsentAlwaysRows(t *testing.T) {
	source := &specCollectionSource{
		specs: []core.CollectionSpec{
			{ID: "empty_col", Name: "Empty Collection", MediaType: "movie", Enabled: true, Targets: core.CollectionTargets{HomeRow: true, Users: core.AllUsers()}},
			{ID: "disabled_col", Name: "Disabled Collection", MediaType: "movie", Enabled: false, Targets: core.CollectionTargets{HomeRow: true, Users: core.AllUsers()}},
			{ID: "private_col", Name: "Private Collection", MediaType: "movie", Enabled: true, Targets: core.CollectionTargets{HomeRow: true, Users: core.UserTargets{IDs: []int64{99}}}},
			{ID: "movies_only", Name: "Movies Only", MediaType: "movie", Enabled: true, Targets: core.CollectionTargets{HomeRow: true, Users: core.AllUsers()}},
		},
	}
	var blockAllAvail bool
	h := newHarness(t, func(o *Options) {
		o.Collections = source
		o.OwnerRowsWait = time.Millisecond
		o.FilterAvailable = func(_ context.Context, items []core.Title) []core.Title {
			if blockAllAvail {
				return nil
			}
			return items
		}
	})
	ctx := context.Background()
	hc := hubContext(testNow, core.DeviceTV)

	// 1. Typical viewer: personal rows explain typical_viewer_personal; disabled/unshared/empty collections explain why.
	typLayout := core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{
		{Row: KindTop10, Status: "always"},
		{Row: "collection:empty_col", Status: "always"},
		{Row: "collection:disabled_col", Status: "always"},
		{Row: "collection:private_col", Status: "always"},
		{Row: KindFavorites, Status: "always"},
		{Row: KindUpNext, Status: "always"},
		{Row: KindTraktRecs, Status: "always"},
		{Row: KindLLM, Status: "always"},
	}}}}
	prev, err := h.e.PreviewLayout(ctx, &core.User{}, hc, "home", 12, typLayout)
	if err != nil {
		t.Fatal(err)
	}
	wantTypical := map[string]string{
		KindTop10:                 "typical_viewer_personal",
		"collection:empty_col":    "collection_empty",
		"collection:disabled_col": "collection_disabled",
		"collection:private_col":  "collection_not_shared",
		KindFavorites:             "typical_viewer_personal",
		KindUpNext:                "typical_viewer_personal",
		KindTraktRecs:             "typical_viewer_personal",
		KindLLM:                   "typical_viewer_personal",
	}
	for kind, wantCode := range wantTypical {
		r := rowByKind(prev.Rows, kind)
		if r == nil || !r.Hidden || r.HiddenCode != wantCode {
			t.Fatalf("typical viewer kind %s = %+v, want hiddenCode=%s", kind, r, wantCode)
		}
	}

	// 2. Real viewer with zero history: viewer-specific reasons reported, live HomeHubsFor omits empty rows.
	coldViewer := core.User{ID: 42}
	h.rebuild(coldViewer)
	coldPrev, err := h.e.PreviewLayout(ctx, &coldViewer, hc, "home", 12, typLayout)
	if err != nil {
		t.Fatal(err)
	}
	wantCold := map[string]string{
		KindTop10:     "needs_watch_history",
		KindFavorites: "no_favorites",
		KindUpNext:    "no_in_progress",
		KindTraktRecs: "needs_trakt",
		KindLLM:       "ai_unavailable",
	}
	for kind, wantCode := range wantCold {
		r := rowByKind(coldPrev.Rows, kind)
		if r == nil || !r.Hidden || r.HiddenCode != wantCode {
			t.Fatalf("cold viewer kind %s = %+v, want hiddenCode=%s", kind, r, wantCode)
		}
	}
	if _, err := h.e.SetHomeLayout(ctx, typLayout); err != nil {
		t.Fatal(err)
	}
	liveHubs, err := h.e.HomeHubsFor(ctx, &coldViewer, hc, 12)
	if err != nil {
		t.Fatal(err)
	}
	for _, hub := range liveHubs {
		if hub.ID == "home."+KindTop10 || (hub.ID != "home.continue" && len(hub.Items) == 0) {
			t.Fatalf("live HomeHubsFor returned empty/synthetic Always row: %+v", hub)
		}
	}

	// 3. Wrong surface on TV page.
	showsLayout := core.HomeLayout{Global: []core.LayoutPage{{Section: "shows", Rows: []core.RowPolicy{
		{Row: "collection:movies_only", Status: "always"},
		{Row: KindAcclaimed, Status: "always"},
	}}}}
	showsPrev, err := h.e.PreviewLayout(ctx, &coldViewer, hc, "shows", 12, showsLayout)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"collection:movies_only", KindAcclaimed} {
		if r := rowByKind(showsPrev.Rows, kind); r == nil || !r.Hidden || r.HiddenCode != "wrong_surface" {
			t.Fatalf("shows surface kind %s = %+v, want wrong_surface", kind, r)
		}
	}

	// 4. Availability filter emptying all candidates reports filtered_by_availability.
	blockAllAvail = true
	availLayout := core.HomeLayout{Global: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{
		{Row: KindTrending, Status: "always"},
	}}}}
	availPrev, err := h.e.PreviewLayout(ctx, &coldViewer, hc, "home", 12, availLayout)
	if err != nil {
		t.Fatal(err)
	}
	if r := rowByKind(availPrev.Rows, KindTrending); r == nil || !r.Hidden || r.HiddenCode != "filtered_by_availability" {
		t.Fatalf("filtered_by_availability kind %s = %+v", KindTrending, r)
	}
}

// The same policy resolution feeds owner previews and every client adapter.
func TestTop10RankedDefault(t *testing.T) {
	h := newHarness(t, nil)
	on, off := true, false
	for _, tc := range []struct {
		name, id, kind string
		ranked         bool
		global, viewer *bool
		policy         bool
		want           bool
	}{
		{name: "today default by ID", id: "home.top10.today", want: true},
		{name: "personal default by ID", id: "movie.top10", want: true},
		{name: "today default by kind", id: "custom", kind: KindTop10Today, want: true},
		{name: "designer default", id: "tv.top10.today", policy: true, want: true},
		{name: "owner off", id: "home.top10.today", ranked: true, global: &off},
		{name: "owner on", id: "home.top10", global: &on, want: true},
		{name: "viewer off", id: "home.top10.today", global: &on, viewer: &off},
		{name: "other row unchanged", id: "home.trending"},
		{name: "collection opt in retained", id: "collection.saved", ranked: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var layout core.HomeLayout
			if tc.policy || tc.global != nil {
				layout.Global = []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: tc.id, Status: "eligible", RankedTop10: tc.global}}}}
			}
			if tc.viewer != nil {
				layout.Viewers = []core.ViewerLayout{{UserID: 1, Pages: []core.LayoutPage{{Section: "home", Rows: []core.RowPolicy{{Row: tc.id, Status: "eligible", RankedTop10: tc.viewer}}}}}}
			}
			ctx := context.WithValue(context.Background(), draftLayoutKey{}, layout)
			// The deliberately misleading title must never determine the default.
			candidate := core.NeutralHomeRow{ID: tc.id, Kind: tc.kind, Title: "Top 10 on Renamed Server Today", ItemCount: 10, RankedTop10: tc.ranked}
			got, err := h.e.FinalizeNeutralHome(ctx, &core.User{ID: 1}, core.HomeSituation{Now: testNow, Device: core.DeviceTV}, "home", []core.NeutralHomeRow{candidate})
			if err != nil || len(got) != 1 {
				t.Fatalf("neutral rows = %+v, %v", got, err)
			}
			if got[0].RankedTop10 != tc.want {
				t.Fatalf("neutral ranked = %v, want %v", got[0].RankedTop10, tc.want)
			}
			if tc.kind == "" {
				hubs, err := h.e.FinalizeHome(ctx, &core.User{ID: 1}, hubContext(testNow, core.DeviceTV), "home", []core.Hub{{ID: tc.id, Title: candidate.Title, ItemCount: 10, RankedTop10: tc.ranked}})
				if err != nil || len(hubs) != 1 || hubs[0].RankedTop10 != tc.want {
					t.Fatalf("Plex hubs = %+v, %v; want ranked %v", hubs, err, tc.want)
				}
			}
		})
	}
}
