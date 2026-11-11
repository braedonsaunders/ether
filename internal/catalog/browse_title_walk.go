package catalog

import (
	"context"
	"reflect"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	// A genre's title-sorted page walks catalog_titles_type_browse_sort in
	// title order and keeps the rows of the genre. The release gate every
	// listing applies (release_known, released_ts) was not in the index, so
	// the walk read each title row it passed: a page 1,000 titles into
	// Drama read some 12,000 rows. Carrying the two columns makes the walk
	// index-only; only the page's own rows are read.
	store.Register("catalog", 56, `DROP INDEX IF EXISTS catalog_titles_type_browse_sort;
CREATE INDEX catalog_titles_type_browse_sort ON catalog_titles(type, browsable, adult, sort_title ASC, key ASC, release_known, released_ts);`)
}

// studioNamesExpr builds a positive studio predicate's SQL: titles whose
// studio is one of the vocabulary names the subquery names returns.
func studioINListSQL(names string) string {
	return `t.key IN (SELECT s.key FROM catalog_studios s WHERE s.studio IN (` + names + `))`
}

// studioExistsSQL is the same predicate as a correlated probe, so a walk in
// title order checks each title's studio against the (studio, key) index and
// stops at the page rather than enumerating the whole matching set.
func studioExistsSQL(names string) string {
	return `EXISTS (SELECT 1 FROM catalog_studios s WHERE s.key=t.key AND s.studio IN (` + names + `))`
}

// studioProbe rewrites f's positive studio predicate in where, if there is
// exactly one, from the IN-list form to the correlated probe, for a page
// query that reads only the page's rows. The count and seek keep the
// IN-list form, which drives from the studio index. genres are handled the
// same way (genreProbe).
func (c *Catalog) studioProbe(where string, f core.Filter) string {
	names, _, ok := c.positiveStudioNames(f)
	if !ok {
		return where
	}
	return strings.Replace(where, studioINListSQL(names), studioExistsSQL(names), 1)
}

// genreExistsSQL is browseWhere's genre predicate for n genres.
func genreExistsSQL(n int) string {
	return `EXISTS (SELECT 1 FROM catalog_genres g WHERE g.key=t.key AND g.genre IN (` + placeholders(n) + `))`
}

// genreProbe makes f's genre predicate in where a per-row probe for a
// walk in an index order, which checks the genre of each title it passes.
// The probe seeks the genre's own (genre, key) range: by the primary key
// (key, genre) each probe lands on another page of the whole table, and a
// deep page's 25,000 probes read some 5,000 pages instead of 500.
// PostgreSQL otherwise hashes the genre's every title and, past the first
// pages, joins them to the type's whole index range and sorts; LIMIT 1
// OFFSET 0 keeps the subquery correlated, as browseWhere's series date
// predicates do.
func (c *Catalog) genreProbe(where string, f core.Filter) string {
	if len(f.Genres) == 0 {
		return where
	}
	in := `g.key=t.key AND g.genre IN (` + placeholders(len(f.Genres)) + `)`
	return strings.Replace(where, genreExistsSQL(len(f.Genres)), `EXISTS (SELECT 1 FROM catalog_genres g`+c.indexedBy("catalog_genres_genre")+` WHERE `+in+` LIMIT 1 OFFSET 0)`, 1)
}

// titleWalkPage reports whether q is a title-sorted page of one type whose
// filter is all predicates on the rows a walk of
// catalog_titles_type_browse_sort passes (the release and visibility
// gates, genres, years, a name range): its keys are read alone
// (titlePageKeys), then its rows. A filter better driven from its own
// index (a person, a collection, keywords, a studio) keeps the one query.
func titleWalkPage(q core.BrowseQuery) bool {
	return q.Sort == core.SortTitle && len(q.SecondarySorts) == 0 && !q.SortLastViewed && !q.Filter.LocalOnly &&
		(q.Filter.Type == core.TypeMovie || q.Filter.Type == core.TypeShow) && walkableFilter(q.Filter)
}

// walkableFilter reports whether f has only predicates checked on each
// title a walk in title order passes. Any other field set (a person, a
// collection, keywords, a studio, a viewer-state group) is a filter
// better driven from its own index, and so is any field added later.
func walkableFilter(f core.Filter) bool {
	f.Type, f.Genres, f.CatalogCandidates = "", nil, false
	f.YearFrom, f.YearTo, f.Years, f.ExcludeYears = 0, 0, nil, nil
	f.CatalogViewerID, f.CatalogRuleRevision = 0, 0
	f.DigitalDiscovery, f.DigitalDateEvidence, f.HomeReleaseOffsetDays, f.AvailabilityKeepKeys = false, false, 0, nil
	clearGateFields(&f)
	f.HiddenMovieSeq, f.HiddenShowSeq, f.HiddenEpisodeSeq = 0, 0, 0
	f.ExcludeKeys, f.ExcludeGenres, f.ExcludeKeywords = nil, nil, nil
	f.MaxContentRating, f.MinContentRating, f.SaferPlexRating, f.ContentRatings, f.ExcludeContentRatings = "", "", false, nil, nil
	f.IncludeCollectionNames, f.ExcludeCollectionNames = nil, nil
	f.FirstCharacter, f.NameStartsWith, f.NameStartsWithOrGreater, f.NameLessThan = "", "", "", ""
	f.ReleasedFilters, f.MinRating, f.MinRuntime, f.MaxRuntime = nil, 0, 0, 0
	v := reflect.ValueOf(f)
	for i := range v.NumField() {
		if field := v.Field(i); !field.IsZero() && (field.Kind() != reflect.Slice || field.Len() > 0) {
			return false
		}
	}
	return true
}

// titlePageKeys is the keys of a title-sorted page, from a walk of
// catalog_titles_type_browse_sort that reads no title row: the release
// gate is in the index and a genre is a probe of catalog_genres. A page
// offset rows deep passes the offset's titles and those the filter
// rejects between them, never the rest of the library. Reading the page's
// rows apart matters most on PostgreSQL, whose index scan fetches the row
// of every title it skips: 24,000 titles deep, 800 ms against 11 ms.
func (c *Catalog) titlePageKeys(ctx context.Context, where string, args []any, q core.BrowseQuery, offset, limit int) ([]core.RatingKey, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT t.key FROM catalog_titles t WHERE `+c.genreProbe(where, q.Filter)+` ORDER BY `+browseOrder(q)+`, t.key ASC LIMIT ? OFFSET ?`, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []core.RatingKey
	for rows.Next() {
		var key int64
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, core.RatingKey(key))
	}
	return keys, rows.Err()
}
