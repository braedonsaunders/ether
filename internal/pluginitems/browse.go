package pluginitems

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
)

// BrowseQuery specifies a keyset-paged browse over a plugin library and level
// (spec §2.3, §3.4).
type BrowseQuery struct {
	// NoTotal avoids count queries for clients using cursor continuation.
	NoTotal   bool
	Policy    *libraryfilter.Filter
	Hidden    bool
	LibraryID core.LibraryID
	ParentRef core.ItemRef
	Level     string
	Sort      string              // title (default), added, released, year, rating, index
	Desc      bool                // false = default direction for Sort; true = reverse
	Facets    map[string][]string // field key -> required values (OR within key, AND across keys)
	Cursor    string              // opaque keyset cursor
	Limit     int
}

// BrowsePage is a keyset page of plugin items.
type BrowsePage struct {
	Items      []core.KindItem `json:"items"`
	Total      int             `json:"total"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// FacetCount is a single facet value and item count.
type FacetCount struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

type browseCursor struct {
	Sort     string  `json:"s"`
	Desc     bool    `json:"d,omitempty"`
	StrVal   string  `json:"sv,omitempty"`
	IntVal   int64   `json:"iv,omitempty"`
	FloatVal float64 `json:"fv,omitempty"`
	ItemRef  string  `json:"r"`
}

func encodeBrowseCursor(c browseCursor) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeBrowseCursor(raw string) (browseCursor, error) {
	var c browseCursor
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return c, fmt.Errorf("pluginitems: invalid cursor: %w", err)
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("pluginitems: invalid cursor payload: %w", err)
	}
	if c.ItemRef == "" {
		return c, fmt.Errorf("pluginitems: cursor missing item_ref")
	}
	return c, nil
}

// normalizeSortField returns the canonical column name, value kind ("str", "int", "float"),
// and default descending flag for a browse sort name.
func normalizeSortField(sortName string) (col string, kind string, defaultDesc bool) {
	switch strings.ToLower(strings.TrimSpace(sortName)) {
	case "", "title", "sort_title":
		return "sort_title", "str", false
	case "added", "added_at", "date_added":
		return "added_at", "int", true
	case "released", "originally_available_at", "release_date":
		return "released", "int", true
	case "year":
		return "year", "int", true
	case "rating":
		return "rating", "float", true
	case "index", "idx":
		return "idx", "int", false
	default:
		return "sort_title", "str", false
	}
}

// Browse returns a keyset-paged slice of items in a library (spec §2.3, §3.4).
// Unfiltered totals are read from plugin_item_tally in O(1); filtered or
// child-scoped totals execute a bounded COUNT(*) with the same WHERE clauses.
func (s *Service) Browse(ctx context.Context, q BrowseQuery) (BrowsePage, error) {
	if s == nil || s.db == nil {
		return BrowsePage{}, fmt.Errorf("pluginitems: nil service")
	}
	if q.LibraryID <= 0 {
		return BrowsePage{}, fmt.Errorf("pluginitems: library_id is required")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultBrowseLimit
	}
	if limit > MaxBrowseLimit {
		limit = MaxBrowseLimit
	}

	col, kind, defaultDesc := normalizeSortField(q.Sort)
	desc := defaultDesc
	if q.Desc {
		desc = !defaultDesc
	}
	if strings.HasSuffix(strings.ToLower(strings.TrimSpace(q.Sort)), ":desc") {
		col, kind, _ = normalizeSortField(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(q.Sort)), ":desc"))
		desc = true
	} else if strings.HasSuffix(strings.ToLower(strings.TrimSpace(q.Sort)), ":asc") {
		col, kind, _ = normalizeSortField(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(q.Sort)), ":asc"))
		desc = false
	}

	level := strings.TrimSpace(q.Level)
	parentRef := strings.TrimSpace(string(q.ParentRef))
	hasFacets := hasActiveFacets(q.Facets) || !q.Policy.Passive() || q.Hidden

	// Base filter predicate.
	var baseWhere strings.Builder
	baseArgs := []any{int64(q.LibraryID)}
	baseWhere.WriteString(`i.library_id = ?`)
	if parentRef != "" {
		baseWhere.WriteString(` AND i.parent_ref = ?`)
		baseArgs = append(baseArgs, parentRef)
	} else if level != "" {
		baseWhere.WriteString(` AND i.level = ?`)
		baseArgs = append(baseArgs, level)
	} else {
		// Default to root items when neither Level nor ParentRef is specified.
		baseWhere.WriteString(` AND i.parent_ref = ''`)
	}
	appendFacetClauses(&baseWhere, &baseArgs, q.Facets, nil)
	if err := s.appendVisibility(ctx, &baseWhere, &baseArgs, q.LibraryID, q.Policy, q.Hidden); err != nil {
		return BrowsePage{}, err
	}

	// Compute total count.
	var total int
	if q.NoTotal {
		// Count-free request: the page and cursor below are sufficient.
	} else if !hasFacets && parentRef == "" && level != "" {
		_ = s.db.QueryRowContext(ctx,
			`SELECT COALESCE(n, 0) FROM plugin_item_tally WHERE library_id = ? AND level = ?`,
			int64(q.LibraryID), level).Scan(&total)
	} else if !hasFacets && parentRef == "" && level == "" {
		// Root level: look up the kind's root level from plugin_kinds if available,
		// falling back to COUNT(*).
		if k, err := s.GetKindByLibrary(ctx, q.LibraryID); err == nil && len(k.Decl.Levels) > 0 {
			_ = s.db.QueryRowContext(ctx,
				`SELECT COALESCE(n, 0) FROM plugin_item_tally WHERE library_id = ? AND level = ?`,
				int64(q.LibraryID), k.Decl.Levels[0].Key).Scan(&total)
		} else {
			if err := s.db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM plugin_items i WHERE `+baseWhere.String(),
				baseArgs...).Scan(&total); err != nil {
				return BrowsePage{}, err
			}
		}
	} else {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM plugin_items i WHERE `+baseWhere.String(),
			baseArgs...).Scan(&total); err != nil {
			return BrowsePage{}, err
		}
	}

	pageWhere := baseWhere.String()
	pageArgs := append([]any(nil), baseArgs...)

	if strings.TrimSpace(q.Cursor) != "" {
		cur, err := decodeBrowseCursor(q.Cursor)
		if err != nil {
			return BrowsePage{}, err
		}
		var curVal any
		switch kind {
		case "str":
			curVal = cur.StrVal
		case "int":
			curVal = cur.IntVal
		case "float":
			curVal = cur.FloatVal
		}
		if !desc {
			// Range-friendly keyset predicate: i.<col> >= ? AND (i.<col> > ? OR i.item_ref > ?)
			pageWhere += fmt.Sprintf(` AND (i.%s >= ? AND (i.%s > ? OR i.item_ref > ?))`, col, col)
			pageArgs = append(pageArgs, curVal, curVal, cur.ItemRef)
		} else {
			// Range-friendly DESC predicate with ASC item_ref tie-breaker:
			// i.<col> <= ? AND (i.<col> < ? OR i.item_ref > ?)
			pageWhere += fmt.Sprintf(` AND (i.%s <= ? AND (i.%s < ? OR i.item_ref > ?))`, col, col)
			pageArgs = append(pageArgs, curVal, curVal, cur.ItemRef)
		}
	}

	dir := "ASC"
	if desc {
		dir = "DESC"
	}
	orderBy := fmt.Sprintf("i.%s %s, i.item_ref ASC", col, dir)
	pageArgs = append(pageArgs, limit+1)

	querySQL := `SELECT ` + prefixedItemCols("i.") + `, i.added_at
FROM plugin_items i
WHERE ` + pageWhere + `
ORDER BY ` + orderBy + `
LIMIT ?`

	rows, err := s.db.QueryContext(ctx, querySQL, pageArgs...)
	if err != nil {
		return BrowsePage{}, err
	}
	defer rows.Close()

	type fetchedRow struct {
		item    core.KindItem
		addedAt int64
	}
	var fetched []fetchedRow
	for rows.Next() {
		it, addedAt, err := scanItemRowWithAdded(rows)
		if err != nil {
			return BrowsePage{}, err
		}
		fetched = append(fetched, fetchedRow{item: it, addedAt: addedAt})
	}
	if err := rows.Err(); err != nil {
		return BrowsePage{}, err
	}

	var nextCursor string
	if len(fetched) > limit {
		last := fetched[limit-1]
		fetched = fetched[:limit]
		cur := browseCursor{
			Sort:    col,
			Desc:    desc,
			ItemRef: string(last.item.Ref),
		}
		switch col {
		case "sort_title":
			cur.StrVal = last.item.SortTitle
		case "added_at":
			cur.IntVal = last.addedAt
		case "released":
			cur.IntVal = last.item.Released
		case "year":
			cur.IntVal = int64(last.item.Year)
		case "idx":
			cur.IntVal = int64(last.item.Index)
		case "rating":
			cur.FloatVal = last.item.Rating
		}
		nextCursor = encodeBrowseCursor(cur)
	}

	items := make([]core.KindItem, len(fetched))
	for i := range fetched {
		items[i] = fetched[i].item
	}
	if len(items) > 0 {
		if err := s.hydrateArtSlice(ctx, items); err != nil {
			return BrowsePage{}, err
		}
	}

	if q.NoTotal {
		total = len(items)
	}
	return BrowsePage{
		Items:      items,
		Total:      total,
		NextCursor: nextCursor,
	}, nil
}

func scanItemRowWithAdded(sc rowScanner) (core.KindItem, int64, error) {
	var (
		it             core.KindItem
		refStr         string
		seq            int64
		pluginID       string
		kindKey        string
		libID          int64
		parentRef      string
		idx            int64
		year           int64
		playable       int
		childCount     int64
		leafCount      int64
		hintsRaw       string
		fieldsRaw      string
		unavailableRaw string
		addedAt        int64
	)
	if err := sc.Scan(
		&refStr, &seq, &pluginID, &kindKey, &libID, &it.ID, &parentRef, &it.Level, &idx,
		&it.Title, &it.SortTitle, &it.Summary, &year, &it.Released, &it.Rating,
		&playable, &childCount, &leafCount, &hintsRaw, &fieldsRaw, &it.ETag, &unavailableRaw, &addedAt,
	); err != nil {
		return core.KindItem{}, 0, err
	}
	it.Ref = core.ItemRef(refStr)
	it.Key = core.PluginKey(seq)
	if parentRef != "" {
		it.ParentID = core.ItemRef(parentRef).External()
	}
	it.Index = int(idx)
	it.Year = int(year)
	if unavailableRaw != "" {
		if err := json.Unmarshal([]byte(unavailableRaw), &it.Unavailable); err != nil {
			return core.KindItem{}, 0, err
		}
	}
	it.Playable = playable != 0 && it.Unavailable == nil
	it.ChildCount = int(childCount)
	it.LeafCount = int(leafCount)
	if hintsRaw != "" && hintsRaw != "{}" {
		it.Hints = json.RawMessage(hintsRaw)
	}
	if fieldsRaw != "" && fieldsRaw != "{}" {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(fieldsRaw), &m); err == nil && len(m) > 0 {
			it.Fields = m
		}
	}
	return it, addedAt, nil
}

// Facets returns the distinct values and counts for a declared facetable field
// within a library (optionally scoped to a level).
func (s *Service) Facets(ctx context.Context, libraryID core.LibraryID, level, fieldKey string, limit int) ([]FacetCount, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("pluginitems: nil service")
	}
	fieldKey = strings.TrimSpace(fieldKey)
	if libraryID <= 0 || fieldKey == "" {
		return nil, fmt.Errorf("pluginitems: library_id and field_key are required")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}

	var (
		query string
		args  []any
	)
	if strings.TrimSpace(level) == "" {
		query = `
			SELECT f.v_text, COUNT(*) AS cnt
			FROM plugin_item_fields f
			JOIN plugin_items i ON i.item_ref = f.item_ref
			WHERE i.library_id = ? AND f.field = ? AND f.v_text <> ''
			GROUP BY f.v_text
			ORDER BY cnt DESC, f.v_text ASC
			LIMIT ?`
		args = []any{int64(libraryID), fieldKey, limit}
	} else {
		query = `
			SELECT f.v_text, COUNT(*) AS cnt
			FROM plugin_item_fields f
			JOIN plugin_items i ON i.item_ref = f.item_ref
			WHERE i.library_id = ? AND i.level = ? AND f.field = ? AND f.v_text <> ''
			GROUP BY f.v_text
			ORDER BY cnt DESC, f.v_text ASC
			LIMIT ?`
		args = []any{int64(libraryID), strings.TrimSpace(level), fieldKey, limit}
	}

	if envelopeFacet(fieldKey) {
		query = `SELECT CAST(i.` + fieldKey + ` AS TEXT), COUNT(*) AS cnt FROM plugin_items i WHERE i.library_id = ? AND i.` + fieldKey + ` > 0`
		args = []any{int64(libraryID)}
		if strings.TrimSpace(level) != "" {
			query += ` AND i.level = ?`
			args = append(args, strings.TrimSpace(level))
		}
		query += ` GROUP BY i.` + fieldKey + ` ORDER BY cnt DESC, i.` + fieldKey + ` ASC LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FacetCount
	for rows.Next() {
		var fc FacetCount
		if err := rows.Scan(&fc.Value, &fc.Count); err != nil {
			return nil, err
		}
		out = append(out, fc)
	}
	return out, rows.Err()
}

func envelopeFacet(key string) bool {
	return key == "year" || key == "released" || key == "rating"
}

func hasActiveFacets(facets map[string][]string) bool {
	for k, vals := range facets {
		if strings.TrimSpace(k) == "" {
			continue
		}
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return true
			}
		}
	}
	return false
}

// appendFacetClauses appends ` AND EXISTS (...)` subqueries on plugin_item_fields
// for each facet key to where.
func appendFacetClauses(where *strings.Builder, args *[]any, facets map[string][]string, _ map[string]string) {
	if len(facets) == 0 {
		return
	}
	keys := make([]string, 0, len(facets))
	for k := range facets {
		if strings.TrimSpace(k) != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, key := range keys {
		rawVals := facets[key]
		var cleanVals []string
		for _, v := range rawVals {
			v = strings.TrimSpace(v)
			if v != "" {
				cleanVals = append(cleanVals, v)
			}
		}
		if len(cleanVals) == 0 {
			continue
		}

		if envelopeFacet(key) {
			if len(cleanVals) == 1 && strings.Contains(cleanVals[0], "..") {
				parts := strings.SplitN(cleanVals[0], "..", 2)
				lo, e1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
				hi, e2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
				if e1 == nil && e2 == nil {
					where.WriteString(` AND i.` + key + ` BETWEEN ? AND ?`)
					*args = append(*args, lo, hi)
					continue
				}
			}
			ph := make([]string, len(cleanVals))
			for n, value := range cleanVals {
				ph[n] = "?"
				number, err := strconv.ParseFloat(value, 64)
				if err != nil {
					where.WriteString(` AND 0=1`)
					number = 0
				}
				*args = append(*args, number)
			}
			where.WriteString(` AND i.` + key + ` IN (` + strings.Join(ph, ",") + `)`)
			continue
		}

		// Support integer range filters encoded as "min..max".
		if len(cleanVals) == 1 && strings.Contains(cleanVals[0], "..") {
			parts := strings.SplitN(cleanVals[0], "..", 2)
			minVal, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64)
			maxVal, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
			if err1 == nil && err2 == nil {
				where.WriteString(` AND EXISTS (SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND f.field = ? AND f.v_int BETWEEN ? AND ?)`)
				*args = append(*args, key, minVal, maxVal)
				continue
			}
		}

		placeholders := make([]string, len(cleanVals))
		*args = append(*args, key)
		for idx, v := range cleanVals {
			placeholders[idx] = "?"
			*args = append(*args, v)
		}
		where.WriteString(` AND EXISTS (SELECT 1 FROM plugin_item_fields f WHERE f.item_ref = i.item_ref AND f.field = ? AND f.v_text IN (` + strings.Join(placeholders, ", ") + `))`)
	}
}
