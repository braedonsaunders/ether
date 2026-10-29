package plextv

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/remote"
)

// ConfigStore is the part of *config.Store the registrar uses.
type ConfigStore interface {
	Get() config.Config
	Update(fn func(*config.Config) error) error
}

// UserSync receives the owner and the friend list (implemented by
// internal/auth).
type UserSync interface {
	SetOwner(ctx context.Context, owner Account, serverToken string) error
	SyncFriends(ctx context.Context, friends []Friend) error
}

// WANDetector finds the public IP (remote.WANDetector).
type WANDetector interface {
	Detect(ctx context.Context) (net.IP, error)
}

// PortMapper keeps the remote-access port forwarded (remote.PortMapper).
type PortMapper interface {
	Ensure(ctx context.Context, internal, external int) (remote.MappingStatus, error)
	Status() remote.MappingStatus
	Close(ctx context.Context)
}

// Status is the plex.tv / remote-access state for the admin dashboard.
type Status struct {
	Claimed           bool   `json:"claimed"`
	OwnerUsername     string `json:"ownerUsername,omitempty"`
	MachineIdentifier string `json:"machineIdentifier"`

	LANAddresses []string            `json:"lanAddresses"`
	PublicIP     string              `json:"publicIp,omitempty"`
	PublicIPErr  string              `json:"publicIpError,omitempty"`
	Connections  []remote.Connection `json:"connections"`

	PublishedURIs    []string  `json:"publishedUris"`
	LastPublish      time.Time `json:"lastPublish,omitempty"`
	LastPublishError string    `json:"lastPublishError,omitempty"`
	RegisterError    string    `json:"registerError,omitempty"` // servers.xml (non-fatal)

	// Listed reports whether plex.tv's resources list contains this server;
	// ResourceConnections is what clients will be offered.
	Listed              bool                 `json:"listed"`
	ResourceConnections []ResourceConnection `json:"resourceConnections,omitempty"`
	PublicAddress       string               `json:"publicAddress,omitempty"` // as plex.tv sees us
	PlexPass            bool                 `json:"plexPass"`                // owner subscription active
	OwnerFeatures       []string             `json:"ownerFeatures"`           // granted by plex.tv

	RemoteAccess bool                 `json:"remoteAccess"`
	Reachability *remote.Reachability `json:"reachability,omitempty"`
	UPnP         remote.MappingStatus `json:"upnp"`

	Cert      CertInfo `json:"cert"`
	CertError string   `json:"certError,omitempty"`

	// Presence is the plex.tv pubsub stream that marks the server online.
	Presence PresenceStatus `json:"presence"`
	// PlexReachable is plex.tv's last notifyConnectivity verdict for our
	// published address (nil until one arrives).
	PlexReachable   *bool     `json:"plexReachable,omitempty"`
	Friends         int       `json:"friends"`
	LastFriendSync  time.Time `json:"lastFriendSync,omitempty"`
	FriendSyncError string    `json:"friendSyncError,omitempty"`
	// TokenRejected reports that plex.tv answered the server token with
	// 401/403 (publishing or friend sync): typically the device record
	// still belongs to an account the server was claimed by before.
	TokenRejected bool `json:"tokenRejected,omitempty"`
}

// RegistrarOptions configures a Registrar. Only Config is required; nil
// collaborators get production defaults or are disabled.
type RegistrarOptions struct {
	Config ConfigStore
	// Client carries the server identity; built from Config when nil.
	Client  *Client
	BaseURL string // for the default client (tests)
	Version string // PMS version reported to plex.tv
	Users   UserSync
	WAN     WANDetector
	LAN     func() ([]net.IP, error)
	Mapper  PortMapper // nil creates a remote.PortMapper
	Reach   *remote.ReachabilityChecker
	Certs   *CertManager // nil disables certificate management

	// Intervals (defaults in parentheses).
	Tick                 time.Duration // loop granularity (15s)
	AddressInterval      time.Duration // LAN/WAN re-check (30s)
	PublishInterval      time.Duration // unconditional re-publish (1h)
	FriendSyncInterval   time.Duration // access token sync (1m, observed)
	MappingInterval      time.Duration // UPnP/NAT-PMP renew check (5m)
	CertInterval         time.Duration // certificate expiry check (1w, observed when not near expiry)
	ReachabilityInterval time.Duration // remote self-test (1h, observed)
	OwnerInterval        time.Duration // owner account / Plex Pass re-check (6h)

	// PeerURIs, when set (clustered servers), returns the other live
	// nodes' connection URIs in publication order. PublishNow appends them,
	// de-duplicated, after this node's own URIs so plex.tv lists the union.
	// It is called on every publish and must not block on the network.
	PeerURIs func() []string

	// NodeMapping moves port mapping out of the publishing loop: the owner
	// runs RunPortMapping on every node (each cluster node is reached
	// through its own port forward, plex.publicPort being node-local), and
	// Stop leaves the mapping in place. Off, the loop maps the port and
	// Stop removes the mapping (a standalone server).
	NodeMapping bool

	// PublishPlexDirect also publishes the https plex.direct URIs. Off by
	// default: PMS publishes plain http URIs and plex.tv synthesises the
	// plex.direct https variants for clients asking with includeHttps=1.
	PublishPlexDirect bool

	Logger *slog.Logger
	Now    func() time.Time
}

// Registrar runs Ether's plex.tv presence: claim, connection publishing,
// friend sync, certificate renewal, port mapping and reachability.
type Registrar struct {
	o      RegistrarOptions
	client *Client
	log    *slog.Logger

	wake               chan struct{}
	mapWake            chan struct{} // RunPortMapping
	presenceWake       chan struct{}
	publicationMu      sync.Mutex
	publicationRunMu   sync.Mutex
	publicationCancel  context.CancelFunc
	publicationEnabled bool

	runMu  sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	addrMu      sync.Mutex // serialises address refreshes
	addrRefresh time.Time  // last refreshAddresses

	mu            sync.Mutex
	st            Status
	lastLAN       []string
	lastWAN       string
	next          map[string]time.Time
	pubFail       int
	pubGeneration uint64 // advances whenever fresh connections must be published
	forcePub      bool
	mappingOn     bool
	asyncID       string // outstanding connectivity request
	libraries     []LibrarySection

	presence *Presence

	// claimedToken is the server token while claimed; a switch to
	// unclaimed releases it (releaseClaim).
	claimedToken string
}

// rejectedRetry spaces out friend syncs while plex.tv rejects the token.
const rejectedRetry = 15 * time.Minute

const (
	taskAddress = "address"
	taskPublish = "publish"
	taskFriends = "friends"
	taskMapping = "mapping"
	taskCert    = "cert"
	taskReach   = "reach"
	taskOwner   = "owner"
)

// NewRegistrar validates options and returns a registrar; call Start.
func NewRegistrar(o RegistrarOptions) (*Registrar, error) {
	if o.Config == nil {
		return nil, errors.New("plextv: registrar needs Config")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&o.Tick, 15*time.Second)
	def(&o.AddressInterval, 30*time.Second)
	def(&o.PublishInterval, time.Hour)
	def(&o.FriendSyncInterval, time.Minute)
	def(&o.MappingInterval, 5*time.Minute)
	def(&o.CertInterval, 7*24*time.Hour)
	def(&o.ReachabilityInterval, time.Hour)
	def(&o.OwnerInterval, 6*time.Hour)
	if o.Version == "" {
		o.Version = DefaultServerVersion
	}
	client := o.Client
	if client == nil {
		var err error
		client, err = New(Options{BaseURL: o.BaseURL, Identity: ServerIdentity(o.Config.Get(), o.Version), Logger: o.Logger})
		if err != nil {
			return nil, err
		}
	}
	if o.WAN == nil {
		o.WAN = remote.NewWANDetector(nil, o.Logger)
	}
	if o.LAN == nil {
		o.LAN = func() ([]net.IP, error) { return remote.LANIPs(remote.LANOptions{}) }
	}
	if o.Mapper == nil {
		o.Mapper = remote.NewPortMapper(remote.PortMapperOptions{Description: "Ether", Logger: o.Logger})
	}
	if o.Reach == nil {
		o.Reach = remote.NewReachabilityChecker(nil)
	}
	r := &Registrar{
		o:                  o,
		client:             client,
		log:                o.Logger.With("component", "registrar"),
		wake:               make(chan struct{}, 1),
		mapWake:            make(chan struct{}, 1),
		presenceWake:       make(chan struct{}, 1),
		publicationEnabled: plexAppsEnabled(o.Config.Get()),
		next:               map[string]time.Time{},
	}
	if c := o.Config.Get(); c.Plex.Claimed {
		r.claimedToken = c.Plex.Token
	}
	if sub, ok := o.Config.(interface{ Subscribe(func(config.Config)) }); ok {
		sub.Subscribe(r.onConfig)
	}
	r.presence = NewPresence(PresenceOptions{
		Client: client,
		Credentials: func() (int64, string, string, bool) {
			c := r.o.Config.Get()
			return c.Plex.OwnerPlexID, c.Server.MachineIdentifier, c.Plex.Token,
				c.Plex.Token != "" && c.Plex.OwnerPlexID != 0 && c.Server.MachineIdentifier != ""
		},
		OnMessage: r.onPubsub,
	})
	r.refreshConfigStatus(o.Config.Get())
	return r, nil
}

// Client returns the server-identity plex.tv client.
func (r *Registrar) Client() *Client { return r.client }

// SignInClient returns a client with the setup wizard's identity for PIN
// sign-in (CreatePIN, AuthURL, WaitPIN).
func (r *Registrar) SignInClient() *Client {
	return r.client.WithIdentity(AppIdentity(r.o.Config.Get(), ""))
}

// Start runs the background loop until ctx ends or Stop is called.
func (r *Registrar) Start(ctx context.Context) error {
	r.runMu.Lock()
	defer r.runMu.Unlock()
	if r.cancel != nil {
		return errors.New("plextv: registrar already started")
	}
	ctx, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	done := make(chan struct{})
	r.done = done
	loopDone := make(chan struct{})
	go r.loop(ctx, loopDone)
	presenceDone := make(chan struct{})
	go func() { defer close(presenceDone); r.runPresence(ctx) }()
	go func() { <-loopDone; <-presenceDone; close(done) }()
	return nil
}

func plexAppsEnabled(c config.Config) bool {
	return c.Frontends == nil || c.Frontends["plex"].Enabled
}

// Presence is publication, not account or membership synchronisation.
func (r *Registrar) runPresence(ctx context.Context) {
	var cancel context.CancelFunc
	var done chan struct{}
	stop := func() {
		if cancel != nil {
			cancel()
			<-done
			cancel = nil
		}
	}
	defer stop()
	for {
		if plexAppsEnabled(r.o.Config.Get()) && cancel == nil {
			child, c := context.WithCancel(ctx)
			cancel = c
			done = make(chan struct{})
			go func() { defer close(done); r.presence.Run(child) }()
		} else if !plexAppsEnabled(r.o.Config.Get()) {
			stop()
		}
		select {
		case <-ctx.Done():
			return
		case <-r.presenceWake:
		}
	}
}

// Stop ends the loop and removes any port mapping.
func (r *Registrar) Stop() {
	r.runMu.Lock()
	cancel, done := r.cancel, r.done
	r.cancel, r.done = nil, nil
	r.runMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	if !r.o.NodeMapping {
		r.closeMapping()
	}
}

func (r *Registrar) closeMapping() {
	ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
	defer c()
	r.o.Mapper.Close(ctx)
	r.mu.Lock()
	r.st.UPnP = r.o.Mapper.Status()
	r.mappingOn = false
	r.mu.Unlock()
}

// RunPortMapping keeps this node's port mapping (UPnP IGD or NAT-PMP)
// alive until ctx ends, then removes it. Used with NodeMapping, on every
// cluster node whether or not it leads.
func (r *Registrar) RunPortMapping(ctx context.Context) {
	t := time.NewTicker(r.o.Tick)
	defer t.Stop()
	for {
		if r.due(taskMapping, r.o.MappingInterval) {
			prev := r.o.Mapper.Status()
			r.ensureMapping(ctx, r.o.Config.Get())
			if cur := r.o.Mapper.Status(); cur.Active != prev.Active || cur.ExternalPort != prev.ExternalPort || cur.ExternalIP != prev.ExternalIP {
				// The WAN URI this node heartbeats may have changed:
				// refresh addresses on the next NodeURIs.
				r.addrMu.Lock()
				r.addrRefresh = time.Time{}
				r.addrMu.Unlock()
			}
		}
		select {
		case <-ctx.Done():
			r.closeMapping()
			return
		case <-t.C:
		case <-r.mapWake:
		}
	}
}

// Trigger asks the loop to re-check addresses and re-publish now (e.g. after
// a config change).
func (r *Registrar) Trigger() {
	r.mu.Lock()
	r.requestPublishLocked()
	r.next[taskAddress] = time.Time{}
	r.next[taskMapping] = time.Time{}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
	select {
	case r.mapWake <- struct{}{}:
	default:
	}
}

// RequestPublish asks the running loop to publish connections now without
// re-checking addresses (e.g. after cluster membership changed).
func (r *Registrar) RequestPublish() {
	r.mu.Lock()
	r.requestPublishLocked()
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// SetLibrarySections connects account publishing to the actual PMS library
// identities. Call during front-end construction and whenever sections change.
func (r *Registrar) SetLibrarySections(sections []LibrarySection) {
	r.mu.Lock()
	r.libraries = slices.Clone(sections)
	r.requestPublishLocked()
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// requestPublishLocked marks a new publication request. The generation prevents
// an older in-flight publication from consuming an update it never observed.
func (r *Registrar) requestPublishLocked() {
	r.pubGeneration++
	r.forcePub = true
}

// Status returns a snapshot for the admin dashboard.
func (r *Registrar) Status() Status {
	r.mu.Lock()
	st := r.st
	r.mu.Unlock()
	st.LANAddresses = slices.Clone(st.LANAddresses)
	st.Connections = slices.Clone(st.Connections)
	st.PublishedURIs = slices.Clone(st.PublishedURIs)
	st.ResourceConnections = slices.Clone(st.ResourceConnections)
	st.OwnerFeatures = slices.Clone(st.OwnerFeatures)
	st.UPnP = r.o.Mapper.Status()
	if r.o.Certs != nil {
		st.Cert = r.o.Certs.Info()
	}
	st.Presence = r.presence.Status()
	return st
}

func (r *Registrar) refreshConfigStatus(cfg config.Config) {
	r.mu.Lock()
	r.st.Claimed = cfg.Plex.Claimed && cfg.Plex.Token != ""
	r.st.OwnerUsername = cfg.Plex.OwnerUsername
	r.st.MachineIdentifier = cfg.Server.MachineIdentifier
	r.st.RemoteAccess = cfg.Plex.RemoteAccess
	r.mu.Unlock()
}

func (r *Registrar) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	t := time.NewTicker(r.o.Tick)
	defer t.Stop()
	for {
		r.runDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.wake:
		}
	}
}

// due reports whether task should run now, and schedules its next run.
func (r *Registrar) due(task string, every time.Duration) bool {
	now := r.o.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	// Compare wall time: publishing and friend refresh must account for
	// time spent asleep, just like stored expiry dates.
	if now.Round(0).Before(r.next[task].Round(0)) {
		return false
	}
	r.next[task] = now.Add(every)
	return true
}

func (r *Registrar) schedule(task string, at time.Time) {
	r.mu.Lock()
	r.next[task] = at
	r.mu.Unlock()
}

func (r *Registrar) runDue(ctx context.Context) {
	cfg := r.o.Config.Get()
	r.refreshConfigStatus(cfg)
	if !r.o.NodeMapping && r.due(taskMapping, r.o.MappingInterval) {
		r.ensureMapping(ctx, cfg)
	}
	if r.due(taskAddress, r.o.AddressInterval) {
		r.refreshAddresses(ctx, cfg)
	}
	if ctx.Err() != nil {
		return
	}
	if r.o.Certs != nil && r.due(taskCert, r.o.CertInterval) {
		_ = r.RenewCertificate(ctx, false)
		r.scheduleCertCheck()
	}
	claimed := cfg.Plex.Claimed && cfg.Plex.Token != ""
	if !claimed {
		return
	}
	r.mu.Lock()
	force := r.forcePub
	generation := r.pubGeneration
	r.mu.Unlock()
	if plexAppsEnabled(cfg) && (force || r.due(taskPublish, r.o.PublishInterval)) {
		if err := r.PublishNow(ctx); err != nil && ctx.Err() == nil && plexAppsEnabled(r.o.Config.Get()) {
			r.mu.Lock()
			r.pubFail++
			backoff := time.Minute << min(r.pubFail-1, 5) // 1m .. 32m
			if r.pubGeneration == generation {
				r.forcePub = false // retry this request on the back-off schedule
			}
			r.next[taskPublish] = r.o.Now().Add(backoff)
			r.mu.Unlock()
		}
	}
	if r.due(taskFriends, r.o.FriendSyncInterval) {
		_ = r.SyncFriendsNow(ctx)
	}
	// Plex Pass bought, renewed or lapsed while running: Live TV and the
	// owner features clients see follow without a restart.
	if r.due(taskOwner, r.o.OwnerInterval) {
		if err := r.RefreshOwner(ctx); err != nil && ctx.Err() == nil {
			r.log.Debug("plex.tv owner refresh failed", "err", err)
			r.schedule(taskOwner, r.o.Now().Add(10*time.Minute))
		}
	}
	if plexAppsEnabled(cfg) && cfg.Plex.RemoteAccess && r.due(taskReach, r.o.ReachabilityInterval) {
		r.CheckReachability(ctx)
	}
}

func (r *Registrar) ensureMapping(ctx context.Context, cfg config.Config) {
	want := cfg.Plex.RemoteAccess && cfg.Plex.UseUPnP
	r.mu.Lock()
	on := r.mappingOn
	r.mu.Unlock()
	if !want {
		if on {
			r.o.Mapper.Close(ctx)
			r.mu.Lock()
			r.mappingOn = false
			r.requestPublishLocked()
			r.mu.Unlock()
		}
		return
	}
	prev := r.o.Mapper.Status()
	st, err := r.o.Mapper.Ensure(ctx, serverPort(cfg), publicPort(cfg))
	r.mu.Lock()
	r.mappingOn = true
	if st.Active && (st.ExternalPort != prev.ExternalPort || !prev.Active) {
		r.requestPublishLocked()
	}
	// A gateway may grant (or an adopted entry may have) a lease shorter
	// than MappingInterval: check again when the mapper wants to renew,
	// or the mapping lapses between checks.
	if st.Active && !st.RenewAt.IsZero() && st.RenewAt.Before(r.next[taskMapping]) {
		r.next[taskMapping] = st.RenewAt
	}
	r.mu.Unlock()
	if err != nil && ctx.Err() == nil {
		r.log.Debug("port mapping failed", "err", err)
	}
}

func serverPort(cfg config.Config) int {
	if cfg.Server.Port > 0 {
		return cfg.Server.Port
	}
	return 32400
}

// publicPort is the WAN port: the manual forward, else the server port.
func publicPort(cfg config.Config) int {
	if cfg.Plex.PublicPort > 0 {
		return cfg.Plex.PublicPort
	}
	return serverPort(cfg)
}

// wanPort is the port remote clients reach this server on. While the
// gateway holds a mapping it is the external port actually mapped: a
// NAT-PMP gateway may grant a different port than the one requested
// (RFC 6886 3.3, e.g. when another host already holds it), and publishing
// the requested port would then advertise a dead address. Otherwise it is
// publicPort.
func (r *Registrar) wanPort(cfg config.Config) int {
	if cfg.Plex.RemoteAccess && cfg.Plex.UseUPnP {
		if m := r.o.Mapper.Status(); m.Active && m.ExternalPort > 0 {
			return m.ExternalPort
		}
	}
	return publicPort(cfg)
}

func (r *Registrar) refreshAddresses(ctx context.Context, cfg config.Config) {
	r.addrMu.Lock()
	defer r.addrMu.Unlock()
	r.addrRefresh = r.o.Now()
	var lan []string
	if ips, err := r.o.LAN(); err == nil {
		for _, ip := range ips {
			lan = append(lan, ip.String())
		}
	} else {
		r.log.Warn("lan address enumeration failed", "err", err)
	}
	wan, wanErr := "", ""
	if cfg.Plex.RemoteAccess {
		switch {
		case cfg.Plex.PublicHost != "":
			wan = cfg.Plex.PublicHost
		default:
			if m := r.o.Mapper.Status(); m.Active && m.ExternalIP != "" && remote.IsPublicIP(net.ParseIP(m.ExternalIP)) {
				wan = m.ExternalIP
			} else if ip, err := r.o.WAN.Detect(ctx); err == nil {
				wan = ip.String()
			} else {
				wanErr = err.Error()
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Equal(lan, r.lastLAN) || wan != r.lastWAN {
		if r.lastLAN != nil || r.lastWAN != "" {
			r.log.Info("network addresses changed", "lan", lan, "wan", wan)
		}
		r.requestPublishLocked()
	}
	r.lastLAN, r.lastWAN = lan, wan
	r.st.LANAddresses = lan
	r.st.PublicIP = wan
	r.st.PublicIPErr = wanErr
}

// connections builds the connection list from the latest addresses.
func (r *Registrar) connections(cfg config.Config) []remote.Connection {
	r.mu.Lock()
	lanS, wanS := slices.Clone(r.lastLAN), r.lastWAN
	r.mu.Unlock()
	var lan []net.IP
	for _, s := range lanS {
		if ip := net.ParseIP(s); ip != nil {
			lan = append(lan, ip)
		}
	}
	var wan net.IP
	extraURLs := slices.Clone(cfg.Plex.CustomURLs)
	if wanS != "" {
		if ip := net.ParseIP(wanS); ip != nil {
			wan = ip
		} else if strings.HasPrefix(wanS, "https://") || strings.HasPrefix(wanS, "http://") {
			// PublicHost supplied as a full URL (e.g. https://plex.example.com).
			extraURLs = append(extraURLs, wanS)
		} else {
			// PublicHost is a DNS name: publish it as a custom URL, using https
			// when a custom TLS certificate is installed, port 443 is used, or
			// secure connections are required.
			port := r.wanPort(cfg)
			hostPort := net.JoinHostPort(wanS, fmt.Sprint(port))
			customCert := r.o.Certs != nil && r.o.Certs.Info().Present && r.o.Certs.Info().Custom && !r.o.Certs.Info().SelfSigned
			if cfg.Plex.SecureConnections != remote.SecureDisabled && (customCert || port == 443 || cfg.Plex.SecureConnections == remote.SecureRequired) {
				if port == 443 {
					extraURLs = append(extraURLs, "https://"+wanS)
				} else {
					extraURLs = append(extraURLs, "https://"+hostPort)
				}
			}
			if cfg.Plex.SecureConnections != remote.SecureRequired && port != 443 {
				extraURLs = append(extraURLs, "http://"+hostPort)
			}
		}
	}
	hash := cfg.Plex.CertHash
	if r.o.Certs != nil {
		if info := r.o.Certs.Info(); info.Present && !info.SelfSigned && info.Hash != "" {
			hash = info.Hash
		} else {
			hash = ""
		}
	}
	return remote.BuildConnections(remote.BuildOptions{
		LAN: lan, WAN: wan, LocalPort: serverPort(cfg), PublicPort: r.wanPort(cfg),
		CertHash: hash, Secure: cfg.Plex.SecureConnections, CustomURLs: extraURLs,
	})
}

func publishable(cs []remote.Connection, includePlexDirect bool) []string {
	var out []string
	for _, c := range cs {
		if c.Protocol == "https" && !c.Custom && !includePlexDirect {
			continue
		}
		out = append(out, c.URI)
	}
	return out
}

// publishSet is this node's connections and the URIs it publishes from
// them.
func (r *Registrar) publishSet(cfg config.Config) ([]remote.Connection, []string) {
	conns := r.connections(cfg)
	uris := publishable(conns, r.o.PublishPlexDirect || cfg.Plex.SecureConnections == remote.SecureRequired)
	if !cfg.Plex.RemoteAccess {
		// Only LAN and custom URLs when remote access is off.
		var lanOnly []string
		for _, c := range conns {
			if (c.Local || c.Custom) && slices.Contains(uris, c.URI) {
				lanOnly = append(lanOnly, c.URI)
			}
		}
		uris = lanOnly
	}
	return conns, uris
}

// NodeURIs returns the URIs this node would publish for itself, refreshing
// its LAN/WAN addresses first when they are older than AddressInterval
// (the loop keeps them fresh while it runs). A clustered node heartbeats
// these so the leader can publish the union.
func (r *Registrar) NodeURIs(ctx context.Context) []string {
	cfg := r.o.Config.Get()
	r.addrMu.Lock()
	stale := r.addrRefresh.IsZero() || r.o.Now().Sub(r.addrRefresh) >= r.o.AddressInterval
	r.addrMu.Unlock()
	if stale {
		r.refreshAddresses(ctx, cfg)
	}
	_, uris := r.publishSet(cfg)
	return uris
}

// peerLANAddrs is the LAN IP addresses in the other cluster nodes' URIs
// (PeerURIs), for servers.xml's localAddresses: plex.tv then knows every
// node's LAN address, not only the leader's.
func (r *Registrar) peerLANAddrs() []string {
	if r.o.PeerURIs == nil {
		return nil
	}
	var out []string
	for _, raw := range r.o.PeerURIs() {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		ip := net.ParseIP(u.Hostname())
		if ip == nil {
			ip, _, _ = remote.ParsePlexDirectHost(u.Hostname())
		}
		if ip == nil || !remote.UsableLANIP(ip) || remote.IsPublicIP(ip) {
			continue
		}
		if s := ip.String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

// mergeURIs appends extra to own, skipping duplicates.
func mergeURIs(own, extra []string) []string {
	out := slices.Clone(own)
	for _, u := range extra {
		if u != "" && !slices.Contains(out, u) {
			out = append(out, u)
		}
	}
	return out
}

// PublishNow publishes the current connections to plex.tv and verifies the
// server appears in the resources list.
func (r *Registrar) PublishNow(ctx context.Context) error {
	r.publicationRunMu.Lock()
	defer r.publicationRunMu.Unlock()
	r.publicationMu.Lock()
	if !r.publicationEnabled {
		r.publicationMu.Unlock()
		return errors.New("plextv: Plex apps are disabled")
	}
	ctx, cancel := context.WithCancel(ctx)
	r.publicationCancel = cancel
	r.publicationMu.Unlock()
	defer func() {
		cancel()
		r.publicationMu.Lock()
		r.publicationCancel = nil
		r.publicationMu.Unlock()
	}()
	r.mu.Lock()
	generation := r.pubGeneration
	libraries := slices.Clone(r.libraries)
	r.mu.Unlock()
	cfg := r.o.Config.Get()
	if cfg.Plex.Token == "" {
		return errors.New("plextv: not claimed")
	}
	r.mu.Lock()
	haveAddrs := r.lastLAN != nil || r.lastWAN != ""
	r.mu.Unlock()
	if !haveAddrs {
		r.refreshAddresses(ctx, cfg)
	}
	conns, uris := r.publishSet(cfg)
	if r.o.PeerURIs != nil {
		uris = mergeURIs(uris, r.o.PeerURIs())
	}
	if len(uris) == 0 {
		err := errors.New("plextv: no connection addresses to publish")
		r.mu.Lock()
		r.st.LastPublish = r.o.Now()
		r.st.LastPublishError = err.Error()
		r.mu.Unlock()
		return err
	}
	now := r.o.Now()
	flags := &PublishFlags{HTTPSRequired: cfg.Plex.SecureConnections == "required"}
	if cfg.Plex.SecureConnections != "disabled" {
		if r.o.Certs != nil {
			ci := r.o.Certs.Info()
			flags.HTTPSEnabled = ci.Present && !ci.SelfSigned
		}
		if !flags.HTTPSEnabled {
			for _, u := range uris {
				if strings.HasPrefix(u, "https://") {
					flags.HTTPSEnabled = true
					break
				}
			}
		}
	}
	err := r.client.PublishConnectionsWith(ctx, cfg.Plex.Token, uris, flags)

	r.mu.Lock()
	r.st.Connections = conns
	r.st.LastPublish = now
	if err != nil {
		r.st.LastPublishError = err.Error()
		r.st.TokenRejected = errors.Is(err, core.ErrUnauthorized)
		r.mu.Unlock()
		r.log.Warn("publish connections failed", "err", err)
		return err
	}
	r.st.PublishedURIs = uris
	r.st.LastPublishError = ""
	r.st.TokenRejected = false
	r.pubFail = 0
	if r.pubGeneration == generation {
		r.forcePub = false
	}
	r.next[taskPublish] = now.Add(r.o.PublishInterval)
	lan, wan := slices.Clone(r.lastLAN), r.lastWAN
	r.mu.Unlock()
	r.log.Info("published connections", "count", len(uris))

	mapping := "unknown"
	if cfg.Plex.RemoteAccess {
		if m := r.o.Mapper.Status(); m.Active || cfg.Plex.PublicPort > 0 {
			mapping = "mapped"
		}
	} else {
		mapping = "unmapped"
	}
	reg := ServerRegistration{
		Name: cfg.Server.FriendlyName, Version: r.o.Version, LocalAddresses: mergeURIs(lan, r.peerLANAddrs()), MappingState: mapping,
		PrivatePort: serverPort(cfg),
		Libraries:   libraries,
	}
	if len(lan) > 0 {
		reg.PrivateAddress = lan[0]
	}
	if cfg.Plex.RemoteAccess {
		reg.Port = r.wanPort(cfg)
		if net.ParseIP(wan) != nil {
			reg.Address = wan
		}
	}
	regErr := r.client.RegisterServer(ctx, cfg.Plex.Token, reg)
	if regErr == nil && cfg.Plex.RemoteAccess {
		var id string
		id, regErr = r.client.ReportConnectivity(ctx, cfg.Plex.Token)
		if regErr == nil {
			r.mu.Lock()
			r.asyncID = id
			r.mu.Unlock()
		}
	}
	if regErr != nil && ctx.Err() == nil {
		r.log.Debug("servers.xml registration failed (non-fatal)", "err", regErr)
	}
	res, ferr := r.client.FindServer(ctx, cfg.Plex.Token, cfg.Server.MachineIdentifier)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.RegisterError = ""
	if regErr != nil {
		r.st.RegisterError = regErr.Error()
	}
	if ferr == nil {
		r.st.Listed = true
		r.st.ResourceConnections = res.Connections
		r.st.PublicAddress = res.PublicAddress
	} else if ctx.Err() == nil {
		r.st.Listed = false
		r.st.ResourceConnections = nil
	}
	return nil
}

// SyncFriendsNow refreshes friends and their access tokens.
func (r *Registrar) SyncFriendsNow(ctx context.Context) error {
	cfg := r.o.Config.Get()
	if cfg.Plex.Token == "" {
		return errors.New("plextv: not claimed")
	}
	friends, err := r.client.Friends(ctx, cfg.Plex.Token)
	if err == nil && r.o.Users != nil {
		err = r.o.Users.SyncFriends(ctx, friends)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.LastFriendSync = r.o.Now()
	if err != nil {
		r.st.FriendSyncError = err.Error()
		if errors.Is(err, core.ErrUnauthorized) {
			// A rejected token stays rejected until the owner re-claims:
			// retry rarely instead of every minute.
			r.st.TokenRejected = true
			r.next[taskFriends] = r.o.Now().Add(rejectedRetry)
		}
		r.log.Warn("friend sync failed", "err", err)
		return err
	}
	n := 0
	for _, f := range friends {
		if !f.Owned {
			n++
		}
	}
	r.st.Friends = n
	r.st.FriendSyncError = ""
	return nil
}

// CheckReachability probes the WAN connections and records the result.
func (r *Registrar) CheckReachability(ctx context.Context) remote.Reachability {
	cfg := r.o.Config.Get()
	var uris []string
	// PMS checks its own public plex.direct name on the mapped public port,
	// even when the local listener uses a different port.
	r.mu.Lock()
	wan := r.lastWAN
	r.mu.Unlock()
	if ip := net.ParseIP(wan); ip != nil && cfg.Plex.CertHash != "" {
		hash := cfg.Plex.CertHash
		if r.o.Certs != nil {
			if ci := r.o.Certs.Info(); ci.Present && !ci.SelfSigned && ci.Hash != "" {
				hash = ci.Hash
			}
		}
		if hash != "" {
			uris = append(uris, "https://"+net.JoinHostPort(remote.PlexDirectHost(ip, hash), fmt.Sprint(r.wanPort(cfg))))
		}
	}
	for _, c := range r.connections(cfg) {
		if !c.Local && (len(uris) == 0 || c.Custom) && !slices.Contains(uris, c.URI) {
			uris = append(uris, c.URI)
		}
	}
	res := r.o.Reach.Check(ctx, uris, cfg.Server.MachineIdentifier)
	r.mu.Lock()
	r.st.Reachability = &res
	r.mu.Unlock()
	return res
}

// RenewCertificate runs the certificate manager; force bypasses the
// failure back-off (admin "retry now").
func (r *Registrar) RenewCertificate(ctx context.Context, force bool) error {
	if r.o.Certs == nil {
		return errors.New("plextv: certificate management disabled")
	}
	cfg := r.o.Config.Get()
	r.mu.Lock()
	hosts := slices.Clone(r.lastLAN)
	r.mu.Unlock()
	before := r.o.Certs.Info()
	info, err := r.o.Certs.Ensure(ctx, cfg.Plex.Token, hosts, force)
	r.mu.Lock()
	r.st.CertError = ""
	if err != nil {
		r.st.CertError = err.Error()
	}
	r.mu.Unlock()
	if info.Present && (info.Fingerprint != before.Fingerprint || cfg.Plex.CertFile != info.CertFile || cfg.Plex.CertHash != info.Hash) {
		uerr := r.o.Config.Update(func(c *config.Config) error {
			c.Plex.CertFile, c.Plex.KeyFile = info.CertFile, info.KeyFile
			if !info.SelfSigned {
				c.Plex.CertHash = info.Hash
			} else {
				c.Plex.CertHash = ""
			}
			return nil
		})
		if uerr != nil {
			r.log.Warn("persist certificate paths", "err", uerr)
		}
		r.mu.Lock()
		r.requestPublishLocked()
		r.mu.Unlock()
	}
	return err
}

// scheduleCertCheck brings the next certificate check forward from
// CertInterval to when a failed plex.tv request may be retried, or to when
// the certificate enters its renewal window. Without it a request that
// failed once (a fresh install left on the self-signed fallback, or a
// renewal near expiry) waited a whole CertInterval instead of the
// manager's RetryAfter.
func (r *Registrar) scheduleCertCheck() {
	now := r.o.Now()
	next := now.Add(r.o.CertInterval)
	earlier := func(at time.Time) {
		if at.Before(next) {
			next = at
		}
	}
	info := r.o.Certs.Info()
	r.mu.Lock()
	certError := r.st.CertError
	r.mu.Unlock()
	if certError != "" && info.NextAttempt.IsZero() {
		earlier(now.Add(5 * time.Minute))
	}
	switch {
	case info.Custom:
	case !info.NextAttempt.IsZero():
		if info.NextAttempt.After(now) {
			earlier(info.NextAttempt)
		} else {
			earlier(now.Add(time.Minute))
		}
	case info.Present && !info.SelfSigned:
		at := info.NotAfter.Add(-r.o.Certs.renewBefore(info))
		if !at.After(now) {
			// Due but not requested by this node (e.g. a cluster
			// follower waiting for the leader): look again hourly.
			at = now.Add(time.Hour)
		}
		earlier(at)
	}
	r.schedule(taskCert, next)
}

// Claim claims the server for the account behind userToken (from PIN
// sign-in), persists the server token and owner, informs UserSync and
// triggers publishing.
func (r *Registrar) Claim(ctx context.Context, userToken string) (*ClaimResult, error) {
	// Persist before the remote operation so a crash after claim cannot lose
	// the need to bind a managed certificate to the new device record.
	if r.o.Certs != nil {
		if err := r.o.Certs.RequireReissue(ctx); err != nil {
			return nil, fmt.Errorf("plextv: prepare certificate for claim: %w", err)
		}
	}
	res, err := r.client.Claim(ctx, userToken)
	if err != nil {
		return nil, err
	}
	if err := r.o.Config.Update(func(c *config.Config) error {
		c.Plex.Token = res.ServerToken
		c.Plex.OwnerPlexID = res.Owner.ID
		c.Plex.OwnerUsername = res.Owner.Username
		c.Plex.Claimed = true
		return nil
	}); err != nil {
		return nil, fmt.Errorf("plextv: persist claim: %w", err)
	}
	if r.o.Users != nil {
		if err := r.o.Users.SetOwner(ctx, res.Owner, res.ServerToken); err != nil {
			return nil, fmt.Errorf("plextv: set owner: %w", err)
		}
	}
	r.mu.Lock()
	r.claimedToken = res.ServerToken
	r.st.TokenRejected = false
	r.st.PlexPass = res.Owner.Subscription.Active
	r.st.OwnerFeatures = ownerFeatures(nil, res.Owner.Subscription.Features)
	r.next[taskFriends] = time.Time{}
	r.next[taskCert] = time.Time{}
	// The claim reads the account's subscription flag; the owner refresh
	// also consults the subscriptions list (lifetime passes), so run it on
	// the next tick.
	r.next[taskOwner] = time.Time{}
	r.mu.Unlock()
	featureCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	features, featureErr := r.client.Features(featureCtx, res.ServerToken)
	cancel()
	if featureErr != nil {
		// Claim succeeded. Keep the legacy names and let the immediate owner
		// refresh retry this auxiliary request instead of undoing the claim.
		r.log.Warn("plex.tv owner features unavailable after claim", "err", featureErr)
	} else {
		r.mu.Lock()
		// The owner may have unclaimed or switched accounts during the fetch.
		if r.claimedToken == res.ServerToken {
			r.st.OwnerFeatures = ownerFeatures(features, res.Owner.Subscription.Features)
		}
		r.mu.Unlock()
	}
	r.refreshConfigStatus(r.o.Config.Get())
	r.log.Info("server claimed", "owner", res.Owner.Username, "method", res.Method)
	r.Trigger()
	return res, nil
}

// Unclaim signs the server out of plex.tv: its device record is removed
// from the owner's account (best effort), so another account can claim it,
// and the server token is forgotten.
func (r *Registrar) Unclaim(ctx context.Context) error {
	if r.o.Certs != nil {
		if err := r.o.Certs.RequireReissue(ctx); err != nil {
			return fmt.Errorf("plextv: prepare certificate for unclaim: %w", err)
		}
	}
	token := r.o.Config.Get().Plex.Token
	r.mu.Lock()
	r.claimedToken = "" // released here, not again by onConfig
	r.mu.Unlock()
	if err := r.o.Config.Update(func(c *config.Config) error {
		c.Plex.Claimed = false
		return nil
	}); err != nil {
		return err
	}
	r.releaseClaim(ctx, token)
	return nil
}

// onConfig notices the owner switching Claimed off (Settings) and releases
// the claim. Without the release the old account keeps the device record
// and a later claim by another account is refused by plex.tv with 401s.
func (r *Registrar) onConfig(c config.Config) {
	enabled := plexAppsEnabled(c)
	r.publicationMu.Lock()
	changed := r.publicationEnabled != enabled
	r.publicationEnabled = enabled
	if !enabled && r.publicationCancel != nil {
		r.publicationCancel()
	}
	r.publicationMu.Unlock()
	if changed {
		select {
		case r.presenceWake <- struct{}{}:
		default:
		}
		if enabled {
			r.RequestPublish()
		}
	}
	r.mu.Lock()
	prev := r.claimedToken
	if c.Plex.Claimed {
		r.claimedToken = c.Plex.Token
		r.mu.Unlock()
		return
	}
	r.claimedToken = ""
	r.mu.Unlock()
	if prev != "" {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if r.o.Certs != nil {
				if err := r.o.Certs.RequireReissue(ctx); err != nil {
					r.log.Warn("prepare certificate for unclaim", "err", err)
					return
				}
			}
			r.releaseClaim(ctx, prev)
		}()
	}
}

// releaseClaim removes this server's device record from the account behind
// token, then forgets token if it is still the stored one (a sign-in that
// happened meanwhile keeps its new token) and clears the plex.tv status.
func (r *Registrar) releaseClaim(ctx context.Context, token string) {
	r.mu.Lock()
	r.claimedToken = ""
	r.mu.Unlock()
	if token != "" {
		switch removed, err := r.client.RemoveDevice(ctx, token); {
		case err != nil:
			r.log.Warn("could not remove this server from the previous Plex account; remove it under Authorized Devices on plex.tv before claiming with another account", "err", err)
		case removed:
			r.log.Info("server removed from the previous Plex account")
		}
	}
	if err := r.o.Config.Update(func(c *config.Config) error {
		if !c.Plex.Claimed && c.Plex.Token == token {
			c.Plex.Token = ""
		}
		return nil
	}); err != nil {
		r.log.Warn("forget server token", "err", err)
	}
	r.mu.Lock()
	r.st.Listed = false
	r.st.PlexPass = false
	r.st.OwnerFeatures = nil
	r.st.PublishedURIs = nil
	r.st.ResourceConnections = nil
	r.st.LastPublishError = ""
	r.st.FriendSyncError = ""
	r.st.TokenRejected = false
	r.mu.Unlock()
	r.refreshConfigStatus(r.o.Config.Get())
}

// RefreshOwner re-reads the owner's account (Plex Pass state) and passes it
// to UserSync; call at startup.
func (r *Registrar) RefreshOwner(ctx context.Context) error {
	cfg := r.o.Config.Get()
	if cfg.Plex.Token == "" {
		return errors.New("plextv: not claimed")
	}
	acct, err := r.client.User(ctx, cfg.Plex.Token)
	if err != nil {
		return err
	}
	features, err := r.client.Features(ctx, cfg.Plex.Token)
	if err != nil {
		// An incomplete refresh must not erase a previously verified UUID
		// snapshot. runDue retries in ten minutes instead of six hours.
		return err
	}
	pass := acct.Subscription.Active
	if subs, serr := r.client.UserSubscriptions(ctx, cfg.Plex.Token); serr == nil {
		for _, s := range subs {
			if s.PlexID == acct.ID {
				if s.PlexPassKnown {
					pass = s.PlexPass
				}
				break
			}
		}
	}
	r.mu.Lock()
	if r.claimedToken != cfg.Plex.Token {
		r.mu.Unlock()
		return errors.New("plextv: owner changed during refresh")
	}
	r.st.PlexPass = pass
	r.st.OwnerFeatures = ownerFeatures(features, acct.Subscription.Features)
	r.next[taskOwner] = r.o.Now().Add(r.o.OwnerInterval)
	r.mu.Unlock()
	if r.o.Users != nil {
		return r.o.Users.SetOwner(ctx, *acct, cfg.Plex.Token)
	}
	return nil
}

// onPubsub handles events pushed over the presence stream.
func (r *Registrar) onPubsub(m PubsubMessage) {
	switch m.Command {
	case "notifyConnectivity":
		r.mu.Lock()
		// Like PMS, only trust the answer to our outstanding request.
		if r.asyncID == "" || m.AsyncIdentifier != r.asyncID {
			r.mu.Unlock()
			return
		}
		ok := m.Reachable()
		r.st.PlexReachable = &ok
		r.asyncID = ""
		r.mu.Unlock()
		r.log.Info("plex.tv connectivity check", "reachable", ok)
	case "refreshCertificate":
		r.schedule(taskCert, time.Time{})
		r.Trigger()
	case "notifyResetToken", "notifyAccountChange":
		r.schedule(taskFriends, time.Time{})
		r.Trigger()
	default:
		r.log.Debug("pubsub event", "command", m.Command)
	}
}
