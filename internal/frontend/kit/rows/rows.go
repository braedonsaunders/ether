// Package rows supplies helpers for rendering supported neutral row entries.
package rows

import "github.com/braedonsaunders/ether/internal/frontend"

// Supported returns entries in their original order when their kind is handled.
// Unknown or unsupported kinds are skipped so newer servers remain compatible
// with older front ends.
func Supported(entries []frontend.RowEntry, kinds map[frontend.RowEntryKind]bool) []frontend.RowEntry {
	out := make([]frontend.RowEntry, 0, len(entries))
	for _, entry := range entries {
		if kinds[entry.Kind] {
			out = append(out, entry)
		}
	}
	return out
}
