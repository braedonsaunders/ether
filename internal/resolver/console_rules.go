package resolver

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

// previewRules evaluates a draft without saving, health checks, pins or
// availability writes. Real searches use the existing bounded search cache;
// pasted filenames test the preference rules only (no invented title facts).
func (h *consoleAPI) previewRules(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Profile core.QualityProfile `json:"profile"`
		Key     string              `json:"key"`
		Names   []string            `json:"names"`
	}
	if !decodeBody(w, r, &in) {
		return
	}
	policy, err := releasepolicy.Compile(in.Profile.ReleaseRules)
	if err != nil {
		consoleError(w, 400, "invalid", err.Error())
		return
	}
	warning := releasepolicy.Conflict(in.Profile.ReleaseRules)
	if len(in.Names) > 100 {
		consoleError(w, 400, "invalid", "at most 100 filenames")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	var results []core.Release
	var title *core.Title
	if in.Key != "" {
		if len(in.Names) > 0 {
			consoleError(w, 400, "invalid", "choose a title or filenames, not both")
			return
		}
		key, ok := parseKey(w, in.Key, true)
		if !ok {
			return
		}
		title, ok = h.title(w, ctx, key)
		if !ok {
			return
		}
		if h.s.indexer == nil {
			consoleError(w, 503, "unavailable", "no indexer configured")
			return
		}
		results, _, err = h.s.searchCached(ctx, searchRequestFor(title), false)
		if err != nil {
			h.fail(w, err)
			return
		}
	} else {
		for i, n := range in.Names {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			if len(n) > 2048 {
				consoleError(w, 400, "invalid", "filename too long")
				return
			}
			// Protocol is left empty on live Usenet results. A pasted
			// filename has no protocol, so mark that fact absent
			// instead of letting evaluation assume Usenet.
			results = append(results, core.Release{ID: strconv.Itoa(i), Name: n, Info: releaseparse.Parse(n), Protocol: core.ProtocolUnstated})
		}
	}
	out := []ConsoleRelease{}
	// Filename preview has no catalogue title. Title preview passes
	// that title's Language into the same Context Resolve uses.
	note := originalLanguageNote(in.Profile.ReleaseRules, title)
	if title != nil {
		var trace Trace
		// Console preview is device-neutral, just like normal release search.
		caps := core.ClientCaps{}
		candidates, _, err := h.s.rankCandidates(ctx, core.ResolveRequest{Title: title, Client: caps}, searchRequestFor(title), in.Profile, results, &trace, "")
		if err != nil {
			h.fail(w, err)
			return
		}
		ranks := map[string]int{}
		for i, c := range candidates {
			ranks[c.rel.ID] = i + 1
		}
		for i, c := range trace.Candidates {
			out = append(out, ConsoleRelease{Release: browserRelease(results[i]), Preferences: c.Preferences, Rejected: c.Rejected, Reason: c.Reason, Score: c.Score, Rank: ranks[c.ReleaseID], CustomFormats: c.CustomFormats})
		}
		sort.SliceStable(out, func(i, j int) bool {
			if (out[i].Rank > 0) != (out[j].Rank > 0) {
				return out[i].Rank > 0
			}
			return out[i].Rank < out[j].Rank
		})
	} else {
		for _, rel := range results {
			v := policy.Evaluate(rel, releasepolicy.Context{})
			out = append(out, ConsoleRelease{Release: browserRelease(rel), Preferences: &v, Rejected: v.Rejected})
		}
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].Rejected != out[j].Rejected {
				return !out[i].Rejected
			}
			return releasepolicy.Better(*out[i].Preferences, *out[j].Preferences)
		})
		rank := 0
		for i := range out {
			if !out[i].Rejected {
				rank++
				out[i].Rank = rank
			}
		}
	}
	consoleJSON(w, 200, struct {
		Releases []ConsoleRelease `json:"releases"`
		Warning  string           `json:"warning,omitempty"`
		Note     string           `json:"note,omitempty"`
	}{Releases: out, Warning: warning, Note: note})
}

// originalLanguageNote tells a filename preview, or a title with no
// catalogue language, that original-language audio cannot be decided.
// A selected title with Title.Language set returns no note; the
// comparison then runs inside Evaluate.
func originalLanguageNote(rules []core.ReleaseRule, title *core.Title) string {
	if !releasepolicy.UsesOriginalAudio(rules) {
		return ""
	}
	if title == nil {
		return "original_language_unavailable"
	}
	if strings.TrimSpace(title.Language) == "" {
		return "original_language_unknown"
	}
	return ""
}
