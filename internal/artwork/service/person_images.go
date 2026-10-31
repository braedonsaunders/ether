package service

import (
	"context"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

// PersonImages consumes an already-authorized detached title. It deliberately
// bypasses StoredTitle and all variant/metadata queues. Existing artwork-v1
// reference inputs stay byte-identical to the generic descriptor builder.
func (a Source) PersonImages(ctx context.Context, req frontend.ArtworkRequest, title core.Title) ([]frontend.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Viewer == nil || req.Item == 0 || title.Key != req.Item || req.Channel != "" {
		return nil, core.ErrUnauthorized
	}
	images := []frontend.Image{}
	for _, group := range []struct {
		kind   string
		people []core.Person
	}{{"actor", title.Cast}, {"director", title.Directors}, {"writer", title.Writers}} {
		for _, person := range group.people {
			if person.TMDB <= 0 || person.ThumbPath == "" {
				continue
			}
			subject := "tmdb:" + strconv.FormatInt(person.TMDB, 10)
			ref := artworkReference(req, frontend.ImagePerson, group.kind+"\x00"+subject+"\x00"+person.ThumbPath)
			images = append(images, frontend.Image{Kind: frontend.ImagePerson, Path: ref, Tag: ref, Subject: subject})
		}
	}
	return images, nil
}

var _ frontend.PersonArtwork = Source{}
