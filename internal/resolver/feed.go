package resolver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/braedonsaunders/ether/internal/core"
)

// feedClient is the broad client the evidence check judges for: the default
// owner's profile decides eligibility, never a claim about every device.
var feedClient = core.ClientCaps{HDR10: true, DolbyVision: true}

const feedRejectedReason = "not accepted by the current quality policy"

// FeedCandidate applies the existing quality, identity and block policies without
// searching. Broad capabilities mean the default owner's profile determines
// eligibility; this is source evidence, never a claim about every device.
func (s *Service) FeedCandidate(ctx context.Context, t *core.Title, r core.Release) (core.Release, string, error) {
	e, err := s.feedEvaluationForProfile(ctx, titleMedia(t), s.ProfileForTitle(ctx, feedClient, nil, t))
	if err != nil {
		return r, "", err
	}
	defer e.Close()
	return e.judge(ctx, t, r)
}

// FeedEvaluation prepares one evidence pass: the profile, compiled release
// rules, resolver settings and catalogue language are read once and shared
// by every release the pass judges.
func (s *Service) FeedEvaluation(ctx context.Context) (core.FeedEvaluation, error) {
	return s.feedEvaluationSet(ctx)
}

func (s *Service) feedEvaluationForMedia(ctx context.Context, media string) (*feedEvaluation, error) {
	p := SelectProfileForMedia(s.qualityCfg(), feedClient, nil, media)
	return s.feedEvaluationForProfile(ctx, media, p)
}

func (s *Service) feedEvaluationForProfile(ctx context.Context, media string, p core.QualityProfile) (*feedEvaluation, error) {
	policy, err := s.compileRules(p.ReleaseRules)
	if err != nil {
		return nil, err
	}
	e := &feedEvaluation{s: s, media: normalizeMedia(media), profile: p, env: rankEnv{policy: policy, exclude: map[string]bool{}, rcfg: s.resolverCfg(), lang: s.catalogLanguage()}}
	if usesPluginRanking(p) {
		e.env.hot = s.rankingSession(ctx, feedClient)
	}
	return e, nil
}

// feedEvaluationSet judges a mixed feed under each media's profile. Key
// covers both so a stored verdict is exact for either title kind.
type feedEvaluationSet struct {
	movie, tv *feedEvaluation
}

func (s *Service) feedEvaluationSet(ctx context.Context) (*feedEvaluationSet, error) {
	movie, err := s.feedEvaluationForMedia(ctx, core.MediaMovie)
	if err != nil {
		return nil, err
	}
	tv, err := s.feedEvaluationForMedia(ctx, core.MediaTV)
	if err != nil {
		movie.Close()
		return nil, err
	}
	return &feedEvaluationSet{movie: movie, tv: tv}, nil
}

func (e *feedEvaluationSet) forTitle(t *core.Title) *feedEvaluation {
	if titleMedia(t) == core.MediaTV {
		return e.tv
	}
	return e.movie
}

func (e *feedEvaluationSet) Key() string { return e.movie.Key() + "|" + e.tv.Key() }

func (e *feedEvaluationSet) Evaluate(ctx context.Context, t *core.Title, rs []core.Release) ([]core.FeedVerdict, error) {
	base := e.forTitle(t)
	p := base.s.ProfileForTitle(ctx, feedClient, nil, t)
	if p.SelectionKey == base.profile.SelectionKey {
		return base.Evaluate(ctx, t, rs)
	}
	selected, err := base.s.feedEvaluationForProfile(ctx, titleMedia(t), p)
	if err != nil {
		return nil, err
	}
	defer selected.Close()
	return selected.Evaluate(ctx, t, rs)
}

func (e *feedEvaluationSet) Transient(id string) string { return e.movie.Transient(id) }

func (e *feedEvaluationSet) Close() {
	e.movie.Close()
	e.tv.Close()
}

type feedEvaluation struct {
	s       *Service
	media   string
	profile core.QualityProfile
	env     rankEnv
	key     string
}

// Key covers every input of a durable verdict that is not the title or
// the release: the selected profile, resolver settings (blocked terms,
// custom formats), the catalogue language, whether any profile allows
// pre-release copies, and the rule field table.
func (e *feedEvaluation) Key() string {
	if e.key != "" {
		return e.key
	}
	var registry uint64
	if r := e.s.extendRegistry(); r != nil {
		registry = r.Version()
	}
	profile := e.profile
	profile.Import = nil // provenance is not policy; SyncedAt must not invalidate verdicts
	var libraries string
	if policy, ok := e.s.libProfiles.(interface{ PolicyKey(context.Context) string }); ok {
		libraries = policy.PolicyKey(context.Background())
	}
	quality := e.s.qualityCfg()
	quality.Profiles = slices.Clone(quality.Profiles)
	for i := range quality.Profiles {
		quality.Profiles[i].Import = nil
	}
	b, _ := json.Marshal([]any{"feed-verdict-v4", libraries, quality, e.media, profile.SelectionKey, profile, e.env.rcfg, e.env.lang, e.s.profileAllowsCam(), registry})
	sum := sha256.Sum256(b)
	e.key = hex.EncodeToString(sum[:])
	return e.key
}

func (e *feedEvaluation) Evaluate(ctx context.Context, t *core.Title, rs []core.Release) ([]core.FeedVerdict, error) {
	out := make([]core.FeedVerdict, len(rs))
	req, sreq, env := e.request(t)
	for i, r := range rs {
		_, reason, err := e.judgeWith(ctx, req, sreq, env, r)
		if err != nil {
			return nil, err
		}
		out[i] = core.FeedVerdict{Reason: reason, Volatile: env.hot != nil || volatileFeedReason(reason)}
	}
	return out, nil
}

// Transient replays the short-lived gates rejectReason applies, in its
// order, for a release a durable verdict accepted.
func (e *feedEvaluation) Transient(id string) string {
	if e.s.isBad(id) {
		return "marked bad"
	}
	if e.s.recentlyFailedHealth(id) {
		return "source recently failed health check; retrying later"
	}
	return ""
}

func (e *feedEvaluation) Close() {
	if e.env.hot != nil {
		e.env.hot.Close()
	}
}

func volatileFeedReason(reason string) bool {
	switch reason {
	case "marked bad", "source recently failed health check; retrying later", errFormatTimeout.Error():
		return true
	}
	return false
}

func (e *feedEvaluation) request(t *core.Title) (core.ResolveRequest, core.SearchRequest, rankEnv) {
	req := core.ResolveRequest{Title: t, Client: feedClient}
	env := e.env
	env.audio(e.profile, req)
	return req, searchRequestFor(t), env
}

func (e *feedEvaluation) judge(ctx context.Context, t *core.Title, r core.Release) (core.Release, string, error) {
	req, sreq, env := e.request(t)
	return e.judgeWith(ctx, req, sreq, env, r)
}

// judgeWith ranks the release on its own, exactly as a one-result search.
func (e *feedEvaluation) judgeWith(ctx context.Context, req core.ResolveRequest, sreq core.SearchRequest, env rankEnv, r core.Release) (core.Release, string, error) {
	if reason := packEvidenceReason(r, sreq); reason != "" {
		return r, reason, nil
	}
	trace := Trace{}
	candidates, _, err := e.s.rankWith(ctx, env, req, sreq, e.profile, []core.Release{r}, &trace, "")
	if err != nil {
		return r, "", err
	}
	if len(candidates) == 0 {
		reason := feedRejectedReason
		if len(trace.Candidates) > 0 {
			reason = trace.Candidates[0].Reason
		}
		return r, reason, nil
	}
	return candidates[0].rel, "", nil
}

// packEvidenceReason keeps a feed item from standing in for an episode it is
// not shown to hold. A Usenet season pack is narrowed to one episode only when
// its NZB is fetched, so from its name alone it proves just the episodes it
// lists ("S01E03-E10"): a bare "S01" says nothing about a premiere that may
// not be in it. Torrent packs are unchanged. "" accepts.
func packEvidenceReason(r core.Release, sreq core.SearchRequest) string {
	if sreq.Type != core.TypeEpisode || !r.Info.FullSeason || r.Protocol != core.ProtocolUsenet {
		return ""
	}
	if !slices.Contains(r.Info.Episodes, sreq.Episode) {
		return "season pack does not list the episode"
	}
	return ""
}

// VerifyFeedCandidate validates exactly the discovered release. It never invokes
// Search or rescue searches and never writes a title-wide negative result.
func (s *Service) VerifyFeedCandidate(ctx context.Context, t *core.Title, r core.Release) error {
	r, reason, err := s.FeedCandidate(ctx, t, r)
	if err != nil {
		return err
	}
	if reason != "" {
		return fmt.Errorf("%w: %s", core.ErrReleaseRejected, reason)
	}
	if s.health == nil {
		return core.ErrSourceUnavailable
	}
	payload, err := s.indexer.FetchNZB(ctx, r)
	if err != nil {
		return err
	}
	ok, err := core.CheckReleaseHealth(ctx, s.health, r.ID, payload)
	if err != nil {
		return err
	}
	if !ok {
		return core.ErrSourceUnavailable
	}
	s.markVerified(ctx, r.ID)
	s.rememberReleases(t, []core.Release{r})
	s.setAvailability(ctx, int64(t.Key), true, s.searchTTL())
	return nil
}
