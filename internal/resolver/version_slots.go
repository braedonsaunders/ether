package resolver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// versionSlot names a configured choice, not a discovered release. Its encoded
// selector is stable across cache fills and is revalidated at admission.
type versionSlot struct {
	Kind    string `json:"k"`
	Height  int    `json:"h,omitempty"`
	Source  string `json:"s,omitempty"`
	Ordinal int    `json:"n,omitempty"`
}

const versionSlotPrefix = core.VersionSelectorPrefix

func (v versionSlot) id() string {
	b, _ := json.Marshal(v)
	return versionSlotPrefix + base64.RawURLEncoding.EncodeToString(b)
}
func parseVersionSlot(id string) (versionSlot, bool) {
	var v versionSlot
	if !strings.HasPrefix(id, versionSlotPrefix) || len(id) > 256 {
		return v, false
	}
	b, e := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, versionSlotPrefix))
	if e != nil {
		return v, false
	}
	e = json.Unmarshal(b, &v)
	return v, e == nil && v.id() == id
}

// VersionSlots describes the owner's menu without searching or resolving media.
// Cached labels are optional; count, order and selector identities never depend
// on search results. Only the normal admission path opens a selected slot.
func (s *Service) VersionSlots(ctx context.Context, req core.ResolveRequest, limit int, labels bool) ([]core.ReleaseVariant, error) {
	if req.Title == nil {
		return nil, fmt.Errorf("resolver: nil title")
	}
	// Local/plugin/extra sources own their version contracts and do not
	// admit indexer-backed logical selectors through this resolver.
	if kind := req.Title.Key.Kind(); kind != core.KindMovie && kind != core.KindEpisode {
		return nil, nil
	}
	q := s.qualityCfg()
	lib := s.libProfileForTitle(ctx, req.Title)
	req.Client, _ = applyClientRules(q, req.Client, req.User, titleMedia(req.Title), lib)
	profile := SelectProfileForTitle(q, req.Client, req.User, req.Title, lib)
	slots := s.configuredSlots(profile, req.Client, limit)
	var candidates []candidate
	var auto core.Release
	if labels {
		if results, ok := s.peekSearchCache(searchCacheKey(searchRequestFor(req.Title))); ok {
			pinID := ""
			if pin, _ := s.pinForResolve(req); pin != nil {
				pinID = pin.Release.ID
				results = withRelease(results, pin.Release)
			}
			ranked, _, err := s.rankCandidates(ctx, req, searchRequestFor(req.Title), profile, results, nil, pinID)
			if err == nil {
				candidates = pinFirst(acceptableVariantAudio(clearIdentityVariants(ranked)), pinID)
				if len(candidates) > 0 {
					auto = candidates[0].rel
				}
				if s.resolverCfg().OfferHigherVersions {
					if relaxed, ok := relaxedVariantProfile(profile, req.Client); ok {
						if pool, _, e := s.rankCandidates(ctx, req, searchRequestFor(req.Title), relaxed, results, nil, pinID); e == nil {
							candidates = acceptableVariantAudio(clearIdentityVariants(pool))
						}
					}
				}
			}
		}
	}
	picks := slotChoices(slots, candidates, auto)
	out := make([]core.ReleaseVariant, len(slots))
	wanted := wantedAudio(profile, req.User, s.catalogLanguage())
	for i, slot := range slots {
		height := slot.Height
		if height == 0 {
			height = profile.MaxHeight
		}
		if height <= 0 {
			height = 1080
		}
		label := slotLabel(slot)
		r := core.Release{ID: slot.id(), Name: label, Info: core.ReleaseInfo{Height: height, VideoCodec: "h264", AudioCodec: "aac", Source: core.SourceWEBDL}}
		info := estimateVariantInfo(req.Title, r, wanted)
		if i < len(picks) && picks[i].ID != "" {
			r = picks[i]
			info = estimateVariantInfo(req.Title, r, wanted)
			label = variantLabel(r, "alt")
			if i == 0 {
				label = variantLabel(r, "auto")
			}
		}
		out[i] = core.ReleaseVariant{ID: slot.id(), Label: label, Auto: i == 0, Edition: r.Info.Edition, Release: r, Info: info}
	}
	return out, nil
}
func (s *Service) configuredSlots(profile core.QualityProfile, caps core.ClientCaps, limit int) []versionSlot {
	if limit <= 0 || limit > core.MaxVariants {
		limit = core.MaxVariants
	}
	cfg := s.resolverCfg()
	alts := max(0, min(cfg.SameQualityAlternatives, maxAlternativesLimit))
	slots := []versionSlot{{Kind: "auto"}}
	add := func(v versionSlot) {
		if len(slots) < limit {
			slots = append(slots, v)
		}
	}
	if !cfg.AlternativesPerResolution && !cfg.AlternativesPerSource {
		for n := 0; n < alts; n++ {
			add(versionSlot{Kind: "alternative", Ordinal: n})
		}
		// The original menu reserves three distinct tier/cut choices after Auto.
		for n := 0; n < maxTierVariants-1; n++ {
			add(versionSlot{Kind: "distinct", Ordinal: n})
		}
		return slots
	}
	ceiling := profile.MaxHeight
	if cfg.OfferHigherVersions {
		ceiling = caps.MaxHeight
	}
	if ceiling <= 0 {
		ceiling = 2160
	}
	heights := []int{}
	for _, h := range []int{2160, 1080, 720} {
		if h <= ceiling {
			heights = append(heights, h)
		}
	}
	if len(heights) == 0 {
		heights = []int{min(480, ceiling)}
	}
	// Reserve a choice in each tier before another source/alternative can
	// consume the bounded menu. A busy 4K tier must not crowd out 720p.
	sources := []string{""}
	if cfg.AlternativesPerSource {
		sources = []string{"remux", "disc", "web"}
	}
	for _, source := range sources {
		for _, h := range heights {
			add(versionSlot{Kind: "tier", Height: h, Source: source})
		}
	}
	if cfg.AlternativesPerResolution {
		for n := 0; n < alts; n++ {
			for _, h := range heights {
				add(versionSlot{Kind: "tier", Height: h, Ordinal: n + 1})
			}
		}
	}
	return slots
}
func slotLabel(v versionSlot) string {
	switch v.Kind {
	case "auto":
		return "Auto"
	case "alternative":
		return fmt.Sprintf("Same-quality version %d", v.Ordinal+1)
	case "distinct":
		return fmt.Sprintf("Other version %d", v.Ordinal+1)
	default:
		label := fmt.Sprintf("%dp", v.Height)
		if v.Height == 2160 {
			label = "4K"
		}
		if v.Source != "" {
			label += " " + map[string]string{"remux": "Remux", "disc": "Blu-ray", "web": "WEB-DL"}[v.Source]
		}
		if v.Ordinal > 0 {
			label += fmt.Sprintf(" · Alternative %d", v.Ordinal)
		}
		return label
	}
}
func slotChoices(slots []versionSlot, ranked []candidate, auto core.Release) []core.Release {
	out := make([]core.Release, len(slots))
	if len(ranked) == 0 {
		return out
	}
	used := map[string]bool{}
	keys := map[string]bool{}
	for i, slot := range slots {
		for _, c := range ranked {
			r := c.rel
			if used[r.ID] {
				continue
			}
			match := false
			switch slot.Kind {
			case "auto":
				match = r.ID == auto.ID
			case "alternative":
				match = auto.ID != "" && variantKey(r.Info) == variantKey(auto.Info)
			case "distinct":
				match = !keys[variantKey(r.Info)]
			case "tier":
				match = r.Info.Height == slot.Height && (slot.Source == "" || sourceClass(r.Info.Source) == slot.Source)
			}
			if match {
				out[i] = r
				used[r.ID] = true
				keys[variantKey(r.Info)] = true
				break
			}
		}
	}
	return out
}
