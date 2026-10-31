package catalog

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/tmdb"
)

// slowTMDB answers movie details only when released.
type slowTMDB struct {
	TMDB
	release chan struct{}
	calls   atomic.Int64
}

func (s *slowTMDB) MovieDetails(ctx context.Context, id int64) (*tmdb.MovieDetails, error) {
	s.calls.Add(1)
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &tmdb.MovieDetails{Movie: tmdb.Movie{ID: id, Title: "Fresh", ReleaseDate: "2001-01-01"}}, nil
}

// A read never waits on TMDB for stored details, waits at most hydrateWait
// for a title with none, and the detached fetch still lands in the mirror.
func TestReadsDoNotWaitOnSlowMetadata(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	slow := &slowTMDB{release: make(chan struct{})}
	c, err := New(db, slow, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	stale, summary := core.MovieKey(1), core.MovieKey(2)
	for _, key := range []core.RatingKey{stale, summary} {
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: key.TMDB()}, Title: "Stored", Released: now.AddDate(0, -1, 0)}, true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1, detail_at=? WHERE key=?`, now.Add(-48*time.Hour).Unix(), int64(stale)); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	got, err := c.Get(ctx, stale)
	if err != nil || got.Title != "Stored" {
		t.Fatalf("stale read = %+v, %v", got, err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("stale read waited %s on TMDB", d)
	}

	start = time.Now()
	got, err = c.Get(ctx, summary)
	if err != nil || got.Title != "Stored" {
		t.Fatalf("summary read = %+v, %v", got, err)
	}
	if d := time.Since(start); d < hydrateWait || d > hydrateWait+time.Second {
		t.Fatalf("summary read took %s, want about %s", d, hydrateWait)
	}

	close(slow.release)
	c.WaitRefreshes()
	for _, key := range []core.RatingKey{stale, summary} {
		if got, err := c.Get(ctx, key); err != nil || got.Title != "Fresh" {
			t.Fatalf("after refresh %s = %+v, %v", key, got, err)
		}
	}
	if n := slow.calls.Load(); n != 2 {
		t.Fatalf("TMDB calls = %d, want one per title", n)
	}
}

// Concurrent readers of one title share a single refresh.
func TestConcurrentReadsShareOneRefresh(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	slow := &slowTMDB{release: make(chan struct{})}
	c, err := New(db, slow, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	key := core.MovieKey(7)
	if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: 7}, Title: "Stored", Released: now.AddDate(0, -1, 0)}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE catalog_titles SET detail=1, detail_at=? WHERE key=?`, now.Add(-48*time.Hour).Unix(), int64(key)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	for range 20 {
		go func() {
			_, _ = c.Get(ctx, key)
			done <- struct{}{}
		}()
	}
	for range 20 {
		<-done
	}
	close(slow.release)
	c.WaitRefreshes()
	if n := slow.calls.Load(); n != 1 {
		t.Fatalf("TMDB calls = %d, want 1", n)
	}
}

// The trigram index answers title searches with exactly the rows LIKE does.
func TestTitleSearchIndexMatchesLike(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, err := New(db, nil, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	titles := []string{"Star Wars", "Lone Star", "STARGATE", "Amélie", "AMÉLIE", "The Starling", "Mustard", "Ça", "100% Wolf", "Under_score"}
	for i, name := range titles {
		key := core.MovieKey(int64(i + 1))
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: key.TMDB()}, Title: name, Released: now.AddDate(-1, 0, 0)}, true); err != nil {
			t.Fatal(err)
		}
	}
	// Renames keep the index in step.
	if err := c.UpsertTitle(ctx, core.Title{Key: core.MovieKey(7), Type: core.TypeMovie, IDs: core.IDs{TMDB: 7}, Title: "Custard Star", Released: now.AddDate(-1, 0, 0)}, true); err != nil {
		t.Fatal(err)
	}
	// Alternative titles match too, and stay in step through replacement.
	if err := c.UpsertTitle(ctx, core.Title{Key: core.MovieKey(4), Type: core.TypeMovie, IDs: core.IDs{TMDB: 4}, Title: "Amélie", AlternativeTitles: []string{"Le Fabuleux Destin", "Sousou no Frieren"}, Released: now.AddDate(-1, 0, 0)}, true); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertTitle(ctx, core.Title{Key: core.MovieKey(4), Type: core.TypeMovie, IDs: core.IDs{TMDB: 4}, Title: "Amélie", AlternativeTitles: []string{"Sousou no Frieren"}, Released: now.AddDate(-1, 0, 0)}, true); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"star", "STAR", "amé", "AMÉ", "élie", "tar", "ard", "% W", "r_s", "zzz", "mus", "frieren", "Fabuleux"} {
		cond, args := c.titleMatch("", q)
		indexed := keysWhere(t, db, cond, args)
		like := "%" + escapeLike(q) + "%"
		op := store.Like(store.DialectOf(db))
		plain := keysWhere(t, db, `(title `+op+` ? ESCAPE '\' OR original_title `+op+` ? ESCAPE '\' OR EXISTS (SELECT 1 FROM catalog_alt_titles a WHERE a.key=catalog_titles.key AND a.title `+op+` ? ESCAPE '\'))`, []any{like, like, like})
		if strings.Join(indexed, ",") != strings.Join(plain, ",") {
			t.Errorf("%q: index %v, LIKE %v", q, indexed, plain)
		}
	}
	if got := keysWhere(t, db, func() string { c, _ := c.titleMatch("", "frieren"); return c }(), []any{"%frieren%", "%frieren%", "%frieren%"}); len(got) != 1 {
		t.Fatalf("alternative title search = %v", got)
	}
	if got := keysWhere(t, db, func() string { c, _ := c.titleMatch("", "fabuleux"); return c }(), []any{"%fabuleux%", "%fabuleux%", "%fabuleux%"}); len(got) != 0 {
		t.Fatalf("a removed alternative title still matches: %v", got)
	}
}

func keysWhere(t *testing.T, db *sql.DB, cond string, args []any) []string {
	t.Helper()
	rows, err := db.Query(`SELECT key FROM catalog_titles WHERE `+cond+` ORDER BY key`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

// The people-name index follows credits as titles are stored and replaced,
// and matches word prefixes.
func TestPeopleNameIndexFollowsCredits(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, err := New(db, nil, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	store1 := func(key core.RatingKey, cast ...core.Person) {
		t.Helper()
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: key.TMDB()}, Title: "Film", Released: now.AddDate(-1, 0, 0), Cast: cast}, true); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE catalog_titles SET browsable=1 WHERE key=?`, int64(key)); err != nil {
			t.Fatal(err)
		}
	}
	store1(core.MovieKey(1), core.Person{TMDB: 7, Name: "Ringo Starr"}, core.Person{TMDB: 8, Name: "Mustard Plant"})
	store1(core.MovieKey(2), core.Person{TMDB: 7, Name: "Ringo Starr"})
	// MatchPeople returns candidates (a substring match on PostgreSQL);
	// callers keep the names with a word starting with every term.
	names := func(terms ...string) []string {
		t.Helper()
		got, err := c.MatchPeople(ctx, terms, 10)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range got {
			words := strings.Fields(strings.ToLower(m.Name))
			all := true
			for _, term := range terms {
				found := false
				for _, w := range words {
					found = found || strings.HasPrefix(w, term)
				}
				all = all && found
			}
			if all {
				out = append(out, m.Name)
			}
		}
		return out
	}
	if got := names("star"); strings.Join(got, ",") != "Ringo Starr" {
		t.Fatalf("star = %v (word prefixes only)", got)
	}
	if got := names("ringo", "sta"); strings.Join(got, ",") != "Ringo Starr" {
		t.Fatalf("ringo sta = %v", got)
	}
	keys, err := c.PersonCreditKeys(ctx, 7, []core.MediaType{core.TypeMovie}, 10)
	if err != nil || len(keys) != 2 {
		t.Fatalf("credit keys = %v, %v", keys, err)
	}
	// Replacing a title's cast drops a person whose last credit went.
	store1(core.MovieKey(1), core.Person{TMDB: 7, Name: "Ringo Starr"})
	if got := names("mustard"); len(got) != 0 {
		t.Fatalf("uncredited person still found: %v", got)
	}
}

// A request's reads share one wait budget across every title they touch.
func TestReadWaitBudgetBoundsARequest(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	slow := &slowTMDB{release: make(chan struct{})}
	c, err := New(db, slow, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		close(slow.release)
		c.WaitRefreshes()
	}()
	var keys []core.RatingKey
	for i := int64(1); i <= 4; i++ {
		key := core.MovieKey(i)
		keys = append(keys, key)
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: core.TypeMovie, IDs: core.IDs{TMDB: i}, Title: "Stored", Released: now.AddDate(0, -1, 0)}, true); err != nil {
			t.Fatal(err)
		}
	}
	reqCtx := WithReadWaitBudget(ctx, 500*time.Millisecond)
	start := time.Now()
	for _, key := range keys {
		if got, err := c.Get(reqCtx, key); err != nil || got.Title != "Stored" {
			t.Fatalf("%s = %+v, %v", key, got, err)
		}
	}
	if d := time.Since(start); d > 900*time.Millisecond {
		t.Fatalf("four titles waited %s in all, want about the 500ms budget", d)
	}
}

// The exclusion SQL hides exactly the titles the Go filter hides.
func TestExclusionSQLMatchesFilter(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c, err := New(db, nil, config.Catalog{}, nil, WithNow(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	facets := []struct {
		countries, genres []string
		typ               core.MediaType
	}{
		{nil, nil, core.TypeMovie}, {[]string{"IN"}, nil, core.TypeMovie}, {[]string{"US", "IN"}, []string{"Drama"}, core.TypeShow},
		{[]string{"FR"}, []string{"Horror"}, core.TypeMovie}, {[]string{"JP"}, []string{"Animation", "Drama"}, core.TypeShow}, {nil, []string{"Horror"}, core.TypeShow},
	}
	var keys []core.RatingKey
	for i, f := range facets {
		key := core.MovieKey(int64(i + 1))
		if f.typ == core.TypeShow {
			key = core.ShowKey(int64(i + 1))
		}
		keys = append(keys, key)
		if err := c.UpsertTitle(ctx, core.Title{Key: key, Type: f.typ, IDs: core.IDs{TMDB: key.TMDB()}, Title: "T", Released: now.AddDate(-1, 0, 0), Countries: f.countries, Genres: f.genres}, true); err != nil {
			t.Fatal(err)
		}
	}
	rule := func(field, op, raw string) core.CollectionRule {
		return core.CollectionRule{Field: field, Op: op, Value: []byte(raw)}
	}
	for name, specs := range map[string][]core.LibraryExclusion{
		"country in":       {{Name: "G", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{rule("country", "in", `["IN","JP"]`)}}},
		"country notIn":    {{Name: "G", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{rule("country", "notIn", `["US","FR"]`)}}},
		"genre any":        {{Name: "G", MediaType: "show", Match: "any", Rules: []core.CollectionRule{rule("genre", "in", `["Horror"]`), rule("country", "is", `"JP"`)}}},
		"genre isNot all":  {{Name: "G", MediaType: "mixed", Match: "all", Rules: []core.CollectionRule{rule("genre", "isNot", `"Drama"`), rule("country", "in", `["FR","IN"]`)}}},
		"two groups mixed": {{Name: "G", MediaType: "movie", Match: "all", Rules: []core.CollectionRule{rule("country", "in", `["IN"]`)}}, {Name: "G", MediaType: "show", Match: "all", Rules: []core.CollectionRule{rule("genre", "notIn", `["Drama"]`)}}},
	} {
		f, err := libraryfilter.Compile(specs)
		if err != nil {
			t.Fatal(name, err)
		}
		where, args := f.SQLHidden()
		var sqlHidden []string
		rows, err := db.QueryContext(ctx, `SELECT key FROM catalog_titles t WHERE `+where+` ORDER BY key`, args...)
		if err != nil {
			t.Fatal(name, err)
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				t.Fatal(err)
			}
			sqlHidden = append(sqlHidden, k)
		}
		rows.Close()
		var goHidden []string
		for _, key := range keys {
			title, err := c.StoredTitle(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			if f.Hidden(title) {
				goHidden = append(goHidden, strconv.FormatInt(int64(key), 10))
			}
		}
		sort.Strings(goHidden)
		sort.Strings(sqlHidden)
		if strings.Join(sqlHidden, ",") != strings.Join(goHidden, ",") {
			t.Errorf("%s: SQL hides %v, filter hides %v", name, sqlHidden, goHidden)
		}
	}
}
