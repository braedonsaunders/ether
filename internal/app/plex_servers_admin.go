package app

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// Is lets the console tell an account already in use from other failures.
func (e errPlexAccountInUse) Is(target error) bool { return target == admin.ErrPlexAccountInUse }

// plexServerView is one server as the console shows it.
type plexServerView struct {
	ID            string `json:"id"`
	Main          bool   `json:"main"`
	Name          string `json:"name"`
	NameInherited bool   `json:"nameInherited"`
	Account       string `json:"account,omitempty"`
	State         string `json:"state"`
	Running       bool   `json:"running"`
	Claimed       bool   `json:"claimed"`
	SeatsUsed     int    `json:"seatsUsed"`
	AddedAt       int64  `json:"addedAt,omitempty"`
	ActivatedAt   int64  `json:"activatedAt,omitempty"`
	Retired       bool   `json:"retired,omitempty"`
	Blocked       bool   `json:"blocked,omitempty"`
	BlockedReason string `json:"blockedReason,omitempty"`
	Problem       string `json:"problem,omitempty"`
}

// plexServersView is the console's Plex servers panel and the dashboard
// section the blocked-server warning reads.
type plexServersView struct {
	// Enabled: the owner turned extra Plex servers on (off by default).
	Enabled bool `json:"enabled"`
	// MaxExtra is how many extra Plex servers can be added; ExtraCount how
	// many are.
	MaxExtra       int              `json:"maxExtra"`
	ExtraCount     int              `json:"extraCount"`
	SeatsPerServer int              `json:"seatsPerServer"`
	SeatsUsed      int              `json:"seatsUsed"`
	Servers        []plexServerView `json:"servers"`
	// Standby counts added accounts the owner has not started.
	Standby int `json:"standby"`
	// Claimed: the main server is linked to plex.tv (extra servers need it).
	Claimed bool `json:"claimed"`
	// Blocked names the servers plex.tv seems to have blocked or banned.
	Blocked []string `json:"blocked,omitempty"`
	// Room: some server still takes new viewers.
	Room bool `json:"room"`
}

func (a *App) plexExtrasStatus(ctx context.Context) any {
	if a.plexExtras == nil {
		return nil
	}
	return a.plexExtras.view(ctx)
}

func (ps *plexExtras) view(ctx context.Context) plexServersView {
	cfg := ps.app.cfg.Get()
	used := ps.seatsUsed(ctx)
	out := plexServersView{
		Enabled:        cfg.Plex.ExtraServersEnabled,
		MaxExtra:       cfg.PlexMaxExtraServers(),
		ExtraCount:     len(cfg.PlexExtraServers),
		SeatsPerServer: cfg.PlexSeatsPerServer(),
		Claimed:        cfg.Plex.Claimed,
	}
	out.Servers = append(out.Servers, plexServerView{
		ID: "main", Main: true, Name: cfg.Server.FriendlyName, Account: cfg.Plex.OwnerUsername,
		State: config.PlexServerActive, Running: cfg.Plex.Claimed, Claimed: cfg.Plex.Claimed, SeatsUsed: used[""],
		Retired: cfg.Plex.MainRetired,
	})
	h := ps.health("")
	out.Servers[0].Blocked, out.Servers[0].BlockedReason, out.Servers[0].Problem = h.Blocked, h.Reason, h.Problem
	for _, s := range append(activePlexOrder(cfg), standbyPlexOrder(cfg)...) {
		v := plexServerView{
			ID: s.ID, Name: s.AdvertisedName(cfg), NameInherited: s.Name == "", Account: s.Account.Username,
			State: s.State, Running: ps.runtime(s.ID) != nil, Claimed: s.Claimed, SeatsUsed: used[s.ID],
			AddedAt: s.AddedAt, ActivatedAt: s.ActivatedAt,
			Retired: s.Retired,
		}
		h := ps.health(s.ID)
		v.Blocked, v.BlockedReason, v.Problem = h.Blocked, h.Reason, h.Problem
		if !s.Active() {
			out.Standby++
		}
		out.Servers = append(out.Servers, v)
	}
	for _, v := range out.Servers {
		if v.Blocked {
			out.Blocked = append(out.Blocked, v.Name)
		}
		if v.State == config.PlexServerActive && v.Claimed && !v.Blocked && !v.Retired && v.SeatsUsed < out.SeatsPerServer {
			out.Room = true
		}
		if v.State == config.PlexServerActive {
			out.SeatsUsed += v.SeatsUsed
		}
	}
	return out
}

// plexEnrollment is one pending sign-in for a new extra server. It owns the
// setup identifier allocated before its PIN was created: client sends that
// identifier for the PIN, the approval page, every poll and the account
// lookup, and the stored profile keeps it. plex.tv need not echo it.
type plexEnrollment struct {
	clientID string
	client   *plextv.Client
	pin      *plextv.PIN
	// sem serialises completion: concurrent polls of one sign-in store one
	// profile, and the others report the same outcome.
	sem chan struct{}
	// The fields below are guarded by sem. token is the approved account
	// token, kept for a retry after a failure; err is a terminal failure
	// (expired, account in use).
	token  string
	done   bool
	result map[string]any
	err    error
}

// plexEnrollments holds the pending sign-ins. pins is bounded and expired by
// rememberPIN; flows holds the same keys, so a PIN and its setup identifier
// are always dropped together.
type plexEnrollments struct {
	mu    sync.Mutex
	pins  map[string]*plextv.PIN
	flows map[string]*plexEnrollment
}

// prune drops every flow whose PIN rememberPIN discarded. The caller holds mu.
func (s *plexEnrollments) prune() {
	for id := range s.flows {
		if s.pins[id] == nil {
			delete(s.flows, id)
		}
	}
}

func (s *plexEnrollments) add(id string, e *plexEnrollment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rememberPIN(s.pins, id, e.pin)
	s.flows[id] = e
	s.prune()
}

// get returns the pending sign-in id; expired reports one that ran out.
func (s *plexEnrollments) get(id string) (e *plexEnrollment, expired bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e = s.flows[id]
	if e == nil {
		return nil, false
	}
	if !e.pin.ExpiresAt.IsZero() && !time.Now().Before(e.pin.ExpiresAt) {
		delete(s.pins, id)
		delete(s.flows, id)
		return nil, true
	}
	return e, false
}

// drop forgets the sign-in id if it is still e.
func (s *plexEnrollments) drop(id string, e *plexEnrollment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.flows[id] == e {
		delete(s.pins, id)
		delete(s.flows, id)
	}
}

// clientIDPending reports whether a pending sign-in already uses clientID.
func (s *plexEnrollments) clientIDPending(clientID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.flows {
		if e.clientID == clientID {
			return true
		}
	}
	return false
}

var (
	errPlexEnrollmentUnknown = errors.New("unknown sign-in request; start again")
	errPlexEnrollmentExpired = errors.New("sign-in request expired; start again")
)

// plexExtrasAdmin is the console API (admin.PlexExtras).
type plexExtrasAdmin struct {
	app     *App
	pending *plexEnrollments
}

func newPlexExtrasAdmin(a *App) plexExtrasAdmin {
	return plexExtrasAdmin{app: a, pending: &plexEnrollments{pins: map[string]*plextv.PIN{}, flows: map[string]*plexEnrollment{}}}
}

func (p plexExtrasAdmin) Status(ctx context.Context) any { return p.app.plexExtras.view(ctx) }

// enrollmentClient returns a client for one new profile's sign-in: the setup
// wizard's product identity with clientID as its identifier, on a copy of the
// plex.tv client (same transport, endpoints and timeouts). The shared client
// and the main setup identity are not changed.
func (p plexExtrasAdmin) enrollmentClient(clientID string) *plextv.Client {
	id := plextv.AppIdentity(p.app.cfg.Get(), "")
	id.ClientIdentifier = clientID
	return p.app.registrar.Client().WithIdentity(id)
}

// newSetupID allocates an identifier no profile or pending sign-in uses.
func (p plexExtrasAdmin) newSetupID() (string, error) {
	cfg := p.app.cfg.Get()
	for i := 0; i < 8; i++ {
		id := config.NewPlexClientIdentifier()
		if !cfg.PlexClientIdentifierInUse(id) && !p.pending.clientIDPending(id) {
			return id, nil
		}
	}
	return "", errors.New("could not allocate a Plex setup identifier")
}

func (p plexExtrasAdmin) StartPIN(ctx context.Context) (id, code, authURL string, err error) {
	if p.app.registrar == nil {
		return "", "", "", errors.New("plex.tv is not available")
	}
	cfg := p.app.cfg.Get()
	if !cfg.Plex.Claimed {
		return "", "", "", errors.New("link the main Plex server first")
	}
	if !cfg.Plex.ExtraServersEnabled {
		return "", "", "", errPlexExtrasOff
	}
	if n := cfg.PlexMaxExtraServers(); len(cfg.PlexExtraServers) >= n {
		return "", "", "", errPlexExtrasLimit{max: n}
	}
	clientID, err := p.newSetupID()
	if err != nil {
		return "", "", "", err
	}
	c := p.enrollmentClient(clientID)
	pin, err := c.CreatePIN(ctx)
	if err != nil {
		return "", "", "", err
	}
	id = strconv.FormatInt(pin.ID, 10)
	p.pending.add(id, &plexEnrollment{clientID: clientID, client: c, pin: pin, sem: make(chan struct{}, 1)})
	return id, pin.Code, c.AuthURL(pin, ""), nil
}

func (p plexExtrasAdmin) PollPIN(ctx context.Context, id string) (bool, any, error) {
	e, expired := p.pending.get(id)
	if expired {
		return false, nil, errPlexEnrollmentExpired
	}
	if e == nil {
		return false, nil, errPlexEnrollmentUnknown
	}
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return false, nil, ctx.Err()
	}
	defer func() { <-e.sem }()
	// A poll that waited behind the one that finished reports its outcome.
	if e.done {
		if e.err != nil {
			return false, nil, e.err
		}
		return true, e.result, nil
	}
	fail := func(err error) (bool, any, error) {
		e.done, e.err, e.token = true, err, ""
		p.pending.drop(id, e)
		return false, nil, err
	}
	if !e.pin.ExpiresAt.IsZero() && !time.Now().Before(e.pin.ExpiresAt) {
		return fail(errPlexEnrollmentExpired)
	}
	if e.token == "" {
		got, err := e.client.CheckPIN(ctx, e.pin.ID, e.pin.Code)
		if errors.Is(err, core.ErrNotFound) {
			// plex.tv deletes expired PINs.
			return fail(errPlexEnrollmentExpired)
		}
		if err != nil {
			return false, nil, err
		}
		if got.AuthToken == "" {
			return false, nil, nil
		}
		e.token = got.AuthToken
	}
	added, err := p.app.plexExtras.addAccount(ctx, e.client, e.clientID, e.token)
	switch {
	case err == nil:
	case errors.Is(err, admin.ErrPlexAccountInUse), errors.Is(err, errPlexSetupIDInUse), errors.Is(err, core.ErrUnauthorized):
		// Trying again cannot succeed: forget the sign-in.
		return fail(err)
	default:
		// The account lookup or the save failed and nothing was stored. The
		// sign-in stays until it expires, so the next poll tries again with
		// the same identifier and token.
		return false, nil, err
	}
	e.done, e.token = true, ""
	e.result = map[string]any{"id": added.ID, "account": added.Account.Username, "state": added.State}
	p.pending.drop(id, e)
	return true, e.result, nil
}

func (p plexExtrasAdmin) Start(ctx context.Context, id string) error {
	return p.app.plexExtras.activate(ctx, id)
}

func (p plexExtrasAdmin) Rename(id, name string) error { return p.app.plexExtras.rename(id, name) }

func (p plexExtrasAdmin) Remove(ctx context.Context, id string) error {
	return p.app.plexExtras.remove(ctx, id)
}

func (p plexExtrasAdmin) Retire(id string, on bool) error {
	if id == "main" {
		id = ""
	}
	return p.app.plexExtras.setRetired(id, on)
}

func (p plexExtrasAdmin) Move(ctx context.Context, userIDs []int64, to string, removeOld bool) (any, error) {
	return p.app.plexExtras.move(ctx, userIDs, to, removeOld)
}

func (p plexExtrasAdmin) Members(ctx context.Context) any {
	return p.app.plexExtras.memberNames(ctx)
}

func (p plexExtrasAdmin) Invite(ctx context.Context, handle string, plexID int64) (any, error) {
	return p.app.plexExtras.invite(ctx, handle, plexID)
}
