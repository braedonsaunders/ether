package personalize

import (
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func TestWatchRefreshKeepsUnchangedCardsInTheirShelves(t *testing.T) {
	cat := newFakeCatalog(testNow)
	var keys []core.RatingKey
	for g := 0; g < 5; g++ {
		for i := 1; i <= 12; i++ {
			keys = append(keys, movieKey(g, i))
		}
	}
	now := testNow
	e := &Engine{opts: Options{Now: func() time.Time { return now }}}
	makeAssembly := func(reverse bool, removed core.RatingKey) *assembly {
		ks := slices.Clone(keys)
		if reverse {
			slices.Reverse(ks)
		}
		ks = slices.DeleteFunc(ks, func(k core.RatingKey) bool { return k == removed })
		first := testRow(KindPopular, "popular", items(cat, ks...))
		second := testRow(KindHiddenGems, "gems", items(cat, ks...))
		first.scale, second.scale = 20, 1
		if reverse {
			first.scale, second.scale = 1, 20
		}
		return &assembly{model: emptyModel(first, second), user: core.User{ID: 1},
			hc: hubContext(testNow, core.DeviceWeb), section: core.TypeMovie, limit: 12,
			maxRows: 16, minRow: 12, day: dayOf(testNow, time.UTC)}
	}
	key := memoKey{user: 1, section: core.TypeMovie, limit: 12, model: 1}
	before := e.assemblePage(makeAssembly(false, 0), key, false)
	if len(before) != 2 {
		t.Fatalf("before: %v", kindsOf(before))
	}
	watched := before[0].Items[0].Key
	// Model rebuild plus new statistics/intent/CW/home lead would defeat the
	// ordinary memo. The new candidate pool excludes the watched title and
	// deliberately reverses both shelf scores and card relevance.
	key.model, key.stats, key.intent, key.lead, key.cw = 2, 3, 4, 5, "changed"
	after := e.assemblePage(makeAssembly(true, watched), key, false)
	if len(after) != len(before) {
		t.Fatalf("rows changed: %v", kindsOf(after))
	}
	seen := map[core.RatingKey]bool{}
	for i, row := range after {
		if row.HubID != before[i].HubID {
			t.Fatalf("row moved: %s -> %s", before[i].HubID, row.HubID)
		}
		var retained []core.RatingKey
		for _, title := range before[i].Items {
			if title.Key != watched {
				retained = append(retained, title.Key)
			}
		}
		var got []core.RatingKey
		for _, title := range row.Items {
			if title.Key == watched || seen[title.Key] {
				t.Fatalf("watched or duplicate card: %v", title.Key)
			}
			seen[title.Key] = true
			got = append(got, title.Key)
		}
		if len(got) != 12 {
			t.Fatalf("short row: %v", got)
		}
		if !reflect.DeepEqual(got[:len(retained)], retained) {
			t.Fatalf("cards jumped: before %v, after %v", retained, got)
		}
	}
	// A draft preview must reflect fresh scoring, not mutate or borrow the
	// currently browsed page's presentation.
	draft := e.assemblePage(makeAssembly(true, watched), key, true)
	if draft[0].HubID == before[0].HubID {
		t.Fatal("draft kept old rank")
	}
	// An idle browsing session expires, allowing newly learned scores to lead.
	now = now.Add(presentationTTL + time.Second)
	fresh := e.assemblePage(makeAssembly(true, watched), key, false)
	if fresh[0].HubID != draft[0].HubID {
		t.Fatal("expired presentation kept old rank")
	}
}

func TestPresentationScopesAndExplicitRefresh(t *testing.T) {
	k := memoKey{user: 1, section: core.TypeMovie, limit: 25, day: 20261021, hour: 12, bucket: "web", layout: 1, owner: 2, limits: "PG", tuning: "a"}
	want := presentationKey(k, false)
	dynamic := k
	dynamic.model, dynamic.stats, dynamic.intent, dynamic.lead, dynamic.owner, dynamic.cw = 9, 9, 9, 9, 9, "cw"
	if presentationKey(dynamic, false) != want {
		t.Fatal("live feedback changes presentation scope")
	}
	for name, change := range map[string]func(*memoKey){
		"viewer": func(k *memoKey) { k.user++ }, "section": func(k *memoKey) { k.section = core.TypeShow },
		"device": func(k *memoKey) { k.bucket = "tv" }, "day": func(k *memoKey) { k.day++ },
		"layout": func(k *memoKey) { k.layout++ },
		"limits": func(k *memoKey) { k.limits = "R" }, "tuning": func(k *memoKey) { k.tuning = "b" },
	} {
		changed := k
		change(&changed)
		if presentationKey(changed, false) == want {
			t.Errorf("scope shared across %s", name)
		}
	}
	// Hour and page size are one visit. Android and Android TV poll with a
	// new count, and the hour changing mid-session must not reshuffle rows.
	for name, change := range map[string]func(*memoKey){
		"schedule": func(k *memoKey) { k.hour++ }, "page size": func(k *memoKey) { k.limit++ },
	} {
		changed := k
		change(&changed)
		if presentationKey(changed, false) != want {
			t.Errorf("scope split across %s", name)
		}
	}
	if presentationKey(k, true) == want {
		t.Fatal("generic model freezes personal warmup")
	}
	other := k
	other.user = 2
	e := &Engine{presentations: map[memoKey]presentation{k: {}, other: {}}, memo: map[memoKey]*memoEntry{k: {}, other: {}}}
	e.clearPresentation(1)
	if len(e.presentations) != 1 || len(e.memo) != 1 {
		t.Fatal("refresh did not isolate viewer")
	}
	if _, ok := e.presentations[other]; !ok {
		t.Fatal("cleared another viewer")
	}
}

func TestPresentationDoesNotOverrideOrderedRows(t *testing.T) {
	cat := newFakeCatalog(testNow)
	k := movieKey(0, 1)
	trend := testRow(KindTrending, "trending", items(cat, k, movieKey(1, 2), movieKey(2, 3), movieKey(3, 4)))
	next := testRow(KindUpNext, "upnext", items(cat, k, movieKey(0, 2)))
	next.keepOrder, next.minItems = true, 1
	rows := run(emptyModel(trend, next), func(a *assembly) {
		a.previous = []presentedRow{{id: "home.trending", keys: []core.RatingKey{k}}}
	})
	var found bool
	for _, r := range rows {
		for _, it := range r.Items {
			if it.Key == k {
				if r.Kind != KindUpNext {
					t.Fatal("old discovery card overrode live ordered row")
				}
				found = true
			}
		}
	}
	if !found {
		t.Fatal("live ordered card disappeared")
	}
}

func TestPresentationVisitSeedIsStableUntilIdleOrNewClientSession(t *testing.T) {
	now := testNow
	e := &Engine{opts: Options{Now: func() time.Time { return now }}}
	key := memoKey{user: 1, client: "apple-tv", session: "visit-a"}
	makeA := func() *assembly {
		return &assembly{model: emptyModel(), hc: hubContext(testNow, core.DeviceTV), user: core.User{ID: 1}}
	}
	a := makeA()
	e.assemblePage(a, key, false)
	first := a.visit
	now = now.Add(4 * time.Minute)
	e.touchPresentation(key, false)
	now = now.Add(4 * time.Minute)
	a = makeA()
	e.assemblePage(a, key, false)
	if a.visit != first {
		t.Fatal("active browsing shuffled")
	}
	now = now.Add(presentationTTL + time.Second)
	a = makeA()
	e.assemblePage(a, key, false)
	if a.visit == first {
		t.Fatal("idle visit did not rotate")
	}
	first = a.visit
	now = now.Add(time.Second)
	key.session = "visit-b"
	key.client = "android-tv"
	key.limit = 4
	key.hour = 23
	a = makeA()
	e.assemblePage(a, key, false)
	if a.visit != first {
		t.Fatal("count, session or client id started a new visit")
	}
	key.user = 2
	a = makeA()
	e.assemblePage(a, key, false)
	if a.visit == first {
		t.Fatal("another viewer reused the visit")
	}
}
