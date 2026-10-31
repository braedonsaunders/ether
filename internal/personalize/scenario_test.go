package personalize

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
)

// familyViewer watches family and animation films on weekend afternoons.
func familyViewer(h *harness, uid int64) {
	sat := time.Date(2026, 10, 10, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		day := sat.AddDate(0, 0, -7*i)
		h.finish(uid, movieKey(5, i+1), day)                  // Family
		h.finish(uid, movieKey(4, i+1), day.Add(3*time.Hour)) // Animation
	}
}

// comedyBinger watches 24-minute comedy episodes late on weeknights, in
// runs.
func comedyBinger(h *harness, uid int64) {
	show := showKey(2, 2)
	ep := 1
	for _, d := range []int{5, 6, 7} { // Mon–Wed of the week before
		night := time.Date(2026, 10, d, 22, 30, 0, 0, time.UTC)
		for i := 0; i < 2 && ep <= 6; i++ {
			h.finish(uid, core.EpisodeKey(show.TMDB(), 1, ep), night.Add(time.Duration(i)*30*time.Minute))
			ep++
		}
	}
	h.finish(uid, movieKey(2, 3), time.Date(2026, 10, 9, 22, 0, 0, 0, time.UTC))
	h.finish(uid, core.EpisodeKey(showKey(2, 3).TMDB(), 1, 1), time.Date(2026, 10, 12, 23, 0, 0, 0, time.UTC))
}

func describe(rows []core.HomeRow) string {
	var b strings.Builder
	for _, r := range rows {
		if r.Hidden {
			continue
		}
		var titles []string
		for i, t := range r.Items {
			if i >= 4 {
				titles = append(titles, "…")
				break
			}
			titles = append(titles, t.Title)
		}
		fmt.Fprintf(&b, "  %-26s %-40q %.3f  %s\n", r.Kind, r.Title, r.Score, strings.Join(titles, ", "))
	}
	return b.String()
}

func itemSet(rows []core.HomeRow) map[core.RatingKey]bool {
	out := map[core.RatingKey]bool{}
	for _, r := range rows {
		if r.Hidden || r.Source == core.RowSourcePlex {
			continue
		}
		for _, t := range r.Items {
			out[t.Key] = true
		}
	}
	return out
}

func jaccard(a, b map[core.RatingKey]bool) float64 {
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 1
	}
	return float64(inter) / float64(union)
}

func TestScenarioDifferentViewersGetDifferentHomes(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	alice := core.User{ID: 1, Username: "alice", IsOwner: true}
	bob := core.User{ID: 2, Username: "bob", MaxContentRating: "PG"}
	carol := core.User{ID: 3, Username: "carol"}
	dave := core.User{ID: 4, Username: "dave"}
	h.users.users = []core.User{alice, bob, carol, dave}
	sciFiFan(h, alice.ID)
	familyViewer(h, bob.ID)
	comedyBinger(h, carol.ID)
	h.e.syncEvents(ctx)
	for _, u := range h.users.users {
		h.rebuild(u)
	}

	wednesdayTV := hubContext(testNow, core.DeviceTV) // Wed 20:00
	homes := map[string][]core.HomeRow{}
	for _, u := range h.users.users {
		u := u
		hc := wednesdayTV
		if u.ID == carol.ID {
			hc = hubContext(time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC), core.DeviceMobile) // Wed late, phone
		}
		p, err := h.e.PreviewHome(ctx, &u, hc, "", 12)
		if err != nil {
			t.Fatal(err)
		}
		homes[u.Username] = p.Rows
		t.Logf("%s (%s, %s):\n%s", u.Username, hc.Device, hc.Daypart, describe(p.Rows))
	}

	// Alice: her taste everywhere.
	a := homes["alice"]
	if rowByKind(a, KindTop10) == nil || rowByKind(a, KindBecause) == nil {
		t.Fatalf("alice lacks personal rows: %v", kindsOf(a))
	}
	if share := genreShare(a, "Science Fiction", KindTrending, KindNewReleases, KindPopularServer, KindDifferent); share < 0.5 {
		t.Fatalf("alice's personal rows are %.0f%% science fiction", share*100)
	}
	// Her lenses (the saga, her director) live on Movies → Recommended.
	mp, err := h.e.PreviewHome(ctx, &alice, wednesdayTV, "movies", 12)
	if err != nil {
		t.Fatal(err)
	}
	am := mp.Rows
	t.Logf("alice, Movies:\n%s", describe(am))
	// Finish the Franchise is usually one or two titles, so it is off
	// unless the owner adds it to a page.
	if r := rowByKind(am, KindFranchise); r != nil {
		t.Fatalf("franchise row shown by default: %v", kindsOf(am))
	}
	saga := core.HomeLayout{Global: []core.LayoutPage{{Section: "movies", Rows: []core.RowPolicy{{Row: KindFranchise, Status: "eligible"}}}}}
	if fp, err := h.e.PreviewLayout(ctx, &alice, wednesdayTV, "movies", 12, saga); err != nil {
		t.Fatal(err)
	} else if r := rowByKind(fp.Rows, KindFranchise); r == nil || r.Hidden || r.Title != "Finish Star Saga Collection" {
		t.Fatalf("an owner policy should bring the saga back on Movies: %v", kindsOf(fp.Rows))
	}
	// The fake catalogue has 48 science fiction films and her Home leads
	// with most of the ones Vera directed, so her director lens shows on a
	// Movies page that explicitly includes person rows and does not hold Home titles back.
	off := false
	draft := core.HomeLayout{Global: []core.LayoutPage{{Section: "movies", DedupeHome: &off, Rows: []core.RowPolicy{{Row: KindPerson, Status: "always"}}}}}
	mp2, err := h.e.PreviewLayout(ctx, &alice, wednesdayTV, "movies", 12, draft)
	if err != nil {
		t.Fatal(err)
	}
	vera := false
	for _, r := range mp2.Rows {
		vera = vera || !r.Hidden && r.Kind == KindPerson && r.Title == "More from Vera Villeneuve"
	}
	if !vera {
		t.Fatalf("alice's director row missing from Movies:\n%s", describe(mp2.Rows))
	}
	if rowByKind(a, KindFranchise) != nil || rowByKind(a, KindPerson) != nil {
		t.Fatalf("lens rows on alice's Home by default: %v", kindsOf(a))
	}

	// Bob: family titles, nothing above PG.
	b := homes["bob"]
	for _, r := range b {
		for _, it := range r.Items {
			if !contentrating.Allowed(it.ContentRating, "PG") {
				t.Fatalf("bob (PG) sees %s rated %s in %s", it.Title, it.ContentRating, r.HubID)
			}
		}
	}
	if share := genreShare(b, "Family"); share+genreShare(b, "Animation") < 0.5 {
		t.Fatalf("bob's Home is not family-led: family %.2f animation %.2f", genreShare(b, "Family"), genreShare(b, "Animation"))
	}

	// Carol on her phone late on a weeknight: short things and her show.
	c := homes["carol"]
	if r := rowByKind(c, KindUpNext); r == nil || len(r.Items) == 0 {
		t.Fatalf("carol's Up Next missing: %v", kindsOf(c))
	}
	pos := map[string]int{}
	for i, r := range c {
		if _, ok := pos[r.Kind]; !ok {
			pos[r.Kind] = i
		}
	}
	if p, ok := pos[KindShortOnTime]; !ok || p > 8 {
		t.Fatalf("Short on Time should follow the discovery head for carol (pos %v): %v", p, kindsOf(c))
	}
	if r := rowByKind(c, KindShortOnTime); r != nil {
		for i, it := range r.Items {
			if !isShort(&it) {
				t.Fatalf("%s (%v) in carol's Short on Time", it.Title, it.Runtime)
			}
			if i < 3 && it.Type != core.TypeShow {
				t.Fatalf("carol watches short episodes on her phone; %s leads Short on Time", it.Title)
			}
		}
	}

	// Dave: brand new, popular rows only.
	d := homes["dave"]
	for _, kind := range []string{KindTop10, KindBecause, KindPerson, KindHiddenGems} {
		if rowByKind(d, kind) != nil {
			t.Fatalf("dave (no history) got %s", kind)
		}
	}
	if rowByKind(d, KindTrending) == nil {
		t.Fatalf("dave's Home lacks trending: %v", kindsOf(d))
	}

	// Visibly different Homes.
	names := []string{"alice", "bob", "carol", "dave"}
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			if s := jaccard(itemSet(homes[names[i]]), itemSet(homes[names[j]])); s > 0.5 {
				t.Errorf("%s and %s share %.0f%% of their titles", names[i], names[j], s*100)
			}
		}
	}

	// Context changes Alice's Home too: Saturday night on the TV versus a
	// weeknight on her phone. The fixed discovery head is intentionally stable;
	// context still changes the personal rows below it.
	sat, _ := h.e.PreviewHome(ctx, &alice, hubContext(time.Date(2026, 10, 17, 20, 30, 0, 0, time.UTC), core.DeviceTV), "", 12)
	phone, _ := h.e.PreviewHome(ctx, &alice, hubContext(time.Date(2026, 10, 14, 23, 0, 0, 0, time.UTC), core.DeviceMobile), "", 12)
	t.Logf("alice, Saturday TV:\n%s\nalice, weeknight phone:\n%s", describe(sat.Rows), describe(phone.Rows))
	same := true
	sk, pk := kindsOf(sat.Rows), kindsOf(phone.Rows)
	for i := 0; i < len(sk) && i < len(pk); i++ {
		if sk[i] != pk[i] {
			same = false
		}
	}
	if same {
		t.Fatalf("context did not change alice's row order: %v", sk)
	}
}
