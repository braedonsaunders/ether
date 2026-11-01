package plextv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Friend is an account with access to this server and the server access
// token plex.tv issued it. Official apps send that token (from their
// resources list's accessToken) as X-Plex-Token when they talk to the
// server, so the server can recognise friends without asking plex.tv.
type Friend struct {
	PlexID     int64    `json:"plexId"`
	UUID       string   `json:"uuid,omitempty"`
	Username   string   `json:"username"`
	Title      string   `json:"title"`
	Email      string   `json:"email,omitempty"`
	Thumb      string   `json:"thumb,omitempty"`
	Token      string   `json:"-"`
	Owned      bool     `json:"owned"` // the owner's own access token
	Home       bool     `json:"home"`
	Restricted bool     `json:"restricted"` // managed user
	Pending    bool     `json:"pending"`    // invite not accepted yet
	AllLibs    bool     `json:"allLibraries"`
	Sections   []string `json:"sections,omitempty"` // shared section keys
	Track      TrackDefaults
	Share      ShareDefaults
}

// DisplayName prefers title over username.
func (f Friend) DisplayName() string {
	if f.Title != "" {
		return f.Title
	}
	return f.Username
}

// AccessTokens fetches the access tokens of every account that can use this
// server: GET servers.plex.tv/api/v2/server/access_tokens, called with
// the server token and the server identity. This is the call PMS makes
// ("MyPlex: updating with N access tokens").
func (c *Client) AccessTokens(ctx context.Context, serverToken string) ([]Friend, error) {
	resp, err := c.do(ctx, request{
		method:     http.MethodGet,
		host:       c.serversHost(),
		path:       "/api/v2/server/access_tokens",
		token:      serverToken,
		tokenQuery: "auth_token",
		accept:     "application/xml",
	})
	if err != nil {
		return nil, err
	}
	root, err := parseXMLNode(resp.body)
	if err != nil {
		return nil, fmt.Errorf("plextv: decode access tokens: %w", err)
	}
	var out []Friend
	for _, n := range root.findAll("token") {
		if n.boolAttr("tokenDisabled", "disabled") {
			continue
		}
		f := Friend{
			Token:      n.attr("token", "accessToken", "authToken"),
			PlexID:     n.intAttr("id", "userID", "user_id", "userId", "invitedId"),
			UUID:       n.attr("uuid"),
			Username:   n.attr("username"),
			Title:      n.attr("title"),
			Email:      n.attr("email"),
			Thumb:      n.attr("thumb"),
			Owned:      n.boolAttr("owned"),
			Home:       n.boolAttr("home"),
			Restricted: n.boolAttr("restricted"),
			AllLibs:    n.boolAttr("allLibraries", "all_libraries"),
		}
		// Observed v2 responses carry a friend's identity on an <invited>
		// child (<token type="server"><invited id=… username=…/>), not on
		// the token element. Without it every friend has no Plex id and the
		// sync drops them all.
		if inv := childElement(n, "invited"); inv != nil {
			if f.PlexID == 0 {
				f.PlexID = inv.intAttr("id")
			}
			fill := func(dst *string, src string) {
				if *dst == "" {
					*dst = src
				}
			}
			fill(&f.UUID, inv.attr("uuid"))
			fill(&f.Username, inv.attr("username"))
			fill(&f.Title, inv.attr("title"))
			fill(&f.Email, inv.attr("email"))
			fill(&f.Thumb, inv.attr("thumb"))
			f.Home = f.Home || inv.boolAttr("home")
			f.Restricted = f.Restricted || inv.boolAttr("restricted")
			if profile := childElement(inv, "profile"); profile != nil {
				f.Track = profileDefaults(profile)
			}
		}
		if settings := childElement(n, "settings"); settings != nil {
			f.Share = shareDefaults(settings)
		} else if sync := optionalBoolAttr(n, "allowSync"); sync != nil {
			f.Share.AllowSync = sync
			f.Share.HasSettings = true
		}
		for _, s := range n.findAll("section") {
			if k := s.attr("key", "id"); k != "" {
				f.Sections = append(f.Sections, k)
			}
		}
		if f.Token == "" {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// childElement returns n's first direct child element named name.
func childElement(n *xmlNode, name string) *xmlNode {
	want := normName(name)
	for i := range n.Nodes {
		if normName(n.Nodes[i].XMLName.Local) == want {
			return &n.Nodes[i]
		}
	}
	return nil
}

// SharedServers lists the owner's shares of this server with their access
// tokens: GET /api/servers/{machineIdentifier}/shared_servers (owner token).
// It is the documented fallback for AccessTokens.
func (c *Client) SharedServers(ctx context.Context, ownerToken string) ([]Friend, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/servers/" + url.PathEscape(c.id.ClientIdentifier) + "/shared_servers",
		token:  ownerToken,
		accept: "application/xml",
	})
	if err != nil {
		return nil, err
	}
	root, err := parseXMLNode(resp.body)
	if err != nil {
		return nil, fmt.Errorf("plextv: decode shared servers: %w", err)
	}
	var out []Friend
	for _, n := range root.findAll("SharedServer") {
		f := Friend{
			Token:    n.attr("accessToken", "token"),
			PlexID:   n.intAttr("userID", "user_id"),
			Username: n.attr("username"),
			Title:    n.attr("username"),
			Email:    n.attr("email"),
			Owned:    n.boolAttr("owned"),
			AllLibs:  n.boolAttr("allLibraries"),
			Share: ShareDefaults{
				FilterMovies:     n.attr("filterMovies"),
				FilterTelevision: n.attr("filterTelevision"),
				AllowSync:        optionalBoolAttr(n, "allowSync"),
				HasSettings:      n.attr("filterMovies") != "" || n.attr("filterTelevision") != "" || n.attr("allowSync") != "",
			},
		}
		accepted := n.attr("acceptedAt")
		f.Pending = accepted == "" || accepted == "0"
		for _, s := range n.findAll("Section") {
			if s.attr("shared") == "0" {
				continue
			}
			if k := s.attr("key", "id"); k != "" {
				f.Sections = append(f.Sections, k)
			}
		}
		if f.Token == "" || f.PlexID == 0 {
			continue
		}
		out = append(out, f)
	}
	return out, nil
}

// SharedUser is an entry of the owner's friends/home list (v2 server users).
type SharedUser struct {
	PlexID        int64
	UUID          string
	Username      string
	Title         string
	Email         string
	Thumb         string
	Home          bool
	Restricted    bool
	PlexPass      bool
	PlexPassKnown bool
	// Servers are the machine identifiers shared with the user.
	Servers []string
}

// Users lists the owner's friends and home users from the observed v2 API.
func (c *Client) Users(ctx context.Context, ownerToken string) ([]SharedUser, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, host: c.serversHost(), path: "/api/v2/server/users", token: ownerToken, tokenQuery: "auth_token", accept: "application/xml"})
	if err != nil {
		return nil, err
	}
	root, err := parseXMLNode(resp.body)
	if err != nil {
		return nil, fmt.Errorf("plextv: decode users: %w", err)
	}
	var out []SharedUser
	for _, n := range root.findAll("User") {
		u := SharedUser{
			PlexID:     n.intAttr("id"),
			UUID:       n.attr("uuid"),
			Username:   n.attr("username"),
			Title:      n.attr("title"),
			Email:      n.attr("email"),
			Thumb:      n.attr("thumb"),
			Home:       n.boolAttr("home"),
			Restricted: n.boolAttr("restricted"),
		}
		for i := range n.Nodes {
			s := &n.Nodes[i]
			if strings.EqualFold(s.XMLName.Local, "Server") {
				if m := s.attr("machineIdentifier"); m != "" {
					u.Servers = append(u.Servers, m)
				}
			}
		}
		if u.PlexID != 0 {
			out = append(out, u)
		}
	}
	return out, nil
}

// UserSubscriptions reads the server-scoped subscription snapshot. Plex
// returns an XML users collection; the owner's subscription feature names
// are retained for the admin UI, while Active reflects Plex Pass entitlement.
func (c *Client) UserSubscriptions(ctx context.Context, serverToken string) ([]SharedUser, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, host: c.serversHost(), path: "/api/v2/server/users/subscriptions", token: serverToken, tokenQuery: "auth_token", accept: "application/xml"})
	if err != nil {
		return nil, err
	}
	root, err := parseXMLNode(resp.body)
	if err != nil {
		return nil, fmt.Errorf("plextv: decode user subscriptions: %w", err)
	}
	var out []SharedUser
	for _, n := range root.findAll("user") {
		u := SharedUser{PlexID: n.intAttr("id"), UUID: n.attr("uuid"), Username: n.attr("username"), Title: n.attr("title"), Email: n.attr("email")}
		if u.PlexID == 0 {
			continue
		}
		for _, s := range n.findAll("subscription") {
			u.PlexPassKnown = true
			if s.boolAttr("active") || strings.EqualFold(s.attr("status"), "active") {
				u.PlexPass = true
			}
		}
		out = append(out, u)
	}
	return out, nil
}

// Service describes a Plex service entitlement attached to an account.
// Credentials are intentionally omitted from the public type.
type Service struct {
	PlexID     int64
	Identifier string
	Endpoint   string
	Status     string
}

// UserServices fetches the current server-scoped service snapshot.
func (c *Client) UserServices(ctx context.Context, serverToken string) ([]Service, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, host: c.serversHost(), path: "/api/v2/server/users/services", token: serverToken, tokenQuery: "auth_token", accept: "application/xml"})
	if err != nil {
		return nil, err
	}
	root, err := parseXMLNode(resp.body)
	if err != nil {
		return nil, fmt.Errorf("plextv: decode user services: %w", err)
	}
	var out []Service
	for _, user := range root.findAll("user") {
		uid := user.intAttr("id")
		for _, s := range user.findAll("service") {
			out = append(out, Service{PlexID: uid, Identifier: s.attr("identifier"), Endpoint: s.attr("endpoint"), Status: s.attr("status")})
		}
	}
	return out, nil
}

// Friends returns everyone with access to this server and their tokens.
// It uses AccessTokens (what PMS calls) and falls back to SharedServers,
// then enriches entries with profile data from Users when available.
func (c *Client) Friends(ctx context.Context, serverToken string) ([]Friend, error) {
	friends, err := c.AccessTokens(ctx, serverToken)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c.log.Warn("access_tokens.xml failed, trying shared_servers", "err", err)
		var err2 error
		friends, err2 = c.SharedServers(ctx, serverToken)
		if err2 != nil {
			return nil, errors.Join(err, err2)
		}
	} else {
		// The current v2 response can contain server/device tokens without a
		// Plex account id. Supplement those rows from the legacy share listing,
		// which still carries userID/accessToken, without discarding v2 tokens.
		needsProfiles := true
		for _, f := range friends {
			if f.PlexID != 0 {
				needsProfiles = false
				break
			}
		}
		if needsProfiles {
			if legacy, lerr := c.SharedServers(ctx, serverToken); lerr == nil {
				seen := map[string]bool{}
				for _, f := range friends {
					seen[f.Token] = true
				}
				for _, f := range legacy {
					if !seen[f.Token] {
						friends = append(friends, f)
					}
				}
			}
		}
	}
	users, uerr := c.Users(ctx, serverToken)
	if uerr != nil {
		c.log.Debug("users enrichment failed", "err", uerr)
		return friends, nil
	}
	// PMS refreshes subscription and service data alongside users. Keep these
	// reads in the same sync so owner entitlement and service state stay fresh.
	if _, err := c.UserSubscriptions(ctx, serverToken); err != nil {
		c.log.Debug("subscription sync failed", "err", err)
	}
	if _, err := c.UserServices(ctx, serverToken); err != nil {
		c.log.Debug("service sync failed", "err", err)
	}
	byID := make(map[int64]SharedUser, len(users))
	byName := make(map[string]SharedUser, len(users))
	for _, u := range users {
		byID[u.PlexID] = u
		if u.Username != "" {
			byName[strings.ToLower(u.Username)] = u
		}
	}
	for i := range friends {
		f := &friends[i]
		u, ok := byID[f.PlexID]
		if !ok && f.Username != "" {
			u, ok = byName[strings.ToLower(f.Username)]
		}
		if !ok {
			continue
		}
		if f.PlexID == 0 {
			f.PlexID = u.PlexID
		}
		fill := func(dst *string, src string) {
			if *dst == "" {
				*dst = src
			}
		}
		fill(&f.UUID, u.UUID)
		fill(&f.Username, u.Username)
		fill(&f.Title, u.Title)
		fill(&f.Email, u.Email)
		fill(&f.Thumb, u.Thumb)
		f.Home = f.Home || u.Home
		f.Restricted = f.Restricted || u.Restricted
	}
	return friends, nil
}
