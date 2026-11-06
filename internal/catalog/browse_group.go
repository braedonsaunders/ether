package catalog

import (
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"strings"
)

func (c *Catalog) compileBrowseGroup(g *core.FilterGroup, alias string, kind core.KeyKind) (string, []any, error) {
	if g.ParentOnly && kind != 0 {
		copy := *g
		copy.ParentOnly = false
		return c.compileBrowseGroup(&copy, "t", 0)
	}
	if g.StateField != "" {
		if g.StateField == "viewedLeafCount" || g.StateField == "viewedOrInProgressLeafCount" || g.StateField == "unviewedLeafCount" || g.StateField == "unwatchedLeaves" {
			op := ""
			switch g.StateOperator {
			case "=", "==":
				op = "="
			case "!=", "!==":
				op = "<>"
			case "<<", "<":
				op = "<"
			case "<<=", "<=":
				op = "<="
			case ">>", ">":
				op = ">"
			case ">>=", ">=":
				op = ">="
			default:
				return "", nil, fmt.Errorf("invalid leaf operator")
			}
			predicate := "COALESCE(w.view_count,0)>0"
			if g.StateField == "viewedOrInProgressLeafCount" {
				predicate = "(COALESCE(w.view_count,0)>0 OR COALESCE(w.view_offset_ns,0)>0)"
			}
			if g.StateField == "unviewedLeafCount" || g.StateField == "unwatchedLeaves" {
				predicate = "COALESCE(w.view_count,0)=0"
			}
			seasonClause := " AND (" + alias + ".type IN ('show','episode') OR e.season=" + alias + ".season)"
			if g.StateField == "unwatchedLeaves" {
				seasonClause = ""
			}
			col := "(SELECT COUNT(*) FROM catalog_titles e LEFT JOIN state_watch w ON w.rating_key=e.key AND w.user_id=? WHERE e.show_key=CASE WHEN " + alias + ".type='show' THEN " + alias + ".key ELSE " + alias + ".show_key END AND e.type='episode' AND e.season>0" + seasonClause + " AND e.adult=0 AND e.release_known=1 AND e.released_ts<=? AND " + predicate + ")"
			value := g.StateValue
			if g.StateField == "unwatchedLeaves" {
				if value == 1 {
					op = ">"
				} else {
					op = "="
				}
				value = 0
			}
			return col + op + "?", []any{g.UserID, c.now().Unix(), value}, nil
		}
		column := ""
		scale := int64(1)
		switch g.StateField {
		case "viewCount":
			column = "w.view_count"
		case "skipCount":
			column = "0"
		case "viewOffset":
			column = "w.view_offset_ns"
			scale = 1000000
		case "lastViewedAt":
			column = "w.last_viewed_at/1000000000"
		case "lastRatedAt":
			column = "w.rating_at/1000000000"
		case "lastSkippedAt":
			column = "0"
		default:
			return "", nil, fmt.Errorf("invalid state field")
		}
		op := ""
		switch g.StateOperator {
		case "=", "==":
			op = "="
		case "!=", "!==":
			op = "<>"
		case "<<", "<":
			op = "<"
		case "<<=", "<=":
			op = "<="
		case ">>", ">":
			op = ">"
		case ">>=", ">=":
			op = ">="
		default:
			return "", nil, fmt.Errorf("invalid state operator")
		}
		if g.StateField == "viewOffset" || g.StateField == "lastViewedAt" || g.StateField == "lastRatedAt" || g.StateField == "lastSkippedAt" {
			return "COALESCE((SELECT " + column + " FROM state_watch w WHERE w.user_id=? AND w.rating_key=" + alias + ".key),0)" + op + "?", []any{g.UserID, g.StateValue * scale}, nil
		}
		return "EXISTS (SELECT 1 FROM state_watch w WHERE w.user_id=? AND w.rating_key=" + alias + ".key AND " + column + op + "?)", []any{g.UserID, g.StateValue}, nil
	}
	if g.Unwatched != nil || g.InProgress != nil || g.LastViewedAfter > 0 || g.LastViewedBefore > 0 {
		var parts []string
		var args []any
		state := func(column string, want bool) {
			if g.UserID == 0 {
				if want {
					parts = append(parts, "1=0")
				} else {
					parts = append(parts, "1=1")
				}
				return
			}
			predicate := "EXISTS (SELECT 1 FROM state_watch w WHERE w.user_id=? AND w.rating_key=" + alias + ".key AND w." + column + ">0)"
			if !want {
				predicate = "NOT " + predicate
			}
			parts = append(parts, predicate)
			args = append(args, g.UserID)
		}
		if g.Unwatched != nil {
			state("view_count", !*g.Unwatched)
		}
		if g.InProgress != nil {
			state("view_offset_ns", *g.InProgress)
		}
		if g.LastViewedAfter > 0 || g.LastViewedBefore > 0 {
			if g.UserID == 0 {
				parts = append(parts, "1=0")
			} else {
				p := "EXISTS (SELECT 1 FROM state_watch w WHERE w.user_id=? AND w.rating_key=" + alias + ".key AND w.last_viewed_at>0"
				args = append(args, g.UserID)
				// state_watch timestamps are nanoseconds; Plex filter bounds
				// and unixOrZero use whole seconds.
				const maxSeconds = int64(9223372036)
				if g.LastViewedAfter > 0 {
					if g.LastViewedAfter > maxSeconds {
						p += " AND 1=0"
					} else {
						p += " AND w.last_viewed_at>=?"
						args = append(args, g.LastViewedAfter*1_000_000_000)
					}
				}
				if g.LastViewedBefore > 0 && g.LastViewedBefore < maxSeconds {
					p += " AND w.last_viewed_at<?"
					args = append(args, (g.LastViewedBefore+1)*1_000_000_000)
				}
				parts = append(parts, p+")")
			}
		}
		return strings.Join(parts, " AND "), args, nil
	}
	if g.Filter != nil {
		f := *g.Filter
		if kind == 0 {
			return c.browseWhere(f)
		}
		show, excluded := splitStoredFilter(f, kind)
		sql, args, err := c.browseWhere(show)
		if err != nil {
			return "", nil, err
		}
		child, vals := c.childFilterConds(alias, f, excluded)
		if len(child) > 0 {
			sql += " AND " + strings.Join(child, " AND ")
			args = append(args, vals...)
		}
		return sql, args, nil
	}
	if g.Operator != "and" && g.Operator != "or" {
		return "", nil, fmt.Errorf("catalog: invalid filter group operator %q", g.Operator)
	}
	parts := make([]string, 0, len(g.Children))
	var args []any
	for i := range g.Children {
		sql, vals, err := c.compileBrowseGroup(&g.Children[i], alias, kind)
		if err != nil {
			return "", nil, err
		}
		parts = append(parts, "("+sql+")")
		args = append(args, vals...)
	}
	if len(parts) == 0 {
		return "1=1", nil, nil
	}
	op := " AND "
	if g.Operator == "or" {
		op = " OR "
	}
	return strings.Join(parts, op), args, nil
}
