package vela

import (
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// mediaSummary reports only facts already present on the authorized detail.
// Reading a title must not resolve a source just to fill a badge.
func mediaSummary(d frontend.ItemDetail) object {
	if d.TracksEstimated {
		return nil
	}
	video, audio := []object{}, []object{}
	for _, s := range d.Tracks {
		entry := object{}
		if s.Codec != "" {
			entry["codec"] = s.Codec
		}
		switch s.Type {
		case "video":
			if s.Width > 0 {
				entry["width"] = s.Width
			}
			if s.Height > 0 {
				entry["height"] = s.Height
			}
			depth := s.BitDepth
			if s.BitsPerRawSample != nil && *s.BitsPerRawSample > 0 {
				depth = *s.BitsPerRawSample
			}
			if depth > 0 {
				entry["bitDepth"] = depth
			}
			if s.ColorTransfer != "" {
				entry["colorTransfer"] = s.ColorTransfer
			}
			profile := s.DolbyVisionProfile
			if s.ProbedDolbyVisionProfile != nil {
				profile = *s.ProbedDolbyVisionProfile
			}
			if profile > 0 {
				entry["dolbyVisionProfile"] = profile
			}
			if len(entry) > 0 {
				video = append(video, entry)
			}
		case "audio":
			if s.Channels > 0 {
				entry["channels"] = s.Channels
			}
			if s.Language != "" {
				entry["language"] = s.Language
			}
			if len(entry) > 0 {
				audio = append(audio, entry)
			}
		}
	}
	if len(video) == 0 && len(audio) == 0 {
		return nil
	}
	return object{"video": video, "audio": audio}
}

func crew(t frontend.Title) []object {
	out := []object{}
	seen := map[string]bool{}
	for _, group := range [][]frontend.Person{t.Crew, t.Directors, t.Writers} {
		for _, p := range group {
			if p.Name == "" {
				continue
			}
			identity := p.Name
			if p.TMDB > 0 {
				identity = "tmdb:" + strconv.FormatInt(p.TMDB, 10)
			} else if p.IMDB != "" {
				identity = "imdb:" + p.IMDB
			} else if p.ID != "" {
				identity = "id:" + p.ID
			}
			key := identity + "\x00" + p.Role
			if seen[key] {
				continue
			}
			seen[key] = true
			entry := object{"name": p.Name}
			if p.Role != "" {
				entry["role"] = p.Role
			}
			out = append(out, entry)
		}
	}
	return out
}

// Detail children are bounded first pages in the neutral service's order.
// Subsequent selections use /titles/:id/children; no episode is inferred.
func (i *instance) detailChildren(r *http.Request, v *frontend.Viewer, token string, d frontend.ItemDetail, out object) error {
	key := d.Title.Key
	if key.Kind() != frontend.KindShow && key.Kind() != frontend.KindSeason {
		return nil
	}
	page, err := i.host.Experience.Children(r.Context(), v, requestDevice(r), key, frontend.ItemQuery{Limit: 50, NoTotal: true})
	if err != nil {
		return err
	}
	converted, err := i.titles(r, v, token, page.Items)
	if err != nil {
		return err
	}
	if key.Kind() == frontend.KindSeason {
		out["episodes"] = converted
		return nil
	}
	out["seasons"] = converted
	for _, season := range page.Items {
		if season.Key.Kind() != frontend.KindSeason {
			continue
		}
		episodes, err := i.host.Experience.Children(r.Context(), v, requestDevice(r), season.Key, frontend.ItemQuery{Limit: 50, NoTotal: true})
		if err != nil {
			return err
		}
		converted, err = i.titles(r, v, token, episodes.Items)
		if err != nil {
			return err
		}
		out["episodes"] = converted
		break
	}
	return nil
}
