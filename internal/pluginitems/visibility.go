package pluginitems

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/plugins"
)

// RuleFields exposes only rule facts backed by indexed storage.
func RuleFields(decl plugins.MediaKindMeta) map[string]string {
	out := map[string]string{}
	aliases := map[string][]string{"genre": {"genre", "genres", "tags", "tag"}, "studio": {"studio", "studios"}, "country": {"country", "countries"}, "keyword": {"keyword", "keywords"}, "language": {"language"}, "runtime": {"runtime", "duration", "length"}, "votes": {"votes"}, "popularity": {"popularity"}}
	for field, keys := range aliases {
		for _, key := range keys {
			for _, f := range decl.Fields {
				text := f.Type == "string" || f.Type == "strings" || f.Type == "enum" || f.Type == "people"
				numeric := f.Type == "int" || f.Type == "float" || f.Type == "duration"
				wantNumeric := field == "runtime" || field == "votes" || field == "popularity"
				if f.Key == key && f.Indexed && ((wantNumeric && numeric) || (!wantNumeric && text)) {
					out[field] = key
					break
				}
			}
			if out[field] != "" {
				break
			}
		}
	}
	return out
}

func (s *Service) policySQL(ctx context.Context, libraryID core.LibraryID, policy *libraryfilter.Filter) (string, []any, error) {
	if policy.Passive() {
		return "0=1", nil, nil
	}
	rec, err := s.GetKindByLibrary(ctx, libraryID)
	if err != nil {
		return "", nil, err
	}
	typ := core.TypeMovie
	if len(rec.Decl.Levels) > 1 {
		typ = core.TypeShow
	}
	q, args := policy.PluginSQLHidden(typ, RuleFields(rec.Decl))
	return q, args, nil
}

func (s *Service) appendVisibility(ctx context.Context, where *strings.Builder, args *[]any, lib core.LibraryID, policy *libraryfilter.Filter, hidden bool) error {
	if policy.Passive() && !hidden {
		return nil
	}
	q, a, err := s.policySQL(ctx, lib, policy)
	if err != nil {
		return err
	}
	where.WriteString(" AND ")
	if !hidden {
		where.WriteString("NOT ")
	}
	where.WriteString("(" + q + ")")
	*args = append(*args, a...)
	return nil
}

// PreviewVisibility aggregates in the indexed item store and fetches at most
// twelve examples per side. It never projects or loads the whole library.
func (s *Service) PreviewVisibility(ctx context.Context, lib core.LibraryID, specs []core.LibraryExclusion, fallback string) (any, error) {
	policy, err := libraryfilter.CompilePluginPolicy(specs, fallback)
	if err != nil {
		return nil, err
	}
	rec, err := s.GetKindByLibrary(ctx, lib)
	if err != nil {
		return nil, err
	}
	level := ""
	if len(rec.Decl.Levels) > 0 {
		level = rec.Decl.Levels[0].Key
	}
	hidden, err := s.Browse(ctx, BrowseQuery{LibraryID: lib, Level: level, Policy: policy, Hidden: true, Limit: 12})
	if err != nil {
		return nil, err
	}
	shown, err := s.Browse(ctx, BrowseQuery{LibraryID: lib, Level: level, Policy: policy, Limit: 12})
	if err != nil {
		return nil, err
	}
	project := func(items []core.KindItem) []core.Title {
		out := make([]core.Title, 0, len(items))
		for _, it := range items {
			out = append(out, Project(it, rec.Decl, rec.Decl.Present))
		}
		return out
	}
	typ := core.TypeMovie
	if len(rec.Decl.Levels) > 1 {
		typ = core.TypeShow
	}
	counts := make([]map[string]int, 0, len(specs))
	if columns, args := policy.PluginCatchCountsSQL(typ, RuleFields(rec.Decl)); columns != "" {
		args = append(args, int64(lib), level)
		values := make([]int, len(specs)*2)
		dest := make([]any, len(values))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := s.db.QueryRowContext(ctx, "SELECT "+columns+" FROM plugin_items i WHERE i.library_id=? AND i.level=?", args...).Scan(dest...); err != nil {
			return nil, err
		}
		for i := range specs {
			counts = append(counts, map[string]int{"movies": values[i*2], "shows": values[i*2+1]})
		}
	}
	movies, shows := hidden.Total, 0
	if typ == core.TypeShow {
		movies, shows = 0, hidden.Total
	}
	return map[string]any{"total": hidden.Total + shown.Total, "hidden": hidden.Total, "visible": shown.Total, "movies": movies, "shows": shows, "rules": counts, "items": project(hidden.Items), "shown": project(shown.Items)}, nil
}

func LibraryPolicy(ctx context.Context, lib core.Library) (*libraryfilter.Filter, error) {
	if len(lib.Rules) == 0 && lib.VisibilityDefault != "hide" {
		return nil, nil
	}
	f, err := libraryfilter.CompilePluginPolicy(lib.Rules, lib.VisibilityDefault)
	if err == nil && core.ExplicitSearch(ctx) {
		f = f.ForSearch()
	}
	return f, err
}

func (s *Service) ItemHidden(ctx context.Context, lib core.Library, ref core.ItemRef) (bool, error) {
	policy, err := LibraryPolicy(ctx, lib)
	if err != nil {
		return false, err
	}
	if policy.Passive() {
		return false, nil
	}
	q, args, err := s.policySQL(ctx, lib.ID, policy)
	if err != nil {
		return false, err
	}
	args = append(args, int64(lib.ID), string(ref))
	var hidden int
	err = s.db.QueryRowContext(ctx, `SELECT CASE WHEN (`+q+`) THEN 1 ELSE 0 END FROM plugin_items i WHERE i.library_id=? AND i.item_ref=?`, args...).Scan(&hidden)
	return hidden != 0, err
}
