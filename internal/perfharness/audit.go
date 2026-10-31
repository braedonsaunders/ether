package perfharness

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/store"
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
	searchRe   = regexp.MustCompile(`^SEARCH (\S+)(?: AS \S+)? USING (?:COVERING )?INDEX (\S+) \((.*)\)`)
)

// indexStats reads sqlite_stat1: for each index, the estimated rows per
// distinct value of its leading 1, 2, ... columns.
func indexStats(ctx context.Context, db *sql.DB) map[string][]int64 {
	out := map[string][]int64{}
	rows, err := db.QueryContext(ctx, `SELECT idx, stat FROM sqlite_stat1 WHERE idx IS NOT NULL`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var idx, stat string
		if rows.Scan(&idx, &stat) != nil {
			continue
		}
		var nums []int64
		for _, f := range strings.Fields(stat) {
			v, err := strconv.ParseInt(f, 10, 64)
			if err != nil {
				break
			}
			nums = append(nums, v)
		}
		out[idx] = nums
	}
	return out
}

// audit runs EXPLAIN QUERY PLAN for every statement the trace attributes to
// a request and ranks them by the time they cost, flagging full scans of
// large tables, temporary sort trees, automatic indexes, correlated
// subqueries and statements repeated within one request.
func Audit(ctx context.Context, dbPath string, trace map[string]TraceRecord, w io.Writer) error {
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
	stats := indexStats(ctx, db)
	return auditWith(trace, w, func(q string) ([]string, []string) { return explain(ctx, db, q, sizes, stats) })
}

// AuditPostgres audits the same statements against a PostgreSQL database
// (a cluster node's, or a copy of one) with EXPLAIN (GENERIC_PLAN), which
// plans a parameterized statement without values (PostgreSQL 16+). It flags
// sequential scans of large tables, sorts and correlated subplans.
func AuditPostgres(ctx context.Context, url string, trace map[string]TraceRecord, w io.Writer) error {
	db, err := sql.Open(store.PostgresDriverName, url)
	if err != nil {
		return err
	}
	defer db.Close()
	sizes := map[string]int64{}
	rows, err := db.QueryContext(ctx, `SELECT relname, GREATEST(reltuples, 0)::bigint FROM pg_class WHERE relkind='r' AND relnamespace=current_schema()::regnamespace`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		var n int64
		if rows.Scan(&name, &n) == nil {
			sizes[name] = n
		}
	}
	rows.Close()
	return auditWith(trace, w, func(q string) ([]string, []string) { return explainPostgres(ctx, db, q, sizes) })
}

var (
	pgSeqScan = regexp.MustCompile(`Seq Scan on (\S+)`)
	pgSort    = regexp.MustCompile(`^\s*(?:->\s+)?(?:Incremental )?Sort `)
	pgSubPlan = regexp.MustCompile(`SubPlan \d+`)
)

func explainPostgres(ctx context.Context, db *sql.DB, query string, sizes map[string]int64) ([]string, []string) {
	q := strings.TrimSpace(query)
	upper := strings.ToUpper(q)
	if !(strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH") || strings.HasPrefix(upper, "UPDATE") || strings.HasPrefix(upper, "DELETE") || strings.HasPrefix(upper, "INSERT")) {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, "EXPLAIN (GENERIC_PLAN) "+q)
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
		var line string
		if rows.Scan(&line) != nil {
			continue
		}
		plan = append(plan, line)
		if m := pgSeqScan.FindStringSubmatch(line); m != nil && sizes[m[1]] >= 2000 {
			add(fmt.Sprintf("sequential scan of %s (%d rows)", m[1], sizes[m[1]]))
		}
		if pgSort.MatchString(line) {
			add("sort")
		}
		if pgSubPlan.MatchString(line) {
			add("correlated subplan")
		}
	}
	return plan, issues
}

func auditWith(trace map[string]TraceRecord, w io.Writer, explainOne func(string) ([]string, []string)) error {
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
		e.Plan, e.Issues = explainOne(e.SQL)
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

func explain(ctx context.Context, db *sql.DB, query string, sizes map[string]int64, stats map[string][]int64) ([]string, []string) {
	q := strings.TrimSpace(query)
	upper := strings.ToUpper(q)
	if !(strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH") || strings.HasPrefix(upper, "UPDATE") || strings.HasPrefix(upper, "DELETE") || strings.HasPrefix(upper, "INSERT")) {
		return nil, nil
	}
	// Parameters are bound to NULL; the plan does not depend on them.
	rows, err := db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q, make([]any, placeholders(q))...)
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
		if m := searchRe.FindStringSubmatch(detail); m != nil {
			// An equality prefix that still selects thousands of rows is a
			// scan in disguise (type IN ('movie','show') on 500k rows).
			eq := strings.Count(m[3], "=?") - strings.Count(m[3], "<=?") - strings.Count(m[3], ">=?")
			if st := stats[m[2]]; eq >= 1 && eq < len(st) && st[eq] >= 2000 && !strings.Contains(m[3], "rowid=") {
				add(fmt.Sprintf("index %s selects ~%d rows per probe", m[2], st[eq]))
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

// placeholders counts a statement's parameters: ? and ?NNN outside string
// literals, quoted identifiers and comments.
func placeholders(q string) int {
	n, numbered := 0, 0
	for i := 0; i < len(q); i++ {
		switch c := q[i]; c {
		case '\'', '"', '`':
			for i++; i < len(q) && q[i] != c; i++ {
			}
		case '-':
			if i+1 < len(q) && q[i+1] == '-' {
				for i < len(q) && q[i] != '\n' {
					i++
				}
			}
		case '?':
			j := i + 1
			for j < len(q) && q[j] >= '0' && q[j] <= '9' {
				j++
			}
			if j > i+1 {
				v := 0
				for _, d := range q[i+1 : j] {
					v = v*10 + int(d-'0')
				}
				numbered = max(numbered, v)
				i = j - 1
			} else {
				n++
			}
		}
	}
	return max(n, numbered)
}
