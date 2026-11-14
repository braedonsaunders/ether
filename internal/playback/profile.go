package playback

import (
	"github.com/braedonsaunders/ether/internal/core"
)

// ratingMedia scopes a profile choice to the title kind. A key with no
// usable kind leaves the choice media-neutral.
func ratingMedia(key core.RatingKey) string {
	switch key.Kind() {
	case core.KindMovie:
		return core.MediaMovie
	case core.KindShow, core.KindSeason, core.KindEpisode:
		return core.MediaTV
	}
	return core.MediaAny
}

// selectProfile picks the profile for a title key.
func (m *Manager) selectProfile(caps core.ClientCaps, user *core.User, key core.RatingKey) core.QualityProfile {
	if m.deps.SelectTitleProfile != nil {
		return m.deps.SelectTitleProfile(caps, user, key)
	}
	return m.deps.SelectProfile(caps, user, ratingMedia(key))
}
