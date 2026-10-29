// Package libpath defines the shared virtual library names exposed by clients.
package libpath

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode"

	"github.com/braedonsaunders/ether/internal/core"
)

// Safe converts a title to one cross-platform path component. It never returns
// a traversal component; callers must still detect duplicate names per folder.
func Safe(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return ' '
		}
		return r
	}, value)
	value = strings.Trim(strings.TrimSpace(value), ". ")
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return "Untitled"
	}
	base := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || (len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9') {
		value = "_" + value
	}
	return value
}

func MovieName(title core.Title) string {
	name := Safe(title.Title)
	if title.Year > 0 {
		name += fmt.Sprintf(" (%d)", title.Year)
	}
	return name
}
func Movie(title core.Title) string {
	name := MovieName(title)
	return path.Join("Movies", name, name+".mkv")
}
func Show(title core.Title) string { return path.Join("TV", Safe(title.Title)) }
func Season(show string, season int) string {
	return path.Join("TV", Safe(show), fmt.Sprintf("Season %02d", season))
}
func Episode(title core.Title) string {
	show := Safe(title.ShowTitle)
	return path.Join(Season(show, title.Season), fmt.Sprintf("%s - S%02dE%02d.mkv", show, title.Season, title.Episode))
}

// Collection places a safe filename beneath a collection directory.
func Collection(name, filename string) string {
	return path.Join("Collections", Safe(name), Safe(filename))
}

// NamedKey names a sibling using its stable catalogue identity.
type NamedKey struct {
	Name string
	Key  core.RatingKey
}

// Disambiguate returns one stable name per key. Names that collide on
// case-insensitive filesystems acquire their rating key as a suffix.
func Disambiguate(items []NamedKey) map[core.RatingKey]string {
	byKey := make(map[core.RatingKey]string, len(items))
	for _, item := range items {
		if item.Key != 0 {
			byKey[item.Key] = Safe(item.Name)
		}
	}
	keys := make([]core.RatingKey, 0, len(byKey))
	counts := map[string]int{}
	for key, name := range byKey {
		keys = append(keys, key)
		counts[strings.ToLower(name)]++
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	used := map[string]bool{}
	result := make(map[core.RatingKey]string, len(keys))
	// Reserve unique original names before allocating generated suffixes.
	for _, key := range keys {
		name := byKey[key]
		if counts[strings.ToLower(name)] == 1 {
			used[strings.ToLower(name)] = true
			result[key] = name
		}
	}
	for _, key := range keys {
		if _, ok := result[key]; ok {
			continue
		}
		name := byKey[key]
		for {
			name += fmt.Sprintf(" [%d]", key)
			if !used[strings.ToLower(name)] {
				break
			}
		}
		used[strings.ToLower(name)] = true
		result[key] = name
	}
	return result
}
