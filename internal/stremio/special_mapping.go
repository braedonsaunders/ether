package stremio

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// SpecialEpisodes translates a catalogue special into the numbering used by
// IMDb stream routes. A missing or ambiguous title must return ok=false.
type SpecialEpisodes interface {
	LookupSpecial(context.Context, core.SearchRequest) (episode int, ok bool)
}

type specialEntry struct {
	until  time.Time
	videos []specialVideo
}
type specialVideo struct {
	Episode int    `json:"episode"`
	Season  int    `json:"season"`
	Name    string `json:"name"`
	Title   string `json:"title"`
}

// SpecialMapper owns a small, demand-loaded metadata cache. The composition
// root supplies both the metadata origin and its HTTP transport; constructing
// it performs no network or background work.
type SpecialMapper struct {
	client  *http.Client
	base    string
	mu      sync.Mutex
	entries map[string]specialEntry
	loading map[string]bool
}

func NewSpecialMapper(client *http.Client, base string) *SpecialMapper {
	return &SpecialMapper{client: client, base: strings.TrimRight(base, "/"), entries: map[string]specialEntry{}, loading: map[string]bool{}}
}

func (m *SpecialMapper) LookupSpecial(ctx context.Context, req core.SearchRequest) (int, bool) {
	id := IMDbID(req.IDs.IMDB)
	if m == nil || m.client == nil || id == "" || strings.TrimSpace(req.EpisodeTitle) == "" {
		return 0, false
	}
	m.mu.Lock()
	entry, cached := m.entries[id]
	if cached && time.Now().Before(entry.until) {
		m.mu.Unlock()
		return matchSpecialTitle(req, entry.videos)
	}
	if m.loading[id] {
		m.mu.Unlock()
		return 0, false
	}
	m.loading[id] = true
	m.mu.Unlock()
	videos, ok := m.fetch(ctx, id)
	ttl := time.Hour
	if ok {
		ttl = 24 * time.Hour
	}
	m.mu.Lock()
	delete(m.loading, id)
	if len(m.entries) >= 256 {
		for key := range m.entries {
			delete(m.entries, key)
			break
		}
	}
	m.entries[id] = specialEntry{until: time.Now().Add(ttl), videos: videos}
	m.mu.Unlock()
	return matchSpecialTitle(req, videos)
}

func (m *SpecialMapper) fetch(ctx context.Context, id string) ([]specialVideo, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.base+"/meta/series/"+id+".json", nil)
	if err != nil {
		return nil, false
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var answer struct {
		Meta struct {
			Videos []specialVideo `json:"videos"`
		} `json:"meta"`
	}
	if json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 4<<20)).Decode(&answer) != nil {
		return nil, false
	}
	videos := []specialVideo{}
	for _, v := range answer.Meta.Videos {
		if v.Season == 0 && v.Episode > 0 {
			if v.Name == "" {
				v.Name = v.Title
			}
			videos = append(videos, v)
		}
	}
	return videos, true
}

// A unique exact title wins. Otherwise require at least half of both titles
// and two shared meaningful words. Numbers remain meaningful: drama 1 and
// drama 3 cannot be mapped by their shared series prefix alone.
func matchSpecialTitle(req core.SearchRequest, videos []specialVideo) (int, bool) {
	shows := append([]string{req.Title, req.OriginalTitle}, req.AlternativeTitles...)
	want := releaseparse.SpecialTitleWords(req.EpisodeTitle, shows...)
	meaningful := false
	for w := range want {
		if _, err := strconv.Atoi(w); err != nil {
			meaningful = true
		}
	}
	if !meaningful {
		return 0, false
	}
	best, number, tied := 0.0, 0, false
	for _, v := range videos {
		got := releaseparse.SpecialTitleWords(v.Name, shows...)
		shared := 0
		for word := range want {
			if got[word] {
				shared++
			}
		}
		exact := shared == len(want) && shared == len(got)
		if !exact && (shared < 2 || shared*2 < len(want) || shared*2 < len(got)) {
			continue
		}
		if releaseparse.SpecialTitleNumberConflict(want, got) {
			continue
		}
		score := float64(2*shared) / float64(len(want)+len(got))
		if score > best {
			best, number, tied = score, v.Episode, false
		} else if score == best && number != v.Episode {
			tied = true
		}
	}
	return number, number > 0 && !tied
}
