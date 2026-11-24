package config

import (
	"strings"
)

// Extra Plex servers are other Plex accounts the owner connects to this Ether.
// Each appears in Plex as its own server on the same port and serves the same
// Ether: libraries, viewers, watch state and settings are shared; only the
// plex.tv identity differs. The feature is off by default and capped
// (Plex.ExtraServersEnabled, PlexMaxExtraServers).

// PlexServerStandby is a signed-in account whose server is not claimed or
// published yet. It starts (PlexServerActive) only when the owner starts it.
const (
	PlexServerStandby = "standby"
	PlexServerActive  = "active"
)

// DefaultPlexSeatsPerServer is how many viewers one Plex server takes before
// new invites go to the next started server.
const DefaultPlexSeatsPerServer = 99

// PlexServerAccount is the plex.tv account that owns an extra server.
// Token is the account's sign-in token, kept to claim the server when it
// starts; the JSON keys token and username are sealed at rest.
type PlexServerAccount struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

// PlexExtraServer is one extra Plex server identity.
type PlexExtraServer struct {
	// ID is a short stable key ("p2", "p3", ...) used in routes and logs.
	ID string `json:"id"`
	// Name overrides the advertised name; empty uses Server.FriendlyName.
	Name string `json:"name,omitempty"`
	// MachineIdentifier and ClientIdentifier are generated when the server
	// is added and never change. MachineIdentifier is the server's plex.tv
	// device id. ClientIdentifier is the profile's own app-instance id: the
	// one its account sign-in (PIN) used, so the account's plex.tv
	// "authorized devices" entry is not shared with other profiles. Entries
	// added before sign-ins carried their own id hold an unrelated random id.
	MachineIdentifier string            `json:"machineIdentifier"`
	ClientIdentifier  string            `json:"clientIdentifier"`
	State             string            `json:"state"`
	Account           PlexServerAccount `json:"account"`
	// Token is the server's own plex.tv token once claimed.
	Token         string `json:"token,omitempty"`
	OwnerPlexID   int64  `json:"ownerPlexId,omitempty"`
	OwnerUsername string `json:"ownerUsername,omitempty"`
	Claimed       bool   `json:"claimed,omitempty"`
	CertHash      string `json:"certHash,omitempty"`
	AddedAt       int64  `json:"addedAt,omitempty"`
	ActivatedAt   int64  `json:"activatedAt,omitempty"`
	// Retired: the owner stopped new invites here (a ban, or emptying it);
	// the server keeps running for the viewers still on it.
	Retired bool `json:"retired,omitempty"`
}

// Active reports whether the server is started (claimed and published).
func (s PlexExtraServer) Active() bool { return s.State == PlexServerActive }

// AdvertisedName is the name Plex apps show for this server.
func (s PlexExtraServer) AdvertisedName(c Config) string {
	if n := strings.TrimSpace(s.Name); n != "" {
		return n
	}
	return strings.TrimSpace(c.Server.FriendlyName)
}

// DefaultMaxExtraPlexServers is how many extra Plex servers an owner can add
// unless they raise the limit.
const DefaultMaxExtraPlexServers = 2

// MaxExtraPlexServersLimit is the highest limit the setting accepts.
const MaxExtraPlexServersLimit = 10

// PlexMaxExtraServers is how many extra Plex servers can be added.
func (c Config) PlexMaxExtraServers() int {
	if c.Plex.MaxExtraServers > 0 {
		return c.Plex.MaxExtraServers
	}
	return DefaultMaxExtraPlexServers
}

// PlexSeatsPerServer is how many viewers fill one Plex server.
func (c Config) PlexSeatsPerServer() int {
	if c.Plex.SeatsPerServer > 0 {
		return c.Plex.SeatsPerServer
	}
	return DefaultPlexSeatsPerServer
}

// PlexSeatWarning is the stored Plex.SeatWarning or its default. Nothing acts
// on it any more; it is kept so saved settings still load and validate.
func (c Config) PlexSeatWarning() int {
	seats := c.PlexSeatsPerServer()
	if w := c.Plex.SeatWarning; w > 0 && w <= seats {
		return w
	}
	w := seats * 9 / 10
	if w < 1 {
		w = 1
	}
	return w
}

// PlexExtraServer returns the extra server with id.
func (c Config) PlexExtraServer(id string) (PlexExtraServer, bool) {
	for _, s := range c.PlexExtraServers {
		if s.ID == id {
			return s, true
		}
	}
	return PlexExtraServer{}, false
}

// WithPlexServer returns c as the extra server s sees it: c with the server
// identity and plex.tv account fields replaced by s's. Everything else
// (ports, remote access, addresses) is shared with the main server. The
// owner's custom certificate belongs to the main server only.
func (c Config) WithPlexServer(s PlexExtraServer) Config {
	out := c
	out.Server.MachineIdentifier = s.MachineIdentifier
	out.Server.FriendlyName = s.AdvertisedName(c)
	out.Plex.ClientIdentifier = s.ClientIdentifier
	out.Plex.Token = s.Token
	out.Plex.OwnerPlexID = s.OwnerPlexID
	out.Plex.OwnerUsername = s.OwnerUsername
	out.Plex.Claimed = s.Claimed
	out.Plex.CertHash = s.CertHash
	out.Plex.CertFile, out.Plex.KeyFile = "", ""
	return out
}

// StorePlexServer copies the identity fields of view (a WithPlexServer view
// edited by plex.tv code: a claim, a certificate) back into the entry id.
func (c *Config) StorePlexServer(id string, view Config) {
	for i := range c.PlexExtraServers {
		s := &c.PlexExtraServers[i]
		if s.ID != id {
			continue
		}
		s.Token = view.Plex.Token
		s.OwnerPlexID = view.Plex.OwnerPlexID
		s.OwnerUsername = view.Plex.OwnerUsername
		s.Claimed = view.Plex.Claimed
		s.CertHash = view.Plex.CertHash
		return
	}
}

// NewPlexExtraServer returns a standby entry for acct with fresh identifiers.
func NewPlexExtraServer(id string, acct PlexServerAccount, now int64) PlexExtraServer {
	return PlexExtraServer{
		ID:                id,
		MachineIdentifier: randomID(20),
		ClientIdentifier:  randomID(16),
		State:             PlexServerStandby,
		Account:           acct,
		AddedAt:           now,
	}
}

// NewPlexClientIdentifier returns a fresh app-instance client identifier for
// one profile's plex.tv sign-in.
func NewPlexClientIdentifier() string { return randomID(16) }

// PlexClientIdentifierInUse reports whether id is the main setup identifier
// or belongs to an extra server (either of its identifiers).
func (c Config) PlexClientIdentifierInUse(id string) bool {
	if id == "" || id == c.Plex.ClientIdentifier || id == c.Server.MachineIdentifier {
		return true
	}
	for _, s := range c.PlexExtraServers {
		if id == s.ClientIdentifier || id == s.MachineIdentifier {
			return true
		}
	}
	return false
}

// NextPlexServerID is the first unused id of the form p2, p3, ...
func (c Config) NextPlexServerID() string {
	used := map[string]bool{}
	for _, s := range c.PlexExtraServers {
		used[s.ID] = true
	}
	for n := 2; ; n++ {
		id := "p" + itoa(n)
		if !used[id] {
			return id
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
