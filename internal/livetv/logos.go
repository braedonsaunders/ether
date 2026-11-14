package livetv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

func logoName(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, s)
}

// matchLogos is called on the import path, never while serving the guide.
// The owner supplies the index; Ether ships none. A failed index leaves the
// channels intact and the existing name fallback remains available.
func (s *Service) matchLogos(ctx context.Context, cs []Channel) {
	if s.o.LogoIndexURL == nil {
		return
	}
	raw := strings.TrimSpace(s.o.LogoIndexURL())
	if raw == "" {
		return
	}
	data, err := s.fetchLogoIndex(ctx, raw)
	if err != nil {
		return
	}
	var entries []struct {
		Name    string `json:"name"`
		Logo    string `json:"logo"`
		URL     string `json:"url"`
		Channel string `json:"channel"`
	}
	if json.Unmarshal(data, &entries) != nil {
		return
	}
	logos := map[string]string{}
	ambiguous := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name
		if name == "" {
			name = entry.Channel
		}
		key := logoName(name)
		logo := entry.Logo
		if logo == "" {
			logo = entry.URL
		}
		if key == "" || !validHTTP(logo) {
			continue
		}
		if prior, ok := logos[key]; ok && prior != logo {
			ambiguous[key] = true
		}
		logos[key] = logo
	}
	for i := range cs {
		if cs[i].Logo != "" {
			continue
		}
		key := logoName(cs[i].Name)
		if !ambiguous[key] {
			cs[i].Logo = logos[key]
			cs[i].LogoMatched = cs[i].Logo != ""
		}
	}
}

// fetchLogoIndex downloads the owner's logo index (8 MiB at most) through
// the guarded client: public addresses only, no referrer.
func (s *Service) fetchLogoIndex(ctx context.Context, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("livetv: invalid logo index URL")
	}
	client := s.o.LogoHTTP
	if client == nil {
		client = (&safefetch.Guard{}).Client(egress.Client(egress.CapLiveTV, 20*time.Second))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("livetv: logo index from %s: download failed", u.Host)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("livetv: logo index from %s: HTTP %d", u.Host, resp.StatusCode)
	}
	const limit = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(data) > limit {
		return nil, fmt.Errorf("livetv: logo index from %s is unreadable or too large", u.Host)
	}
	return data, nil
}
