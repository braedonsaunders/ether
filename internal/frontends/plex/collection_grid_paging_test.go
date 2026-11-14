package plex

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

type collectionGridPolicy struct{ key core.RatingKey }

func (collectionGridPolicy) SetMaxRating(context.Context, *core.User, string) error {
	return core.ErrUnauthorized
}

func (p collectionGridPolicy) Allowed(_ *core.User, t *core.Title) bool { return t.Key != p.key }

type collectionGridSource struct {
	core.Collections
	calls int
	fail  bool
}

func (s *collectionGridSource) Items(ctx context.Context, u *core.User, key core.RatingKey, offset, limit int) (core.Page, error) {
	s.calls++
	if s.fail {
		return core.Page{}, errors.New("collection read failed")
	}
	return s.Collections.Items(ctx, u, key, offset, limit)
}

// Client continuation offsets count returned cards, not unfiltered membership.
// Exercise full pages, duplicates, changed discovery, current authorization,
// aliases, and a failed generation build through the real HTTP adapter.
func TestCollectionGridPagingInvariant(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	key := core.SmartKey(9821)
	f.cols.cols[key] = core.Collection{Key: key, Title: "Grid", Type: core.TypeMovie}
	var expected []string
	for i := 0; i < 60; i++ {
		title := fightClub
		title.Key = core.MovieKey(int64(40000 + i))
		title.Title = fmt.Sprintf("Movie %02d", i)
		f.cols.items[key] = append(f.cols.items[key], title)
		f.catalog.titles[title.Key] = title
		if i < 10 || i > 12 {
			expected = append(expected, title.Key.String())
		}
	}
	f.cols.items[key] = append(f.cols.items[key], f.cols.items[key][2])
	source := &collectionGridSource{Collections: f.cols}
	f.srv.setCollections(source)
	f.srv.setFilterAvailable(func(_ context.Context, in []core.Title) []core.Title {
		out := make([]core.Title, 0, len(in))
		for _, v := range in {
			if v.Key.TMDB() < 40010 || v.Key.TMDB() > 40012 {
				out = append(out, v)
			}
		}
		return out
	})
	get := func(prefix string, start, size int) model.MediaContainer {
		t.Helper()
		response := f.get(t, fmt.Sprintf("%s?X-Plex-Container-Start=%d&X-Plex-Container-Size=%d", prefix, start, size), jsonAccept...)
		if response.Code != 200 {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		return decodeShelfContainer(t, response.Body.Bytes())
	}
	path := "/library/collections/" + key.String() + "/children"
	source.fail = true
	if response := f.get(t, path, jsonAccept...); response.Code == 200 {
		t.Fatal("source error published an empty successful page")
	}
	source.fail = false
	var got []string
	for _, part := range []struct{ start, size int }{{0, 8}, {8, 24}, {32, 24}, {56, 24}} {
		page := get(path, part.start, part.size)
		if page.TotalSize == nil || *page.TotalSize != len(expected) {
			t.Fatalf("page=%+v", page)
		}
		want := min(part.size, len(expected)-part.start)
		if page.Size != want {
			t.Fatalf("start=%d size=%d want=%d", part.start, page.Size, want)
		}
		for _, v := range page.Metadata {
			got = append(got, v.RatingKey)
		}
		// Discovery changes cannot rewrite an already published browsing cut.
		f.srv.setFilterAvailable(func(context.Context, []core.Title) []core.Title { return nil })
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("page union=%v want=%v", got, expected)
	}
	if source.calls != 2 {
		t.Fatalf("source rebuilt on continuation: %d calls", source.calls)
	}
	alias := get("/library/metadata/"+key.String()+"/children", 8, 24)
	if len(alias.Metadata) != 24 || alias.Metadata[0].RatingKey != expected[8] {
		t.Fatalf("alias shifted: %+v", alias)
	}
	f.srv.setTestPolicy(collectionGridPolicy{key: core.MovieKey(40020)})
	restricted := get(path, 0, 100)
	if restricted.TotalSize == nil || *restricted.TotalSize != len(expected)-1 {
		t.Fatalf("authorization not applied before count: %+v", restricted)
	}
	for _, item := range restricted.Metadata {
		if item.RatingKey == core.MovieKey(40020).String() {
			t.Fatal("retained generation bypassed current authorization")
		}
	}
}

type collectionAdmissionCatalog struct {
	core.Catalog
	open bool
}

func (c *collectionAdmissionCatalog) TitleOpenable(t *core.Title) bool {
	return c.open && !t.Adult
}

// A series-only list must have one offset space across arbitrary slicing,
// screen-cache expiry, episode-fact updates and live discovery changes.
func TestSeriesCollectionSlicingInvariant(t *testing.T) {
	f := newFixture()
	defer f.srv.Close()
	key := core.SmartKey(9822)
	var clock atomic.Int64
	clock.Store(fixedNow.UnixNano())
	f.srv.setTestNow(func() time.Time { return time.Unix(0, clock.Load()) })
	admission := &collectionAdmissionCatalog{Catalog: f.catalog, open: true}
	f.srv.setCatalog(admission)
	f.cols.cols[key] = core.Collection{Key: key, Title: "Series", Type: core.TypeShow, UpdatedAt: fixedNow, MembershipRevision: "first"}
	var expected []string
	for i := 0; i < 160; i++ {
		title := breakingBad
		title.Key = core.ShowKey(int64(50000 + i))
		title.Title = fmt.Sprintf("Series %03d", i)
		f.cols.items[key] = append(f.cols.items[key], title)
		f.catalog.titles[title.Key] = title
		expected = append(expected, title.Key.String())
	}
	f.cols.items[key] = append(f.cols.items[key], f.cols.items[key][5])
	source := &collectionGridSource{Collections: f.cols}
	f.srv.setCollections(source)
	path := "/library/collections/" + key.String() + "/children"
	read := func(path string, start, size int) []string {
		t.Helper()
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		response := f.get(t, fmt.Sprintf("%s%sX-Plex-Container-Start=%d&X-Plex-Container-Size=%d", path, sep, start, size), jsonAccept...)
		if response.Code != 200 {
			t.Fatalf("status=%d: %s", response.Code, response.Body.String())
		}
		mc := decodeShelfContainer(t, response.Body.Bytes())
		if mc.TotalSize == nil || *mc.TotalSize != len(expected) {
			t.Fatalf("total=%v want=%d", mc.TotalSize, len(expected))
		}
		var keys []string
		for _, item := range mc.Metadata {
			keys = append(keys, item.RatingKey)
		}
		return keys
	}
	first := read(path, 0, 84)
	clock.Add(int64(6 * time.Minute))
	admission.open = false
	// Simulate a changed series ordering/new episode and discovery gate while
	// the underlying collection membership generation is unchanged.
	slices.Reverse(f.cols.items[key])
	f.srv.setFilterAvailable(func(context.Context, []core.Title) []core.Title { return nil })
	second := read(path, 84, 48)
	full := read(path, 0, 132)
	if !reflect.DeepEqual(append(first, second...), full) || !reflect.DeepEqual(full, expected[:132]) {
		t.Fatal("full fetch differs from concatenated pages after cache expiry")
	}
	for size := 1; size <= 61; size += 10 {
		var keys []string
		for start := 0; start < len(expected); start += size {
			keys = append(keys, read(path, start, size)...)
		}
		if !reflect.DeepEqual(keys, expected) {
			t.Fatalf("slicing changed membership at size=%d", size)
		}
	}
	if source.calls != 1 {
		t.Fatalf("unchanged membership rebuilt %d times", source.calls)
	}
	// Preview assembly may contain only the initial cards. The key must page
	// the full canonical source order, even for a Home row without a section.
	f.srv.setFilterAvailable(nil)
	pr := &Request{User: &ownerUser, ClientID: "client-1"}
	row := f.srv.renderHub(context.Background(), pr, hubSpec{Hub: core.Hub{
		ID: "collection.series", Key: key, Title: "Series", Type: core.TypeShow,
		Items: f.cols.items[key][:8], More: true,
	}}, hubCtx{visible: 8})
	if row.Size != 8 || !row.More || row.TotalSize == nil || *row.TotalSize != len(expected) || shelfToken(strings.SplitN(row.Key, "?", 2)[1]) == "" {
		t.Fatalf("preview cannot page full membership: %+v", row)
	}
	// A changed source stamp creates a new cut; the published preview token
	// remains an immutable list when the unversioned collection moves on.
	admission.open = true
	c := f.cols.cols[key]
	c.MembershipRevision = "second"
	f.cols.cols[key] = c
	old := read(row.Key, 0, len(expected))
	current := read(path, 0, len(expected))
	if reflect.DeepEqual(old, current) {
		t.Fatal("changed collection did not create a new generation")
	}
}
