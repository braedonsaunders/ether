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

func crewPeople(t frontend.Title) []frontend.Person {
	out := []frontend.Person{}
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
			out = append(out, p)
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

// cast and crew use only portraits selected with the authorized title detail.
func (i *instance) cast(r *http.Request, token string, d frontend.ItemDetail) ([]object, error) {
	out := make([]object, 0, len(d.Title.Cast))
	for _, person := range d.Title.Cast {
		out = append(out, object{"name": person.Name, "role": person.Role})
	}
	return i.creditArtwork(r, token, d, d.Title.Cast, out)
}

func (i *instance) crew(r *http.Request, token string, d frontend.ItemDetail) ([]object, error) {
	people := crewPeople(d.Title)
	out := make([]object, 0, len(people))
	for _, person := range people {
		entry := object{"name": person.Name}
		if person.Role != "" {
			entry["role"] = person.Role
		}
		out = append(out, entry)
	}
	return i.creditArtwork(r, token, d, people, out)
}

// Each batch proves current credit membership and admits portrait grants;
// no person lookup or metadata refresh is needed here.
func (i *instance) creditArtwork(r *http.Request, token string, d frontend.ItemDetail, people []frontend.Person, out []object) ([]object, error) {
	portraits := map[string]frontend.Image{}
	if d.ImagesAuthoritative {
		for _, image := range d.Images {
			if image.Kind == frontend.ImagePerson && image.Subject != "" && image.Path != "" {
				if _, exists := portraits[image.Subject]; !exists {
					portraits[image.Subject] = image
				}
			}
		}
	}
	targets := []frontend.PersonImageTarget{}
	positions := []int{}
	for n, person := range people {
		if person.TMDB <= 0 {
			continue
		}
		if image, ok := portraits["tmdb:"+strconv.FormatInt(person.TMDB, 10)]; ok {
			targets = append(targets, frontend.PersonImageTarget{Item: d.Title.Key, Image: image})
			positions = append(positions, n)
		}
	}
	signer, ok := i.host.ImageGrants.(frontend.PersonImageGrantBatch)
	if !ok {
		return out, nil
	}
	for start := 0; start < len(targets); start += 100 {
		end := min(start+100, len(targets))
		secrets, err := signer.IssuePersonImages(r.Context(), token, requestDevice(r), targets[start:end])
		if err != nil {
			return nil, err
		}
		if len(secrets) != end-start {
			return nil, frontend.ErrNotAvailable
		}
		for n, secret := range secrets {
			if secret == "" {
				continue
			}
			image := targets[start+n].Image
			out[positions[start+n]]["artwork"] = object{"id": hash(image.Path), "kind": frontend.ImagePerson, "tag": image.Tag, "sizes": []object{imageSize(image, secret)}, "blurHash": image.BlurHash, "thumbHash": image.ThumbHash}
		}
	}
	return out, nil
}
