package resolver

import (
	"context"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

// DiscoveryCandidate is local, name-based negative evidence only. It does not
// certify quality or fetch a provider. Feed and playback share this policy.
func (s *Service) DiscoveryCandidate(t *core.Title, r core.Release) string {
	return sourceSuitability(r, t, s.ProfileForTitle(context.Background(), feedClient, nil, t), false)
}

func sourceSuitability(r core.Release, t *core.Title, p core.QualityProfile, explicit bool) string {
	// Reparse the name: saved feed payloads can contain older parser results.
	info := releaseparse.Parse(r.Name)
	if !p.AllowCam {
		// AllowCam is the one pre-release switch: cinema recordings,
		// screeners, R5s and workprints all play only where it is on.
		switch {
		case info.Source == core.SourceCam || r.Info.Source == core.SourceCam:
			return "cam not allowed"
		case info.Source == core.SourceScreener || r.Info.Source == core.SourceScreener:
			return "screener not allowed"
		}
	}
	// A profile's specific Language is a hard gate, as in Radarr; an
	// explicitly chosen release (a pin) bypasses it, as it does the
	// minimum custom format score.
	if !explicit {
		if reason := profileLanguageRejection(info, p, t); reason != "" {
			return reason
		}
	}
	if explicit || !releaseparse.ExplicitDub(r.Name) || len(info.Languages) == 0 {
		return ""
	}
	want := append([]string(nil), p.PreferredLanguages...)
	// A dub into the profile's own language is what that language asks
	// for, not a mismatch.
	if lang, _ := profileLanguageMode(p); lang != "" {
		want = append(want, lang)
	}
	if t != nil && t.Language != "" {
		want = append(want, assumedLanguage(t))
	}
	if len(want) == 0 {
		return ""
	}
	for _, lang := range info.Languages {
		if lang == releaseparse.LanguageMulti || lang == releaseparse.LanguageDual {
			return ""
		}
		for _, pref := range want {
			if releaseparse.LanguageMatch(lang, pref) != releaseparse.LanguageNoMatch {
				return ""
			}
		}
	}
	return "dub audio does not match the original or preferred languages"
}

// KnownUnavailable returns only unexpired resolver negatives, in one local
// batch. It never searches, probes, or turns missing evidence into a negative.
func (s *Service) KnownUnavailable(ctx context.Context, keys []core.RatingKey) map[core.RatingKey]bool {
	out := map[core.RatingKey]bool{}
	for k := range s.KnownUnavailableSince(ctx, keys) {
		out[k] = true
	}
	return out
}
func (s *Service) KnownUnavailableSince(ctx context.Context, keys []core.RatingKey) map[core.RatingKey]time.Time {
	return s.knownUnavailableSince(ctx, keys, true)
}

// KnownUnavailableSinceCurrent reads durable observations without a node-local override.
func (s *Service) KnownUnavailableSinceCurrent(ctx context.Context, keys []core.RatingKey) map[core.RatingKey]time.Time {
	return s.knownUnavailableSince(ctx, keys, false)
}

func (s *Service) knownUnavailableSince(ctx context.Context, keys []core.RatingKey, memo bool) map[core.RatingKey]time.Time {
	out := map[core.RatingKey]time.Time{}
	now := s.now()
	if s.db != nil {
		for start := 0; start < len(keys); start += 500 {
			batch := keys[start:min(start+500, len(keys))]
			marks := make([]string, len(batch))
			args := make([]any, 0, len(batch)+1)
			for i, k := range batch {
				marks[i] = "?"
				args = append(args, k)
			}
			args = append(args, now.Unix())
			rows, err := s.db.QueryContext(ctx, `SELECT rating_key,checked_at,profile_key FROM resolver_availability WHERE rating_key IN (`+strings.Join(marks, ",")+`) AND available=0 AND expires_at>?`, args...)
			if err != nil {
				continue
			}
			policies := make(map[core.RatingKey]string)
			for rows.Next() {
				var k core.RatingKey
				var checked int64
				var policy string
				if rows.Scan(&k, &checked, &policy) == nil {
					policies[k] = policy
					out[k] = time.Unix(checked, 0)
				}
			}
			rows.Close()
			for k, policy := range policies {
				if policy != s.availabilityProfileKey(ctx, k) {
					delete(out, k)
				}
			}
		}
	}
	if !memo {
		return out
	}
	for _, k := range keys {
		s.mu.RLock()
		e, ok := s.avail[int64(k)]
		s.mu.RUnlock()
		if ok {
			if !e.available && now.Before(e.expires) && e.policy == s.availabilityProfileKey(ctx, k) {
				out[k] = e.checked
			} else {
				delete(out, k)
			}
		}
	}
	return out
}
