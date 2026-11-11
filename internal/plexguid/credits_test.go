package plexguid

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// The canonical Plex record can carry the right TMDB id, title and year while
// its credits belong to a different work (Plex merged two works): a real PMS
// shows the other work's cast to its apps. When both the canonical record and
// the catalogue list directors that share no name, the identity is refused so
// no plex:// guid is served and clients fall back to Ether's metadata.
func TestDisjointDirectorsNotServed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		local         []string
		canonical     []string
		localCast     []string
		canonicalCast []string
		want          bool
	}{
		{"overlapping director", []string{"Scott Waugh"}, []string{"Scott Waugh"}, nil, nil, true},
		{"disjoint director", []string{"Scott Waugh"}, []string{"Don Owen"}, nil, nil, false},
		{"case insensitive director", []string{"scott waugh"}, []string{"Scott Waugh"}, nil, nil, true},
		{"diacritics insensitive director", []string{"Zoe North"}, []string{"Zoë North"}, nil, nil, true},
		{"one side without director", []string{"Scott Waugh"}, nil, nil, nil, true},
		{"canonical director, local none, differing cast withheld", nil, []string{"Don Owen"}, []string{"A"}, []string{"B"}, false},
		{"shared director, differing cast withheld", []string{"Scott Waugh"}, []string{"Scott Waugh"}, []string{"Alan Ritchson"}, []string{"Bruce Kidd"}, false},
		{"shared director, overlapping cast accepted", []string{"Scott Waugh"}, []string{"Scott Waugh"}, []string{"Alan Ritchson"}, []string{"Owen Wilson", "Alan Ritchson"}, true},
		{"neither side with director, overlap cast", nil, nil, []string{"A", "B"}, []string{"B", "C"}, true},
		{"neither side with director, disjoint cast", nil, nil, []string{"A", "B"}, []string{"C", "D"}, false},
		{"directors present, cast ignored", []string{"Scott Waugh"}, []string{"Don Owen"}, []string{"A"}, []string{"B"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := store.OpenMemory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			const guid = "plex://movie/5d7768265af944001f1f6976"
			if _, err = db.Exec(`INSERT INTO catalog_titles(key,type,tmdb_id,title,year) VALUES(?,?,?,?,?)`, int64(core.MovieKey(770)), "movie", 770, "Runner", 2026); err != nil {
				t.Fatal(err)
			}
			for i, n := range tc.local {
				if _, err = db.Exec(`INSERT INTO catalog_credits(key,person_tmdb,name,kind,ord) VALUES(?,?,?, 'director',?)`, int64(core.MovieKey(770)), 1000+i, n, i); err != nil {
					t.Fatal(err)
				}
			}
			for i, n := range tc.localCast {
				if _, err = db.Exec(`INSERT INTO catalog_credits(key,person_tmdb,name,kind,ord) VALUES(?,?,?, 'cast',?)`, int64(core.MovieKey(770)), 2000+i, n, i); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"guid":%q,"type":"movie","title":"Runner","year":2026,"Guid":[{"id":"tmdb://770"}]%s%s}]}}`,
					guid, directorJSON(tc.canonical), roleJSON(tc.canonicalCast))
			}))
			defer srv.Close()
			s := New(db, Options{BaseURL: srv.URL, Token: func() string { return "fixture" }})
			defer s.Close()
			got, err := s.Resolve(ctx, core.MovieKey(770))
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.want {
				want = guid
			}
			if got != want {
				t.Fatalf("Resolve=%q, want %q", got, want)
			}
			if s.Cached(ctx, core.MovieKey(770)) != want {
				t.Fatalf("Cached=%q, want %q", s.Cached(ctx, core.MovieKey(770)), want)
			}
		})
	}
}

func directorJSON(names []string) string {
	if len(names) == 0 {
		return ""
	}
	out := `,"Director":[`
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf(`{"tag":%q}`, n)
	}
	return out + "]"
}

func roleJSON(names []string) string {
	if len(names) == 0 {
		return ""
	}
	out := `,"Role":[`
	for i, n := range names {
		if i > 0 {
			out += ","
		}
		out += fmt.Sprintf(`{"tag":%q}`, n)
	}
	return out + "]"
}

// An identity record written before the credit rule existed keeps serving, but
// a repair pass re-verifies it: a record whose canonical credits belong to
// another work is dropped, so an upgraded server corrects a merged identity
// without blanking the library.
func TestExistingIdentityRecheckedUnderCreditRule(t *testing.T) {
	ctx := context.Background()
	db, err := store.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(`INSERT INTO catalog_titles(key,type,tmdb_id,title,year) VALUES(?,?,?,?,?)`, int64(core.MovieKey(770)), "movie", 770, "Runner", 2026); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO catalog_credits(key,person_tmdb,name,kind,ord) VALUES(?,?,?, 'director', 0)`, int64(core.MovieKey(770)), 1, "Scott Waugh"); err != nil {
		t.Fatal(err)
	}
	const (
		merged = "plex://movie/5d7768265af944001f1f6962" // canonical record: wrong (1962) directors
		good   = "plex://movie/5d7768265af944001f1f6976" // canonical record: right directors
	)
	now := time.Now().Unix()
	// Two records verified under the old (pre-credit) rule: credits_rule=0.
	for _, pair := range []struct {
		key  core.RatingKey
		guid string
		y    int
	}{{core.MovieKey(770), merged, 1962}, {core.MovieKey(551), good, 2026}} {
		if _, err = db.Exec(`INSERT INTO plexguid_mappings(rating_key,guid,checked_at) VALUES(?,?,?)`, int64(pair.key), pair.guid, now); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO plexguid_verified(rating_key,guid) VALUES(?,?)`, int64(pair.key), pair.guid); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(`INSERT INTO plexguid_identity(rating_key,guid,tmdb,title,year,credits_rule) VALUES(?,?,?,?,?,0)`, int64(pair.key), pair.guid, pair.key.TMDB(), "Runner", pair.y); err != nil {
			t.Fatal(err)
		}
	}
	// The second title's own local credits disagree with the merged record the
	// same way; it is the 2026 film and its canonical record names the right
	// director.
	if _, err = db.Exec(`INSERT INTO catalog_titles(key,type,tmdb_id,title,year) VALUES(?,?,?,?,?)`, int64(core.MovieKey(551)), "movie", 551, "Runner", 2026); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO catalog_credits(key,person_tmdb,name,kind,ord) VALUES(?,?,?, 'director', 0)`, int64(core.MovieKey(551)), 2, "Someone Else"); err != nil {
		t.Fatal(err)
	}
	s := New(db, Options{Background: true})
	defer s.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/library/metadata/matches":
			// No search hit proves identity; verification uses the record.
			fmt.Fprint(w, `{"MediaContainer":{"Metadata":[]}}`)
		case "/library/metadata/" + merged[len(merged)-24:]:
			fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"guid":%q,"type":"movie","title":"Runner","year":2026,"Guid":[{"id":"tmdb://770"}]%s}]}}`, merged, directorJSON([]string{"Don Owen"}))
		case "/library/metadata/" + good[len(good)-24:]:
			fmt.Fprintf(w, `{"MediaContainer":{"Metadata":[{"guid":%q,"type":"movie","title":"Runner","year":2026,"Guid":[{"id":"tmdb://551"}]%s}]}}`, good, directorJSON([]string{"Someone Else"}))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s.o.BaseURL = srv.URL
	s.o.Token = func() string { return "fixture" }

	// Rows verified under the old rule are enqueued for priority re-check when
	// served, but still served until the check runs.
	if got := s.Cached(ctx, core.MovieKey(770)); got != merged {
		t.Fatalf("stale-rule record withheld before re-check: %q", got)
	}
	if !s.prioQueued[core.MovieKey(770)] && len(s.priority) == 0 {
		t.Fatal("stale-rule record not enqueued for priority re-check")
	}

	if _, err = s.reverify(ctx, core.MovieKey(770)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.reverify(ctx, core.MovieKey(551)); err != nil {
		t.Fatal(err)
	}
	if got := s.Cached(ctx, core.MovieKey(770)); got != "" {
		t.Fatalf("merged identity survived repair: %q", got)
	}
	if got := s.Cached(ctx, core.MovieKey(551)); got != good {
		t.Fatalf("agreeing identity lost after repair: %q", got)
	}
	var n int
	if err = db.QueryRow(`SELECT COUNT(*) FROM plexguid_identity WHERE rating_key=? AND guid=? AND credits_rule=1`, int64(core.MovieKey(551)), good).Scan(&n); err != nil || n != 1 {
		t.Fatalf("agreeing record not re-marked under credit rule: n=%d err=%v", n, err)
	}
}
