package resolver

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// A series year in a release name disambiguates shows that share a title
// (a reboot, a remake, a namesake); it is not proof of identity on its own.
// Indexers often list a show under the wrong year. A release whose year
// differs from the show's is rejected only when that year names a different
// show with the same title. Otherwise the year is the indexer's mistake: the
// release is accepted and ranks after correctly-labelled releases of the
// same resolution. This mirrors how Sonarr treats a year in a release title.

// seriesYearTTL bounds how long a catalogue answer is reused.
const seriesYearTTL = 24 * time.Hour

// maxSeriesYearLookups bounds catalogue lookups for one ranking pass.
const maxSeriesYearLookups = 4

type seriesYearVerdict struct {
	conflict bool
	at       time.Time
}

type seriesYears struct {
	mu   sync.Mutex
	seen map[string]seriesYearVerdict
}

func seriesYearKey(show core.RatingKey, year int) string {
	return strconv.FormatInt(int64(show), 10) + "/" + strconv.Itoa(year)
}

// prepareSeriesYears answers, before ranking, whether each differing series
// year among the results names another show. Lookups run once per show and
// year (cached), never per release; failures stay unanswered and reject.
func (s *Service) prepareSeriesYears(ctx context.Context, t *core.Title, sreq core.SearchRequest, results []core.Release) {
	if s.seriesYearConflict == nil || t == nil || sreq.Type != core.TypeEpisode || sreq.Year == 0 || t.ShowKey == 0 {
		return
	}
	years := map[int]bool{}
	for _, rel := range results {
		y := releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year)
		if !releaseparse.SeriesYearMatches(y, sreq.Year) {
			years[y] = true
		}
	}
	now := s.now()
	lookups := 0
	for y := range years {
		key := seriesYearKey(t.ShowKey, y)
		s.years.mu.Lock()
		v, ok := s.years.seen[key]
		s.years.mu.Unlock()
		if ok && now.Sub(v.at) < seriesYearTTL {
			continue
		}
		if lookups == maxSeriesYearLookups {
			break
		}
		lookups++
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		conflict, err := s.seriesYearConflict(lctx, t.ShowKey, sreq.Title, y)
		cancel()
		if err != nil {
			s.log.Debug("series year lookup failed; keeping the year check", "show", t.ShowKey, "year", y, "err", err)
			continue
		}
		s.years.mu.Lock()
		if s.years.seen == nil {
			s.years.seen = map[string]seriesYearVerdict{}
		}
		if len(s.years.seen) > 4096 {
			for k, old := range s.years.seen {
				if now.Sub(old.at) >= seriesYearTTL {
					delete(s.years.seen, k)
				}
			}
		}
		s.years.seen[key] = seriesYearVerdict{conflict: conflict, at: now}
		s.years.mu.Unlock()
	}
}

// seriesYearAccepted reports a differing series year the catalogue showed
// names no other show. Unknown answers keep the year check.
func (s *Service) seriesYearAccepted(show core.RatingKey, year int) bool {
	s.years.mu.Lock()
	defer s.years.mu.Unlock()
	v, ok := s.years.seen[seriesYearKey(show, year)]
	return ok && !v.conflict && s.now().Sub(v.at) < seriesYearTTL
}

// seriesYearGuessed reports an episode release accepted despite a series
// year that differs from the show's.
func seriesYearGuessed(rel core.Release, sreq core.SearchRequest) bool {
	return sreq.Type == core.TypeEpisode && !releaseparse.SeriesYearMatches(releaseparse.EpisodeSeriesYear(rel.Name, rel.Info.Year), sreq.Year)
}
