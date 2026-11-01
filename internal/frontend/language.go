package frontend

import "github.com/braedonsaunders/ether/internal/langtag"

// NormalizeLanguage folds a client language tag to the form Ether stores.
func NormalizeLanguage(tag string) string { return langtag.Normalize(tag) }
