package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// auditEntry is one distinct SQL statement seen on a request path.
type auditEntry struct {
	SQL       string
	Sites     map[string]bool
	Steps     map[string]int64
	Count     int64
	Rows      int64
	Time      time.Duration
	MaxPerReq int64 // most executions inside one request (N+1)
	Plan      []string
	Issues    []string
	Score     float64
}

var (
	scanRe     = regexp.MustCompile(`^SCAN (\S+)(?: AS \S+)?(.*)$`)
	tempBTree  = regexp.MustCompile(`USE TEMP B-TREE FOR (.*)`)
	autoIndex  = regexp.MustCompile(`AUTOMATIC (PARTIAL )?COVERING INDEX`)
	correlated = regexp.MustCompile(`CORRELATED (SCALAR|LIST) SUBQUERY`)
)

// audit runs EXPLAIN QUERY PLAN for every statement the trace attributes to
// a request and ranks them by the time they cost, flagging full scans of
// large tables, temporary sort trees, automatic indexes, correlated
// subqueries and statements repeated within one request.
func audit(ctx context.Context, dbPath string, trace map[string]traceRecord, w io.Writer) error {
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer db.Close()
	sizes := map[string]int64{}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			tables = append(tables, n)
		}
	}
	rows.Close()
	for _, t := range tables {
		var n int64
		if db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+t+`"`).Scan(&n) == nil {
			sizes[t] = n
		}
	}
	entries := map[string]*auditEntry{}
	for tag, rec := range trace {
		parts := strings.SplitN(tag, "|", 4)
		if len(parts) < 3 {
			continue
		}
		label := parts[1] + " " + parts[2]
		for _, st := range rec.Statements {
			e := entries[st.SQL]
			if e == nil {
				e = &auditEntry{SQL: st.SQL, Sites: map[string]bool{}, Steps: map[string]int64{}}
				entries[st.SQL] = e
			}
			if st.Site != "" {
				e.Sites[st.Site] = true
			}
			e.Steps[label] += int64(st.Time)
			e.Count += st.Count
			e.Rows += st.Rows
			e.Time += st.Time
			e.MaxPerReq = max(e.MaxPerReq, st.Count)
		}
	}
	list := make([]*auditEntry, 0, len(entries))
	for _, e := range entries {
		e.Plan, e.Issues = explain(ctx, db, e.SQL, sizes)
		if e.MaxPerReq >= 20 {
			e.Issues = append(e.Issues, fmt.Sprintf("N+1: %d executions in one request", e.MaxPerReq))
		}
		if e.Count > 0 && e.Rows/e.Count >= 2000 {
			e.Issues = append(e.Issues, fmt.Sprintf("reads %d rows per execution", e.Rows/e.Count))
		}
		weight := 1.0
		if len(e.Issues) > 0 {
			weight = 2
		}
		e.Score = float64(e.Time) * weight
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Score > list[j].Score })
	fmt.Fprintf(w, "# Query plan audit\n\n%d distinct statements reached from request handlers, ranked by request time spent in them (doubled when flagged).\n\n", len(list))
	fmt.Fprintln(w, "| # | total | count | max/request | rows/exec | issues | site | top steps | statement |")
	fmt.Fprintln(w, "|---:|---:|---:|---:|---:|---|---|---|---|")
	for i, e := range list {
		if i == 150 {
			break
		}
		fmt.Fprintf(w, "| %d | %s | %d | %d | %d | %s | %s | %s | `%s` |\n", i+1, ms(e.Time), e.Count, e.MaxPerReq, e.Rows/max(e.Count, 1),
			strings.Join(e.Issues, "; "), strings.Join(keys(e.Sites, 3), " "), strings.Join(topSteps(e.Steps, 3), "; "), clip(e.SQL, 220))
	}
	fmt.Fprintln(w, "\n## Plans of flagged statements")
	n := 0
	for _, e := range list {
		if len(e.Issues) == 0 {
			continue
		}
		n++
		if n > 60 {
			break
		}
		fmt.Fprintf(w, "\n### %d. %s — %s\n\nsites: %s\n\n```sql\n%s\n```\n\n```\n%s\n```\n", n, ms(e.Time), strings.Join(e.Issues, "; "), strings.Join(keys(e.Sites, 10), ", "), clip(e.SQL, 2000), strings.Join(e.Plan, "\n"))
	}
	return nil
}

func explain(ctx context.Context, db *sql.DB, query string, sizes map[string]int64) ([]string, []string) {
	q := strings.TrimSpace(query)
	upper := strings.ToUpper(q)
	if !(strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH") || strings.HasPrefix(upper, "UPDATE") || strings.HasPrefix(upper, "DELETE") || strings.HasPrefix(upper, "INSERT")) {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q)
	if err != nil {
		return []string{"explain failed: " + err.Error()}, nil
	}
	defer rows.Close()
	var plan, issues []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			issues = append(issues, s)
		}
	}
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if rows.Scan(&id, &parent, &notused, &detail) != nil {
			continue
		}
		plan = append(plan, detail)
		if m := scanRe.FindStringSubmatch(detail); m != nil {
			table, rest := m[1], m[2]
			n := sizes[table]
			switch {
			case strings.Contains(rest, "COVERING INDEX") && n >= 10000:
				add(fmt.Sprintf("full covering-index scan of %s (%d rows)", table, n))
			case strings.Contains(rest, "USING INDEX") && n >= 10000:
				add(fmt.Sprintf("full index scan of %s (%d rows)", table, n))
			case !strings.Contains(rest, "INDEX") && n >= 2000:
				add(fmt.Sprintf("full table scan of %s (%d rows)", table, n))
			}
		}
		if m := tempBTree.FindStringSubmatch(detail); m != nil {
			add("temp b-tree for " + strings.ToLower(m[1]))
		}
		if autoIndex.MatchString(detail) {
			add("automatic index: " + detail)
		}
		if correlated.MatchString(detail) {
			add(strings.ToLower(detail))
		}
	}
	return plan, issues
}

func keys(m map[string]bool, n int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func topSteps(m map[string]int64, n int) []string {
	type kv struct {
		k string
		v int64
	}
	var s []kv
	for k, v := range m {
		s = append(s, kv{k, v})
	}
	sort.Slice(s, func(i, j int) bool { return s[i].v > s[j].v })
	var out []string
	for i, x := range s {
		if i == n {
			break
		}
		out = append(out, x.k)
	}
	return out
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "|", "¦")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
