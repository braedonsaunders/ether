package resolver

import (
	"sort"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	chooserAuto   = "auto"
	chooserViewer = "viewer"
	// decisionCandidateCap bounds the support-capture candidate list.
	// The winner and ceiling rejections are kept ahead of the rest.
	decisionCandidateCap = 24
	decisionCeilingCap   = 8
)

// noteChooser records who selected the winner. A pin stays "auto": the
// owner pin is a separate badge. A viewer pick that lost to fallback is
// "auto", because Ether chose the release that played.
func noteChooser(trace *Trace, req core.ResolveRequest) {
	if trace == nil || trace.Winner == "" {
		return
	}
	if req.Prefer != "" && trace.Winner == req.Prefer {
		trace.Chooser = chooserViewer
		for _, c := range trace.Candidates {
			if c.ReleaseID == trace.Winner {
				trace.ChosenVersion = c.Name
				break
			}
		}
		return
	}
	trace.Chooser = chooserAuto
}

// LatestDecision implements core.DecisionTraceSource. It reads the newest
// in-memory trace for the title, then compacts candidates so a large
// search still fits a support capture.
func (s *Service) LatestDecision(key core.RatingKey) (core.DecisionTrace, bool) {
	s.tracesMu.Lock()
	defer s.tracesMu.Unlock()
	for i := len(s.traces) - 1; i >= 0; i-- {
		tr := s.traces[i]
		if tr.TitleKey != key || tr.Winner == "" {
			continue
		}
		return decisionFromTrace(tr), true
	}
	return core.DecisionTrace{}, false
}

func decisionFromTrace(tr Trace) core.DecisionTrace {
	d := core.DecisionTrace{
		Profile:       tr.Profile,
		Chooser:       tr.Chooser,
		ChosenVersion: tr.ChosenVersion,
		Candidates:    compactDecisionCandidates(tr),
	}
	if tr.Limits != nil {
		d.ClientMaxHeight = tr.Limits.ClientMaxHeight
		d.EffectiveMaxHeight = tr.Limits.ProfileMaxHeight
		d.AllowHDR = tr.Limits.AllowHDR
		d.AllowDolbyVision = tr.Limits.AllowDolbyVision
		if tr.Limits.ProfileConfiguredMaxHeight != nil {
			d.ProfileMaxHeight = *tr.Limits.ProfileConfiguredMaxHeight
		} else {
			d.ProfileMaxHeight = tr.Limits.ProfileMaxHeight
		}
	}
	if d.ChosenVersion == "" {
		for _, c := range tr.Candidates {
			if c.ReleaseID == tr.Winner {
				d.ChosenVersion = c.Name
				break
			}
		}
	}
	return d
}

func compactDecisionCandidates(tr Trace) []core.DecisionCandidate {
	cands := append([]CandidateTrace(nil), tr.Candidates...)
	sort.SliceStable(cands, func(i, j int) bool {
		ri, rj := decisionRank(tr.Winner, cands[i]), decisionRank(tr.Winner, cands[j])
		if ri != rj {
			return ri < rj
		}
		return cands[i].Score > cands[j].Score
	})
	out := make([]core.DecisionCandidate, 0, decisionCandidateCap)
	ceilings := 0
	for _, c := range cands {
		if len(out) >= decisionCandidateCap {
			break
		}
		rank := decisionRank(tr.Winner, c)
		if rank == 1 {
			if ceilings >= decisionCeilingCap {
				continue
			}
			ceilings++
		}
		out = append(out, core.DecisionCandidate{
			ReleaseID: c.ReleaseID, Name: c.Name, Height: c.Height, Size: c.Size,
			Score: c.Score, Rejected: c.Rejected, Reason: c.Reason,
			Healthy: c.Healthy, HealthChecked: c.HealthChecked,
		})
	}
	return out
}

func decisionRank(winner string, c CandidateTrace) int {
	switch {
	case c.ReleaseID != "" && c.ReleaseID == winner:
		return 0
	case c.Rejected && ceilingRejection(c.Reason):
		return 1
	case c.HealthChecked:
		return 2
	default:
		return 3
	}
}

func ceilingRejection(reason string) bool {
	switch reason {
	case "above profile max height", "above client max height", "dolby vision not allowed for client", "size too large for runtime":
		return true
	default:
		return false
	}
}
