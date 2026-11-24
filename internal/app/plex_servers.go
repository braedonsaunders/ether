package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plextv"
)

// Extra Plex servers (config.PlexExtraServer): other Plex accounts the owner
// connects to this Ether, each presented to plex.tv as its own server on the
// same port. The feature is off until the owner turns it on, and the number
// of extra servers is capped (config.PlexMaxExtraServers, 2 by default).
//
// Each active extra server runs the same plex.tv code as the main server
// (claim, connection publishing, certificate, presence, friend sync) through
// a view of the config that swaps in its identity (plexServerStore). The
// Plex front end picks the identity per request (plexIdentityForRequest).
//
// A signed-in account is added as standby: nothing is claimed or published
// until the owner starts it. Nothing starts on its own. New invites go to the
// first started server with room, oldest first.

// errPlexServersFull reports that every running server is full.
var errPlexServersFull = errors.New("no Plex server has room for another viewer")

// errPlexExtrasOff refuses extra-server work while the owner has the feature
// turned off (Apps > Plex > Plex servers).
var errPlexExtrasOff = errors.New("extra Plex servers are turned off: turn them on under Apps > Plex > Plex servers")

// errPlexExtrasLimit refuses another account once the owner's limit is reached.
type errPlexExtrasLimit struct{ max int }

func (e errPlexExtrasLimit) Error() string {
	return fmt.Sprintf("this Ether already has %d extra Plex servers, the limit set under Apps > Plex > Plex servers", e.max)
}

type plexServerRuntime struct {
	id        string
	store     *plexServerStore
	client    *plextv.Client
	certs     *plextv.CertManager
	registrar *plextv.Registrar
	cancel    context.CancelFunc
}

type plexExtras struct {
	app *App
	log *slog.Logger

	mu      sync.Mutex
	ctx     context.Context
	running map[string]*plexServerRuntime
	// seats is each server's share count from its last friend sync ("" is
	// the main server): accepted and pending shares, not Plex Home members.
	seats     map[string]int
	seatsAt   map[string]time.Time
	finishing bool
	// lifeMu orders every start and stop of an extra server: the on/off
	// setting, Start (activate) and remove, so a switch turned off while a
	// server is being claimed cannot leave it running. toggleOn is the
	// setting value last applied (toggleSeen once one was).
	lifeMu     sync.Mutex
	toggleSeen bool
	toggleOn   bool
	// sections is the library list the main server publishes; every extra
	// server publishes the same libraries.
	sections []plextv.LibrarySection
	// tokenServers caches which server a token belongs to (a minute).
	tokenServers sync.Map
	// afterAdd runs after a new account is stored (tests).
	afterAdd func()
}

func newPlexExtras(a *App) *plexExtras {
	return &plexExtras{
		app:     a,
		log:     a.log.With("component", "plex-servers"),
		running: map[string]*plexServerRuntime{},
		seats:   map[string]int{},
		seatsAt: map[string]time.Time{},
	}
}

// plexServerStore is the config as one extra server sees it.
type plexServerStore struct {
	base *config.Store
	id   string
	// stopped ends config notifications once the server is stopped: the
	// subscription itself cannot be removed from the config store.
	stopped atomic.Bool
}

func (s *plexServerStore) Get() config.Config {
	c := s.base.Get()
	e, _ := c.PlexExtraServer(s.id)
	return c.WithPlexServer(e)
}

func (s *plexServerStore) Update(fn func(*config.Config) error) error {
	return s.base.Update(func(c *config.Config) error {
		e, ok := c.PlexExtraServer(s.id)
		if !ok {
			return fmt.Errorf("plex server %s was removed", s.id)
		}
		view := c.WithPlexServer(e)
		if err := fn(&view); err != nil {
			return err
		}
		c.StorePlexServer(s.id, view)
		return nil
	})
}

func (s *plexServerStore) Subscribe(fn func(config.Config)) {
	s.base.Subscribe(func(c config.Config) {
		if s.stopped.Load() {
			return
		}
		if e, ok := c.PlexExtraServer(s.id); ok {
			fn(c.WithPlexServer(e))
		}
	})
}

// plexSeatCounter wraps a server's viewer sync to count its shares.
type plexSeatCounter struct {
	ps *plexExtras
	id string // "" is the main server
}

func countShares(friends []plextv.Friend) int {
	n := 0
	for _, f := range friends {
		if !f.Owned && !f.Home && f.PlexID != 0 {
			n++
		}
	}
	return n
}

func (u plexSeatCounter) SetOwner(ctx context.Context, owner plextv.Account, serverToken string) error {
	if u.id == "" {
		return u.ps.app.users.SetOwner(ctx, owner, serverToken)
	}
	// An extra server's account belongs to the Ether owner; it never
	// replaces the main owner.
	return u.ps.app.users.SetServerToken(ctx, u.id, serverToken)
}

func (u plexSeatCounter) SyncFriends(ctx context.Context, friends []plextv.Friend) error {
	var err error
	if u.id == "" {
		err = u.ps.app.users.SyncFriends(ctx, friends)
	} else {
		err = u.ps.app.users.SyncServerFriends(ctx, u.id, friends)
	}
	if err == nil {
		u.ps.noteSeats(u.id, countShares(friends))
	}
	return err
}

func (ps *plexExtras) noteSeats(id string, n int) {
	ps.mu.Lock()
	ps.seats[id] = n
	ps.seatsAt[id] = time.Now()
	ps.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		ps.finishMoves(ctx)
	}()
}

// noteInvited counts an invite just sent on server id ("" is main) until the
// next friend sync reports the real count, so a run of invites or moves does
// not send everyone to one server past its seats.
func (ps *plexExtras) noteInvited(ctx context.Context, id string) {
	n := ps.seatsUsed(ctx)[id] + 1
	ps.mu.Lock()
	ps.seats[id] = n
	ps.seatsAt[id] = time.Now()
	ps.mu.Unlock()
}

// seatsUsed is each server's share count: the last friend sync, else the
// viewers the database records for it.
func (ps *plexExtras) seatsUsed(ctx context.Context) map[string]int {
	out := map[string]int{}
	if ps.app.users != nil {
		if members, err := ps.app.users.ServerMembers(ctx); err == nil {
			for id, n := range members {
				out[id] = n
			}
		}
	}
	ps.mu.Lock()
	for id, n := range ps.seats {
		out[id] = n
	}
	ps.mu.Unlock()
	return out
}

// order is the fill order: the main server, then active extra servers by the
// time they were added.
func activePlexOrder(c config.Config) []config.PlexExtraServer {
	var out []config.PlexExtraServer
	for _, s := range c.PlexExtraServers {
		if s.Active() {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt < out[j].AddedAt })
	return out
}

func standbyPlexOrder(c config.Config) []config.PlexExtraServer {
	var out []config.PlexExtraServer
	for _, s := range c.PlexExtraServers {
		if !s.Active() {
			out = append(out, s)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].AddedAt < out[j].AddedAt })
	return out
}

// start runs every active extra server when the feature is on. Called once
// the main registrar runs.
func (ps *plexExtras) start(ctx context.Context) {
	ps.mu.Lock()
	ps.ctx = ctx
	ps.mu.Unlock()
	// Extra servers run only while the owner has them turned on, and they
	// start only when the owner starts them: nothing starts on its own.
	// Every config change, local or from another cluster node, brings the
	// running servers in line with the config without a restart.
	ps.app.cfg.Subscribe(func(config.Config) {
		go ps.syncWithConfig(ctx)
	})
	ps.syncWithConfig(ctx)
}

// applyEnabled brings the running extra servers in line with the config:
// with the feature on, every started server runs; a server removed (on this
// node or, in a cluster, on another one whose change arrived through the
// shared config) or the feature turned off stops. It runs on every config
// change, so no restart is needed. Turning the feature off also withdraws
// what the extra servers shared.
func (ps *plexExtras) applyEnabled(ctx context.Context, on bool) {
	ps.lifeMu.Lock()
	defer ps.lifeMu.Unlock()
	ps.applyLocked(ctx, on)
}

// syncWithConfig is applyEnabled for the current setting, read under the
// lock: config notifications can arrive out of order, so the value they
// carry is not trusted.
func (ps *plexExtras) syncWithConfig(ctx context.Context) {
	ps.lifeMu.Lock()
	defer ps.lifeMu.Unlock()
	ps.applyLocked(ctx, ps.app.cfg.Get().Plex.ExtraServersEnabled)
}

func (ps *plexExtras) applyLocked(ctx context.Context, on bool) {
	turnedOff := !on && (!ps.toggleSeen || ps.toggleOn)
	ps.toggleSeen, ps.toggleOn = true, on
	cfg := ps.app.cfg.Get()
	want := map[string]bool{}
	var order []string
	if on {
		for _, s := range activePlexOrder(cfg) {
			want[s.ID] = true
			order = append(order, s.ID)
		}
	}
	ps.mu.Lock()
	var stopIDs []string
	for id := range ps.running {
		if !want[id] {
			stopIDs = append(stopIDs, id)
		}
	}
	ps.mu.Unlock()
	for _, id := range stopIDs {
		ps.stop(id)
		ps.log.Info("extra plex server stopped", "server", id)
	}
	if turnedOff {
		// A server that is off shares with no one: its tokens and
		// memberships go, and the main server's next sync switches off
		// viewers only it shared with. Its next start syncs them back.
		withdrawn := 0
		if ps.app.users != nil {
			for _, s := range cfg.PlexExtraServers {
				if err := ps.app.users.RemoveServer(ctx, s.ID); err != nil {
					ps.log.Warn("extra plex server access not withdrawn", "server", s.ID, "err", err)
					continue
				}
				withdrawn++
			}
		}
		if withdrawn > 0 && ps.app.registrar != nil {
			if err := ps.app.registrar.SyncFriendsNow(ctx); err != nil {
				ps.log.Warn("plex friend sync after turning extra servers off", "err", err)
			}
		}
		if len(stopIDs) > 0 || withdrawn > 0 {
			ps.log.Info("extra plex servers turned off", "stopped", len(stopIDs))
		}
		return
	}
	for _, id := range order {
		if ps.runtime(id) != nil {
			continue
		}
		if err := ps.run(ctx, id); err != nil {
			ps.log.Warn("extra plex server did not start", "server", id, "err", err)
		}
	}
}

func (ps *plexExtras) build(id string) (*plexServerRuntime, error) {
	a := ps.app
	store := &plexServerStore{base: a.cfg, id: id}
	view := store.Get()
	client, err := plextv.New(plextv.Options{
		BaseURL:       parityPlexTVBaseURL(),
		HTTPClient:    a.opts.plextvHTTP,
		Identity:      plextv.ServerIdentity(view, ""),
		VersionSource: func() string { return plextv.PMSVersion(a.cfg.Get()) },
	})
	if err != nil {
		return nil, fmt.Errorf("plex.tv client: %w", err)
	}
	log := ps.log.With("server", id)
	certs, err := plextv.NewCertManager(plextv.CertManagerOptions{
		Dir:      filepath.Join(a.opts.DataDir, "certs", "plex-"+id),
		Client:   client,
		Logger:   log.With("component", "certs"),
		OnChange: a.publishDashboard,
	})
	if err != nil {
		return nil, fmt.Errorf("certificates: %w", err)
	}
	ropts := plextv.RegistrarOptions{
		Config: store,
		Client: client,
		Users:  plexSeatCounter{ps: ps, id: id},
		Certs:  certs,
		Logger: log.With("component", "registrar"),
		// The main server maps the shared port; extra servers never do.
		NodeMapping: true,
		OnChange:    a.publishDashboard,
	}
	if a.clustered() {
		ropts.PeerURIs = a.peers.get
	}
	reg, err := plextv.NewRegistrar(ropts)
	if err != nil {
		certs.Close()
		return nil, fmt.Errorf("registrar: %w", err)
	}
	ps.mu.Lock()
	sections := ps.sections
	ps.mu.Unlock()
	if sections != nil {
		reg.SetLibrarySections(sections)
	}
	return &plexServerRuntime{id: id, store: store, client: client, certs: certs, registrar: reg}, nil
}

// run starts the extra server id's plex.tv presence (it must be claimed).
func (ps *plexExtras) run(ctx context.Context, id string) error {
	ps.mu.Lock()
	if _, ok := ps.running[id]; ok {
		ps.mu.Unlock()
		return nil
	}
	ps.mu.Unlock()
	rt, err := ps.build(id)
	if err != nil {
		return err
	}
	return ps.runRuntime(ctx, rt)
}

// runRuntime starts a built server's registrar and records it as running.
func (ps *plexExtras) runRuntime(ctx context.Context, rt *plexServerRuntime) error {
	id := rt.id
	rctx, cancel := context.WithCancel(ctx)
	rt.cancel = cancel
	if ps.app.clustered() {
		go ps.app.cluster.RunAsLeader(rctx, "plex.tv registrar "+id, func(lctx context.Context) {
			if err := rt.registrar.Start(lctx); err != nil {
				ps.log.Warn("extra plex server registrar", "server", id, "err", err)
				return
			}
			defer rt.registrar.Stop()
			rt.registrar.RequestPublish()
			<-lctx.Done()
		})
	} else if err := rt.registrar.Start(rctx); err != nil {
		cancel()
		rt.certs.Close()
		return err
	}
	rt.registrar.ListenerReady()
	ps.mu.Lock()
	if old := ps.running[id]; old != nil && old != rt {
		// Lifecycle calls hold lifeMu, so this cannot happen; keep one
		// runtime per server regardless.
		ps.mu.Unlock()
		rt.store.stopped.Store(true)
		cancel()
		rt.registrar.Stop()
		rt.certs.Close()
		return nil
	}
	ps.running[id] = rt
	ps.mu.Unlock()
	ps.app.publishDashboard()
	return nil
}

func (ps *plexExtras) stop(id string) {
	ps.mu.Lock()
	rt := ps.running[id]
	delete(ps.running, id)
	delete(ps.seats, id)
	delete(ps.seatsAt, id)
	ps.mu.Unlock()
	if rt == nil {
		return
	}
	if rt.store != nil {
		rt.store.stopped.Store(true)
	}
	if rt.cancel != nil {
		rt.cancel()
	}
	if rt.registrar != nil {
		rt.registrar.Stop()
	}
	if rt.certs != nil {
		rt.certs.Close()
	}
}

func (ps *plexExtras) close() {
	ps.mu.Lock()
	ids := make([]string, 0, len(ps.running))
	for id := range ps.running {
		ids = append(ids, id)
	}
	ps.mu.Unlock()
	for _, id := range ids {
		ps.stop(id)
	}
}

func (ps *plexExtras) runtime(id string) *plexServerRuntime {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.running[id]
}

// activate claims the standby server id with its account and publishes it.
func (ps *plexExtras) activate(ctx context.Context, id string) error {
	ps.lifeMu.Lock()
	defer ps.lifeMu.Unlock()
	cfg := ps.app.cfg.Get()
	if !cfg.Plex.ExtraServersEnabled {
		return errPlexExtrasOff
	}
	e, ok := cfg.PlexExtraServer(id)
	if !ok {
		return fmt.Errorf("plex server %s not found", id)
	}
	if e.Active() {
		return nil
	}
	if e.Account.Token == "" {
		return fmt.Errorf("plex server %s has no signed-in account", id)
	}
	rt, err := ps.build(id)
	if err != nil {
		return err
	}
	if _, err := rt.registrar.Claim(ctx, e.Account.Token); err != nil {
		rt.certs.Close()
		return fmt.Errorf("claim on plex.tv: %w", err)
	}
	now := time.Now().Unix()
	if err := ps.app.cfg.Update(func(c *config.Config) error {
		for i := range c.PlexExtraServers {
			if c.PlexExtraServers[i].ID == id {
				c.PlexExtraServers[i].State = config.PlexServerActive
				c.PlexExtraServers[i].ActivatedAt = now
				return nil
			}
		}
		return fmt.Errorf("plex server %s was removed", id)
	}); err != nil {
		rt.certs.Close()
		return err
	}
	ps.mu.Lock()
	ctxRun := ps.ctx
	ps.mu.Unlock()
	if ctxRun == nil {
		ctxRun = context.Background()
	}
	if err := ps.runRuntime(ctxRun, rt); err != nil {
		return err
	}
	ps.log.Info("extra plex server started", "server", id, "account", e.Account.Username)
	return nil
}

// pick is the server a new invite goes to: the first, oldest first, with a
// free seat. "" is the main server.
func (ps *plexExtras) pick(ctx context.Context) (string, error) {
	return ps.pickExcept(ctx, nil)
}

// share invites a viewer on the server pick chose.
func (ps *plexExtras) share(ctx context.Context, usernameOrEmail string, userID int64) (string, error) {
	// Without extra servers in use, invites behave exactly as before: the
	// main server, with no seat count of Ether's own.
	if cfg := ps.app.cfg.Get(); !cfg.Plex.ExtraServersEnabled || len(cfg.PlexExtraServers) == 0 {
		return "", ps.app.shareOnMainServer(ctx, usernameOrEmail, userID)
	}
	id, err := ps.pick(ctx)
	if err != nil {
		return "", err
	}
	if id == "" {
		err := ps.app.shareOnMainServer(ctx, usernameOrEmail, userID)
		if err == nil {
			ps.noteInvited(ctx, id)
		}
		return "", err
	}
	rt := ps.runtime(id)
	e, ok := ps.app.cfg.Get().PlexExtraServer(id)
	if rt == nil || !ok || e.Token == "" {
		return "", fmt.Errorf("plex server %s is not running", id)
	}
	if err := rt.client.ShareServer(ctx, e.Token, usernameOrEmail, userID, nil); err != nil {
		return id, err
	}
	ps.noteInvited(ctx, id)
	return id, nil
}

// syncAll re-reads every server's friend list.
func (ps *plexExtras) syncAll(ctx context.Context) error {
	var errs []error
	if ps.app.registrar != nil {
		if err := ps.app.registrar.SyncFriendsNow(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	ps.mu.Lock()
	rts := make([]*plexServerRuntime, 0, len(ps.running))
	for _, rt := range ps.running {
		rts = append(rts, rt)
	}
	ps.mu.Unlock()
	for _, rt := range rts {
		if err := rt.registrar.SyncFriendsNow(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rt.id, err))
		}
	}
	return errors.Join(errs...)
}

// syncOnUnknownToken lets an unknown Plex token trigger a re-read of every
// server's access tokens (a viewer shared on an extra server since the last
// sync).
func (ps *plexExtras) syncOnUnknownToken(ctx context.Context) error {
	var errs []error
	if ps.app.registrar != nil {
		if err := ps.app.registrar.SyncFriendsOnUnknownToken(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	ps.mu.Lock()
	rts := make([]*plexServerRuntime, 0, len(ps.running))
	for _, rt := range ps.running {
		rts = append(rts, rt)
	}
	ps.mu.Unlock()
	for _, rt := range rts {
		if err := rt.registrar.SyncFriendsOnUnknownToken(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// errPlexAccountInUse refuses an account that already owns one of the servers.
type errPlexAccountInUse struct{ name string }

func (e errPlexAccountInUse) Error() string {
	return fmt.Sprintf("this Plex account already runs %q: sign in with another Plex account", e.name)
}

// errPlexSetupIDInUse refuses a sign-in whose setup identifier another
// profile took meanwhile; a new sign-in gets a new one.
var errPlexSetupIDInUse = errors.New("this sign-in's identifier is already used by another Plex server; start again")

// addAccount stores a signed-in plex.tv account as a standby server. client
// is the sign-in's own client and clientID the identifier it sent for the
// PIN; the account is read with them and the profile keeps clientID. Nothing
// is stored unless the account is new and the config saved.
func (ps *plexExtras) addAccount(ctx context.Context, client *plextv.Client, clientID, token string) (config.PlexExtraServer, error) {
	if client == nil || clientID == "" || client.Identity().ClientIdentifier != clientID {
		return config.PlexExtraServer{}, errors.New("plex sign-in has no setup identifier; start again")
	}
	acct, err := client.User(ctx, token)
	if err != nil {
		return config.PlexExtraServer{}, fmt.Errorf("plex.tv account: %w", err)
	}
	var added config.PlexExtraServer
	err = ps.app.cfg.Update(func(c *config.Config) error {
		if !c.Plex.ExtraServersEnabled {
			return errPlexExtrasOff
		}
		if n := c.PlexMaxExtraServers(); len(c.PlexExtraServers) >= n {
			return errPlexExtrasLimit{max: n}
		}
		if c.Plex.OwnerPlexID != 0 && c.Plex.OwnerPlexID == acct.ID {
			return errPlexAccountInUse{name: c.Server.FriendlyName}
		}
		for _, s := range c.PlexExtraServers {
			if s.Account.ID == acct.ID || (s.OwnerPlexID != 0 && s.OwnerPlexID == acct.ID) {
				return errPlexAccountInUse{name: s.AdvertisedName(*c)}
			}
		}
		if c.PlexClientIdentifierInUse(clientID) {
			return errPlexSetupIDInUse
		}
		added = config.NewPlexExtraServer(c.NextPlexServerID(), config.PlexServerAccount{ID: acct.ID, Username: acct.Username, Token: token}, time.Now().Unix())
		// The profile keeps the identifier its sign-in used. The machine
		// identifier stays the freshly generated one.
		added.ClientIdentifier = clientID
		c.PlexExtraServers = append(c.PlexExtraServers, added)
		return nil
	})
	if err != nil {
		return config.PlexExtraServer{}, err
	}
	ps.log.Info("plex account added as a standby server", "server", added.ID, "account", acct.Username)
	if ps.afterAdd != nil {
		ps.afterAdd()
	}
	return added, nil
}

func (ps *plexExtras) rename(id, name string) error {
	name = strings.TrimSpace(name)
	if len(name) > 64 {
		return errors.New("name is longer than 64 characters")
	}
	if err := ps.app.cfg.Update(func(c *config.Config) error {
		for i := range c.PlexExtraServers {
			if c.PlexExtraServers[i].ID == id {
				c.PlexExtraServers[i].Name = name
				return nil
			}
		}
		return fmt.Errorf("plex server %s not found", id)
	}); err != nil {
		return err
	}
	if rt := ps.runtime(id); rt != nil {
		rt.registrar.Trigger()
	}
	return nil
}

// remove signs the server out of plex.tv (if claimed) and forgets it.
func (ps *plexExtras) remove(ctx context.Context, id string) error {
	ps.lifeMu.Lock()
	defer ps.lifeMu.Unlock()
	if rt := ps.runtime(id); rt != nil {
		if err := rt.registrar.Unclaim(ctx); err != nil {
			ps.log.Warn("extra plex server unclaim", "server", id, "err", err)
		}
	}
	ps.stop(id)
	if err := ps.app.cfg.Update(func(c *config.Config) error {
		// A new slice: the old one may still back a snapshot someone holds.
		out := make([]config.PlexExtraServer, 0, len(c.PlexExtraServers))
		found := false
		for _, s := range c.PlexExtraServers {
			if s.ID == id {
				found = true
				continue
			}
			out = append(out, s)
		}
		if !found {
			return fmt.Errorf("plex server %s not found", id)
		}
		c.PlexExtraServers = out
		return nil
	}); err != nil {
		return err
	}
	if ps.app.users != nil {
		return ps.app.users.RemoveServer(ctx, id)
	}
	return nil
}

// ---- one port, several servers ----

// plexDirectHash is the certificate hash label of a *.plex.direct host
// ("1-2-3-4.<hash>.plex.direct"), or "".
func plexDirectHash(host string) string {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if h, _, ok := strings.Cut(host, ":"); ok && !strings.Contains(h, "]") {
		host = h
	}
	if !strings.HasSuffix(host, ".plex.direct") {
		return ""
	}
	labels := strings.Split(strings.TrimSuffix(host, ".plex.direct"), ".")
	if len(labels) < 2 {
		return ""
	}
	return labels[len(labels)-1]
}

// requestToken is the Plex token a request carries.
func requestToken(r *http.Request) string {
	if t := strings.TrimSpace(r.Header.Get("X-Plex-Token")); t != "" {
		return t
	}
	return strings.TrimSpace(r.URL.Query().Get("X-Plex-Token"))
}

type tokenServerEntry struct {
	id      string
	ok      bool
	expires time.Time
}

// serverForRequest names the extra server a request is for, or "" for the
// main server: the plex.direct host it reached (each server has its own
// certificate hash), else the server its token was issued for.
func (ps *plexExtras) serverForRequest(r *http.Request) string {
	cfg := ps.app.cfg.Get()
	if len(cfg.PlexExtraServers) == 0 || !cfg.Plex.ExtraServersEnabled {
		return ""
	}
	host := r.Host
	if r.TLS != nil && r.TLS.ServerName != "" {
		host = r.TLS.ServerName
	}
	if hash := plexDirectHash(host); hash != "" {
		if cfg.Plex.CertHash != "" && hash == strings.ToLower(cfg.Plex.CertHash) {
			return ""
		}
		for _, s := range cfg.PlexExtraServers {
			if s.Active() && s.CertHash != "" && hash == strings.ToLower(s.CertHash) {
				return s.ID
			}
		}
	}
	tok := requestToken(r)
	if tok == "" || ps.app.users == nil {
		return ""
	}
	key := auth.HashToken(tok)
	now := time.Now()
	if v, ok := ps.tokenServers.Load(key); ok {
		if e := v.(tokenServerEntry); now.Before(e.expires) {
			return e.id
		}
	}
	id, ok := ps.app.users.TokenServer(r.Context(), tok)
	if ok {
		if e, found := cfg.PlexExtraServer(id); !found || !e.Active() {
			id = ""
		}
	}
	// Only tokens this Ether issued are remembered (a minute): an unknown
	// token is looked up again, so random tokens cannot grow the cache and
	// a friend sync can tie a new token to its server at once.
	if ok {
		ps.tokenServers.Store(key, tokenServerEntry{id: id, ok: ok, expires: now.Add(time.Minute)})
	}
	return id
}

// identityForRequest is the frontend.Host ServerIdentityFor hook.
func (ps *plexExtras) identityForRequest(r *http.Request) (frontend.ServerIdentity, bool) {
	id := ps.serverForRequest(r)
	if id == "" {
		return frontend.ServerIdentity{}, false
	}
	cfg := ps.app.cfg.Get()
	e, ok := cfg.PlexExtraServer(id)
	if !ok {
		return frontend.ServerIdentity{}, false
	}
	acct := &frontend.ServerAccount{OwnerUsername: e.OwnerUsername, Claimed: e.Claimed}
	if rt := ps.runtime(id); rt != nil {
		st := rt.registrar.Status()
		acct.PlexPass, acct.OwnerFeatures = st.PlexPass, st.OwnerFeatures
	}
	return frontend.ServerIdentity{MachineID: e.MachineIdentifier, FriendlyName: e.AdvertisedName(cfg), Account: acct}, true
}

// tlsConfig serves each server's own plex.direct certificate on the shared
// port, chosen by the name the client asked for.
func (ps *plexExtras) tlsConfig() *tls.Config {
	main := ps.app.certs
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hash := plexDirectHash(hello.ServerName); hash != "" {
			cfg := ps.app.cfg.Get()
			for _, s := range cfg.PlexExtraServers {
				if s.CertHash == "" || hash != strings.ToLower(s.CertHash) {
					continue
				}
				if rt := ps.runtime(s.ID); rt != nil {
					return rt.certs.GetCertificate(hello)
				}
			}
		}
		return main.GetCertificate(hello)
	}}
}

// plexTLSConfig is the shared port's TLS config: each Plex server's own
// plex.direct certificate.
func (a *App) plexTLSConfig() *tls.Config {
	if a.plexExtras == nil {
		return a.certs.TLSConfig()
	}
	return a.plexExtras.tlsConfig()
}

// plexIdentityForRequest is the Plex front end's per-request identity hook.
func (a *App) plexIdentityForRequest(r *http.Request) (frontend.ServerIdentity, bool) {
	if a.plexExtras == nil {
		return frontend.ServerIdentity{}, false
	}
	return a.plexExtras.identityForRequest(r)
}
