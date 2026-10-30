package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"time"
)

// Mirror exposes bounded local reads for catalogue consumers. It owns schema
// knowledge; callers never compose SQL against catalog_* tables.
type Mirror struct {
	db  *sql.DB
	now func() time.Time
}

func NewMirror(db *sql.DB, now func() time.Time) *Mirror {
	if now == nil {
		now = time.Now
	}
	return &Mirror{db: db, now: now}
}

type MirrorFingerprint struct {
	Count            int
	MaxFetch, MaxAdd int64
}

func (m *Mirror) Fingerprint(ctx context.Context) (MirrorFingerprint, error) {
	var fp MirrorFingerprint
	err := m.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(fetched_at),0),COALESCE(MAX(added_at),0) FROM catalog_titles t WHERE `+mirrorBrowsableWhere).Scan(&fp.Count, &fp.MaxFetch, &fp.MaxAdd)
	return fp, err
}

type MirrorItem struct {
	Title    core.Title
	AddedAt  int64
	Detailed bool
}

// mirrorBrowsableWhere is the browsable set as Browse serves it: adult
// titles are never shown, whatever the sync settings.
const mirrorBrowsableWhere = `t.type IN ('movie','show') AND t.browsable=1 AND t.adult=0`

func (m *Mirror) Snapshot(ctx context.Context) ([]MirrorItem, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT t.key,t.type,t.title,t.sort_title,t.year,t.released_ts,t.added_at,t.runtime_sec,t.rating,t.vote_count,t.popularity,t.language,t.content_rating,t.collection_id,t.poster,t.backdrop,t.detail FROM catalog_titles t WHERE `+mirrorBrowsableWhere+` ORDER BY t.popularity DESC,t.key ASC`)
	if err != nil {
		return nil, err
	}
	out := []MirrorItem{}
	byKey := map[core.RatingKey]int{}
	for rows.Next() {
		var e MirrorItem
		var released, runtime int64
		t := &e.Title
		if err := rows.Scan(&t.Key, &t.Type, &t.Title, &t.SortTitle, &t.Year, &released, &e.AddedAt, &runtime, &t.Rating, &t.VoteCount, &t.Popularity, &t.Language, &t.ContentRating, &t.CollectionID, &t.PosterPath, &t.BackdropPath, &e.Detailed); err != nil {
			rows.Close()
			return nil, err
		}
		if released != 0 {
			t.Released = time.Unix(released, 0).UTC()
		}
		t.Runtime = time.Duration(runtime) * time.Second
		byKey[t.Key] = len(out)
		out = append(out, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	pairs := func(query string, apply func(*core.Title, string)) error {
		rows, err := m.db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k core.RatingKey
			var v string
			if err := rows.Scan(&k, &v); err != nil {
				return err
			}
			if i, ok := byKey[k]; ok {
				apply(&out[i].Title, v)
			}
		}
		return rows.Err()
	}
	for _, pair := range []struct {
		query string
		apply func(*core.Title, string)
	}{
		{`SELECT g.key,g.genre FROM catalog_genres g JOIN catalog_titles t ON t.key=g.key WHERE ` + mirrorBrowsableWhere + ` ORDER BY g.key,g.genre`, func(t *core.Title, v string) { t.Genres = append(t.Genres, v) }},
		{`SELECT g.key,g.country FROM catalog_countries g JOIN catalog_titles t ON t.key=g.key WHERE ` + mirrorBrowsableWhere, func(t *core.Title, v string) { t.Countries = append(t.Countries, v) }},
		{`SELECT g.key,g.studio FROM catalog_studios g JOIN catalog_titles t ON t.key=g.key WHERE ` + mirrorBrowsableWhere, func(t *core.Title, v string) { t.Studios = append(t.Studios, v) }},
		{`SELECT g.key,g.keyword FROM catalog_keywords g JOIN catalog_titles t ON t.key=g.key WHERE ` + mirrorBrowsableWhere, func(t *core.Title, v string) { t.Keywords = append(t.Keywords, v) }},
	} {
		if err := pairs(pair.query, pair.apply); err != nil {
			return nil, err
		}
	}
	rows, err = m.db.QueryContext(ctx, `SELECT c.key,c.person_tmdb,c.kind FROM catalog_credits c JOIN catalog_titles t ON t.key=c.key WHERE `+mirrorBrowsableWhere)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var key core.RatingKey
		var pid int64
		var kind string
		if err := rows.Scan(&key, &pid, &kind); err != nil {
			rows.Close()
			return nil, err
		}
		i, ok := byKey[key]
		if !ok {
			continue
		}
		t := &out[i].Title
		p := core.Person{TMDB: pid}
		switch kind {
		case "director":
			t.Directors = append(t.Directors, p)
		case "writer":
			t.Writers = append(t.Writers, p)
		case "crew":
			t.Crew = append(t.Crew, p)
		default:
			t.Cast = append(t.Cast, p)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	titles := make([]core.Title, len(out))
	for i := range out {
		titles[i] = out[i].Title
	}
	if err = applyAirAddedTimes(ctx, m.db, titles, m.now()); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Title.AddedAt = titles[i].AddedAt
	}
	return out, nil
}

func (m *Mirror) Franchises(ctx context.Context) ([]core.Collection, error) {
	rows, err := m.db.QueryContext(ctx, `
		SELECT t.collection_id,
		       COALESCE(NULLIF(MAX(t.collection_name),''), MAX(cc.name), ''),
		       COUNT(*),
		       COALESCE(MAX(cc.poster),''), COALESCE(MAX(cc.backdrop),''),
		       COALESCE(MAX(cc.summary),'')
		FROM catalog_titles t
		LEFT JOIN catalog_collections cc ON cc.tmdb_id = t.collection_id
		WHERE t.type='movie' AND t.browsable=1 AND t.collection_id>0
		GROUP BY t.collection_id HAVING COUNT(*)>=2
		ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Collection
	for rows.Next() {
		var id int64
		var name, poster, backdrop, summary string
		var n int
		if err := rows.Scan(&id, &name, &n, &poster, &backdrop, &summary); err != nil {
			return nil, err
		}
		if name == "" {
			name = fmt.Sprintf("Collection %d", id)
		}
		out = append(out, core.Collection{Key: core.CollectionKey(id), Title: name, Summary: summary,
			Kind: "franchise", Type: core.TypeMovie, PosterPath: poster, BackdropPath: backdrop, ChildCount: n})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Mirror) CollectionArt(ctx context.Context, id int64) (core.Collection, error) {
	c := core.Collection{Key: core.CollectionKey(id)}
	err := m.db.QueryRowContext(ctx, `SELECT poster,backdrop,summary FROM catalog_collections WHERE tmdb_id=?`, id).Scan(&c.PosterPath, &c.BackdropPath, &c.Summary)
	return c, err
}
