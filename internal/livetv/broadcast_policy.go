package livetv

import (
	"context"
	"encoding/json"
	"reflect"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// Immutable local facts captured by the planner, never a request-side catalogue
// lookup. The pointer is the policy identity carried through the encoder.
type broadcastPolicyTag struct {
	programme Programme
	title     *core.Title
	catalog   bool
}

func (s *Service) programmePolicy(ctx context.Context, p Programme) *broadcastPolicyTag {
	tag := &broadcastPolicyTag{programme: p, catalog: s.o.Catalog != nil}
	tag.programme.policyTag = nil
	if p.Item.Key != 0 && s.o.Catalog != nil {
		title, err := s.o.Catalog.Get(ctx, p.Item.Key)
		if err == nil && title != nil {
			copy := *title
			tag.title = &copy
		}
	}
	return tag
}

type broadcastViewerPolicy struct {
	epoch     int64
	revision  uint64
	raw       string
	user      core.User
	channel   Channel
	policy    Policy
	decisions map[*broadcastPolicyTag]bool
}
type broadcastPolicyContextKey struct{}

// One indexed statement observes policy writes from every node and channel
// edits. Parsing and decisions are reused for the life of the viewer session.
func (s *Service) broadcastPolicyContext(ctx context.Context, ss *session, u *core.User) (context.Context, error) {
	if cached, ok := ctx.Value(broadcastPolicyContextKey{}).(*session); ok && cached == ss {
		return ctx, nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	var epoch int64
	var raw string
	err := s.o.DB.QueryRowContext(ctx, `SELECT generation,COALESCE((SELECT data FROM livetv_policies WHERE user_id=?),'{}') FROM livetv_channel_epoch WHERE id=1`, u.ID).Scan(&epoch, &raw)
	if err != nil {
		return ctx, err
	}
	s.virtualGuide.mu.RLock()
	revision := s.virtualGuide.revision
	s.virtualGuide.mu.RUnlock()
	cache := &ss.broadcastPolicy
	if cache.decisions == nil || cache.epoch != epoch || cache.revision != revision || cache.raw != raw || !reflect.DeepEqual(cache.user, *u) {
		c := ss.info.Channel

		// Channel saves/background refresh publish decoded records. An external
		// edit waits for that refresh instead of decoding a recipe on this request.
		s.chanSnap.mu.Lock()
		if s.chanSnap.generation != epoch {
			s.chanSnap.mu.Unlock()
			s.wakeVirtualGuide()
			return ctx, core.ErrNotAvailable
		}
		at, exists := s.chanSnap.byID[c.ID]
		if exists {
			c = s.chanSnap.list[at]
		}
		s.chanSnap.mu.Unlock()
		if !exists {
			return ctx, core.ErrNotFound
		}

		var pol Policy
		if err = json.Unmarshal([]byte(raw), &pol); err != nil {
			return ctx, err
		}
		pol.indexChannels()
		*cache = broadcastViewerPolicy{epoch: epoch, revision: revision, raw: raw, user: *u, channel: c, policy: pol, decisions: map[*broadcastPolicyTag]bool{}}
	}
	ss.user = *u
	ctx = context.WithValue(ctx, loadedPolicyKey{}, loadedPolicy{id: u.ID, user: u, pol: cache.policy})
	return context.WithValue(ctx, broadcastPolicyContextKey{}, ss), nil
}
func (s *Service) broadcastTagsAllowed(ctx context.Context, ss *session, tags []any) bool {
	ss.mu.Lock()
	user := ss.user
	ss.mu.Unlock()
	ctx, err := s.broadcastPolicyContext(ctx, ss, &user)
	if err != nil {
		return false
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	cache := &ss.broadcastPolicy
	c := cache.channel
	if !c.Enabled || (!user.IsOwner && (s.sweepState(c) == "unavailable" || !cache.policy.Permits(c.ID, c.Private))) || (c.Personal != nil && (s.o.Picker == nil || (cache.policy.Personal != nil && cache.policy.Personal.Enabled != nil && !*cache.policy.Personal.Enabled))) {
		return false
	}
	for _, identity := range tags {
		tag, ok := identity.(*broadcastPolicyTag)
		if !ok {
			return false
		}
		if tag == nil {
			if !s.allowedWithLookup(ctx, &user, c, nil, nil, false) {
				return false
			}
			continue
		} // channel-only screen
		allowed, found := cache.decisions[tag]
		if !found {
			var lookup func(context.Context, core.RatingKey) (*core.Title, error)
			if tag.catalog {
				lookup = func(context.Context, core.RatingKey) (*core.Title, error) { return tag.title, nil }
			}
			allowed = s.allowedWithLookup(ctx, &user, cache.channel, &tag.programme, lookup, false)
			if len(cache.decisions) >= 2048 {
				clear(cache.decisions)
			}
			cache.decisions[tag] = allowed
		}
		if !allowed {
			return false
		}
	}
	return true
}
func (s *Service) broadcastSegmentAllowed(ctx context.Context, ss *session, seg transcode.Segment) bool {
	// Untagged virtual media must never bypass programme restrictions.
	if len(seg.PolicyTags) == 0 {
		return false
	}
	return s.broadcastTagsAllowed(ctx, ss, seg.PolicyTags)
}

// Audience admission is independent of the programme's rating. Checking a
// channel's empty rating first would incorrectly hide a permitted programme.
func (s *Service) audienceAllowed(ctx context.Context, u *core.User, c Channel) bool {
	if u == nil || !c.Enabled || (!u.IsOwner && s.sweepState(c) == "unavailable") {
		return false
	}
	_, pol, err := s.withPolicy(ctx, u)
	if err != nil {
		return false
	}
	if c.Personal != nil && (s.o.Picker == nil || (pol.Personal != nil && pol.Personal.Enabled != nil && !*pol.Personal.Enabled)) {
		return false
	}
	return u.IsOwner || pol.Permits(c.ID, c.Private)
}
