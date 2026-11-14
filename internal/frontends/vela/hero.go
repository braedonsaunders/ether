package vela

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"net/http"
)

// heroSource is an internal sidecar, never a client-supplied or wire row name.
type heroSource struct {
	kind   string
	pinned bool
}

type heroCandidate struct {
	hero                       object
	personal, pinned, playable bool
}

// heroesFromRows consumes the already-ranked, policy-checked visit. It does
// no catalogue hydration, trailer discovery, or new ranking work. Source order
// is the ranker's order, including the owner's Home Designer placements.
func heroesFromRows(rows []object, provenance ...map[string]heroSource) ([]object, []object) {
	candidates := []heroCandidate{}
	for _, row := range rows {
		id, _ := row["id"].(string)
		source := heroSource{}
		if len(provenance) > 0 {
			source = provenance[0][id]
		}
		entries, _ := row["items"].([]object)
		for _, entry := range entries {
			candidate, ok := heroFromEntry(row, entry, source)
			if ok {
				candidates = append(candidates, candidate)
			}
		}
	}
	heroes := []object{}
	chosen := map[string]bool{}
	// Series cards and their episodes share one slot and one shelf exclusion.
	families := map[string]bool{}
	add := func(c heroCandidate) bool {
		title := c.hero["title"].(object)
		id, _ := title["id"].(string)
		family := heroFamily(title)
		if chosen[id] || families[family] {
			return false
		}
		heroes = append(heroes, c.hero)
		chosen[id], families[family] = true, true
		return true
	}
	pick := func(match func(heroCandidate) bool) bool {
		if len(heroes) >= 6 {
			return false
		}
		for _, c := range candidates {
			if match(c) && add(c) {
				return true
			}
		}
		return false
	}
	// Owner placements take precedence, with the candidate's honest reason.
	pick(func(c heroCandidate) bool { return c.pinned })
	if len(heroes) == 0 {
		pick(func(c heroCandidate) bool { return c.personal && c.playable })
	}
	if len(heroes) == 0 {
		pick(func(heroCandidate) bool { return true })
	}
	for _, wanted := range []string{"personal", "trending", "personal", "newRelease", "trending"} {
		if !pick(func(c heroCandidate) bool {
			if wanted == "personal" {
				return c.personal
			}
			return c.hero["reason"] == wanted
		}) {
			pick(func(heroCandidate) bool { return true })
		}
	}
	if len(heroes) == 0 {
		return nil, rows
	}
	shelves := make([]object, 0, len(rows))
	for _, row := range rows {
		entries, _ := row["items"].([]object)
		kept := make([]object, 0, len(entries))
		for _, entry := range entries {
			if entry["kind"] == "title" {
				if title, ok := entry["title"].(object); ok {
					if families[heroFamily(title)] {
						continue
					}
				}
			}
			kept = append(kept, entry)
		}
		// Keep the original issued cursor: its offset includes entries now shown
		// in heroes. Recomputing it from the shorter shelf would repeat cards.
		if len(kept) == 0 && row["continuation"] == "exhausted" {
			continue
		}
		shelf := object{}
		for key, value := range row {
			shelf[key] = value
		}
		shelf["items"] = kept
		shelves = append(shelves, shelf)
	}
	return heroes, shelves
}

// The backdrop kind is a host assertion about real source art, independent of
// lazy pixel metadata. Reject a known non-landscape image; unknown dimensions
// stay unknown and never trigger an image download on bootstrap.
func heroBackdrop(title object) bool {
	images, _ := title["artwork"].([]object)
	for _, img := range images {
		if img["kind"] != "backdrop" {
			continue
		}
		sizes, _ := img["sizes"].([]object)
		for _, size := range sizes {
			url, _ := size["url"].(string)
			w, h := heroNumber(size["width"]), heroNumber(size["height"])
			if url != "" && (w <= 0 || h <= 0 || w > h) {
				return true
			}
		}
	}
	return false
}

func heroNumber(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

func heroFamily(title object) string {
	if show, _ := title["showId"].(string); show != "" {
		return show
	}
	id, _ := title["id"].(string)
	return id
}

func heroFromEntry(row, entry object, source heroSource) (heroCandidate, bool) {
	title, ok := entry["title"].(object)
	receipt, _ := entry["receiptToken"].(string)
	id, _ := title["id"].(string)
	if !ok || entry["kind"] != "title" || receipt == "" || id == "" || !heroBackdrop(title) {
		return heroCandidate{}, false
	}
	playable := title["kind"] == "movie" || title["kind"] == "episode"
	reason := ""
	personal := false
	purpose := frontend.RowPurpose("")
	switch p := row["purpose"].(type) {
	case frontend.RowPurpose:
		purpose = p
	case string:
		purpose = frontend.RowPurpose(p)
	}
	watch, _ := title["watch"].(object)
	position := heroNumber(watch["positionSeconds"])
	action := object{"kind": "details", "titleId": id}
	if playable {
		action["kind"] = "play"
	}
	switch {
	case (purpose == frontend.RowContinue || source.kind == "continue-watching") && playable && position > 0:
		reason, personal = "resume", true
		action["kind"], action["positionSeconds"] = "resume", position
	case (purpose == frontend.RowNextUp || source.kind == "up-next") && title["kind"] == "episode":
		reason, personal = "nextEpisode", true
	case purpose == frontend.RowRecentlyAdded || purpose == frontend.RowRecentlyReleased:
		reason = "newRelease"
	default:
		switch source.kind {
		case "trending", "popular", "popular-on-server", "top-10-today", "trending-in-your-taste", "trending-with-fans":
			reason = "trending"
		case "new-this-week":
			reason, personal = "newRelease", true
		case "new-releases", "recently-added", "recentlyAdded":
			reason = "newRelease"
		case "top-10", "because-you-watched", "more-from-person", "hidden-gems", "comfort-rewatches", "something-different", "short-on-time", "finish-the-franchise", "llm", "fans-also-loved", "trakt-recommendations", "taste-cluster", "era-favourites", "world-cinema", "occasion", "acclaimed-unseen", "top-rated-in-genre", "taste-bridge", "more-from-studio", "start-something-new", "airing-for-you", "more-from-network":
			reason, personal = "forYou", true
		}
	}
	if reason == "" {
		return heroCandidate{}, false
	}
	hero := object{"title": title, "receiptToken": receipt, "reason": reason, "action": action}
	if enabled, _ := row["previewOnFocus"].(bool); enabled {
		hero["previewOnFocus"] = true
	}
	return heroCandidate{hero: hero, personal: personal, pinned: source.pinned, playable: playable}, true
}

// heroFallback requests just the open slots through Experience, so selection,
// owner layout, current viewer policy and receipts remain host-owned.
func (i *instance) heroFallback(r *http.Request, viewer *frontend.Viewer, token string, heroes []object, sources map[string]heroSource) ([]object, error) {
	excluded := make([]frontend.RatingKey, 0, len(heroes))
	for _, hero := range heroes {
		key, err := ids.ParseRatingKeyGUID(heroFamily(hero["title"].(object)))
		if err != nil {
			return nil, err
		}
		excluded = append(excluded, key)
	}
	rows, err := i.host.Experience.Rows(r.Context(), frontend.RowRequest{Viewer: viewer, Device: requestDevice(r), Placement: frontend.PlacementHome, Limit: 6 - len(heroes), HomeFallbackOnly: true, ExcludeItems: excluded})
	if err != nil {
		return nil, err
	}
	out := []object{}
	for _, row := range rows {
		titles := []frontend.Title{}
		for _, entry := range row.Entries {
			if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
				titles = append(titles, *entry.Title)
			}
		}
		converted, err := i.titles(r, viewer, token, titles)
		if err != nil {
			return nil, err
		}
		byID := map[string]object{}
		for _, title := range converted {
			byID[title["id"].(string)] = title
		}
		entries := []object{}
		for _, entry := range row.Entries {
			if entry.Kind != frontend.RowEntryTitle || entry.Title == nil {
				continue
			}
			title := byID[ids.RatingKeyGUID(entry.Title.Key)]
			if title != nil {
				entries = append(entries, object{"kind": "title", "title": title, "receiptToken": entry.Handle})
			}
		}
		sources[row.ID] = heroSource{kind: row.Kind, pinned: row.Layout.Always}
		out = append(out, object{"id": row.ID, "purpose": row.Purpose, "previewOnFocus": row.PreviewOnFocus, "items": entries, "continuation": "exhausted"})
	}
	return out, nil
}
