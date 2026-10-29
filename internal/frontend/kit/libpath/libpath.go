// Package libpath defines the shared virtual library names exposed by clients.
package libpath

import (
	"fmt"
	"path"
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
