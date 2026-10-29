package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type surfaceTrackPreferences struct{ app *App }

func (s surfaceTrackPreferences) active(ctx context.Context, v *frontend.Viewer) error {
	if v == nil || v.ID <= 0 || s.app.trackprefs == nil {
		return core.ErrUnauthorized
	}
	u, err := s.app.users.ActiveUser(ctx, v.ID)
	if err != nil || u == nil {
		return core.ErrUnauthorized
	}
	return nil
}
func (s surfaceTrackPreferences) Get(ctx context.Context, v *frontend.Viewer) (frontend.TrackPreferences, error) {
	if err := s.active(ctx, v); err != nil {
		return frontend.TrackPreferences{}, err
	}
	rules, err := s.app.trackprefs.GetRules(ctx, v.ID)
	if err != nil {
		return frontend.TrackPreferences{}, err
	}
	return frontend.TrackPreferences{AudioLanguages: rules.AudioLanguages, SubtitleLanguages: rules.SubtitleLanguages, SubtitleMode: rules.SubtitleMode}, nil
}
func (s surfaceTrackPreferences) SetSubtitle(ctx context.Context, v *frontend.Viewer, languages []string, mode string) error {
	if err := s.active(ctx, v); err != nil {
		return err
	}
	return s.app.trackprefs.SetSubtitlePreferences(ctx, v.ID, languages, mode)
}
