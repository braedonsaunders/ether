package livetv

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"math"
	"strings"
	"time"
)

type PersonalIngredient struct {
	Kind        string           `json:"kind"`
	Share       float64          `json:"share"`
	Enabled     bool             `json:"enabled"`
	Collections []core.RatingKey `json:"collections,omitempty"`
}
type PersonalRecipe struct {
	Ingredients     []PersonalIngredient `json:"ingredients"`
	PauseWhenAway   bool                 `json:"pauseWhenAway"`
	NameFormat      string               `json:"nameFormat"`
	RepeatHours     int                  `json:"repeatHours"`
	MaxShowShare    int                  `json:"maxShowShare"`
	NightMaxMinutes int                  `json:"nightMaxMinutes"`
}
type PersonalPolicy struct {
	Enabled *bool  `json:"enabled"`
	Name    string `json:"name"`
}

func viewerName(u *core.User) string {
	if u.Title != "" {
		return u.Title
	}
	return u.Username
}
func (s *Service) personalChannel(ctx context.Context, c Channel, u *core.User) Channel {
	if c.Personal == nil {
		return c
	}
	c.personalViewer = u
	if u == nil {
		return c
	}
	format := c.Personal.NameFormat
	if format == "" {
		format = "{viewer} TV"
	}
	c.Name = strings.ReplaceAll(format, "{viewer}", viewerName(u))
	_, pol, err := s.withPolicy(ctx, u)
	if err == nil && pol.Personal != nil && pol.Personal.Name != "" {
		c.Name = pol.Personal.Name
	}
	return c
}

// viewerSchedule reads materialized programming only; it never calls the picker.
func (s *Service) viewerSchedule(ctx context.Context, c Channel, from, until time.Time) []Programme {
	if c.Personal == nil {
		return s.virtualProgrammes(c, from, until, "raw")
	}
	ps, err := s.personalProgrammes(ctx, c, c.personalViewer, from, until)
	if err != nil {
		return nil
	}
	return ps
}

func (s *Service) Rundown(ctx context.Context, id string, viewer int64) ([]Programme, error) {
	if viewer <= 0 {
		return nil, fmt.Errorf("viewer must be positive")
	}
	c, err := s.Channel(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.Personal == nil {
		return nil, fmt.Errorf("channel has no personal recipe")
	}
	if s.o.Users == nil {
		return []Programme{}, nil
	}
	users, err := s.o.Users.Users(ctx)
	if err != nil {
		return nil, err
	}
	for _, u := range users {
		if u.ID == viewer {
			c = s.personalChannel(ctx, c, &u)
			if !s.allowed(ctx, &u, c, nil) {
				return nil, core.ErrNotFound
			}
			ps, err := s.personalProgrammes(ctx, c, &u, s.o.Now(), s.o.Now().Add(48*time.Hour))
			if err != nil {
				return nil, err
			}
			out := []Programme{}
			for _, p := range ps {
				if s.allowed(ctx, &u, c, &p) {
					out = append(out, p)
				}
			}
			return out, nil
		}
	}
	return nil, core.ErrNotFound
}

func (s *Service) LineupChangedFor(u *core.User) time.Time {
	base := s.LineupChanged()
	if u == nil {
		return base
	}
	s.personalMu.Lock()
	defer s.personalMu.Unlock()
	if at := s.personalLineup[u.ID]; at.After(base) {
		return at
	}
	return base
}

func (s *Service) viewerGuide(ctx context.Context, c Channel, from, until time.Time) []Programme {
	if c.Personal == nil {
		return s.virtualProgrammes(c, from, until, "guide")
	}
	return s.viewerSchedule(ctx, c, from, until)
}

// ChannelForViewer resolves channel identity for metadata after guide filtering.
func (s *Service) ChannelForViewer(ctx context.Context, u *core.User, id string) (Channel, error) {
	c, err := s.Channel(ctx, id)
	if err != nil {
		return c, err
	}
	c = s.personalChannel(ctx, c, u)
	if !s.allowed(ctx, u, c, nil) {
		return Channel{}, core.ErrNotFound
	}
	return c, nil
}

func validatePersonal(c Channel) error {
	r := c.Personal
	if r == nil {
		return nil
	}
	if c.Kind != "virtual" {
		return fmt.Errorf("personal recipe requires a virtual channel")
	}
	if len(r.Ingredients) > 32 || r.RepeatHours < 0 || r.RepeatHours > 720 || r.MaxShowShare < 0 || r.MaxShowShare > 100 || r.NightMaxMinutes < 0 || r.NightMaxMinutes > 1440 || len(r.NameFormat) > 200 {
		return fmt.Errorf("invalid personal recipe limits")
	}
	for _, i := range r.Ingredients {
		switch i.Kind {
		case "continue", "nextEpisodes", "picked", "watchlist", "comfort", "newReleases", "collections":
		default:
			return fmt.Errorf("unknown personal ingredient %q", i.Kind)
		}
		if math.IsNaN(i.Share) || math.IsInf(i.Share, 0) || i.Share < 0 || i.Share > 100 || len(i.Collections) > 100 {
			return fmt.Errorf("invalid ingredient share or collections")
		}
	}
	return nil
}
