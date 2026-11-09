package store

import (
	"regexp"
	"strings"
	"sync"
)

// A logical backup restores into a server whose schema may have moved on:
// migrations registered after the backup dropped tables and columns or
// renamed columns. schemaHistory is what every registered migration did,
// read from its SQL (SQLite and PostgreSQL texts), so ImportLogical can
// skip a dumped table or column a migration dropped and load a renamed
// column under its new name, instead of refusing the backup. Nothing has
// to be registered by hand: a new DROP or RENAME is covered the day it is
// written. Migrations written in Go (RegisterApply) and SQLite table
// rebuilds that copy into a new table are not visible here; one that drops
// a column that way needs RegisterRetiredColumn.
type schemaHistory struct {
	tables  map[string]bool
	columns map[string]map[string]bool
	// renamed is table -> old column -> new column.
	renamed map[string]map[string]string
}

var (
	reDropTable  = regexp.MustCompile(`(?i)\bDROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?([A-Za-z_][A-Za-z0-9_]*)`)
	reDropColumn = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?([A-Za-z_][A-Za-z0-9_]*)"?\s+DROP\s+COLUMN\s+(?:IF\s+EXISTS\s+)?"?([A-Za-z_][A-Za-z0-9_]*)`)
	reRename     = regexp.MustCompile(`(?i)\bALTER\s+TABLE\s+(?:IF\s+EXISTS\s+)?"?([A-Za-z_][A-Za-z0-9_]*)"?\s+RENAME\s+COLUMN\s+"?([A-Za-z_][A-Za-z0-9_]*)"?\s+TO\s+"?([A-Za-z_][A-Za-z0-9_]*)`)

	retiredMu      sync.Mutex
	retiredTables  = map[string]bool{}
	retiredColumns = map[string]map[string]bool{}
)

// RegisterRetiredTable records a table dropped or dialect-only in a way the
// SQL scan cannot see, allowing logical restore to skip it safely if missing on
// the restore target.
func RegisterRetiredTable(table string) {
	retiredMu.Lock()
	defer retiredMu.Unlock()
	retiredTables[strings.ToLower(table)] = true
}

// RegisterRetiredColumn records a column a migration drops in a way the
// SQL scan cannot see (a Go migration or a table rebuild).
func RegisterRetiredColumn(table, column string) {
	retiredMu.Lock()
	defer retiredMu.Unlock()
	if retiredColumns[table] == nil {
		retiredColumns[table] = map[string]bool{}
	}
	retiredColumns[table][column] = true
}

func migrationHistory() schemaHistory {
	h := schemaHistory{tables: map[string]bool{}, columns: map[string]map[string]bool{}, renamed: map[string]map[string]string{}}
	drop := func(table, column string) {
		table, column = strings.ToLower(table), strings.ToLower(column)
		if h.columns[table] == nil {
			h.columns[table] = map[string]bool{}
		}
		h.columns[table][column] = true
	}
	for _, m := range registered() {
		texts := []string{m.sql}
		for _, o := range m.overrides {
			texts = append(texts, o)
		}
		for _, text := range texts {
			for _, g := range reDropTable.FindAllStringSubmatch(text, -1) {
				h.tables[strings.ToLower(g[1])] = true
			}
			for _, g := range reDropColumn.FindAllStringSubmatch(text, -1) {
				drop(g[1], g[2])
			}
			for _, g := range reRename.FindAllStringSubmatch(text, -1) {
				t := strings.ToLower(g[1])
				if h.renamed[t] == nil {
					h.renamed[t] = map[string]string{}
				}
				h.renamed[t][strings.ToLower(g[2])] = strings.ToLower(g[3])
			}
		}
	}
	retiredMu.Lock()
	for t := range retiredTables {
		h.tables[t] = true
	}
	for t, cs := range retiredColumns {
		for c := range cs {
			drop(t, c)
		}
	}
	retiredMu.Unlock()
	return h
}

// column maps a dumped column of table onto the server's columns: its own
// name, the name a rename gave it, or "" when a migration dropped it. ok is
// false for a column the server never had.
func (h schemaHistory) column(table, col string, server map[string]bool) (name string, ok bool) {
	t, c := strings.ToLower(table), strings.ToLower(col)
	if server[c] {
		return c, true
	}
	seen := map[string]bool{c: true}
	for n, ok := h.renamed[t][c]; ok && !seen[n]; n, ok = h.renamed[t][n] {
		if server[n] {
			return n, true
		}
		seen[n] = true
	}
	if h.columns[t][c] {
		return "", true
	}
	return "", false
}
