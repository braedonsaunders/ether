package core

import (
	"fmt"
	"strconv"
	"strings"
)

// SearchCategories parses comma-separated Newznab category IDs or a standard
// family name. Empty means the normal movie/TV search behaviour.
func SearchCategories(category string) ([]int, error) {
	category = strings.ToLower(strings.TrimSpace(category))
	if category == "" {
		return nil, nil
	}
	aliases := map[string]string{"console": "1000", "movies": "2000", "movie": "2000", "audio": "3000", "music": "3000", "pc": "4000", "tv": "5000", "adult": "6000", "books": "7000", "comics": "7030", "other": "8000"}
	var out []int
	for _, value := range strings.Split(category, ",") {
		value = strings.TrimSpace(value)
		if alias, ok := aliases[value]; ok {
			value = alias
		}
		n, err := strconv.Atoi(value)
		if err != nil || n < 1000 || n > 8999 {
			return nil, fmt.Errorf("invalid Newznab category %q", value)
		}
		out = append(out, n)
	}
	return out, nil
}
