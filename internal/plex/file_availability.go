package plex

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

type fileAvailabilityChecker interface {
	CheckFileAvailability(context.Context, *core.User, core.RatingKey, core.ClientCaps, string) (bool, error)
}

func (s *Server) projectFileAvailability(r *http.Request, mc *model.MediaContainer) {
	q := r.URL.Query()
	if !metadataOptionOn(q.Get("checkFiles")) && !metadataOptionOn(q.Get("asyncCheckFiles")) && !metadataOptionOn(q.Get("checkFileAvailability")) {
		return
	}
	checker, ok := s.d.Playback.(fileAvailabilityChecker)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	pr := FromContext(ctx)
	for i := range mc.Metadata {
		md := &mc.Metadata[i]
		for j := range md.Media {
			for k := range md.Media[j].Part {
				part := &md.Media[j].Part[k]
				key, release, found := s.partTitle(part.ID)
				if !found {
					continue
				}
				live := false
				if found {
					var err error
					live, err = checker.CheckFileAvailability(ctx, pr.User, key, s.clientCaps(pr), release)
					if err != nil && !errors.Is(err, core.ErrNotAvailable) && !errors.Is(err, core.ErrNotFound) {
						continue
					}
				}
				part.Exists, part.Accessible = model.Ptr(live), model.Ptr(live)
			}
		}
	}
}
