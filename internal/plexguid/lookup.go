package plexguid

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ErrAmbiguous is returned when one Plex GUID is stored for more than one
// local title, or the stored title's kind does not match the GUID kind.
var ErrAmbiguous = errors.New("plexguid: ambiguous identity")

// ExactKey returns the single local title already stored for guid.
// It does not call Plex and it does not derive an episode from a show's
// TMDB id. A miss is core.ErrNotFound.
func (s *Service) ExactKey(ctx context.Context, guid string) (core.RatingKey, error) {
	kind, ok := guidKind(guid)
	if !ok {
		return 0, core.ErrNotFound
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rating_key FROM plexguid_mappings WHERE guid=? LIMIT 2`, guid)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var found core.RatingKey
	n := 0
	for rows.Next() {
		var k core.RatingKey
		if err = rows.Scan(&k); err != nil {
			return 0, err
		}
		found = k
		n++
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, core.ErrNotFound
	}
	if n != 1 || !found.Valid() || found.Kind() != kind {
		return 0, ErrAmbiguous
	}
	// A legacy row on a namesake title is not a usable identity: withhold it
	// and let the priority repair prove or drop it.
	if s.withheldLegacy(ctx, found) {
		return 0, core.ErrNotFound
	}
	// A served identity predating the credit check is re-verified soon.
	s.recheckStaleCredits(ctx, found)
	return found, nil
}

func guidKind(guid string) (core.KeyKind, bool) {
	switch {
	case validKindGUID(guid, "movie"):
		return core.KindMovie, true
	case validKindGUID(guid, "show"):
		return core.KindShow, true
	case validKindGUID(guid, "season"):
		return core.KindSeason, true
	case validKindGUID(guid, "episode"):
		return core.KindEpisode, true
	default:
		return 0, false
	}
}

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
	rows, err := s.db.QueryContext(ctx, `SELECT m.rating_key FROM plexguid_mappings m WHERE m.guid=? AND m.checked_at>? AND `+identityVerified+` LIMIT 2`, guid, time.Now().Add(-cacheTTL).Unix())
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
		if s.withheldLegacy(ctx, cached) {
			return 0, core.ErrNotFound
		}
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
	// The reverse direction must not be looser than forward resolution: the
	// canonical record must carry the same title and a year that cannot be a
	// namesake, and the identity it proves is what gets stored. Reuse the
	// record already fetched rather than calling Plex a second time.
	id := s.verifyItems(ctx, found, kind, guid, items, s.localIdentity(ctx, found))
	if id.GUID == "" {
		return 0, core.ErrNotFound
	}
	return found, s.storeMappingIdentity(ctx, found, id, time.Now().Unix(), 0, "")
}

// Verified is one stored title whose plex:// GUID passed verification.
type Verified struct {
	Key  core.RatingKey
	GUID string
}

// VerifiedRange pages through stored, verified identities with
// after < key < below, in key order, at most limit at a time. It reads only
// the local tables (primary-key range scan) and never calls Plex. Season and
// episode identities are included only once their parentage is recorded, as
// Resolve requires.
func (s *Service) VerifiedRange(ctx context.Context, after, below core.RatingKey, limit int) ([]Verified, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.rating_key, m.guid, `+identityExists+`, `+staleCreditsRule+` FROM plexguid_mappings m
		WHERE m.rating_key>? AND m.rating_key<? AND m.guid<>'' AND `+identityVerified+`
		  AND (m.rating_key<3000000000000000 OR m.rating_key>=5000000000000000
		       OR EXISTS (SELECT 1 FROM plexguid_parentage p WHERE p.rating_key=m.rating_key))
		ORDER BY m.rating_key LIMIT ?`, int64(after), int64(below), limit)
	if err != nil {
		return nil, err
	}
	// Read every row before consulting the namesake lookup: the latter issues
	// its own queries, and a cursor must not be open across them.
	type candidate struct {
		key          core.RatingKey
		guid         string
		hasIdentity  bool
		staleCredits bool
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.key, &c.guid, &c.hasIdentity, &c.staleCredits); err != nil {
			rows.Close()
			return nil, err
		}
		cands = append(cands, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]Verified, 0, len(cands))
	for _, c := range cands {
		// A legacy row on a namesake title must not be imported as this
		// title's identity: the guid may name the other work. Withhold it and
		// enqueue it for priority repair like the other read paths.
		if !c.hasIdentity && s.hasNamesake(ctx, c.key) {
			s.enqueuePriority(c.key)
			continue
		}
		// A served row whose identity predates the credit check is re-verified
		// soon but still imported.
		if c.staleCredits {
			s.enqueuePriority(c.key)
		}
		if kind, ok := guidKind(c.guid); ok && kind == c.key.Kind() {
			out = append(out, Verified{Key: c.key, GUID: c.guid})
		} else {
			// Keep the page boundary moving past a malformed row.
			out = append(out, Verified{Key: c.key})
		}
	}
	return out, nil
}
