package resolver

import (
	"fmt"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// explainCeiling says why a resolution's winner is below 4K, or "" when it
// is 4K or nothing won. In order: a profile that stops below 4K (and why
// that profile applied), a client that reported a lower ceiling, else the
// fate of each 4K release (rejected, ranked lower, failed its check).
func (s *Service) explainCeiling(tr *Trace, profile core.QualityProfile, req core.ResolveRequest) string {
	if tr.Winner == "" {
		return ""
	}
	winner := 0
	for _, c := range tr.Candidates {
		if c.ReleaseID == tr.Winner {
			winner = c.Height
		}
	}
	if winner >= 2160 {
		return ""
	}
	configured := configuredMaxHeight(profile)
	if configured > 0 && configured < 2160 {
		return fmt.Sprintf("The %s profile stops at %dp: %s.", profile.Name, configured, profileReason(s.qualityCfg(), req.Client, req.User, profile.Name))
	}
	if profile.MaxHeight > 0 && profile.MaxHeight < 2160 {
		return fmt.Sprintf("The client reported it plays up to %dp, so 4K releases were not considered.", profile.MaxHeight)
	}
	reasons := map[string]int{}
	found := 0
	for _, c := range tr.Candidates {
		if c.Height < 2160 {
			continue
		}
		found++
		switch {
		case c.Rejected && c.Attempted:
			reasons["failed its check"]++
		case c.Rejected:
			reasons[strings.TrimPrefix(c.Reason, "release preference: ")]++
		case strings.Contains(c.CompatibilityReason, "presumed upscale"):
			reasons["presumed upscale"]++
		case strings.Contains(c.CompatibilityReason, "too little bitrate"):
			reasons["too little bitrate for 4K"]++
		case strings.Contains(c.CompatibilityReason, "accepted audio language"):
			reasons["audio in another language"]++
		case strings.Contains(c.CompatibilityReason, "connection carries"):
			reasons["too large for the client's connection"]++
		default:
			reasons["ranked lower by the owner's release preferences"]++
		}
	}
	if found == 0 {
		return "No 4K release was found for this title."
	}
	parts := make([]string, 0, len(reasons))
	for r, n := range reasons {
		parts = append(parts, fmt.Sprintf("%d %s", n, r))
	}
	sort.Strings(parts)
	return fmt.Sprintf("%d 4K release(s) lost: %s.", found, strings.Join(parts, "; "))
}

// profileReason says why SelectProfile chose the named profile.
func profileReason(q config.Quality, caps core.ClientCaps, user *core.User, name string) string {
	if caps.Remote && q.Remote == name {
		return "it is the profile for remote viewers"
	}
	for _, rule := range q.Rules {
		if ruleMatches(rule, caps, user) && rule.Profile == name {
			return "a client rule chose it for this " + clientLabel(caps)
		}
	}
	if q.Default == name {
		return "it is the default, and no client rule matched this " + clientLabel(caps)
	}
	return "it is the first profile"
}

func clientLabel(caps core.ClientCaps) string {
	if p := strings.TrimSpace(caps.Product); p != "" {
		return "client (" + p + ")"
	}
	return "client"
}
