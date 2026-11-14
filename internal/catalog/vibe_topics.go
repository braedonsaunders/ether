package catalog

import (
	"strings"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// topicMatch uses existing indexes only. Each keyword range yields at most
// 256 keys, and overview matching reads at most 512 popular movies and 512
// shows, by indexed key lookup. Sparse topics never walk the library, build
// an index, hydrate remote titles, or start background catalogue work. This
// deliberately trades long-tail overview recall for a fixed request budget.
func (c *Catalog) topicMatch(topic string) (string, []any) {
	topic = strings.ToLower(strings.Join(strings.Fields(topic), " "))
	if len(topic) == 0 || len(topic) > 300 {
		return "1=0", nil
	}
	words := strings.FieldsFunc(topic, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	if len(words) == 0 || len(words) > 12 {
		return "1=0", nil
	}
	phrases := []string{topic}
	switch topic {
	case "space exploration":
		phrases = append(phrases, "space travel", "space mission", "astronaut")
	case "time travel":
		phrases = append(phrases, "time machine", "time loop")
	case "heist":
		phrases = append(phrases, "bank robbery", "robbery")
	}
	var parts []string
	var args []any
	for _, phrase := range phrases {
		// LOWER(keyword),key is already indexed. A word-boundary prefix catches
		// "heist film" without accidentally matching "heisting".
		parts = append(parts, `SELECT key FROM (SELECT key FROM catalog_keywords WHERE LOWER(keyword)=? LIMIT 256)`,
			`SELECT key FROM (SELECT key FROM catalog_keywords WHERE LOWER(keyword)>=? AND LOWER(keyword)<? LIMIT 256)`)
		args = append(args, phrase, phrase+" ", phrase+"!")
	}
	var conditions []string
	for _, word := range words {
		conditions = append(conditions, `LOWER(SUBSTR(summary,1,4096)) LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(word)+"%")
	}
	var popular []string
	for _, typ := range []string{"movie", "show"} {
		if !c.rankReady.Load() && c.titleIndexesSwapped.Load() {
			continue
		}
		q := `SELECT key FROM catalog_titles WHERE type='` + typ + `' AND browsable=1 ORDER BY popularity DESC LIMIT 512`
		if c.rankReady.Load() {
			q = `SELECT key FROM catalog_rank WHERE type='` + typ + `' AND browsable=1 AND adult=0 ORDER BY rk,key LIMIT 512`
		}
		popular = append(popular, `SELECT key FROM (`+q+`)`)
	}
	if len(popular) > 0 {
		parts = append(parts, `SELECT key FROM catalog_titles WHERE key IN (`+strings.Join(popular, " UNION ")+`) AND `+strings.Join(conditions, " AND "))
	} else {
		args = args[:len(args)-len(words)]
	}
	return `t.key IN (` + strings.Join(parts, " UNION ") + `)`, args
}

// SQLite otherwise sometimes walks the entire type/popularity index to avoid
// sorting a bounded set. NOT INDEXED still permits the integer primary key.
func (c *Catalog) topicIndexHint(f core.Filter) string {
	if store.DialectOf(c.db) != store.Postgres {
		for _, leaf := range mandatoryConsoleFilters(f) {
			if leaf.Topic != "" {
				return " NOT INDEXED"
			}
		}
	}
	return ""
}
