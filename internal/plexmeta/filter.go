package plexmeta

// RatingSQL is the catalogue SQL helper for Plex viewer limits.
// Catalog calls HideExceeding instead of naming plexmeta_titles itself.
type RatingSQL struct{}

// HideExceeding is true unless a cached overlay for keyColumn exceeds the
// bound level AND that overlay's guid still matches the verified
// plex:// mapping. A remapped title is ignored so a stale adult rating
// cannot hide it.
func (RatingSQL) HideExceeding(keyColumn string) string {
	return `NOT EXISTS (SELECT 1 FROM plexmeta_titles p WHERE p.rating_key=` + keyColumn + ` AND p.rating_level>? AND EXISTS (SELECT 1 FROM plexguid_mappings g WHERE g.rating_key=p.rating_key AND g.guid=p.guid))`
}
