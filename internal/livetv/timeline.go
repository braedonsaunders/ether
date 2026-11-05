package livetv

import (
	"errors"

	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

type TimelineDaypart struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Days  []int  `json:"days"`
}
type TimelineElement struct {
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Start       int64             `json:"start"`
	End         int64             `json:"end"`
	Title       string            `json:"title"`
	Subtitle    string            `json:"subtitle,omitempty"`
	MediaType   string            `json:"mediaType,omitempty"`
	BlockID     string            `json:"blockId,omitempty"`
	SourceID    string            `json:"sourceId,omitempty"`
	Pinned      bool              `json:"pinned"`
	BreakPoints []BreakPoint      `json:"breakPoints,omitempty"`
	Art         string            `json:"art"`
	Rule        string            `json:"rule"`
	Children    []TimelineElement `json:"children,omitempty"`
}
type Timeline struct {
	ChannelID string            `json:"channelId"`
	Timezone  string            `json:"timezone"`
	Now       int64             `json:"now"`
	Dayparts  []TimelineDaypart `json:"dayparts"`
	Elements  []TimelineElement `json:"elements"`
}

func (s *Service) timelineRoute(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, e1 := strconv.ParseInt(q.Get("from"), 10, 64)
	end, e2 := strconv.ParseInt(q.Get("to"), 10, 64)
	if e1 != nil || e2 != nil || end <= start || uint64(end)-uint64(start) > uint64((8*24*time.Hour)/time.Millisecond) {
		ownerResult(w, nil, fmt.Errorf("timeline range must be positive and at most eight days"))
		return
	}
	viewer := int64(0)
	if q.Get("viewer") != "" {
		var err error
		viewer, err = strconv.ParseInt(q.Get("viewer"), 10, 64)
		if err != nil || viewer <= 0 {
			ownerResult(w, nil, fmt.Errorf("viewer must be positive"))
			return
		}
	}
	c, err := s.Channel(r.Context(), chi.URLParam(r, "id"))
	if errors.Is(err, core.ErrNotAvailable) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "Guide snapshot is warming", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		ownerResult(w, nil, err)
		return
	}
	user := &core.User{IsOwner: true}
	if viewer == 0 && s.o.InputUser != nil {
		current, e := s.o.InputUser(r.Context())
		if e != nil {
			ownerResult(w, nil, e)
			return
		}
		if current != nil {
			user = current
		}
	}
	if viewer > 0 {
		if s.o.Users == nil {
			ownerResult(w, nil, core.ErrNotFound)
			return
		}
		users, e := s.o.Users.Users(r.Context())
		if e != nil {
			ownerResult(w, nil, e)
			return
		}
		user = nil
		for i := range users {
			if users[i].ID == viewer {
				user = &users[i]
				break
			}
		}
		if user == nil {
			ownerResult(w, nil, core.ErrNotFound)
			return
		}
	}
	programmes, err := s.ProgrammesSnapshot(r.Context(), user, c.ID, time.UnixMilli(start), time.UnixMilli(end), SnapshotPlayback)
	if errors.Is(err, core.ErrNotAvailable) {
		w.Header().Set("Retry-After", "2")
		http.Error(w, "Guide snapshot is warming", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		ownerResult(w, nil, err)
		return
	}
	ownerResult(w, buildTimeline(c, programmes, start, end, s.o.Now()), nil)
}

// buildTimeline is a bounded projection of a snapshot; it clips the requested
// edges and fills holes, without changing programme identities or guide slots.
func buildTimeline(c Channel, programmes []Programme, start, end int64, now time.Time) Timeline {
	out := Timeline{ChannelID: c.ID, Timezone: orText(c.Timezone, "UTC"), Now: now.UnixMilli(), Dayparts: []TimelineDaypart{}, Elements: []TimelineElement{}}
	for i, b := range c.Blocks {
		out.Dayparts = append(out.Dayparts, TimelineDaypart{ID: orText(b.ID, fmt.Sprintf("block-%d", i)), Name: b.Name, Start: b.Start, End: b.End, Days: append([]int{}, b.Days...)})
	}
	ps := append([]Programme(nil), programmes...)
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Start.Before(ps[j].Start) })
	cursor := start
	gap := func(to int64) {
		if to > cursor {
			out.Elements = append(out.Elements, TimelineElement{ID: fmt.Sprintf("%s-gap-%d", c.ID, cursor), Kind: "offair", Start: cursor, End: to, Title: offAirTitle, Rule: "Outside programmed hours"})
			cursor = to
		}
	}
	for _, p := range ps {
		a, z := max(start, p.Start.UnixMilli()), min(end, p.End.UnixMilli())
		if z <= cursor || a >= end {
			continue
		}
		gap(a)
		a = max(a, cursor)
		if z <= a {
			continue
		}
		kind := "programme"
		switch {
		case p.Item.ReelClipID != "":
			kind = "spot"
		case strings.HasPrefix(p.Item.Slate, "plugin:"):
			kind = "plugin"
		case p.Item.Slate != "":
			kind = p.Item.Slate
		case p.Pinned && strings.Contains(p.Rule, "dated special"):
			kind = "special"
		}
		if p.Pinned {
			for _, pin := range c.Pins {
				if pin.ID == p.BlockID && pin.Date != "" {
					kind = "special"
					break
				}
			}
		}
		if kind == "ident" && len(out.Elements) > 0 {
			previous := out.Elements[len(out.Elements)-1]
			if previous.Kind == "pod" && previous.End == a {
				kind = "stationId"
			}
		}
		el := TimelineElement{ID: p.ID, Kind: kind, Start: a, End: z, Title: p.Item.Title, MediaType: p.Item.MediaType, BlockID: p.BlockID, SourceID: orText(p.SourceID, p.Item.SourceID), Pinned: p.Pinned, Art: orText(p.Item.Art, p.Item.Thumb), Rule: p.Rule, BreakPoints: append([]BreakPoint(nil), p.Item.BreakPoints...)}
		if el.Rule == "" {
			el.Rule = orText(p.Reason, "Channel programming")
		}
		if p.Item.Episode > 0 {
			el.Subtitle = fmt.Sprintf("S%02dE%02d", p.Item.Season, p.Item.Episode)
			if p.Item.EpisodeTitle != "" {
				el.Subtitle += " · " + p.Item.EpisodeTitle
			}
		}
		if len(el.BreakPoints) == 0 {
			for _, ms := range p.Item.BreakPointsMS {
				el.BreakPoints = append(el.BreakPoints, BreakPoint{AtMS: ms, Source: "chapters"})
			}
		}
		if el.MediaType == "" {
			switch {
			case kind == "spot":
				el.MediaType = "clip"
			case kind == "plugin":
				el.MediaType = "segment"
			case p.Item.ShowKey != 0:
				el.MediaType = "episode"
			case p.Item.Key != 0:
				el.MediaType = "movie"
			}
		}
		// Consecutive commercial pieces share a pod parent; child times remain
		// absolute so keyboard and zoom inspection can select individual spots.
		commercial := kind == "spot" || kind == "break" || kind == "back" || kind == "stationId"
		if commercial {
			n := len(out.Elements)
			if n > 0 && out.Elements[n-1].Kind == "pod" && out.Elements[n-1].End == a {
				pod := &out.Elements[n-1]
				pod.End = z
				pod.Children = append(pod.Children, el)
			} else {
				out.Elements = append(out.Elements, TimelineElement{ID: p.ID + "-pod", Kind: "pod", Start: a, End: z, Title: "Commercial break", Rule: el.Rule, BlockID: el.BlockID, Children: []TimelineElement{el}})
			}
		} else {
			out.Elements = append(out.Elements, el)
		}
		cursor = z
	}
	gap(end)
	return out
}
