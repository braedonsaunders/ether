package plex

import (
	"context"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// homeLayoutFacts is the renderer's layout report: identity, title, type,
// context and the painted item count. Metadata, keys and programme sidecars
// stay on the wire row. Callers record the report when they paint the row.
func homeLayoutFacts(row model.Hub) (core.Hub, core.NeutralHomeRow) {
	count := max(row.Size, len(row.Metadata)+len(row.Directory))
	hub := core.Hub{ID: row.HubIdentifier, Title: row.Title, Type: core.MediaType(row.Type), Context: row.Context, ItemCount: count}
	neutral := core.NeutralHomeRow{ID: row.HubIdentifier, Title: row.Title, Type: core.MediaType(row.Type), Purpose: experience.HomePurpose(core.Hub{ID: row.HubIdentifier, Type: core.MediaType(row.Type)}), ItemCount: count}
	return hub, neutral
}

// arrangeHomeRows reorders painted rows from facts captured at paint time.
// A missing provider or a layout error leaves the original rows in place.
// Empty and duplicate rows are dropped by the hubs handlers after this returns.
func (s *Server) arrangeHomeRows(ctx context.Context, pr *Request, hc core.HubContext, section string, rows []model.Hub, hubs []core.Hub, neutral []core.NeutralHomeRow) []model.Hub {
	if len(rows) != len(hubs) || len(rows) != len(neutral) {
		if s.log != nil {
			s.log.Warn("Home layout unavailable", "error", "paint facts diverged")
		}
		return rows
	}
	if _, ok := s.d.Recommender.(core.NeutralHomeLayoutSource); !ok {
		if _, layout := s.d.Recommender.(core.HomeLayoutSource); !layout {
			return rows
		}
	}
	indices, err := experience.ArrangeHome(ctx, s.d.Recommender, pr.User, hc, section, hubs, neutral)
	if err != nil {
		if s.log != nil {
			s.log.Warn("Home layout unavailable", "error", err)
		}
		return rows
	}
	out := make([]model.Hub, 0, len(indices))
	for _, i := range indices {
		if i < 0 || i >= len(rows) {
			if s.log != nil {
				s.log.Warn("Home layout unavailable", "error", "index out of range")
			}
			return rows
		}
		out = append(out, rows[i])
	}
	return out
}

// finalizeHomeRows is the wire-level entry used by layout tests. Production
// home and section handlers pass facts captured while painting.
func (s *Server) finalizeHomeRows(ctx context.Context, pr *Request, hc core.HubContext, section string, rows []model.Hub) []model.Hub {
	hubs := make([]core.Hub, len(rows))
	neutral := make([]core.NeutralHomeRow, len(rows))
	for i, row := range rows {
		hubs[i], neutral[i] = homeLayoutFacts(row)
	}
	return s.arrangeHomeRows(ctx, pr, hc, section, rows, hubs, neutral)
}

// stableHomeHubs drops rows a client would draw empty, and keeps the first
// copy when the same hub was produced twice (promoted and section builders,
// or a recommender repeating a row the front end already built).
func stableHomeHubs(rows []model.Hub) []model.Hub {
	seen := map[string]bool{}
	out := make([]model.Hub, 0, len(rows))
	for _, h := range rows {
		if len(h.Metadata) == 0 && len(h.Directory) == 0 {
			continue
		}
		if h.HubIdentifier != "" {
			if seen[h.HubIdentifier] {
				continue
			}
			seen[h.HubIdentifier] = true
		}
		out = append(out, h)
	}
	return out
}

// heroReadyItem reports whether a painted title has the artwork iOS needs
// to draw a Home hero tile: a backdrop and a poster.
func heroReadyItem(m model.Metadata) bool {
	art, thumb := strings.TrimSpace(m.Art), strings.TrimSpace(m.Thumb)
	for _, img := range m.Image {
		switch img.Type {
		case "background":
			if art == "" {
				art = strings.TrimSpace(img.URL)
			}
		case "coverPoster":
			if thumb == "" {
				thumb = strings.TrimSpace(img.URL)
			}
		}
	}
	if art == "" {
		art = strings.TrimSpace(m.GrandparentArt)
	}
	if thumb == "" {
		thumb = strings.TrimSpace(m.GrandparentThumb)
	}
	return art != "" && thumb != ""
}

// paintHeroItems keeps titles that can fill a hero tile and rewrites Size
// and HubKey to match. A hub with no such titles is omitted so the client
// never reserves the slot.
func paintHeroItems(h model.Hub) (model.Hub, bool) {
	kept := make([]model.Metadata, 0, len(h.Metadata))
	keys := make([]string, 0, len(h.Metadata))
	for _, m := range h.Metadata {
		if !heroReadyItem(m) {
			continue
		}
		kept = append(kept, m)
		if m.RatingKey != "" {
			keys = append(keys, m.RatingKey)
		}
	}
	if len(kept) == 0 {
		return h, false
	}
	if len(kept) != len(h.Metadata) {
		if len(keys) > 0 {
			h.HubKey = "/library/metadata/" + strings.Join(keys, ",")
		} else {
			h.HubKey = ""
		}
	}
	h.Metadata = kept
	h.Size = len(kept)
	return h, true
}

// heroReadyHomeHubs is the Home-only pass after stableHomeHubs. Plex iOS
// draws the first hub, and any style=hero hub, as the large spotlight. A
// cold or not-yet-warm row with no backdrop must not be sent: either the
// next already-painted, art-ready shelf is promoted into that slot, or
// the hero is omitted. Later shelves stay as they were.
func heroReadyHomeHubs(rows []model.Hub) []model.Hub {
	out := make([]model.Hub, 0, len(rows))
	haveHero := false
	for _, h := range rows {
		if strings.EqualFold(h.Style, "hero") || !haveHero {
			painted, ok := paintHeroItems(h)
			if !ok {
				continue
			}
			if !strings.EqualFold(painted.Style, "hero") {
				painted.Style = "hero"
			}
			out = append(out, painted)
			haveHero = true
			continue
		}
		out = append(out, h)
	}
	return out
}
