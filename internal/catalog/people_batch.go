package catalog

import (
	"context"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// A people search checks, for each person whose name matches, that the
// viewer can see at least one title crediting them. Per person that was a
// credit query, a full title read (every credit of every title) and a
// re-read of the portrait title: a statement set per matching person,
// growing with the library. These read a page of people in a few
// statements, and only the facts a visibility decision needs.

// peopleBatch bounds the people one credits statement covers.
const peopleBatch = 25

// PersonCreditsMany is PersonCreditKeys for several people at once: each
// person's most popular credited titles of the given types (at most limit
// each), with the person's credit rows on them, most popular first.
func (c *Catalog) PersonCreditsMany(ctx context.Context, persons []int64, types []core.MediaType, limit int) (map[int64][]core.PersonCredit, error) {
	out, _, err := c.personCredits(ctx, persons, types, limit, false)
	return out, err
}

// PersonCreditTitles is PersonCreditsMany with the credited titles'
// visibility rows (CachedVisibilityRows, with facts) read in the same
// statements: a people search's credits and the rows it judges them by.
func (c *Catalog) PersonCreditTitles(ctx context.Context, persons []int64, types []core.MediaType, limit int, facts core.TitleFacts) (map[int64][]core.PersonCredit, map[core.RatingKey]core.Title, error) {
	credits, titles, err := c.personCredits(ctx, persons, types, limit, true)
	if err != nil {
		return nil, nil, err
	}
	items := make([]core.Title, 0, len(titles))
	for _, t := range titles {
		items = append(items, t)
	}
	if err := c.hydrateFacts(ctx, items, facts|c.viewerPolicyFacts(ctx)|c.policyFacts(c.visibilityFor(ctx))); err != nil {
		return nil, nil, err
	}
	rows := make(map[core.RatingKey]core.Title, len(items))
	for i := range items {
		if items[i].Type != core.TypeSeason && c.cachedListVisible(&items[i]) {
			rows[items[i].Key] = items[i]
		}
	}
	return credits, rows, nil
}

// personCredits reads persons' credits in one statement per batch, a row
// per credit; withTitles also reads each credited title's row.
func (c *Catalog) personCredits(ctx context.Context, persons []int64, types []core.MediaType, limit int, withTitles bool) (map[int64][]core.PersonCredit, map[core.RatingKey]core.Title, error) {
	out := map[int64][]core.PersonCredit{}
	titles := map[core.RatingKey]core.Title{}
	if len(persons) == 0 || len(types) == 0 || limit <= 0 {
		return out, titles, nil
	}
	cols := ""
	if withTitles {
		cols = qualifiedTitleCols("t") + ", "
	}
	for part := range store.Chunks(persons, peopleBatch) {
		// One statement: each person's most popular credited titles with
		// that person's credit rows on them, a row per credit.
		var parts []string
		var args []any
		for _, p := range part {
			if p <= 0 {
				continue
			}
			q, a := personCreditKeysSQL(p, types, limit)
			join := ""
			if withTitles {
				join = ` JOIN catalog_titles t ON t.key=k.key`
			}
			parts = append(parts, `SELECT `+cols+`CAST(? AS BIGINT) AS person, k.key AS credit_key, k.popularity AS popularity, cr.kind AS kind, cr.name AS name, cr.role AS role, cr.thumb AS thumb, cr.provider_id AS provider_id, COALESCE(pp.imdb,'') AS imdb, cr.ord AS ord
		FROM (`+q+`) AS k JOIN catalog_credits cr ON cr.person_tmdb=? AND cr.key=k.key LEFT JOIN catalog_people pp ON pp.person_tmdb=cr.person_tmdb`+join)
			args = append(append(append(args, p), a...), p)
		}
		if len(parts) == 0 {
			continue
		}
		rows, err := c.db.QueryContext(ctx, strings.Join(parts, "\nUNION ALL\n")+"\nORDER BY person, popularity DESC, credit_key, kind, ord", args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var person, key, ord int64
			var pop float64
			var cr core.PersonCredit
			dest := []any{&person, &key, &pop, &cr.Kind, &cr.Person.Name, &cr.Person.Role, &cr.Person.ThumbPath, &cr.Person.ID, &cr.Person.IMDB, &ord}
			if withTitles {
				st, err := scanTitle(extraScan{rows, dest})
				if err != nil {
					rows.Close()
					return nil, nil, err
				}
				titles[st.t.Key] = st.t
			} else if err := rows.Scan(dest...); err != nil {
				rows.Close()
				return nil, nil, err
			}
			cr.Key, cr.Person.TMDB = core.RatingKey(key), person
			out[person] = append(out[person], cr)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
	}
	return out, titles, nil
}

// qualifiedTitleCols is titleCols read through alias.
func qualifiedTitleCols(alias string) string {
	cols := strings.Split(titleCols, ",")
	for i := range cols {
		cols[i] = alias + "." + strings.TrimSpace(cols[i])
	}
	return strings.Join(cols, ",")
}

type personKey struct {
	person int64
	key    core.RatingKey
}

// CachedVisibleTitles is CachedCreditTitles for a visibility decision only:
// the stored rows, plus the facts visibility rules and the release gate
// read when they read any, without credits or alternative titles.
func (c *Catalog) CachedVisibleTitles(ctx context.Context, keys []core.RatingKey, cfg config.Catalog, now time.Time) (map[core.RatingKey]core.Title, error) {
	filter := c.visibilityFilter()
	digital := cfg.ReleaseMode == config.ModeDigital
	out := make(map[core.RatingKey]core.Title, len(keys))
	for part := range store.Chunks(keys, 200) {
		args := make([]any, len(part))
		for i, k := range part {
			args[i] = int64(k)
		}
		rows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE key IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		var items []core.Title
		for rows.Next() {
			st, err := scanTitle(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			items = append(items, st.t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if !filter.Passive() || digital {
			if err := c.hydrateVisibilityFacts(ctx, items); err != nil {
				return nil, err
			}
		}
		var seasons []*core.Title
		for i := range items {
			if items[i].Type == core.TypeSeason {
				seasons = append(seasons, &items[i])
			}
		}
		if len(seasons) > 0 {
			if err := applyAiredCounts(ctx, c.db, now, seasons, c.WriteGeneration()); err != nil {
				return nil, err
			}
		}
		for i := range items {
			t := &items[i]
			upcoming := cfg.ShowUpcomingMovies && t.Type == core.TypeMovie && !t.Released.IsZero() && t.Released.After(now)
			if t.Adult || (!upcoming && !t.IsReleased(now)) || (!c.isTitleOrShowOwnerIncluded(t) && filter.Hidden(t)) || (!upcoming && digital && !homeReleaseVisible(t, now, cfg.HomeReleaseOffsetDays)) || (t.Type == core.TypeSeason && !listableSeason(t, now)) {
				continue
			}
			out[t.Key] = *t
		}
	}
	return out, nil
}

// CachedVisibilityRows is CachedGetMany for a visibility decision: stored
// rows that pass the list gates, with the facts the catalogue's policy for
// ctx's viewer reads (viewerPolicyFacts) and facts, the caller's own, and
// without credits. The partial titles are not cached.
func (c *Catalog) CachedVisibilityRows(ctx context.Context, keys []core.RatingKey, facts core.TitleFacts) (map[core.RatingKey]core.Title, error) {
	out := make(map[core.RatingKey]core.Title, len(keys))
	// A full snapshot already has every requested fact. Reuse it exactly as
	// CachedGetMany does, so repeated policy/facet checks on a listing do
	// not replace cache hits with fresh SQL reads. Partial rows stay uncached.
	gen, minute := c.writeGen.Load(), c.minuteBucket()
	var missing []core.RatingKey
	for _, key := range keys {
		if title, ok := c.cachedStored(key, gen, minute); ok {
			if title.Type != core.TypeSeason && c.cachedListVisible(&title) {
				out[key] = title
			}
			continue
		}
		missing = append(missing, key)
	}
	facts |= c.viewerPolicyFacts(ctx) | c.policyFacts(c.visibilityFor(ctx))
	for part := range store.Chunks(missing, 200) {
		args := make([]any, len(part))
		for i, k := range part {
			args[i] = int64(k)
		}
		rows, err := c.db.QueryContext(ctx, `SELECT `+titleCols+` FROM catalog_titles WHERE key IN (`+placeholders(len(part))+`)`, args...)
		if err != nil {
			return nil, err
		}
		var items []core.Title
		for rows.Next() {
			st, err := scanTitle(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			items = append(items, st.t)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		if err := c.hydrateFacts(ctx, items, facts); err != nil {
			return nil, err
		}
		for i := range items {
			if items[i].Type != core.TypeSeason && c.cachedListVisible(&items[i]) {
				out[items[i].Key] = items[i]
			}
		}
	}
	return out, nil
}
