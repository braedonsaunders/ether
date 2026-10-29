// Package language exposes the shared language-tag validation to frontends.
package language

import "github.com/braedonsaunders/ether/internal/langtag"

// Valid reports whether value is an accepted BCP 47 language tag.
func Valid(value string) bool {
	_, ok := langtag.ParseBCP47(value)
	return ok
}
