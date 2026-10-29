package plexguid

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Lookup reverses a verified Plex movie/show identity. It returns an identity,
// not a visibility decision: callers must load and authorize the title.
// A cold lookup accepts only an exact provider GUID with one TMDB identity.
func (s *Service) Lookup(ctx context.Context, guid string) (core.RatingKey, error) {
	kind := "movie"
	keyFor := core.MovieKey
	if validKindGUID(guid, "show") {
		kind, keyFor = "show", core.ShowKey
	} else if !validKindGUID(guid, kind) {
		return 0, core.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key FROM plexguid_mappings WHERE guid=? AND checked_at>? LIMIT 2`, guid, time.Now().Add(-cacheTTL).Unix())
	if err != nil {
		return 0, err
	}
	var cached core.RatingKey
	for rows.Next() {
		var k core.RatingKey
		if err = rows.Scan(&k); err != nil {
			break
		}
		if !k.Valid() || k != keyFor(k.TMDB()) || cached != 0 {
			err = fmt.Errorf("ambiguous Plex identity")
			break
		}
		cached = k
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return 0, err
	}
	if cached != 0 {
		return cached, nil
	}
	if s.o.Token == nil || s.o.Token() == "" {
		return 0, core.ErrNotFound
	}
	items, err := s.getItems(ctx, "/library/metadata/"+guidHex(guid, kind), url.Values{"includeGuids": {"1"}})
	if err != nil {
		return 0, err
	}
	var found core.RatingKey
	for _, item := range items {
		if item.GUID != guid || item.Type != kind {
			continue
		}
		for _, id := range item.IDs {
			if !strings.HasPrefix(id.ID, "tmdb://") {
				continue
			}
			raw := strings.TrimPrefix(id.ID, "tmdb://")
			n, e := strconv.ParseInt(raw, 10, 64)
			if e != nil || n <= 0 || strconv.FormatInt(n, 10) != raw {
				continue
			}
			k := keyFor(n)
			if !k.Valid() || k.TMDB() != n || k.Kind() != keyFor(1).Kind() {
				continue
			}
			if found != 0 && found != k {
				return 0, fmt.Errorf("ambiguous Plex identity")
			}
			found = k
		}
	}
	if found == 0 {
		return 0, core.ErrNotFound
	}
	return found, s.storeMapping(ctx, found, guid, time.Now().Unix())
}
