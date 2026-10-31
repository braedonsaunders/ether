package service

import (
	"context"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
)

type creditedPortrait struct {
	role    string
	subject string
	source  string
	thumb   string
}

func creditedPortraits(title core.Title) []creditedPortrait {
	var out []creditedPortrait
	for _, group := range []struct {
		role   string
		people []core.Person
	}{{"actor", title.Cast}, {"director", title.Directors}, {"writer", title.Writers}, {"crew", title.Crew}} {
		for _, person := range group.people {
			if person.TMDB <= 0 || person.ThumbPath == "" {
				continue
			}
			subject := "tmdb:" + strconv.FormatInt(person.TMDB, 10)
			out = append(out, creditedPortrait{
				role:    group.role,
				subject: subject,
				source:  group.role + "\x00" + subject + "\x00" + person.ThumbPath,
				thumb:   person.ThumbPath,
			})
		}
	}
	return out
}

// PersonImages consumes an already-authorized detached title. It deliberately
// bypasses StoredTitle and all variant/metadata queues.
func (a Source) PersonImages(ctx context.Context, req frontend.ArtworkRequest, title core.Title) ([]frontend.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Viewer == nil || req.Item == 0 || title.Key != req.Item || req.Channel != "" {
		return nil, core.ErrUnauthorized
	}
	images := []frontend.Image{}
	for _, portrait := range creditedPortraits(title) {
		ref := artworkReference(req, frontend.ImagePerson, portrait.source)
		images = append(images, frontend.Image{Kind: frontend.ImagePerson, Path: ref, Tag: ref, Subject: portrait.subject})
	}
	return images, nil
}

var _ frontend.PersonArtwork = Source{}
