package plexmeta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type providerItem struct {
	GUID                  string  `json:"guid"`
	Type                  string  `json:"type"`
	Title                 string  `json:"title"`
	Summary               string  `json:"summary"`
	Tagline               string  `json:"tagline"`
	Year                  int     `json:"year"`
	OriginallyAvailableAt string  `json:"originallyAvailableAt"`
	ContentRating         string  `json:"contentRating"`
	Studio                string  `json:"studio"`
	Thumb                 string  `json:"thumb"`
	Art                   string  `json:"art"`
	Rating                float64 `json:"rating"`
	AudienceRating        float64 `json:"audienceRating"`
	RatingImage           string  `json:"ratingImage"`
	AudienceRatingImage   string  `json:"audienceRatingImage"`
	Index                 *int    `json:"index"`
	ParentIndex           *int    `json:"parentIndex"`
	Genre                 []struct {
		Tag string `json:"tag"`
	} `json:"Genre"`
	Role     []providerPerson `json:"Role"`
	Director []providerPerson `json:"Director"`
	Writer   []providerPerson `json:"Writer"`
}

type providerPerson struct {
	Tag   string `json:"tag"`
	Role  string `json:"role"`
	Thumb string `json:"thumb"`
}

type providerPage struct {
	Size      int            `json:"size"`
	TotalSize int            `json:"totalSize"`
	Metadata  []providerItem `json:"Metadata"`
}

func (s *Service) refresh(ctx context.Context, k core.RatingKey) error {
	guid := s.o.GUID(ctx, k)
	if guid == "" {
		return nil
	}
	hex, kind := guidHex(guid)
	if hex == "" {
		return nil
	}
	items, _, err := s.getItems(ctx, "/library/metadata/"+url.PathEscape(hex), nil, 0)
	if err != nil {
		return err
	}
	now := s.o.Now()
	var found Overlay
	for _, item := range items {
		if item.GUID != "" && item.GUID != guid {
			continue
		}
		if item.Type != "" && item.Type != kind {
			continue
		}
		found = overlayFrom(item, s.o.BaseURL)
		break
	}
	if err := s.store(ctx, k, guid, found, now); err != nil {
		return err
	}
	return nil
}

func overlayFrom(item providerItem, base string) Overlay {
	o := Overlay{
		ContentRating:       strings.TrimSpace(item.ContentRating),
		Year:                item.Year,
		Summary:             strings.TrimSpace(item.Summary),
		Tagline:             strings.TrimSpace(item.Tagline),
		Studio:              strings.TrimSpace(item.Studio),
		ThumbURL:            absURL(base, item.Thumb),
		ArtURL:              absURL(base, item.Art),
		Rating:              item.Rating,
		AudienceRating:      item.AudienceRating,
		RatingImage:         strings.TrimSpace(item.RatingImage),
		AudienceRatingImage: strings.TrimSpace(item.AudienceRatingImage),
	}
	if t, err := time.Parse("2006-01-02", item.OriginallyAvailableAt); err == nil {
		o.AvailableAt = t
		if o.Year == 0 {
			o.Year = t.Year()
		}
	}
	for _, g := range item.Genre {
		if tag := strings.TrimSpace(g.Tag); tag != "" {
			o.Genres = append(o.Genres, tag)
		}
	}
	o.Cast = peopleFrom(item.Role)
	o.Directors = peopleFrom(item.Director)
	o.Writers = peopleFrom(item.Writer)
	return o
}

func peopleFrom(in []providerPerson) []Person {
	out := make([]Person, 0, len(in))
	for _, p := range in {
		if name := strings.TrimSpace(p.Tag); name != "" {
			out = append(out, Person{Name: name, Role: strings.TrimSpace(p.Role), Thumb: strings.TrimSpace(p.Thumb)})
		}
	}
	return out
}

func absURL(base, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "https://") || strings.HasPrefix(raw, "http://") {
		return raw
	}
	if strings.HasPrefix(raw, "/") {
		return strings.TrimRight(base, "/") + raw
	}
	return raw
}

func guidHex(guid string) (hex, kind string) {
	for _, k := range []string{"movie", "show", "season", "episode"} {
		prefix := "plex://" + k + "/"
		if strings.HasPrefix(guid, prefix) && len(guid) == len(prefix)+24 {
			return strings.TrimPrefix(guid, prefix), k
		}
	}
	return "", ""
}

func (s *Service) getItems(ctx context.Context, path string, q url.Values, start int) ([]providerItem, int, error) {
	u := strings.TrimRight(s.o.BaseURL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("Plex metadata request invalid")
	}
	if s.o.Token != nil {
		req.Header.Set("X-Plex-Token", s.o.Token())
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Product", "Ether")
	req.Header.Set("X-Plex-Client-Identifier", s.clientID())
	req.Header.Set("X-Plex-Container-Start", strconv.Itoa(start))
	req.Header.Set("X-Plex-Container-Size", strconv.Itoa(pageSize))
	client := *s.o.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		s.brk.Failure(0)
		return nil, 0, fmt.Errorf("Plex metadata unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		s.brk.Failure(retryAfter(resp.Header.Get("Retry-After")))
		return nil, 0, fmt.Errorf("Plex metadata HTTP %d", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		s.brk.Success()
		return nil, 0, nil
	}
	if resp.StatusCode != http.StatusOK {
		s.brk.Failure(0)
		return nil, 0, fmt.Errorf("Plex metadata HTTP %d", resp.StatusCode)
	}
	var body struct {
		Container providerPage `json:"MediaContainer"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		s.brk.Failure(0)
		return nil, 0, fmt.Errorf("Plex metadata response invalid")
	}
	s.brk.Success()
	total := body.Container.TotalSize
	if total == 0 {
		total = body.Container.Size
	}
	return body.Container.Metadata, total, nil
}

func retryAfter(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	if when, err := http.ParseTime(raw); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// ImageHost reports whether host is a Plex artwork CDN Ether may proxy.
func ImageHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if i := strings.LastIndexByte(h, ':'); i > 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	switch h {
	case "metadata-static.plex.tv", "image.plex.tv", "images.plex.tv", "metadata.provider.plex.tv":
		return true
	}
	return false
}
