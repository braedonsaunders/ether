package model

import (
	"reflect"
	"sort"
)

// AttrSorter is implemented by elements whose attributes PMS emits in
// alphabetical order: MediaContainer always (after size/totalSize/offset),
// and items inside transcode decision responses (with the decision
// attributes appended at the end).
type AttrSorter interface {
	AttrOrder() (sorted bool, leading, trailing []string)
}

// AttrOrder implements AttrSorter.
func (MediaContainer) AttrOrder() (bool, []string, []string) {
	return true, []string{"size", "totalSize", "offset"}, nil
}

var decisionTrailing = []string{"decision", "selected", "location"}

// SetAlphabetical switches the element to PMS's decision-response ordering.
func (m *Metadata) SetAlphabetical() { m.alpha = true }

// AttrOrder implements AttrSorter.
func (m *Metadata) SetOnDeckOrder() { m.onDeck = true }
func (m Metadata) AttrOrder() (bool, []string, []string) {
	if m.onDeck {
		return false, []string{"allowSync", "librarySectionID", "librarySectionTitle", "librarySectionUUID"}, nil
	}
	if m.Type == "playlist" && !m.alpha {
		return true, []string{"allowSync", "ratingKey", "key", "guid", "type", "title", "titleSort", "summary", "smart", "playlistType", "composite", "viewCount", "lastViewedAt", "viewOffset", "userRating", "duration", "leafCount", "addedAt", "updatedAt", "content", "icon"}, nil
	}
	return m.alpha, nil, decisionTrailing
}

// SetAlphabetical switches the element to PMS's decision-response ordering.
func (m *Media) SetAlphabetical() { m.alpha = true }

// AttrOrder implements AttrSorter.
func (m Media) AttrOrder() (bool, []string, []string) { return m.alpha, m.lead, decisionTrailing }

// Transcoded media in PMS decisions list the attributes kept from the
// source first (alphabetically), then the transcode's own (alphabetically).
var (
	transcodeMediaLead = []string{"audioProfile", "hasVoiceActivity", "id", "videoProfile"}
	transcodePartLead  = []string{"audioProfile", "deepAnalysisVersion", "id", "indexes", "requiredBandwidths", "videoProfile"}
)

// SetTranscodeOrder switches to the ordering of a transcoded Media.
func (m *Media) SetTranscodeOrder() { m.alpha, m.lead = true, transcodeMediaLead }

// SetAlphabetical switches the element to PMS's decision-response ordering.
func (p *Part) SetAlphabetical() { p.alpha = true }

// SetTranscodeOrder switches to the ordering of a transcoded Part.
func (p *Part) SetTranscodeOrder() { p.alpha, p.lead = true, transcodePartLead }

// AttrOrder implements AttrSorter.
func (p Part) AttrOrder() (bool, []string, []string) { return p.alpha, p.lead, decisionTrailing }

// SetAlphabetical switches the element to PMS's decision-response ordering.
func (r *RatingTag) SetAlphabetical() { r.alpha = true }

// AttrOrder implements AttrSorter.
func (r RatingTag) AttrOrder() (bool, []string, []string) { return r.alpha, nil, nil }

// SetAlphabetical switches the element to PMS's decision-response ordering.
func (u *UltraBlurColors) SetAlphabetical() { u.alpha = true }

// AttrOrder implements AttrSorter.
func (u UltraBlurColors) AttrOrder() (bool, []string, []string) { return u.alpha, nil, nil }

// SetAlphabetical switches the element to PMS's decision-response ordering.
func (t *Tag) SetAlphabetical() { t.alpha = true }

// AttrOrder implements AttrSorter.
func (t Tag) AttrOrder() (bool, []string, []string) { return t.alpha, nil, nil }

// SetAlphabetical switches the element to PMS's decision-response ordering.
func (s *Stream) SetAlphabetical() { s.alpha = true }

// AttrOrder implements AttrSorter.
func (s Stream) AttrOrder() (bool, []string, []string) { return s.alpha, s.lead, streamTrailing }

// Decision streams sort "selected" with the rest; only decision and
// location trail.
var streamTrailing = []string{"decision", "location"}

// sortAttrs applies AttrSorter ordering: leading names first (in the given
// order), then the rest alphabetically, then trailing names (in order).
// Elements that don't sort keep struct-field order.
func sortAttrs[T any](v reflect.Value, attrs []T, name func(T) string) []T {
	if !v.CanInterface() {
		return attrs
	}
	s, ok := v.Interface().(AttrSorter)
	if !ok {
		return attrs
	}
	sorted, leading, trailing := s.AttrOrder()
	if !sorted && len(leading) == 0 && len(trailing) == 0 {
		return attrs
	}
	rank := func(n string) int {
		for i, l := range leading {
			if l == n {
				return i - len(leading) // negative: before everything
			}
		}
		for i, t := range trailing {
			if t == n {
				return 1 + i // after the alphabetical middle (rank 0)
			}
		}
		return 0
	}
	out := append([]T(nil), attrs...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := rank(name(out[i])), rank(name(out[j]))
		if ri != rj {
			return ri < rj
		}
		if ri != 0 {
			return false
		}
		return sorted && name(out[i]) < name(out[j])
	})
	return out
}

// SetSubtitleSearchOrder applies the search-offer attribute order.
func (s *Stream) SetSubtitleSearchOrder() {
	s.alpha = true
	s.lead = []string{"id", "key", "streamType", "codec", "language", "languageCode", "languageTag", "format", "providerTitle", "score", "sourceKey", "title", "transient", "userID"}
}
