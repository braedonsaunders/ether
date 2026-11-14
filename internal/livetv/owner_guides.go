package livetv

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("livetv", 20, `CREATE TABLE livetv_owner_guides(id INTEGER PRIMARY KEY, data TEXT NOT NULL);`)
}

// GuideGroup identifies a provider group without relying on its display name alone.
type GuideGroup struct {
	SourceID string `json:"sourceId"`
	Name     string `json:"name"`
}
type OwnerGuide struct {
	ID     string       `json:"id"`
	Name   string       `json:"name"`
	Groups []GuideGroup `json:"groups"`
	All    bool         `json:"all,omitempty"`
	Hidden bool         `json:"hidden,omitempty"`
}
type guideChoice struct {
	GuideGroup
	SourceName string `json:"sourceName,omitempty"`
	Count      int    `json:"count"`
}

func (g OwnerGuide) contains(source, group string) bool {
	return g.All || slices.Contains(g.Groups, GuideGroup{SourceID: source, Name: group})
}

// Defaults are computed on demand from the cached channel identity index. Tiny
// groups share one guide; their original groups and channel IDs stay untouched.
func (s *Service) guideChoices(ctx context.Context) ([]guideChoice, error) {
	var rev int64
	if err := s.o.DB.QueryRowContext(ctx, `SELECT generation FROM livetv_channel_epoch WHERE id=1`).Scan(&rev); err != nil {
		return nil, err
	}
	s.ownerGuidesMu.Lock()
	defer s.ownerGuidesMu.Unlock()
	if s.ownerGuideGroups != nil && s.ownerGuideRev == rev {
		return s.ownerGuideGroups, nil
	}
	cs, err := s.ChannelSummaries(ctx)
	if err != nil {
		return nil, err
	}
	out := []guideChoice{}
	at := map[GuideGroup]int{}
	for _, c := range cs {
		key := GuideGroup{c.SourceID, c.Group}
		if i, ok := at[key]; ok {
			out[i].Count++
		} else {
			at[key] = len(out)
			out = append(out, guideChoice{GuideGroup: key, Count: 1})
		}
	}
	sources, err := s.Sources(ctx)
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, src := range sources {
		names[src.ID] = src.Name
	}
	for i := range out {
		out[i].SourceName = names[out[i].SourceID]
	}
	s.ownerGuideGroups = out
	s.ownerGuideRev = rev
	return out, nil
}
func (s *Service) OwnerGuides(ctx context.Context) ([]OwnerGuide, error) {
	var raw []byte
	err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_owner_guides WHERE id=1`).Scan(&raw)
	if err == nil {
		var gs []OwnerGuide
		err = json.Unmarshal(raw, &gs)
		return gs, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	choices, err := s.guideChoices(ctx)
	if err != nil {
		return nil, err
	}
	gs := []OwnerGuide{}
	byName := map[string]int{}
	var small []GuideGroup
	for _, c := range choices {
		if c.Count < 5 || strings.TrimSpace(c.Name) == "" || (len(gs) >= 199 && !slices.ContainsFunc(gs, func(g OwnerGuide) bool { return g.Name == c.Name })) {
			small = append(small, c.GuideGroup)
			continue
		}
		if i, ok := byName[c.Name]; ok {
			gs[i].Groups = append(gs[i].Groups, c.GuideGroup)
		} else {
			byName[c.Name] = len(gs)
			gs = append(gs, OwnerGuide{ID: "group-" + stableID(c.SourceID, c.Name), Name: c.Name, Groups: []GuideGroup{c.GuideGroup}})
		}
	}
	if len(small) > 0 {
		if i, ok := byName["Other channels"]; ok {
			gs[i].Groups = append(gs[i].Groups, small...)
		} else {
			gs = append(gs, OwnerGuide{ID: "other", Name: "Other channels", Groups: small})
		}
	}
	if len(gs) == 0 {
		gs = append(gs, OwnerGuide{ID: "all", Name: "All channels", All: true, Groups: []GuideGroup{}})
	}
	return gs, nil
}
func (s *Service) SaveOwnerGuides(ctx context.Context, gs []OwnerGuide) error {
	if len(gs) > 200 {
		return fmt.Errorf("too many guides")
	}
	seen := map[string]bool{}
	names := map[string]bool{}
	if gs == nil {
		gs = []OwnerGuide{}
	}
	for i := range gs {
		g := &gs[i]
		g.Name = strings.TrimSpace(g.Name)
		if g.ID == "" {
			g.ID = randomID()
		}
		if len(g.ID) > 100 || strings.ContainsAny(g.ID, "/\\?# \t\r\n") || seen[g.ID] || names[strings.ToLower(g.Name)] || g.Name == "" || len(g.Name) > 150 || len(g.Groups) > 2000 {
			return fmt.Errorf("invalid guide")
		}
		seen[g.ID] = true
		names[strings.ToLower(g.Name)] = true
		slices.SortFunc(g.Groups, func(a, b GuideGroup) int {
			if n := strings.Compare(a.SourceID, b.SourceID); n != 0 {
				return n
			}
			return strings.Compare(a.Name, b.Name)
		})
		g.Groups = slices.Compact(g.Groups)
	}
	raw, err := json.Marshal(gs)
	if err != nil {
		return err
	}
	_, err = s.o.DB.ExecContext(ctx, `INSERT INTO livetv_owner_guides(id,data) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, raw)
	if err == nil {
		s.bumpLineup()
	}
	return err
}
func (s *Service) ownerGuidesRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var gs []OwnerGuide
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&gs); err != nil {
			ownerResult(w, nil, err)
			return
		}
		if err := s.SaveOwnerGuides(r.Context(), gs); err != nil {
			ownerResult(w, nil, err)
			return
		}
	}
	gs, err := s.OwnerGuides(r.Context())
	if err != nil {
		ownerResult(w, nil, err)
		return
	}
	choices, err := s.guideChoices(r.Context())
	ownerResult(w, map[string]any{"guides": gs, "groups": choices}, err)
}
func guideMemberships(gs []OwnerGuide, source, group string) []frontend.LiveGuide {
	out := []frontend.LiveGuide{}
	for _, g := range gs {
		if !g.Hidden && g.contains(source, group) {
			out = append(out, frontend.LiveGuide{ID: g.ID, Name: g.Name})
		}
	}
	return out
}
func (h liveTVHost) Guides(ctx context.Context, u *core.User) ([]frontend.LiveGuide, error) {
	cs, err := h.Channels(ctx, u)
	if err != nil {
		return nil, err
	}
	present := map[string]bool{}
	for _, c := range cs {
		for _, g := range c.Guides {
			present[g.ID] = true
		}
	}
	gs, err := h.s.OwnerGuides(ctx)
	if err != nil {
		return nil, err
	}
	out := []frontend.LiveGuide{}
	for _, g := range gs {
		if present[g.ID] {
			out = append(out, frontend.LiveGuide{ID: g.ID, Name: g.Name})
		}
	}
	return out, nil
}
func (h liveTVHost) GuideChannels(ctx context.Context, u *core.User, id string) ([]frontend.LiveTVChannel, error) {
	gs, err := h.s.OwnerGuides(ctx)
	if err != nil {
		return nil, err
	}
	var chosen *OwnerGuide
	for i := range gs {
		if gs[i].ID == id && !gs[i].Hidden {
			chosen = &gs[i]
			break
		}
	}
	if chosen == nil {
		return nil, core.ErrNotFound
	}
	cs, err := h.s.AllowedChannels(ctx, u)
	if err != nil {
		return nil, err
	}
	groups := map[GuideGroup]bool{}
	for _, g := range chosen.Groups {
		groups[g] = true
	}
	out := []frontend.LiveTVChannel{}
	for _, c := range cs {
		if chosen.All || groups[GuideGroup{c.SourceID, c.Group}] {
			out = append(out, ToFrontendChannel(c))
		}
	}
	return out, nil
}
