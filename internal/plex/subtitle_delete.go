package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
)

type subtitleDelete interface {
	DeleteSubtitle(context.Context, core.RatingKey, *core.User, core.StreamInfo) error
}
