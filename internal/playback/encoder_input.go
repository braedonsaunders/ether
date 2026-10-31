package playback

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/braedonsaunders/ether/internal/core"
)

const maxEncoderInputs = 128

var encoderInputSlots = make(chan struct{}, maxEncoderInputs)

type encoderInputKey struct{}

type encoderInputScope struct {
	manager                  *Manager
	session                  *session
	source                   *sourceLifetime
	id                       string
	mu                       sync.Mutex
	claimed, sealed, drained bool
	pending                  int
	done                     chan struct{}
	callback                 func(error)
}

func (*encoderInputScope) String() string   { return "encoder input lease" }
func (*encoderInputScope) GoString() string { return "encoder input lease" }

func (m *Manager) ReserveEncoderInput(ctx context.Context, id string, expected core.SourceRetirement) (core.EncoderInputLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	responsibility, ok := expected.(*sourceLifetime)
	if !ok || responsibility == nil {
		return nil, core.ErrNotAvailable
	}
	// Entropy and allocation precede the manager lock. A collision refuses
	// admission before charging capacity; never overwrite another live scope
	// or retry entropy while holding the manager-global lock.
	scope := &encoderInputScope{manager: m, source: responsibility, id: randomID(32), done: make(chan struct{})}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := m.sessions[id]
	if m.closed || s == nil || s.sources != responsibility {
		return nil, core.ErrNotAvailable
	}
	// Source detachment/seal holds m.mu too. No stale session can mint across
	// that boundary or adopt a replacement having the same public ID.
	responsibility.mu.Lock()
	sealed := responsibility.sealed
	responsibility.mu.Unlock()
	if sealed {
		return nil, core.ErrNotAvailable
	}
	if _, exists := m.encoderInputs[scope.id]; exists {
		return nil, core.ErrNotAvailable
	}
	select {
	case encoderInputSlots <- struct{}{}:
	default:
		return nil, core.ErrNotAvailable
	}
	scope.session = s
	if m.encoderInputs == nil {
		m.encoderInputs = make(map[string]*encoderInputScope)
	}
	m.encoderInputs[scope.id] = scope
	return scope, nil
}

func (s *encoderInputScope) ClaimInput(callback func(error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed || s.sealed || callback == nil {
		return core.ErrNotAvailable
	}
	s.claimed, s.callback = true, callback
	return nil
}
func (s *encoderInputScope) InputDone() <-chan struct{} { return s.done }
func (s *encoderInputScope) InputErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	default:
		return core.ErrEncoderInputPending
	}
}
func (s *encoderInputScope) matches(g *generation) bool {
	return g != nil && g.sess == s.session && g.sess.sources == s.source
}
func (s *encoderInputScope) sourceActive() bool {
	s.source.mu.Lock()
	defer s.source.mu.Unlock()
	return !s.source.sealed
}
func (s *encoderInputScope) HeadersForInput(input string) (map[string]string, error) {
	u, err := url.Parse(input)
	origin, originErr := url.Parse(s.manager.baseURL)
	if err != nil || originErr != nil || u.Scheme != "http" || u.Scheme != origin.Scheme || u.Host != origin.Host || u.User != nil || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" || u.ForceQuery {
		return nil, core.ErrNotAvailable
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q) > 1 || (len(q) == 1 && (len(q["background"]) != 1 || q.Get("background") != "1")) {
		return nil, core.ErrNotAvailable
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "src" {
		return nil, core.ErrNotAvailable
	}
	m := s.manager
	m.mu.Lock()
	// Validate the supplied issued generation. Never replace it with a lookup
	// of whichever generation happens to be current now.
	g := m.tokens[parts[1]+"/"+parts[2]]
	valid := !m.closed && m.encoderInputs[s.id] == s && s.matches(g) && s.sourceActive()
	m.mu.Unlock()
	s.mu.Lock()
	valid = valid && s.claimed && !s.sealed
	s.mu.Unlock()
	if !valid {
		return nil, core.ErrNotAvailable
	}
	return map[string]string{core.EncoderInputScopeHeader: s.id}, nil
}
func (s *encoderInputScope) begin(g *generation) (func(), bool) {
	s.mu.Lock()
	if !s.claimed || s.sealed || !s.matches(g) || !s.sourceActive() {
		s.mu.Unlock()
		return nil, false
	}
	s.pending++
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); s.pending--; s.mu.Unlock(); s.drain() }) }, true
}
func (s *encoderInputScope) SealInput() {
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock()
	s.drain()
}
func (s *encoderInputScope) drain() {
	s.mu.Lock()
	if !s.sealed || s.pending != 0 || s.drained {
		s.mu.Unlock()
		return
	}
	s.drained = true
	callback := s.callback
	s.mu.Unlock()
	m := s.manager
	m.mu.Lock()
	if m.encoderInputs[s.id] == s {
		delete(m.encoderInputs, s.id)
	}
	m.mu.Unlock()
	<-encoderInputSlots
	// Ownership is released and the immutable result is visible before either
	// the completion channel or the trusted reentrant callback can be observed.
	close(s.done)
	if callback != nil {
		callback(nil)
	}
}

func (m *Manager) encoderInputRequest(r *http.Request, g *generation) (*http.Request, error) {
	values := r.Header.Values(core.EncoderInputScopeHeader)
	if len(values) == 0 {
		return r, nil
	}
	if len(values) != 1 || values[0] == "" {
		return nil, core.ErrNotAvailable
	}
	m.mu.Lock()
	scope := m.encoderInputs[values[0]]
	valid := scope != nil && !m.closed && m.tokens[g.sess.id+"/"+g.token] == g && scope.matches(g) && scope.sourceActive()
	m.mu.Unlock()
	if valid {
		scope.mu.Lock()
		valid = scope.claimed && !scope.sealed
		scope.mu.Unlock()
	}
	if !valid {
		return nil, errors.New("playback: unverified encoder input")
	}
	return r.WithContext(context.WithValue(r.Context(), encoderInputKey{}, scope)), nil
}

var _ core.PlaybackEncoderInputs = (*Manager)(nil)
