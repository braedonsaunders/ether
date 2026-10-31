//go:build plexparity

package playback

import (
	"context"
	"fmt"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// ParitySettleWarm starts this title's detail warm-up when none is recorded,
// then waits until that warm-up has finished or failed. Disposable parity
// builds call it before a metadata read so both servers render probed tracks
// instead of racing the 100ms detail bound. Production builds do not include it.
func (m *Manager) ParitySettleWarm(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil || user == nil || caps.ClientID == "" {
		return nil
	}
	m.Prewarm(ctx, user, key, caps, core.PrewarmOptions{Reason: core.PrewarmDetail})
	// Plex warm-ups use the zero surface. Leaving newer identity fields at
	// zero keeps this seam compiling against released main.
	id := identity{userID: user.ID, clientID: caps.ClientID, key: key}
	m.mu.Lock()
	e := m.warm[id]
	live := e != nil && m.now().Before(e.expires)
	m.mu.Unlock()
	if !live {
		return nil
	}
	select {
	case <-e.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ParitySettlePending waits until every recorded warm-up has finished or
// failed. It does not start a probe. Disposable parity builds call it after
// seeding and after each corpus mutation so the next read renders probed
// facts instead of racing the 100ms detail bound. Production builds do not
// include it.
func (m *Manager) ParitySettlePending(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil {
		return nil
	}
	m.mu.Lock()
	now := m.now()
	pending := make([]<-chan struct{}, 0, len(m.warm))
	for _, e := range m.warm {
		if e == nil || !now.Before(e.expires) {
			continue
		}
		select {
		case <-e.done:
		default:
			pending = append(pending, e.done)
		}
	}
	m.mu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ParityProbeNow resolves and probes this title before returning, then stores
// the facts where detail and list reads look them up. It ignores the warm-up
// budget, so both parity servers can probe every fixture title before the
// read pass. A title with no playable release is skipped. Production builds
// do not include it.
func (m *Manager) ParityProbeNow(ctx context.Context, user *core.User, key core.RatingKey, caps core.ClientCaps) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if m == nil || user == nil || caps.ClientID == "" || m.deps.Catalog == nil || m.deps.Engine == nil || m.deps.SelectProfile == nil {
		return nil
	}
	// Plex reads look the probe up without a surface. Leaving newer identity
	// fields zero keeps this seam compiling against released main.
	id := identity{userID: user.ID, clientID: caps.ClientID, key: key}
	m.mu.Lock()
	if e := m.warm[id]; e != nil && m.now().Before(e.expires) && e.probe != nil && !e.probe.info.Estimated && len(e.probe.info.Streams) > 0 {
		select {
		case <-e.done:
			m.mu.Unlock()
			return nil
		default:
		}
	}
	m.mu.Unlock()
	title, err := m.deps.Catalog.Get(ctx, key)
	if err != nil || title == nil || title.Type == core.TypeShow || title.Type == core.TypeSeason {
		return nil
	}
	if !title.Released.IsZero() && title.Released.After(m.now()) {
		return nil
	}
	profile := m.deps.SelectProfile(caps, user)
	res := m.resumeResolution(ctx, user.ID, key, profile, caps, strings.TrimSpace(title.Language))
	if res == nil || !releaseMatchesTitle(res.Release, title) {
		if m.deps.Resolver == nil {
			return nil
		}
		res, err = m.deps.Resolver.Resolve(ctx, core.ResolveRequest{Title: title, Client: caps, User: user})
		if err != nil || res == nil {
			return nil
		}
	}
	src, err := m.deps.Engine.Open(ctx, res.Release.ID, res.NZB)
	if err != nil {
		return fmt.Errorf("parity probe %s: %w", title.Title, err)
	}
	defer src.Close()
	if m.deps.NativeProbe == nil {
		return fmt.Errorf("parity probe %s: native probe unavailable", title.Title)
	}
	info, err := m.deps.NativeProbe(ctx, src)
	if err != nil || info == nil || len(info.Streams) == 0 {
		if err == nil {
			err = fmt.Errorf("no streams")
		}
		return fmt.Errorf("parity probe %s: %w", title.Title, err)
	}
	probed := *info
	probed.Estimated = false
	if probed.Size <= 0 {
		probed.Size = src.Size()
	}
	done := make(chan struct{})
	close(done)
	entry := &warmEntry{
		profileKey: profileKey(profile),
		profileObj: profile,
		caps:       caps,
		done:       done,
		expires:    m.now().Add(preResolveTTL),
		runtime:    title.Runtime,
		res:        res,
		probe:      &probeCache{releaseID: res.Release.ID, info: probed},
	}
	m.mu.Lock()
	m.warm[id] = entry
	m.mu.Unlock()
	return nil
}
