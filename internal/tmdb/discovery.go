package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var languageRE = regexp.MustCompile(`^[A-Za-z]{2}(-[A-Za-z]{2})?$`)

// Localized changes presentation only: certification, adult flag and identity
// remain the canonical catalogue's responsibility, never translated API fields.
func (c *Client) Localized(ctx context.Context, t *core.Title, language string) (*core.Title, error) {
	out := *t
	if !languageRE.MatchString(language) {
		return &out, nil
	}
	path, e := discoveryPath(t)
	if e != nil {
		return &out, e
	}
	var data struct {
		Title    string  `json:"title"`
		Name     string  `json:"name"`
		Overview string  `json:"overview"`
		Tagline  string  `json:"tagline"`
		Poster   string  `json:"poster_path"`
		Backdrop string  `json:"backdrop_path"`
		Images   *Images `json:"images"`
	}
	q := url.Values{"language": {language}, "append_to_response": {"images"}, "include_image_language": {strings.Split(language, "-")[0] + ",null"}}
	if e = c.getJSON(ctx, path, q, &data); e != nil {
		return &out, e
	}
	if data.Title != "" {
		out.Title = data.Title
	} else if data.Name != "" {
		out.Title = data.Name
	}
	if data.Overview != "" {
		out.Summary = data.Overview
	}
	if data.Tagline != "" {
		out.Tagline = data.Tagline
	}
	if data.Poster != "" {
		out.PosterPath = data.Poster
	}
	if data.Backdrop != "" {
		out.BackdropPath = data.Backdrop
	}
	if data.Images != nil {
		for _, img := range data.Images.Backdrops {
			if img.ISO639 == "" {
				out.BackdropPath = img.FilePath
				break
			}
		}
		for _, img := range data.Images.Logos {
			if img.ISO639 == strings.Split(language, "-")[0] {
				out.LogoPath = img.FilePath
				break
			}
		}
	}
	return &out, nil
}
func discoveryPath(t *core.Title) (string, error) {
	switch t.Type {
	case core.TypeMovie:
		return fmt.Sprintf("/movie/%d", t.IDs.TMDB), nil
	case core.TypeShow:
		return fmt.Sprintf("/tv/%d", t.IDs.TMDB), nil
	case core.TypeSeason:
		return fmt.Sprintf("/tv/%d/season/%d", t.ShowKey.TMDB(), t.Season), nil
	case core.TypeEpisode:
		return fmt.Sprintf("/tv/%d/season/%d/episode/%d", t.ShowKey.TMDB(), t.Season, t.Episode), nil
	}
	return "", fmt.Errorf("unsupported localization type")
}
func (c *Client) ArtworkVariants(ctx context.Context, t *core.Title, language string) (*Images, error) {
	p, q, e := artworkVariantsRequest(t, language)
	if e != nil {
		return nil, e
	}
	var out Images
	e = c.getJSON(ctx, p, q, &out)
	return &out, e
}

func artworkVariantsRequest(t *core.Title, language string) (string, url.Values, error) {
	if t == nil {
		return "", nil, fmt.Errorf("tmdb: missing title")
	}
	p, e := discoveryPath(t)
	if e != nil {
		return "", nil, e
	}
	lang := strings.Split(language, "-")[0]
	if lang == "" {
		lang = "en"
	}
	return p + "/images", url.Values{"include_image_language": {lang + ",null"}}, nil
}

// CachedArtworkVariants returns only fresh cached data. It never contacts
// TMDB, waits for an in-flight fetch, or exposes stale data as authoritative.
// A miss can be filled separately using ArtworkVariants.
func (c *Client) CachedArtworkVariants(t *core.Title, language string) (*Images, bool) {
	p, q, err := artworkVariantsRequest(t, language)
	if err != nil {
		return nil, false
	}
	cfg := c.settings()
	if cfg.key == "" {
		return nil, false
	}
	key := strconv.FormatUint(cfg.generation, 10) + ":" + c.buildURL(p, q, cfg)
	c.mu.Lock()
	entry, ok := c.cache.get(key)
	if ok && c.generation == cfg.generation && time.Now().Before(entry.expires) {
		entry.body = append([]byte(nil), entry.body...)
	} else {
		ok = false
	}
	c.mu.Unlock()
	if !ok {
		return nil, false
	}
	var out Images
	if json.Unmarshal(entry.body, &out) != nil {
		return nil, false
	}
	return &out, true
}
