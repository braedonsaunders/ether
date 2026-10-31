package migrate

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
)

// PMS's owner history endpoint includes every account unless explicitly scoped.
// The source's local account ID is distinct from a plex.tv account ID (the PMS
// owner, for example, is account 1). Never infer it from the destination row ID.
func (s *Service) sourceAccount(ctx context.Context, base, token string, v Viewer) (int64, error) {
	accounts, err := s.get(ctx, base, token, "/accounts")
	if err != nil {
		return 0, err
	}
	name := v.Name
	if name == "" {
		if user, err := s.viewer(ctx, v.LocalUserID); err == nil {
			name = user.Username
		}
	}
	for _, account := range accounts.Accounts {
		if account.ID <= 0 {
			continue
		}
		if v.SourceAccountID > 0 {
			if account.ID == v.SourceAccountID {
				return account.ID, nil
			}
		} else if (name != "" && strings.EqualFold(account.Name, name)) || (v.PlexID > 0 && account.ID == v.PlexID) {
			return account.ID, nil
		}
	}
	return 0, errors.New("could not identify the source Plex history account")
}

// One preview can contain thousands of episodes of the same show. Resolve each
// source show once, and reuse the identity for history entries lacking GUIDs.
// The maps are shared by the preview's worker pool.
type sourceMapper struct {
	service       *Service
	base, token   string
	mu            sync.Mutex
	keys, parents map[string]core.RatingKey
	missing       map[string]bool
	inflight      map[string]*mapCall
}

type mapCall struct {
	done chan struct{}
	key  core.RatingKey
	err  error
}

func (m *sourceMapper) cached(source string) core.RatingKey {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.keys[source]
}

func (m *sourceMapper) resolve(ctx context.Context, item media, fetched bool) (core.RatingKey, error) {
	if item.Key == "" {
		return m.resolveBody(ctx, item, fetched)
	}
	m.mu.Lock()
	if key := m.keys[item.Key]; key != 0 {
		m.mu.Unlock()
		return key, nil
	}
	if m.missing[item.Key] {
		m.mu.Unlock()
		return 0, core.ErrNotFound
	}
	if m.inflight == nil {
		m.inflight = map[string]*mapCall{}
	}
	if call := m.inflight[item.Key]; call != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-call.done:
			return call.key, call.err
		}
	}
	call := &mapCall{done: make(chan struct{})}
	m.inflight[item.Key] = call
	m.mu.Unlock()

	key, err := m.resolveBody(ctx, item, fetched)
	m.mu.Lock()
	if err == nil {
		m.keys[item.Key] = key
	} else if !errors.Is(err, context.Canceled) {
		m.missing[item.Key] = true
	}
	delete(m.inflight, item.Key)
	call.key, call.err = key, err
	close(call.done)
	m.mu.Unlock()
	return key, err
}

func (m *sourceMapper) resolveBody(ctx context.Context, item media, fetched bool) (core.RatingKey, error) {
	var key core.RatingKey
	err := core.ErrNotFound
	if item.Type == "episode" && item.Grandparent != "" {
		show, ok := m.showFor(ctx, item.Grandparent)
		if !ok && ctx.Err() != nil {
			return 0, ctx.Err()
		}
		if ok {
			return core.EpisodeKey(show.TMDB(), item.Season, item.Episode), nil
		}
	} else {
		key, err = m.service.mapMedia(ctx, m.base, m.token, item)
	}
	if err != nil && !fetched && item.Key != "" {
		if errors.Is(err, context.Canceled) {
			return 0, err
		}
		c, e := m.service.get(ctx, m.base, m.token, "/library/metadata/"+url.PathEscape(item.Key)+"?includeGuids=1")
		if errors.Is(e, context.Canceled) {
			return 0, e
		}
		if e == nil {
			details := append(c.Videos, c.Metadata...)
			if len(details) > 0 {
				// Same key is already the in-flight call. Resolve the fetched
				// record directly so this worker does not wait on itself.
				if details[0].Key == item.Key {
					key, err = m.resolveBody(ctx, details[0], true)
				} else {
					key, err = m.resolve(ctx, details[0], true)
				}
			}
		}
	}
	return key, err
}

func (m *sourceMapper) showFor(ctx context.Context, grandparent string) (core.RatingKey, bool) {
	const prefix = "gp\x00"
	id := prefix + grandparent
	m.mu.Lock()
	if show, seen := m.parents[grandparent]; seen {
		m.mu.Unlock()
		return show, show != 0
	}
	if m.inflight == nil {
		m.inflight = map[string]*mapCall{}
	}
	if call := m.inflight[id]; call != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return 0, false
		case <-call.done:
		}
		m.mu.Lock()
		show := m.parents[grandparent]
		m.mu.Unlock()
		return show, show != 0
	}
	call := &mapCall{done: make(chan struct{})}
	m.inflight[id] = call
	m.mu.Unlock()

	var show core.RatingKey
	c, e := m.service.get(ctx, m.base, m.token, "/library/metadata/"+url.PathEscape(grandparent)+"?includeGuids=1")
	if e == nil {
		for _, d := range c.Directories {
			show, _ = m.service.mapMedia(ctx, m.base, m.token, media{Type: "show", GUID: d.GUID, GUIDs: d.GUIDs})
			if show != 0 {
				break
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inflight, id)
	close(call.done)
	if errors.Is(e, context.Canceled) && show == 0 {
		return 0, false
	}
	m.parents[grandparent] = show
	return show, show != 0
}
