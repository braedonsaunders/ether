package plextv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
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

type rawShare struct {
	id       int64
	username string
	email    string
	userID   int64
}

func (c *Client) listRawShares(ctx context.Context, ownerToken string) ([]rawShare, error) {
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
	var out []rawShare
	for _, n := range root.findAll("SharedServer") {
		out = append(out, rawShare{
			id:       n.intAttr("id"),
			username: n.attr("username"),
			email:    n.attr("email"),
			userID:   n.intAttr("userID", "user_id"),
		})
	}
	return out, nil
}

// ServerSectionIDs queries plex.tv for this server's registered library section IDs.
func (c *Client) ServerSectionIDs(ctx context.Context, ownerToken string) ([]int64, error) {
	if c.id.ClientIdentifier == "" {
		return nil, errors.New("plextv: client identifier required")
	}
	resp, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/servers/" + url.PathEscape(c.id.ClientIdentifier),
		token:  ownerToken,
		accept: "application/xml",
	})
	if err != nil {
		return nil, err
	}
	root, err := parseXMLNode(resp.body)
	if err != nil {
		return nil, fmt.Errorf("plextv: decode server: %w", err)
	}
	var ids []int64
	for _, s := range root.findAll("Section") {
		if id := s.intAttr("id"); id > 0 {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

type v2InvitedUser struct {
	ID           int64  `json:"id"`
	UUID         string `json:"uuid"`
	Username     string `json:"username"`
	Email        string `json:"email"`
	Title        string `json:"title"`
	FriendlyName string `json:"friendlyName"`
	Thumb        string `json:"thumb"`
}

type v2SharedServer struct {
	ID                int64          `json:"id"`
	MachineIdentifier string         `json:"machineIdentifier"`
	AllLibraries      bool           `json:"allLibraries"`
	LibrarySectionIDs []int64        `json:"librarySectionIds"`
	Settings          map[string]any `json:"settings"`
	InvitedID         int64          `json:"invitedId"`
	InvitedEmail      string         `json:"invitedEmail"`
	Invited           v2InvitedUser  `json:"invited"`
}

type v2PendingInvite struct {
	InvitedID     int64            `json:"invitedId"`
	InvitedEmail  string           `json:"invitedEmail"`
	Invited       v2InvitedUser    `json:"invited"`
	SharedServers []v2SharedServer `json:"sharedServers"`
}

func mergeV2Invited(primary, fallback v2InvitedUser) v2InvitedUser {
	if primary.ID <= 0 {
		primary.ID = fallback.ID
	}
	if primary.UUID == "" {
		primary.UUID = fallback.UUID
	}
	if primary.Username == "" {
		primary.Username = fallback.Username
	}
	if primary.Email == "" {
		primary.Email = fallback.Email
	}
	if primary.Title == "" {
		primary.Title = fallback.Title
	}
	if primary.FriendlyName == "" {
		primary.FriendlyName = fallback.FriendlyName
	}
	if primary.Thumb == "" {
		primary.Thumb = fallback.Thumb
	}
	return primary
}

// IsEmailAddress reports whether s is a plain email address (local@domain)
// without whitespace or display-name wrappers. It guards share fallbacks and
// candidate filtering against treating handles like "@name" or "user@" as emails.
func IsEmailAddress(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || strings.ContainsAny(s, " \t\r\n<>") {
		return false
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Name != "" || addr.Address != s {
		return false
	}
	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at >= len(s)-1 {
		return false
	}
	domain := s[at+1:]
	dot := strings.IndexByte(domain, '.')
	return dot > 0 && dot < len(domain)-1 && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}

func matchesV2Invited(u v2InvitedUser, fallbackID int64, fallbackEmail, usernameOrEmail string, userID int64) bool {
	recordID := u.ID
	if recordID <= 0 {
		recordID = fallbackID
	}
	if userID > 0 {
		if recordID > 0 {
			return recordID == userID
		}
		email := strings.TrimSpace(usernameOrEmail)
		if !IsEmailAddress(email) {
			return false
		}
		return strings.EqualFold(strings.TrimSpace(u.Email), email) ||
			strings.EqualFold(strings.TrimSpace(fallbackEmail), email)
	}
	usernameOrEmail = strings.TrimSpace(usernameOrEmail)
	if usernameOrEmail == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(u.Username), usernameOrEmail) ||
		strings.EqualFold(strings.TrimSpace(u.Email), usernameOrEmail) ||
		strings.EqualFold(strings.TrimSpace(fallbackEmail), usernameOrEmail) ||
		strings.EqualFold(strings.TrimSpace(u.Title), usernameOrEmail) ||
		strings.EqualFold(strings.TrimSpace(u.FriendlyName), usernameOrEmail)
}

func (c *Client) fetchV2AcceptedShares(ctx context.Context, ownerToken string) ([]v2SharedServer, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/v2/shared_servers/owned/accepted",
		token:  ownerToken,
		accept: "application/json",
	})
	if err != nil {
		return nil, err
	}
	var accepted []v2SharedServer
	if err := json.Unmarshal(resp.body, &accepted); err != nil {
		return nil, fmt.Errorf("plextv: decode accepted shared servers: %w", err)
	}
	return accepted, nil
}

func (c *Client) fetchV2PendingInvites(ctx context.Context, ownerToken string) ([]v2PendingInvite, error) {
	resp, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/v2/shared_servers/invites/owned/pending",
		token:  ownerToken,
		accept: "application/json",
	})
	if err != nil {
		return nil, err
	}
	var pending []v2PendingInvite
	if err := json.Unmarshal(resp.body, &pending); err != nil {
		return nil, fmt.Errorf("plextv: decode pending invites: %w", err)
	}
	return pending, nil
}

func (c *Client) findV2Share(ctx context.Context, ownerToken, usernameOrEmail string, userID int64) (*v2SharedServer, error) {
	accepted, err := c.fetchV2AcceptedShares(ctx, ownerToken)
	if err == nil {
		for _, s := range accepted {
			if s.MachineIdentifier == c.id.ClientIdentifier && matchesV2Invited(s.Invited, s.InvitedID, s.InvitedEmail, usernameOrEmail, userID) {
				return &s, nil
			}
		}
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	} else if errors.Is(err, ErrRateLimited) {
		return nil, err
	}

	pending, err := c.fetchV2PendingInvites(ctx, ownerToken)
	if err == nil {
		for _, inv := range pending {
			for _, s := range inv.SharedServers {
				if s.MachineIdentifier != c.id.ClientIdentifier {
					continue
				}
				u := mergeV2Invited(s.Invited, inv.Invited)
				fid := s.InvitedID
				if fid <= 0 {
					fid = inv.InvitedID
				}
				femail := s.InvitedEmail
				if femail == "" {
					femail = inv.InvitedEmail
				}
				if matchesV2Invited(u, fid, femail, usernameOrEmail, userID) {
					return &s, nil
				}
			}
		}
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	} else if errors.Is(err, ErrRateLimited) {
		return nil, err
	}

	return nil, nil
}

// SharedUserDirectory merges plex.tv user records from multiple endpoints by
// PlexID, preserving non-empty profile fields and OR-ing Home/Restricted.
// It is safe for concurrent use.
type SharedUserDirectory struct {
	mu     sync.Mutex
	byID   map[int64]int
	byUser map[string]int
	users  []SharedUser
}

// NewSharedUserDirectory returns an empty SharedUserDirectory.
func NewSharedUserDirectory() *SharedUserDirectory {
	return &SharedUserDirectory{
		byID:   make(map[int64]int),
		byUser: make(map[string]int),
	}
}

// Merge folds list into the directory, deduplicating by PlexID.
func (d *SharedUserDirectory) Merge(list ...SharedUser) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, u := range list {
		if u.PlexID <= 0 {
			continue
		}
		if idx, ok := d.byID[u.PlexID]; ok {
			dst := &d.users[idx]
			if dst.UUID == "" {
				dst.UUID = u.UUID
			}
			if dst.Username == "" && u.Username != "" {
				dst.Username = u.Username
				d.byUser[strings.ToLower(strings.TrimSpace(dst.Username))] = idx
			}
			if dst.Title == "" {
				dst.Title = u.Title
			}
			if dst.FriendlyName == "" {
				dst.FriendlyName = u.FriendlyName
			}
			if dst.Email == "" {
				dst.Email = u.Email
			}
			if dst.Thumb == "" {
				dst.Thumb = u.Thumb
			}
			dst.Home = dst.Home || u.Home
			dst.Restricted = dst.Restricted || u.Restricted
			dst.AcceptedShare = dst.AcceptedShare || u.AcceptedShare
			dst.PendingShare = dst.PendingShare || u.PendingShare
			dst.Friend = dst.Friend || u.Friend
			dst.CommunityFriend = dst.CommunityFriend || u.CommunityFriend
			for _, m := range u.Servers {
				if m == "" {
					continue
				}
				found := false
				for _, cur := range dst.Servers {
					if cur == m {
						found = true
						break
					}
				}
				if !found {
					dst.Servers = append(dst.Servers, m)
				}
			}
			for _, m := range u.PendingServers {
				if m == "" {
					continue
				}
				found := false
				for _, cur := range dst.PendingServers {
					if cur == m {
						found = true
						break
					}
				}
				if !found {
					dst.PendingServers = append(dst.PendingServers, m)
				}
			}
			continue
		}
		if len(u.Servers) > 0 {
			u.Servers = append([]string(nil), u.Servers...)
		}
		if len(u.PendingServers) > 0 {
			u.PendingServers = append([]string(nil), u.PendingServers...)
		}
		idx := len(d.users)
		d.byID[u.PlexID] = idx
		if uname := strings.ToLower(strings.TrimSpace(u.Username)); uname != "" {
			d.byUser[uname] = idx
		}
		d.users = append(d.users, u)
	}
}

// EnrichCommunity merges numeric-ID community friends and fills missing
// display names / avatars on existing directory entries matched by username.
func (d *SharedUserDirectory) EnrichCommunity(friends []CommunityFriend) {
	for _, cf := range friends {
		if id, err := strconv.ParseInt(cf.ID, 10, 64); err == nil && id > 0 {
			d.Merge(SharedUser{
				PlexID:          id,
				Username:        cf.Username,
				FriendlyName:    cf.DisplayName,
				Thumb:           cf.Avatar,
				Friend:          true,
				CommunityFriend: true,
			})
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, cf := range friends {
		uname := strings.ToLower(strings.TrimSpace(cf.Username))
		if uname == "" {
			continue
		}
		idx, ok := d.byUser[uname]
		if !ok {
			continue
		}
		dst := &d.users[idx]
		dst.Friend = true
		dst.CommunityFriend = true
		if cf.DisplayName != "" {
			if dst.FriendlyName == "" {
				dst.FriendlyName = cf.DisplayName
			}
			if dst.Title == "" || strings.EqualFold(dst.Title, dst.Username) {
				dst.Title = cf.DisplayName
			}
		}
		if dst.Thumb == "" && cf.Avatar != "" {
			dst.Thumb = cf.Avatar
		}
	}
}

// Users returns a snapshot copy of the merged users.
func (d *SharedUserDirectory) Users() []SharedUser {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]SharedUser, len(d.users))
	copy(out, d.users)
	return out
}

func compactUserName(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}

// ResolveSharedUser matches a Plex account against users in strict priority order:
// 1. Exact PlexID match (when plexID > 0).
// 2. Unique case-insensitive Username or Email match.
// 3. Unique case-insensitive Title or FriendlyName match.
// 4. Unique whitespace-insensitive Username, Title, or FriendlyName match.
// Ambiguous matches (>1 distinct PlexID at the matching tier) return nil.
func ResolveSharedUser(users []SharedUser, plexID int64, name string) *SharedUser {
	u, _ := resolveSharedUser(users, plexID, name)
	return u
}

func resolveSharedUser(users []SharedUser, plexID int64, name string) (*SharedUser, bool) {
	if len(users) == 0 {
		return nil, false
	}
	if plexID > 0 {
		for i := range users {
			if users[i].PlexID == plexID {
				u := users[i]
				return &u, false
			}
		}
	}
	target := strings.TrimSpace(name)
	if target == "" {
		return nil, false
	}
	unique := func(pred func(u SharedUser) bool) (*SharedUser, bool) {
		var matched *SharedUser
		for i := range users {
			u := users[i]
			if u.PlexID <= 0 || !pred(u) {
				continue
			}
			if matched == nil {
				cp := u
				matched = &cp
				continue
			}
			if matched.PlexID != u.PlexID {
				return nil, true
			}
		}
		return matched, false
	}

	if u, amb := unique(func(u SharedUser) bool {
		return (u.Username != "" && strings.EqualFold(strings.TrimSpace(u.Username), target)) ||
			(u.Email != "" && strings.EqualFold(strings.TrimSpace(u.Email), target))
	}); u != nil || amb {
		return u, amb
	}
	if u, amb := unique(func(u SharedUser) bool {
		return (u.Title != "" && strings.EqualFold(strings.TrimSpace(u.Title), target)) ||
			(u.FriendlyName != "" && strings.EqualFold(strings.TrimSpace(u.FriendlyName), target))
	}); u != nil || amb {
		return u, amb
	}
	wantCompact := compactUserName(target)
	return unique(func(u SharedUser) bool {
		return (u.Username != "" && compactUserName(u.Username) == wantCompact) ||
			(u.Title != "" && compactUserName(u.Title) == wantCompact) ||
			(u.FriendlyName != "" && compactUserName(u.FriendlyName) == wantCompact)
	})
}

func resolveSharedUserByLogin(users []SharedUser, name string) (*SharedUser, bool) {
	target := strings.TrimSpace(name)
	if target == "" {
		return nil, false
	}
	var matched *SharedUser
	for i := range users {
		u := users[i]
		if u.PlexID <= 0 {
			continue
		}
		if (u.Username != "" && strings.EqualFold(strings.TrimSpace(u.Username), target)) ||
			(u.Email != "" && strings.EqualFold(strings.TrimSpace(u.Email), target)) {
			if matched == nil {
				cp := u
				matched = &cp
				continue
			}
			if matched.PlexID != u.PlexID {
				return nil, true
			}
		}
	}
	return matched, false
}

// SharedServerUsers lists invited Plex accounts from the owner's accepted and
// pending v2 server shares across all owned servers.
func (c *Client) SharedServerUsers(ctx context.Context, ownerToken string) ([]SharedUser, error) {
	dir := NewSharedUserDirectory()
	addInvited := func(u v2InvitedUser, fallbackID int64, fallbackEmail, machineID string, accepted, pending bool) {
		uid := u.ID
		if uid <= 0 {
			uid = fallbackID
		}
		if uid <= 0 {
			return
		}
		email := u.Email
		if email == "" {
			email = fallbackEmail
		}
		var (
			servers        []string
			pendingServers []string
		)
		if machineID != "" {
			if accepted {
				servers = []string{machineID}
			}
			if pending {
				pendingServers = []string{machineID}
			}
		}
		dir.Merge(SharedUser{
			PlexID:         uid,
			UUID:           u.UUID,
			Username:       u.Username,
			Title:          u.Title,
			FriendlyName:   u.FriendlyName,
			Email:          email,
			Thumb:          u.Thumb,
			AcceptedShare:  accepted,
			PendingShare:   pending,
			Servers:        servers,
			PendingServers: pendingServers,
		})
	}

	var firstErr error
	var okCount int
	if accepted, err := c.fetchV2AcceptedShares(ctx, ownerToken); err == nil {
		okCount++
		for _, s := range accepted {
			addInvited(s.Invited, s.InvitedID, s.InvitedEmail, s.MachineIdentifier, true, false)
		}
	} else {
		firstErr = err
	}

	if pending, err := c.fetchV2PendingInvites(ctx, ownerToken); err == nil {
		okCount++
		for _, inv := range pending {
			if len(inv.SharedServers) == 0 {
				addInvited(inv.Invited, inv.InvitedID, inv.InvitedEmail, "", false, true)
				continue
			}
			for _, s := range inv.SharedServers {
				u := mergeV2Invited(s.Invited, inv.Invited)
				fid := s.InvitedID
				if fid <= 0 {
					fid = inv.InvitedID
				}
				femail := s.InvitedEmail
				if femail == "" {
					femail = inv.InvitedEmail
				}
				addInvited(u, fid, femail, s.MachineIdentifier, false, true)
			}
		}
	} else if firstErr == nil {
		firstErr = err
	}

	if okCount == 0 && firstErr != nil {
		return nil, firstErr
	}
	return dir.Users(), nil
}

// CommunityFriends fetches the owner's Plex community friends when talking to
// the live plex.tv host, bounded by a short timeout.
func (c *Client) CommunityFriends(ctx context.Context, token string) ([]CommunityFriend, error) {
	if c.base == nil || c.base.String() != DefaultBaseURL || strings.TrimSpace(token) == "" {
		return nil, nil
	}
	commCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return (&CommunityClient{Client: c.hc, MinInterval: time.Millisecond, MaxRetries: -1}).Friends(commCtx, token)
}

func pickShareHandle(u SharedUser) string {
	if em := strings.TrimSpace(u.Email); em != "" && !strings.Contains(em, " ") {
		return em
	}
	if un := strings.TrimSpace(u.Username); un != "" && !strings.Contains(un, " ") {
		return un
	}
	return ""
}

func (c *Client) resolveFriendByName(ctx context.Context, ownerToken, name string) (int64, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, "", nil
	}
	dir := NewSharedUserDirectory()

	// Fast path: Plex usernames and emails are globally unique, so an exact
	// login match in AccountUsers resolves in a single round-trip.
	if users, err := c.AccountUsers(ctx, ownerToken); err == nil {
		dir.Merge(users...)
		if u, amb := resolveSharedUserByLogin(users, name); u != nil && !amb {
			c.log.Debug("resolve friend by name result", "candidates", len(users), "matched", true, "source", "account_users")
			return u.PlexID, pickShareHandle(*u), nil
		}
	} else if ctx.Err() != nil {
		return 0, "", ctx.Err()
	} else if errors.Is(err, ErrRateLimited) {
		return 0, "", err
	}

	// Fetch remaining account/server directories concurrently so Title,
	// FriendlyName, and compact matching check uniqueness across all sources.
	var (
		wg            sync.WaitGroup
		commMu        sync.Mutex
		communityList []CommunityFriend
	)
	wg.Go(func() {
		if users, err := c.Users(ctx, ownerToken); err == nil {
			dir.Merge(users...)
		}
	})
	wg.Go(func() {
		if users, err := c.SharedServerUsers(ctx, ownerToken); err == nil {
			dir.Merge(users...)
		}
	})
	wg.Go(func() {
		if homeUsers, err := c.HomeUsers(ctx, ownerToken); err == nil {
			mapped := make([]SharedUser, 0, len(homeUsers))
			for _, u := range homeUsers {
				if u.ID <= 0 || u.HomeAdmin {
					continue
				}
				mapped = append(mapped, SharedUser{
					PlexID:       u.ID,
					UUID:         u.UUID,
					Username:     u.Username,
					Title:        u.Title,
					FriendlyName: u.Friendly,
					Thumb:        u.Thumb,
					Home:         true,
					Restricted:   u.Restricted,
				})
			}
			dir.Merge(mapped...)
		}
	})
	wg.Go(func() {
		if friends, err := c.AccessTokens(ctx, ownerToken); err == nil {
			mapped := make([]SharedUser, 0, len(friends))
			for _, f := range friends {
				if f.Owned || f.PlexID <= 0 {
					continue
				}
				mapped = append(mapped, SharedUser{
					PlexID:     f.PlexID,
					UUID:       f.UUID,
					Username:   f.Username,
					Title:      f.Title,
					Email:      f.Email,
					Thumb:      f.Thumb,
					Home:       f.Home,
					Restricted: f.Restricted,
				})
			}
			dir.Merge(mapped...)
		}
	})
	wg.Go(func() {
		cFriends, err := c.CommunityFriends(ctx, ownerToken)
		if err != nil {
			c.log.Debug("community friends lookup failed", "err", err)
			return
		}
		commMu.Lock()
		communityList = cFriends
		commMu.Unlock()
	})
	wg.Wait()

	dir.EnrichCommunity(communityList)
	merged := dir.Users()
	if u, amb := resolveSharedUser(merged, 0, name); u != nil {
		c.log.Debug("resolve friend by name result", "candidates", len(merged), "matched", true)
		return u.PlexID, pickShareHandle(*u), nil
	} else if amb {
		c.log.Debug("resolve friend by name result", "candidates", len(merged), "matched", false, "ambiguous", true)
		return 0, "", nil
	}

	// Fallback for community friends whose GraphQL ID is non-numeric and who
	// did not appear in any numeric-ID directory above.
	if handle := resolveCommunityFriendHandle(communityList, name); handle != "" {
		c.log.Debug("resolve friend by name result", "candidates", len(merged), "matched", true, "source", "community")
		return 0, handle, nil
	}
	c.log.Debug("resolve friend by name result", "candidates", len(merged), "matched", false)
	return 0, "", nil
}

func resolveCommunityFriendHandle(friends []CommunityFriend, name string) string {
	if len(friends) == 0 {
		return ""
	}
	unique := func(pred func(cf CommunityFriend) bool) (string, bool) {
		var matched string
		for _, cf := range friends {
			un := strings.TrimSpace(cf.Username)
			if un == "" || strings.Contains(un, " ") || !pred(cf) {
				continue
			}
			if matched == "" {
				matched = un
				continue
			}
			if !strings.EqualFold(matched, un) {
				return "", true
			}
		}
		return matched, false
	}
	if h, amb := unique(func(cf CommunityFriend) bool {
		return strings.EqualFold(strings.TrimSpace(cf.Username), name)
	}); h != "" || amb {
		return h
	}
	if h, amb := unique(func(cf CommunityFriend) bool {
		return cf.DisplayName != "" && strings.EqualFold(strings.TrimSpace(cf.DisplayName), name)
	}); h != "" || amb {
		return h
	}
	wantCompact := compactUserName(name)
	h, _ := unique(func(cf CommunityFriend) bool {
		return compactUserName(cf.Username) == wantCompact ||
			(cf.DisplayName != "" && compactUserName(cf.DisplayName) == wantCompact)
	})
	return h
}

func (c *Client) postV2ShareBody(ctx context.Context, ownerToken string, allLibraries bool, sectionIDs []int64, userID int64, usernameOrEmail string, skipFriendship bool) ([]byte, error) {
	payloadV2 := map[string]any{
		"machineIdentifier": c.id.ClientIdentifier,
		"allLibraries":      allLibraries,
		"librarySectionIds": sectionIDs,
		"skipFriendship":    skipFriendship,
		"settings": map[string]any{
			"allowSync":         true,
			"allowCameraUpload": false,
			"allowChannels":     false,
			"filterMovies":      "",
			"filterTelevision":  "",
			"filterMusic":       "",
		},
	}
	if userID > 0 {
		payloadV2["invitedId"] = userID
	} else {
		payloadV2["invitedEmail"] = usernameOrEmail
	}
	bodyV2, err := json.Marshal(payloadV2)
	if err != nil {
		return nil, fmt.Errorf("plextv: marshal share payload: %w", err)
	}
	const maxRateLimitRetries = 3
	for attempt := 0; ; attempt++ {
		var resp *response
		resp, err = c.do(ctx, request{
			method:      http.MethodPost,
			path:        "/api/v2/shared_servers",
			token:       ownerToken,
			body:        bodyV2,
			contentType: "application/json",
			accept:      "application/json",
		})
		if err == nil {
			if resp == nil {
				return nil, nil
			}
			return resp.body, nil
		}
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusTooManyRequests || attempt >= maxRateLimitRetries {
			return nil, err
		}
		wait := ae.RetryAfter
		if wait <= 0 {
			base := 2 * time.Second
			if c.backoff > 0 && c.backoff < 50*time.Millisecond {
				base = c.backoff
			}
			wait = base << attempt
		}
		if wait > 15*time.Second {
			wait = 15 * time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// postV2Share is postV2ShareBody without the reply.
func (c *Client) postV2Share(ctx context.Context, ownerToken string, allLibraries bool, sectionIDs []int64, userID int64, usernameOrEmail string, skipFriendship bool) error {
	_, err := c.postV2ShareBody(ctx, ownerToken, allLibraries, sectionIDs, userID, usernameOrEmail, skipFriendship)
	return err
}

// ShareServer grants usernameOrEmail access to the server's libraries on plex.tv.
// If sectionIDs is empty, it sets allLibraries: true so all libraries are checked.
// If userID > 0, it passes invitedId to Plex's modern v2 sharing API with
// skipFriendship: true so existing friends immediately receive library access without
// sending a duplicate friendship invite.
// If the share already exists, it updates it so allLibraries remains granted.
// It falls back to v1 XML if v2 is unavailable.
func (c *Client) ShareServer(ctx context.Context, ownerToken, usernameOrEmail string, userID int64, sectionIDs []int64) error {
	_, err := c.ShareServerInvite(ctx, ownerToken, usernameOrEmail, userID, sectionIDs)
	return err
}

// ShareServerInvite is ShareServer that also returns the invite link plex.tv
// answered a new share with, when its reply carries one ("" otherwise). The
// link is the one Plex Web shows at the end of its share flow; plex.tv does
// not document it, so it is read from any plex.tv URL in the reply that names
// an invite.
func (c *Client) ShareServerInvite(ctx context.Context, ownerToken, usernameOrEmail string, userID int64, sectionIDs []int64) (string, error) {
	usernameOrEmail = strings.TrimSpace(usernameOrEmail)
	if userID <= 0 && usernameOrEmail != "" {
		if parsedID, err := strconv.ParseInt(usernameOrEmail, 10, 64); err == nil && parsedID > 0 {
			userID = parsedID
		}
	}
	if usernameOrEmail == "" && userID <= 0 {
		return "", errors.New("plextv: username, email or user ID required")
	}
	if c.id.ClientIdentifier == "" {
		return "", errors.New("plextv: client identifier required")
	}
	if userID <= 0 && usernameOrEmail != "" {
		resolvedID, resolvedHandle, err := c.resolveFriendByName(ctx, ownerToken, usernameOrEmail)
		if err != nil {
			return "", err
		}
		if resolvedID > 0 {
			userID = resolvedID
		}
		if resolvedHandle != "" {
			usernameOrEmail = resolvedHandle
		}
		if userID <= 0 && strings.Contains(usernameOrEmail, " ") {
			return "", fmt.Errorf("plextv: %q contains spaces and could not be matched to a Plex friend; enter their Plex username or email instead", usernameOrEmail)
		}
	}

	allLibraries := len(sectionIDs) == 0
	if allLibraries {
		sectionIDs = []int64{}
	}

	// 1. Try modern Plex v2 API
	replyV2, postV2Err := c.postV2ShareBody(ctx, ownerToken, allLibraries, sectionIDs, userID, usernameOrEmail, true)
	if postV2Err == nil {
		return inviteLink(replyV2), nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(postV2Err, ErrRateLimited) {
		return "", postV2Err
	}

	// If v2 reported already sharing (422 / 400), check existing share via v2.
	// Keep existing library grants and content restrictions; only widen access
	// when the owner explicitly passed sectionIDs.
	existing, findErr := c.findV2Share(ctx, ownerToken, usernameOrEmail, userID)
	if findErr != nil {
		return "", findErr
	}
	if existing != nil && existing.ID > 0 {
		targetAll := existing.AllLibraries
		targetSections := existing.LibrarySectionIDs
		if len(sectionIDs) > 0 {
			targetAll = false
			targetSections = sectionIDs
		} else if !targetAll && len(targetSections) == 0 {
			targetAll = true
		}
		if targetSections == nil {
			targetSections = []int64{}
		}

		updatePayload := map[string]any{
			"allLibraries":      targetAll,
			"librarySectionIds": targetSections,
		}
		if len(existing.Settings) > 0 {
			updatePayload["settings"] = existing.Settings
		} else {
			updatePayload["settings"] = map[string]any{
				"allowSync":         true,
				"allowCameraUpload": false,
				"allowChannels":     false,
				"filterMovies":      "",
				"filterTelevision":  "",
				"filterMusic":       "",
			}
		}
		updateBody, err := json.Marshal(updatePayload)
		if err == nil {
			_, postUpdateErr := c.do(ctx, request{
				method:      http.MethodPost,
				path:        "/api/v2/shared_servers/" + strconv.FormatInt(existing.ID, 10),
				token:       ownerToken,
				body:        updateBody,
				contentType: "application/json",
				accept:      "application/json",
			})
			if postUpdateErr == nil {
				return "", nil
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(postUpdateErr, ErrRateLimited) {
				return "", postUpdateErr
			}
		}
	} else if userID > 0 {
		var ae *APIError
		isAPI := errors.As(postV2Err, &ae)
		if isAPI && (ae.Status == http.StatusBadRequest || ae.Status == http.StatusUnprocessableEntity) {
			err := c.postV2Share(ctx, ownerToken, allLibraries, sectionIDs, userID, "", false)
			if err == nil {
				return "", nil
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(err, ErrRateLimited) {
				return "", err
			}
		}
		if IsEmailAddress(usernameOrEmail) && isAPI &&
			(ae.Status == http.StatusBadRequest || ae.Status == http.StatusNotFound || ae.Status == http.StatusUnprocessableEntity) {
			err := c.postV2Share(ctx, ownerToken, allLibraries, sectionIDs, 0, usernameOrEmail, true)
			if err == nil {
				return "", nil
			}
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			if errors.Is(err, ErrRateLimited) {
				return "", err
			}
		}
	}

	// 2. Fall back to v1 XML API (for test servers or legacy environments)
	v1SectionIDs := sectionIDs
	if len(v1SectionIDs) == 0 {
		var secErr error
		v1SectionIDs, secErr = c.ServerSectionIDs(ctx, ownerToken)
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(secErr, ErrRateLimited) {
			return "", secErr
		}
	}

	v1Target := usernameOrEmail
	if userID > 0 && !IsEmailAddress(v1Target) {
		v1Target = strconv.FormatInt(userID, 10)
	}

	payloadV1 := map[string]any{
		"server_id": c.id.ClientIdentifier,
		"shared_server": map[string]any{
			"invited_email":       v1Target,
			"library_section_ids": v1SectionIDs,
		},
		"sharing_settings": map[string]any{
			"allowSync":         "1",
			"allowCameraUpload": "0",
			"allowChannels":     "0",
			"filterMovies":      "",
			"filterTelevision":  "",
			"filterMusic":       "",
		},
	}
	bodyV1, err := json.Marshal(payloadV1)
	if err != nil {
		return "", fmt.Errorf("plextv: marshal v1 share payload: %w", err)
	}

	_, postV1Err := c.do(ctx, request{
		method:      http.MethodPost,
		path:        "/api/servers/" + url.PathEscape(c.id.ClientIdentifier) + "/shared_servers",
		token:       ownerToken,
		body:        bodyV1,
		contentType: "application/json",
		accept:      "application/json, application/xml",
	})
	if postV1Err == nil {
		return "", nil
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(postV1Err, ErrRateLimited) {
		return "", postV1Err
	}

	// v1 PUT fallback
	shares, listErr := c.listRawShares(ctx, ownerToken)
	if listErr != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if errors.Is(listErr, ErrRateLimited) {
			return "", listErr
		}
		return "", postV2Err
	}
	var v1ShareID int64
	for _, s := range shares {
		if userID > 0 {
			if s.userID > 0 {
				if s.userID == userID {
					v1ShareID = s.id
					break
				}
				continue
			}
			if IsEmailAddress(usernameOrEmail) && strings.EqualFold(s.email, usernameOrEmail) {
				v1ShareID = s.id
				break
			}
			continue
		}
		if usernameOrEmail != "" && (strings.EqualFold(s.username, usernameOrEmail) || strings.EqualFold(s.email, usernameOrEmail)) {
			v1ShareID = s.id
			break
		}
	}
	if v1ShareID == 0 {
		return "", postV2Err
	}

	_, putV1Err := c.do(ctx, request{
		method:      http.MethodPut,
		path:        "/api/servers/" + url.PathEscape(c.id.ClientIdentifier) + "/shared_servers/" + strconv.FormatInt(v1ShareID, 10),
		token:       ownerToken,
		body:        bodyV1,
		contentType: "application/json",
		accept:      "application/json, application/xml",
	})
	if putV1Err != nil {
		return "", fmt.Errorf("plextv: update share %d: %w", v1ShareID, putV1Err)
	}
	return "", nil
}

// SharedUser is an entry of the owner's friends/home list (v2 server users).
type SharedUser struct {
	PlexID          int64
	UUID            string
	Username        string
	Title           string
	FriendlyName    string
	Email           string
	Thumb           string
	Home            bool
	Restricted      bool
	PlexPass        bool
	PlexPassKnown   bool
	AcceptedShare   bool
	PendingShare    bool
	Friend          bool
	CommunityFriend bool
	// Servers are the machine identifiers with accepted shares for the user.
	Servers []string
	// PendingServers are the machine identifiers with pending invites for the user.
	PendingServers []string
}

// Users lists the owner's friends and home users from the observed v2 API.
func (c *Client) Users(ctx context.Context, ownerToken string) ([]SharedUser, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, host: c.serversHost(), path: "/api/v2/server/users", token: ownerToken, tokenQuery: "auth_token", accept: "application/xml"})
	if err != nil {
		return nil, err
	}
	users, err := parseSharedUsers(resp.body)
	if err != nil {
		return nil, err
	}
	for i := range users {
		users[i].AcceptedShare = true
		if c.id.ClientIdentifier != "" && !slices.Contains(users[i].Servers, c.id.ClientIdentifier) {
			users[i].Servers = append(users[i].Servers, c.id.ClientIdentifier)
		}
	}
	return users, nil
}

// AccountUsers lists all friends and Plex Home users on the owner's plex.tv
// account (GET /api/users), including users not yet shared with this server.
func (c *Client) AccountUsers(ctx context.Context, ownerToken string) ([]SharedUser, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/api/users", token: ownerToken, accept: "application/xml"})
	if err != nil {
		return nil, err
	}
	users, err := parseSharedUsers(resp.body)
	if err != nil {
		return nil, err
	}
	for i := range users {
		users[i].Friend = true
		if len(users[i].Servers) > 0 {
			users[i].AcceptedShare = true
		}
	}
	return users, nil
}

func parseSharedUsers(body []byte) ([]SharedUser, error) {
	root, err := parseXMLNode(body)
	if err != nil {
		return nil, fmt.Errorf("plextv: decode users: %w", err)
	}
	var out []SharedUser
	for _, n := range root.findAll("User") {
		u := SharedUser{
			PlexID:       n.intAttr("id"),
			UUID:         n.attr("uuid"),
			Username:     n.attr("username"),
			Title:        n.attr("title"),
			FriendlyName: n.attr("friendlyName"),
			Email:        n.attr("email"),
			Thumb:        n.attr("thumb"),
			Home:         n.boolAttr("home"),
			Restricted:   n.boolAttr("restricted"),
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
		// An account token cannot use this endpoint. Preserve the specific
		// error so the registrar can repair it instead of hiding it in a fallback.
		if serverTokenRequired(err) {
			return nil, err
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

// serverTokenRequired distinguishes a valid account credential from a revoked token.
func serverTokenRequired(err error) bool {
	var api *APIError
	return errors.As(err, &api) && api.Code == 1080
}
