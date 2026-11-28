package resolver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/vault"
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
	req = s.withClientDeliveryBudget(req)
	if labels && core.LocalResponse(ctx) {
		versions, err := s.detailVariantRead(req, limit, true)
		if err != nil || (len(versions) > 0 && versions[0].Release.ID != versions[0].ID) {
			return versions, err
		}
		if req.Title != nil && core.ColdDetail(ctx, req.Title.Key) {
			// The already-cached search can supply tracks before a background
			// version snapshot exists. Plugin ranking remains off the read path.
			if results, ok := s.peekSearchCache(searchCacheKey(searchRequestFor(req.Title))); ok && len(results) > 0 {
				dec := decideProfile(s.qualityCfg(), req.Client, req.User, titleTarget(req.Title, s.membershipFor(ctx, req.Title)))
				if !usesPluginRanking(dec.profile) {
					if cached, err := s.versionSlotsWithResults(ctx, req, limit, true, results, true); err == nil && len(cached) > 0 {
						return cached, nil
					}
				}
			}
		}
		if len(versions) > 0 {
			return versions, nil
		}
		// The configured menu needs no release ranking. Missing labels never
		// remove or renumber a choice while background discovery fills them.
		return s.versionSlots(ctx, req, limit, false)
	}
	versions, partialLive, err := s.evalVersionSlots(ctx, req, limit, labels, true)
	if err == nil && labels && len(versions) > 0 && !partialLive {
		s.cacheDetailVariants(req, limit, true, versions)
	}
	return versions, err
}

func (s *Service) playedReleaseForSlots(req core.ResolveRequest) (core.Release, bool) {
	if req.PlayedRelease == "" {
		return core.Release{}, false
	}
	var rel core.Release
	var ok bool
	if req.PlayedReleaseRecord != nil && req.PlayedReleaseRecord.ID == req.PlayedRelease {
		rel, ok = *req.PlayedReleaseRecord, true
	} else {
		rel, ok = s.LookupRelease(req.PlayedRelease)
	}
	if !ok || rel.ID == "" || s.isBadRelease(rel) || (req.Title != nil && s.rejectedFor(req.Title.Key, rel)) {
		return core.Release{}, false
	}
	return rel, true
}

func (s *Service) versionSlots(ctx context.Context, req core.ResolveRequest, limit int, labels bool) ([]core.ReleaseVariant, error) {
	versions, _, err := s.evalVersionSlots(ctx, req, limit, labels, false)
	return versions, err
}

func (s *Service) evalVersionSlots(ctx context.Context, req core.ResolveRequest, limit int, labels, allowLive bool) ([]core.ReleaseVariant, bool, error) {
	if req.Title == nil {
		return nil, false, fmt.Errorf("resolver: nil title")
	}
	// Local/plugin/extra sources own their version contracts and do not
	// admit indexer-backed logical selectors through this resolver.
	if kind := req.Title.Key.Kind(); kind != core.KindMovie && kind != core.KindEpisode {
		return nil, false, nil
	}
	var results []core.Release
	searchWarm := false
	partialLive := false
	if labels {
		key := searchCacheKey(searchRequestFor(req.Title))
		var ok bool
		results, ok = s.peekSearchCache(key)
		searchWarm = ok
		if !ok && allowLive {
			if live := s.peekLiveSearch(key); len(live) > 0 {
				results = live
				partialLive = true
			}
		}
	}
	versions, err := s.versionSlotsWithResults(ctx, req, limit, labels, results, searchWarm)
	return versions, partialLive, err
}

func (s *Service) versionSlotsWithResults(ctx context.Context, req core.ResolveRequest, limit int, labels bool, results []core.Release, searchWarm bool) ([]core.ReleaseVariant, error) {
	if req.Title == nil {
		return nil, fmt.Errorf("resolver: nil title")
	}
	// Local/plugin/extra sources own their version contracts and do not
	// admit indexer-backed logical selectors through this resolver.
	if kind := req.Title.Key.Kind(); kind != core.KindMovie && kind != core.KindEpisode {
		return nil, nil
	}
	q := s.qualityCfg()
	dec := decideProfile(q, req.Client, req.User, titleTarget(req.Title, s.membershipFor(ctx, req.Title)))
	req.Client = dec.caps
	profile := dec.profile
	slots := s.configuredSlots(profile, req.Client, limit)
	var candidates []candidate
	var auto core.Release
	var autoRemembered bool
	var autoReason core.AutoSelectionReason
	var titleVC *ViewerChoice
	ownerPin, hasOwnerPin := s.PinFor(req.Title.Key)
	if req.Client.Download {
		hasOwnerPin = false
	}
	if hasOwnerPin && ownerPin.Fallback && !s.preferenceEligible(req.Title.Key, ownerPin.Release) {
		// A preference inside its failure memory is not tried; Auto is
		// whatever normal ranking picks.
		hasOwnerPin = false
	}
	if labels {
		titleVC = s.viewerTitleChoiceForResolve(req, nil)
		showVC := s.viewerShowChoiceForResolve(req)
		playedRel, hasPlayed := s.playedReleaseForSlots(req)
		if !searchWarm && len(results) == 0 {
			if titleVC != nil {
				results = withRelease(results, titleVC.Release)
			}
			if hasOwnerPin && !s.isBadRelease(ownerPin.Release) {
				results = withRelease(results, ownerPin.Release)
			}
			if hasPlayed {
				results = withRelease(results, playedRel)
			}
		}
		if len(results) > 0 {
			pinID := ""
			if hasOwnerPin && !s.isBadRelease(ownerPin.Release) {
				pinID = ownerPin.Release.ID
				results = withRelease(results, ownerPin.Release)
			}
			if titleVC != nil {
				results = withRelease(results, titleVC.Release)
			}
			if hasPlayed {
				results = withRelease(results, playedRel)
			}
			sreq := s.cachedSearchRequest(req.Title)
			ranked, _, err := s.rankCandidatesLocal(ctx, req, sreq, profile, results, nil, pinID, true)
			if err == nil {
				strictCands := pinFirst(acceptableVariantAudio(clearIdentityVariants(ranked)), pinID)
				candidates = strictCands
				matchPool := strictCands
				if s.resolverCfg().OfferHigherVersions {
					if relaxed, ok := relaxedVariantProfile(profile, req.Client); ok {
						if pool, _, e := s.rankCandidatesLocal(ctx, req, sreq, relaxed, results, nil, pinID, false); e == nil {
							candidates = pinFirst(acceptableVariantAudio(clearIdentityVariants(pool)), pinID)
							matchPool = candidates
						}
					}
				}
				auto, autoRemembered = s.slotAutoWithSource(req, strictCands, matchPool, titleVC, showVC)
				if hasOwnerPin && ownerPin.Fallback && auto.ID != ownerPin.Release.ID {
					// The preference is not the eligible Auto winner, so it
					// is labelled like any other release.
					hasOwnerPin = false
				}
				if !autoRemembered && !hasOwnerPin {
					if auto.ID != "" && auto.ID == req.PlayedRelease && req.Client.Protocol == "vela" && !core.IsExplicitVersionSlotSelector(req.PlayedSelector) {
						autoReason = core.AutoResumeSource
					} else if idx := slices.IndexFunc(strictCands, func(c candidate) bool { return c.rel.ID == auto.ID }); idx >= 0 {
						autoReason = strictCands[idx].autoReason
					}
				}
			}
		}
	}
	picks := slotPicks(slots, candidates, auto, req, titleVC)
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
		var info core.MediaInfo
		placeholder := true
		if i < len(picks) && picks[i].ID != "" {
			r = picks[i]
			placeholder = false
			if probed, ok := s.probedMediaFor(r); ok {
				info = probed
				if r.Size <= 0 && info.Size > 0 {
					r.Size = info.Size
				}
			} else {
				info = estimateVariantInfo(req.Title, r, wanted)
			}
			label = variantLabel(r, "alt")
			if i == 0 {
				label = variantLabel(r, "auto")
			}
		} else {
			// A cold configured slot has neither a picked release nor a probe;
			// keep one placeholder audio stream without claiming a language it
			// does not know (#624).
			info = estimateVariantInfo(req.Title, r, wanted)
			for j := range info.Streams {
				if info.Streams[j].Type == "audio" {
					info.Streams[j].Language = ""
				}
			}
		}
		v := core.ReleaseVariant{ID: slot.id(), Label: label, Auto: i == 0, Edition: r.Info.Edition, Release: r, Info: info}
		if labels {
			v.Remembered = i == 0 && autoRemembered
			if i == 0 && !placeholder {
				v.AutoSelectionReason = autoReason
			}
			v.Pinned = hasOwnerPin && !placeholder && r.ID == ownerPin.Release.ID && (!ownerPin.Fallback || i == 0)
			v.Availability, v.AvailabilityNote = s.classifyVariantAvailability(ctx, req.Title.Key, r, info, placeholder, searchWarm)
		}
		out[i] = v
	}
	return dedupeVersionSlots(out), nil
}

// dedupeVersionSlots drops a filled slot whose release is already listed by
// an earlier slot: the same release from two indexers (two IDs, one exact
// size), including a remembered or pinned pick that resolves to it.
func dedupeVersionSlots(in []core.ReleaseVariant) []core.ReleaseVariant {
	out := in[:0]
	seenID := map[string]bool{}
	seenSize := map[int64]bool{}
	for _, v := range in {
		placeholder := v.Release.ID == "" || strings.HasPrefix(v.Release.ID, core.VersionSelectorPrefix)
		if !placeholder {
			size := v.Info.Size
			if size <= 0 || v.Info.Estimated {
				size = v.Release.Size
			}
			if seenID[v.Release.ID] || (size > 0 && seenSize[size]) {
				continue
			}
			seenID[v.Release.ID] = true
			if size > 0 {
				seenSize[size] = true
			}
		}
		out = append(out, v)
	}
	return out
}
func (s *Service) configuredSlots(profile core.QualityProfile, caps core.ClientCaps, limit int) []versionSlot {
	if limit <= 0 || limit > core.MaxVariants {
		limit = core.MaxVariants
	}
	cfg := s.resolverCfg()
	alts := max(0, min(cfg.SameQualityAlternatives, maxAlternativesLimit))
	sources, perSource := cfg.SourceVersionPlan()
	slots := []versionSlot{{Kind: "auto"}}
	add := func(v versionSlot) {
		if len(slots) < limit {
			slots = append(slots, v)
		}
	}
	if !cfg.AlternativesPerResolution && !perSource {
		for n := 0; n < alts; n++ {
			add(versionSlot{Kind: "alternative", Ordinal: n})
		}
		// Extra tier/cut choices require the owner's opt-in. Without it,
		// the menu is exactly Auto plus the configured alternatives.
		if cfg.OfferHigherVersions {
			for n := 0; n < maxTierVariants-1; n++ {
				add(versionSlot{Kind: "distinct", Ordinal: n})
			}
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
	// Menu order is resolution first (4K, then 1080p, then 720p), each tier's
	// sources as Remux, Blu-ray, WEB, then the per-resolution alternatives.
	// Which choices fit the bounded menu is decided by priority: Required
	// source choices, then offered ones (higher resolutions first), then
	// alternatives. Filling source by source let six Remux and Blu-ray
	// choices take the whole menu before any WEB release (#309).
	var tiers, required, offered, extra []versionSlot
	for _, h := range heights {
		if !perSource {
			v := versionSlot{Kind: "tier", Height: h}
			tiers, offered = append(tiers, v), append(offered, v)
			continue
		}
		for _, class := range versionSourceClasses {
			v := versionSlot{Kind: "tier", Height: h, Source: class}
			switch sources.Mode(h, class) {
			case config.SourceVersionRequired:
				tiers, required = append(tiers, v), append(required, v)
			case config.SourceVersionOffer:
				tiers, offered = append(tiers, v), append(offered, v)
			}
		}
	}
	if cfg.AlternativesPerResolution {
		for n := 0; n < alts; n++ {
			for _, h := range heights {
				extra = append(extra, versionSlot{Kind: "tier", Height: h, Ordinal: n + 1})
			}
		}
	}
	keep := map[versionSlot]bool{}
	room := limit - len(slots)
	for _, v := range slices.Concat(required, offered, extra) {
		if len(keep) == room {
			break
		}
		keep[v] = true
	}
	for _, v := range slices.Concat(tiers, extra) {
		if keep[v] {
			add(v)
		}
	}
	return slots
}

// versionSourceClasses are the reservable source classes in menu order;
// sourceClass names them from the parsed release source.
var versionSourceClasses = []string{"remux", "disc", "web"}

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

// slotAuto is shared by menu labels and admission. Precedence in the Auto
// slot: viewer remembered choice (title, then show profile), then owner pin,
// then previously played release, then owner quality rules.
func (s *Service) slotAuto(req core.ResolveRequest, ranked []candidate) core.Release {
	autoReq := req
	autoReq.Prefer = ""
	titleVC := s.viewerTitleChoiceForResolve(autoReq, nil)
	showVC := s.viewerShowChoiceForResolve(autoReq)
	auto, _ := s.slotAutoWithSource(autoReq, ranked, ranked, titleVC, showVC)
	return auto
}

func (s *Service) slotAutoWithSource(req core.ResolveRequest, strict, pool []candidate, titleVC, showVC *ViewerChoice) (core.Release, bool) {
	if titleVC != nil {
		for _, c := range pool {
			if c.rel.ID == titleVC.Release.ID {
				return c.rel, true
			}
		}
		for _, c := range strict {
			if c.rel.ID == titleVC.Release.ID {
				return c.rel, true
			}
		}
	}
	if showVC != nil {
		if matched := s.matchShowChoiceCandidates(showVC, pool); len(matched) > 0 {
			return matched[0].rel, true
		}
	}
	autoReq := req
	autoReq.Prefer = ""
	if pin, _ := s.pinForResolve(autoReq); pin != nil && (!pin.Fallback || slotCandidate(strict, pool, pin.Release.ID)) {
		// A preferred pin wins Auto only when it is an eligible candidate.
		return pin.Release, false
	}
	if req.PlayedRelease != "" && !core.IsExplicitVersionSlotSelector(req.PlayedSelector) {
		for _, c := range strict {
			if c.rel.ID != req.PlayedRelease || c.trackPreference < strict[0].trackPreference {
				continue
			}
			if c.deliveryDemotion != core.AutoConnectionEstimate || !slices.ContainsFunc(strict, func(other candidate) bool {
				return other.autoReason == core.AutoConnectionEstimate
			}) {
				return c.rel, false
			}
			break
		}
	}
	skipExplicit := ""
	if req.PlayedRelease != "" && core.IsExplicitVersionSlotSelector(req.PlayedSelector) {
		skipExplicit = req.PlayedRelease
	}
	for _, c := range strict {
		if skipExplicit != "" && c.rel.ID == skipExplicit {
			continue
		}
		return c.rel, false
	}
	return core.Release{}, false
}

func slotCandidate(strict, pool []candidate, id string) bool {
	has := func(c candidate) bool { return c.rel.ID == id }
	return slices.ContainsFunc(strict, has) || slices.ContainsFunc(pool, has)
}

func slotChoices(slots []versionSlot, ranked []candidate, auto core.Release) []core.Release {
	return slotPicks(slots, ranked, auto, core.ResolveRequest{}, nil)
}

func slotPicks(slots []versionSlot, ranked []candidate, auto core.Release, req core.ResolveRequest, titleVC *ViewerChoice) []core.Release {
	out := make([]core.Release, len(slots))
	if len(ranked) == 0 && auto.ID == "" {
		return out
	}
	used := map[string]bool{}
	keys := map[string]bool{}
	// The same release listed by two indexers has two IDs but one exact
	// byte size; it fills one slot, never two.
	sizes := map[int64]bool{}
	take := func(r core.Release) {
		used[r.ID] = true
		keys[variantKey(r.Info)] = true
		if r.Size > 0 {
			sizes[r.Size] = true
		}
	}
	for i, slot := range slots {
		if slot.Kind == "auto" && auto.ID != "" {
			out[i] = auto
			take(auto)
			break
		}
	}
	// Keep an explicitly played release behind its own version slot rather
	// than refilling that slot with another (potentially unprobed) release.
	// When a remembered choice moved into Auto from an explicit non-Auto slot,
	// bind that original slot to the remembered release as well so admission
	// stays consistent and dedupeVersionSlots folds the duplicate row.
	if titleVC != nil && titleVC.Release.ID != "" && titleVC.Release.ID == auto.ID && titleVC.Selector != "" {
		if target, ok := parseVersionSlot(titleVC.Selector); ok && target.Kind != "auto" {
			for i, slot := range slots {
				if slot == target {
					out[i] = auto
					break
				}
			}
		}
	} else if req.PlayedRelease != "" && core.IsExplicitVersionSlotSelector(req.PlayedSelector) {
		if target, ok := parseVersionSlot(req.PlayedSelector); ok && target.Kind != "auto" {
			for i, slot := range slots {
				if slot != target || out[i].ID != "" {
					continue
				}
				for _, c := range ranked {
					if c.rel.ID == req.PlayedRelease && !used[c.rel.ID] && (c.rel.Size <= 0 || !sizes[c.rel.Size]) {
						out[i] = c.rel
						take(c.rel)
						break
					}
				}
				break
			}
		}
	}
	for i, slot := range slots {
		if out[i].ID != "" {
			continue
		}
		for _, c := range ranked {
			r := c.rel
			if used[r.ID] || (r.Size > 0 && sizes[r.Size]) {
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
				take(r)
				break
			}
		}
	}
	return out
}

// slotPlanVariants lists the releases the configured menu shows for these
// candidates: the same plan and picks VersionSlots uses, minus choices no
// release fills. Variants uses it in the per-resolution and per-source modes,
// so Jellyfin and Emby list what Plex, Vela and Silo list.
func slotPlanVariants(t *core.Title, plan []versionSlot, pool []candidate, auto core.Release, req core.ResolveRequest, titleVC *ViewerChoice, wanted []string) []core.ReleaseVariant {
	var out []core.ReleaseVariant
	seen := map[string]bool{}
	for _, r := range slotPicks(plan, pool, auto, req, titleVC) {
		if r.ID == "" || seen[r.ID] {
			continue
		}
		seen[r.ID] = true
		kind := "alt"
		if len(out) == 0 {
			kind = "auto"
		}
		out = append(out, core.ReleaseVariant{ID: r.ID, Label: variantLabel(r, kind), Edition: r.Info.Edition, Auto: kind == "auto", Release: r, Info: estimateVariantInfo(t, sizedForEpisode(r), wanted)})
	}
	return out
}

type slotMatcher struct {
	primary    func(candidate) bool
	sameHeight func(candidate) bool
	stepDown   func(candidate) bool
	targetH    int
}

func newSlotMatcher(slot versionSlot, selected int, picks []core.Release, auto core.Release) slotMatcher {
	var slotPick core.Release
	if selected >= 0 && selected < len(picks) {
		slotPick = picks[selected]
	}
	switch slot.Kind {
	case "tier":
		return slotMatcher{
			targetH: slot.Height,
			primary: func(c candidate) bool {
				return c.rel.Info.Height == slot.Height && (slot.Source == "" || sourceClass(c.rel.Info.Source) == slot.Source)
			},
			sameHeight: func(c candidate) bool {
				return slot.Source != "" && c.rel.Info.Height == slot.Height && sourceClass(c.rel.Info.Source) != slot.Source
			},
			stepDown: func(c candidate) bool {
				return c.rel.Info.Height < slot.Height
			},
		}
	case "alternative":
		targetH := slotPick.Info.Height
		if targetH == 0 {
			targetH = auto.Info.Height
		}
		wantKey := ""
		if auto.ID != "" {
			wantKey = variantKey(auto.Info)
		} else if slotPick.ID != "" {
			wantKey = variantKey(slotPick.Info)
		}
		return slotMatcher{
			targetH: targetH,
			primary: func(c candidate) bool {
				return c.rel.ID != auto.ID && wantKey != "" && variantKey(c.rel.Info) == wantKey
			},
			sameHeight: func(c candidate) bool {
				return c.rel.ID != auto.ID && targetH > 0 && c.rel.Info.Height == targetH && variantKey(c.rel.Info) != wantKey
			},
			stepDown: func(c candidate) bool {
				return c.rel.ID != auto.ID && targetH > 0 && c.rel.Info.Height < targetH
			},
		}
	case "distinct":
		if slotPick.ID != "" {
			targetH := slotPick.Info.Height
			wantKey := variantKey(slotPick.Info)
			return slotMatcher{
				targetH: targetH,
				primary: func(c candidate) bool {
					return variantKey(c.rel.Info) == wantKey
				},
				sameHeight: func(c candidate) bool {
					return targetH > 0 && c.rel.Info.Height == targetH && variantKey(c.rel.Info) != wantKey && c.rel.ID != auto.ID
				},
				stepDown: func(c candidate) bool {
					return targetH > 0 && c.rel.Info.Height < targetH
				},
			}
		}
		targetH := 0
		for i := 0; i < selected && i < len(picks); i++ {
			if picks[i].Info.Height > 0 && (targetH == 0 || picks[i].Info.Height < targetH) {
				targetH = picks[i].Info.Height
			}
		}
		if targetH == 0 {
			targetH = auto.Info.Height
		}
		return slotMatcher{
			targetH: targetH,
			primary: func(candidate) bool { return false },
			sameHeight: func(c candidate) bool {
				return c.rel.ID != auto.ID && targetH > 0 && c.rel.Info.Height == targetH
			},
			stepDown: func(c candidate) bool {
				return c.rel.ID != auto.ID && (targetH == 0 || c.rel.Info.Height < targetH)
			},
		}
	default:
		return slotMatcher{
			primary:    func(candidate) bool { return false },
			sameHeight: func(candidate) bool { return false },
			stepDown:   func(candidate) bool { return false },
		}
	}
}

// partitionSlotCandidates groups pool into exact-slot matches (starting with
// the slot's picked release, then unpicked matches, then earlier-slot matches),
// same-height fallbacks (for source/distinct slots), and strictly lower-tier
// step-downs. Higher-tier candidates are dropped so a picked slot never climbs
// back up to Auto or 4K.
func partitionSlotCandidates(m slotMatcher, selected int, picks []core.Release, pool []candidate) (slotPrimary, sameHeight, stepDown []candidate) {
	var slotPickID string
	if selected >= 0 && selected < len(picks) {
		slotPickID = picks[selected].ID
	}
	earlier := make(map[string]bool, max(selected, 0))
	for i := 0; i < selected && i < len(picks); i++ {
		if picks[i].ID != "" {
			earlier[picks[i].ID] = true
		}
	}
	var picked []candidate
	var unpickedPrimary []candidate
	var earlierPrimary []candidate
	for _, c := range pool {
		switch {
		case m.primary(c):
			if slotPickID != "" && c.rel.ID == slotPickID {
				picked = append(picked, c)
			} else if earlier[c.rel.ID] {
				earlierPrimary = append(earlierPrimary, c)
			} else {
				unpickedPrimary = append(unpickedPrimary, c)
			}
		case m.sameHeight(c):
			sameHeight = append(sameHeight, c)
		case m.stepDown(c):
			stepDown = append(stepDown, c)
		}
	}
	slotPrimary = make([]candidate, 0, len(picked)+len(unpickedPrimary)+len(earlierPrimary))
	slotPrimary = append(slotPrimary, picked...)
	slotPrimary = append(slotPrimary, unpickedPrimary...)
	slotPrimary = append(slotPrimary, earlierPrimary...)
	return slotPrimary, sameHeight, stepDown
}

// groupStepDownTiers splits step-down candidates by resolution height from
// nearest lower tier down (e.g. 1080 -> 720 -> 480 -> unknown), preserving
// ranked order within each tier.
func groupStepDownTiers(cands []candidate) [][]candidate {
	if len(cands) == 0 {
		return nil
	}
	ordered := slices.Clone(cands)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].rel.Info.Height > ordered[j].rel.Info.Height
	})
	var tiers [][]candidate
	for i := 0; i < len(ordered); {
		h := ordered[i].rel.Info.Height
		j := i + 1
		for j < len(ordered) && ordered[j].rel.Info.Height == h {
			j++
		}
		tiers = append(tiers, ordered[i:j])
		i = j
	}
	return tiers
}

func slotSourceClassLabel(class string) string {
	switch class {
	case "remux":
		return "Remux"
	case "disc":
		return "BluRay"
	case "web":
		return "WEB"
	default:
		return class
	}
}

func slotFallbackNote(slot versionSlot, targetHeight int, rel core.Release, triedSlot bool) string {
	played := "an unknown resolution"
	if h := fallbackHeight(rel.Info); h > 0 {
		played = fmt.Sprintf("%dp", h)
		if src := core.SourceLabel(rel.Info.Source); src != "" {
			played += " " + src
		}
	}
	wanted := "picked"
	switch {
	case slot.Kind == "tier" && slot.Height > 0:
		wanted = fmt.Sprintf("%dp", slot.Height)
		if slot.Source != "" {
			wanted += " " + slotSourceClassLabel(slot.Source)
		}
	case slot.Kind == "alternative" && targetHeight > 0 && fallbackHeight(rel.Info) == targetHeight:
		if triedSlot {
			return fmt.Sprintf("Played in %s: no working %dp alternative was available.", played, targetHeight)
		}
		return fmt.Sprintf("Played in %s: no %dp alternative was available.", played, targetHeight)
	case targetHeight > 0:
		wanted = fmt.Sprintf("%dp", targetHeight)
	}
	if triedSlot {
		return fmt.Sprintf("Played in %s: no working %s copy was available.", played, wanted)
	}
	return fmt.Sprintf("Played in %s: no %s copy was available.", played, wanted)
}

func rejectedAttemptedReleaseIDs(tr *Trace, winnerID string) []string {
	if tr == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range tr.Candidates {
		if !c.Attempted || !c.Rejected || c.ReleaseID == "" || c.ReleaseID == winnerID || seen[c.ReleaseID] {
			continue
		}
		seen[c.ReleaseID] = true
		out = append(out, c.ReleaseID)
	}
	return out
}

type slotResolveParams struct {
	slot         versionSlot
	origPrefer   string
	selected     int
	plan         []versionSlot
	req          *core.ResolveRequest
	sreq         core.SearchRequest
	profile      core.QualityProfile
	results      []core.Release
	cands        []candidate
	pinID        string
	searchErr    error
	hasExclude   bool
	inconclusive *unverifiedPool
	trace        *Trace
	rcfg         config.Resolver
	early        *searchPreparation
	selector     *string
	slotRelease  *string
}

// resolveVersionSlot resolves an explicit non-Auto version slot using Auto's
// validation, racing, rescue, and start-up bounds within that slot, stepping
// down only to lower tiers the client allows and never up to Auto/4K.
func (s *Service) resolveVersionSlot(ctx context.Context, p slotResolveParams) (*core.Resolution, error) {
	t := p.req.Title
	cands := p.cands
	results := p.results
	rescueTried := false
	speculative := core.SpeculativeIndexerWork(ctx)
	reqLang := profileRequiredLanguage(p.profile)
	requiredLangRejected := reqLang != "" && traceHasRequiredProfileLanguageRejection(p.trace)

	if len(cands) == 0 {
		if p.req.PreferredOnly {
			if requiredLangRejected {
				langErr := &core.RequiredAudioLanguageError{Language: releaseparse.LanguageDisplayName(reqLang)}
				p.trace.Err = langErr.Error()
				return nil, fmt.Errorf("%q: %w", t.Title, langErr)
			}
			return nil, core.ErrNotAvailable
		}
		if p.searchErr == nil && ctx.Err() == nil && !speculative && !requiredLangRejected {
			rescueTried = true
			rescueReq := *p.req
			rescueReq.Prefer = ""
			var err error
			cands, results, _, err = s.rescueCandidates(ctx, rescueReq, p.sreq, p.profile, results, p.trace, p.pinID)
			if err != nil {
				p.trace.Err = err.Error()
				return nil, err
			}
		}
		if len(cands) == 0 && reqLang != "" && traceHasRequiredProfileLanguageRejection(p.trace) {
			langErr := &core.RequiredAudioLanguageError{Language: releaseparse.LanguageDisplayName(reqLang)}
			p.trace.Err = langErr.Error()
			return nil, fmt.Errorf("%q: %w", t.Title, langErr)
		}
	}

	cands = pinFirst(acceptableVariantAudio(clearIdentityVariants(cands)), p.pinID)
	pool := cands
	autoReq := *p.req
	autoReq.Prefer = ""
	titleVC := s.viewerTitleChoiceForResolve(autoReq, nil)
	showVC := s.viewerShowChoiceForResolve(autoReq)
	slotProfile := p.profile
	if s.resolverCfg().OfferHigherVersions && p.slot.Kind != "alternative" {
		if relaxed, ok := relaxedVariantProfile(p.profile, p.req.Client); ok {
			slotProfile = relaxed
			if ranked, _, e := s.rankCandidatesLocal(ctx, autoReq, p.sreq, relaxed, results, p.trace, p.pinID, false); e == nil {
				pool = pinFirst(acceptableVariantAudio(clearIdentityVariants(ranked)), p.pinID)
			}
		}
	}
	auto, _ := s.slotAutoWithSource(autoReq, cands, pool, titleVC, showVC)
	picks := slotPicks(p.plan, pool, auto, *p.req, titleVC)
	var slotPick core.Release
	if p.selected >= 0 && p.selected < len(picks) {
		slotPick = picks[p.selected]
	}
	allowFulfill := slotPick.ID != ""
	matcher := newSlotMatcher(p.slot, p.selected, picks, auto)
	slotPrimary, sameHeight, stepDown := partitionSlotCandidates(matcher, p.selected, picks, pool)

	if p.req.PreferVideoCodec != "" {
		preferCodec := func(cs []candidate) {
			sort.SliceStable(cs, func(i, j int) bool {
				return strings.EqualFold(cs[i].rel.Info.VideoCodec, p.req.PreferVideoCodec) && !strings.EqualFold(cs[j].rel.Info.VideoCodec, p.req.PreferVideoCodec)
			})
		}
		if len(slotPrimary) > 1 && slotPick.ID != "" && slotPrimary[0].rel.ID == slotPick.ID {
			preferCodec(slotPrimary[1:])
		} else {
			preferCodec(slotPrimary)
		}
		preferCodec(sameHeight)
		preferCodec(stepDown)
	}
	if !p.req.PreferredOnly && len(slotPrimary) > 0 {
		slotPrimary = s.demoteBackingOffUsenet(slotPrimary, p.trace)
	}

	fulfill := func(relID string, matchedSlot bool) {
		if matchedSlot {
			*p.selector = p.origPrefer
			*p.slotRelease = relID
			p.req.Prefer = relID
		} else {
			*p.selector = ""
			*p.slotRelease = ""
			p.req.Prefer = ""
		}
	}

	var (
		checks        candidateChecks
		optimisticErr error
		slotErr       error
		rescuePrimary []candidate
	)
	defer checks.close()

	if len(slotPrimary) > 0 {
		p.early.attach(slotPrimary)
		prepareStart := time.Now()
		slotPrimary = s.prepareSources(ctx, slotPrimary, t, p.trace, &checks)
		slotPrimary = groupRepostCandidates(slotPrimary, p.pinID, slotPick.ID)
		p.trace.SourcePreparationDuration = time.Since(prepareStart)

		if !p.req.PreferredOnly && p.rcfg.OptimisticStart && len(slotPrimary) > 0 && vault.KeyOf(slotPrimary[0].rel) == "" {
			if slotPrimary[0].rel.OpensOnDemand() && !core.OnDemandAllowedAt(ctx, slotPrimary[0].rel, 0) {
				p.trace.Err = errOnDemandForeground.Error()
				return nil, errOnDemandForeground
			}
			members := append([]candidate{slotPrimary[0]}, slotPrimary[0].fallbacks...)
			for mi, m := range members {
				if mi > 0 && (s.isBadRelease(m.rel) || s.recentlyFailedRelease(m.rel)) {
					continue
				}
				at := time.Now()
				if !reserveSpeculativeCheck(ctx) {
					return nil, errHealthBudget
				}
				nzb, err := s.indexer.FetchNZB(ctx, m.rel)
				p.trace.Candidates[m.trace].Attempted = true
				p.trace.Candidates[m.trace].FetchDuration = time.Since(at)
				if err == nil {
					winner := m
					winner.rel, _ = s.learnReleaseContent(ctx, winner.rel, nzb)
					slotPrimary[0] = winner
					p.trace.Winner = winner.rel.ID
					p.trace.Candidates[winner.trace].Reason = "optimistic start: health check pending"
					s.optimisticCheck(winner.rel, nzb)
					fulfill(winner.rel.ID, allowFulfill)
					return s.resolution(t.Key, slotPrimary, 0, nzb, p.trace), nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				p.trace.Candidates[m.trace].Rejected = true
				p.trace.Candidates[m.trace].Reason = "nzb fetch: " + err.Error()
				p.trace.Candidates[m.trace].err = err
				if permanentGrabFailure(err) {
					s.recordFailedValidation(ctx, m.rel, validation{err: err})
				} else if !permanentFailure(err) {
					optimisticErr = err
				}
			}
			slotPrimary = slotPrimary[1:]
		}

		if len(slotPrimary) > 0 {
			index, nzb, err := s.validateCandidates(ctx, slotPrimary, p.trace, p.rcfg.HealthRace)
			if err == nil && index >= 0 {
				winner := slotPrimary[index]
				fulfill(winner.rel.ID, allowFulfill)
				return s.acceptResolution(ctx, t.Key, slotPrimary, index, nzb, p.trace), nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if errors.Is(err, errOnDemandForeground) {
				p.trace.Err = err.Error()
				return nil, err
			}
			slotErr = err
		}
	}

	if p.req.PreferredOnly {
		if slotErr != nil {
			p.trace.Err = slotErr.Error()
			return nil, slotErr
		}
		p.trace.Err = "picked version unavailable"
		return nil, core.ErrNotAvailable
	}

	// If this tier still has unchecked candidates beyond the per-pass health
	// cap, let playback run another pass at this same tier before stepping down.
	if errors.Is(slotErr, errHealthBudget) {
		return nil, slotErr
	}

	// Retry discovery once when the initial pool produced no working copy for
	// this slot, filtering rescue results through the exact same slot matcher.
	if !rescueTried && p.searchErr == nil && ctx.Err() == nil && !speculative {
		rescueReq := *p.req
		rescueReq.Prefer = ""
		rescueCands, updatedResults, _, rerr := s.rescueCandidates(ctx, rescueReq, p.sreq, slotProfile, results, p.trace, p.pinID)
		results = updatedResults
		if rerr == nil && len(rescueCands) > 0 {
			rescueCands = pinFirst(acceptableVariantAudio(clearIdentityVariants(rescueCands)), p.pinID)
			var rescueSame, rescueDown []candidate
			rescuePrimary, rescueSame, rescueDown = partitionSlotCandidates(matcher, -1, nil, rescueCands)
			if len(rescuePrimary) > 0 {
				var rescueChecks candidateChecks
				defer rescueChecks.close()
				at := time.Now()
				rescuePrimary = s.prepareSources(ctx, rescuePrimary, t, p.trace, &rescueChecks)
				rescuePrimary = groupRepostCandidates(rescuePrimary, p.pinID, "")
				p.trace.SourcePreparationDuration += time.Since(at)
				index, nzb, verr := s.validateCandidates(ctx, rescuePrimary, p.trace, p.rcfg.HealthRace)
				if verr == nil && index >= 0 {
					winner := rescuePrimary[index]
					fulfill(winner.rel.ID, allowFulfill || p.slot.Ordinal == 0)
					return s.acceptResolution(ctx, t.Key, rescuePrimary, index, nzb, p.trace), nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				if errors.Is(verr, errHealthBudget) {
					return nil, verr
				}
				if verr != nil && slotErr == nil {
					slotErr = verr
				}
			}
			sameHeight = append(sameHeight, rescueSame...)
			stepDown = append(stepDown, rescueDown...)
		}
	}

	triedSlot := len(slotPrimary)+len(rescuePrimary) > 0 || p.hasExclude
	tryFallback := func(bucket []candidate) (*core.Resolution, error) {
		if len(bucket) == 0 || ctx.Err() != nil {
			return nil, nil
		}
		var fChecks candidateChecks
		defer fChecks.close()
		at := time.Now()
		bucket = s.prepareSources(ctx, bucket, t, p.trace, &fChecks)
		bucket = groupRepostCandidates(bucket, p.pinID, "")
		p.trace.SourcePreparationDuration += time.Since(at)
		index, nzb, verr := s.validateCandidates(ctx, bucket, p.trace, p.rcfg.HealthRace)
		if verr == nil && index >= 0 {
			winner := bucket[index]
			fulfill("", false)
			note := slotFallbackNote(p.slot, matcher.targetH, winner.rel, triedSlot)
			s.log.Info("resolver: playing a lower quality", "title", t.Title, "release", winner.rel.Name, "reason", note)
			res := s.acceptResolution(ctx, t.Key, bucket, index, nzb, p.trace)
			res.QualityFallback = note
			p.trace.Downgrade = note
			return res, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, verr
	}

	if res, err := tryFallback(sameHeight); res != nil || ctx.Err() != nil || errors.Is(err, errHealthBudget) {
		return res, err
	} else if err != nil && slotErr == nil {
		slotErr = err
	}
	for _, tier := range groupStepDownTiers(stepDown) {
		res, err := tryFallback(tier)
		if res != nil || ctx.Err() != nil || errors.Is(err, errHealthBudget) {
			return res, err
		}
		if err != nil && slotErr == nil {
			slotErr = err
		}
	}
	// When an "alternative" slot (same quality as Auto) has no other copy at
	// Auto's tier and no lower tier exists (e.g. a bitrate cap already brought
	// Auto down to the sole fitting 1080p release), Auto itself is at the same
	// tier — never higher — and serves as the final fallback.
	if p.slot.Kind == "alternative" && auto.ID != "" && (matcher.targetH == 0 || auto.Info.Height <= matcher.targetH) && ctx.Err() == nil {
		for _, c := range pool {
			if c.rel.ID == auto.ID {
				if res, err := tryFallback([]candidate{c}); res != nil || ctx.Err() != nil {
					return res, err
				} else if err != nil && slotErr == nil {
					slotErr = err
				}
				break
			}
		}
	}

	all := concat(slotPrimary, rescuePrimary, sameHeight, stepDown)
	if res := s.lastResort(ctx, p.inconclusive, t.Key, all, p.trace); res != nil {
		inPrimary := slices.ContainsFunc(slotPrimary, func(c candidate) bool { return c.rel.ID == res.Release.ID }) ||
			slices.ContainsFunc(rescuePrimary, func(c candidate) bool { return c.rel.ID == res.Release.ID })
		if inPrimary && allowFulfill {
			fulfill(res.Release.ID, true)
		} else {
			fulfill("", false)
			if !inPrimary {
				note := slotFallbackNote(p.slot, matcher.targetH, res.Release, triedSlot)
				res.QualityFallback = note
				p.trace.Downgrade = note
			}
		}
		return res, nil
	}
	if optimisticErr != nil {
		return nil, optimisticErr
	}
	if errors.Is(p.searchErr, core.ErrStreamLimit) {
		return nil, p.searchErr
	}
	if p.searchErr != nil {
		p.trace.Err = p.searchErr.Error()
		return nil, p.searchErr
	}
	if i, payload, uerr, ok := s.uncachedOutcome(ctx, all, results, p.trace); ok {
		if i >= 0 {
			winner := all[i]
			inPrimary := slices.ContainsFunc(slotPrimary, func(c candidate) bool { return c.rel.ID == winner.rel.ID }) ||
				slices.ContainsFunc(rescuePrimary, func(c candidate) bool { return c.rel.ID == winner.rel.ID })
			if inPrimary && allowFulfill {
				fulfill(winner.rel.ID, true)
			} else {
				fulfill("", false)
			}
			res := s.acceptResolution(ctx, t.Key, all, i, payload, p.trace)
			if !inPrimary {
				note := slotFallbackNote(p.slot, matcher.targetH, winner.rel, triedSlot)
				res.QualityFallback = note
				p.trace.Downgrade = note
			}
			return res, nil
		}
		return nil, uerr
	}
	for _, c := range all {
		ct := &p.trace.Candidates[c.trace]
		if c.rel.StreamProvider != "" && errors.Is(ct.err, core.ErrStreamLimit) {
			p.trace.Err = "provider busy"
			return nil, fmt.Errorf("provider busy: %w", ct.err)
		}
	}
	if slotErr != nil && !errors.Is(slotErr, core.ErrNotAvailable) {
		p.trace.Err = slotErr.Error()
		return nil, slotErr
	}
	p.trace.Err = "no healthy candidates"
	for _, c := range all {
		ct := &p.trace.Candidates[c.trace]
		if ct.Attempted && ct.Rejected {
			return nil, fmt.Errorf("%q: %w: %w", t.Title, core.ErrNotAvailable, core.ErrSourceUnavailable)
		}
	}
	if reqLang != "" && traceHasRequiredProfileLanguageRejection(p.trace) {
		langErr := &core.RequiredAudioLanguageError{Language: releaseparse.LanguageDisplayName(reqLang)}
		p.trace.Err = langErr.Error()
		return nil, fmt.Errorf("%q: %w", t.Title, langErr)
	}
	return nil, fmt.Errorf("%q: %w", t.Title, core.ErrNotAvailable)
}
