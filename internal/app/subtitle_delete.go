package app

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

func (p plexSubtitles) DeleteSubtitle(_ context.Context, key core.RatingKey, _ *core.User, stream core.StreamInfo) error {
	return p.a.subtitles.DeleteUpload(key, stream)
}
