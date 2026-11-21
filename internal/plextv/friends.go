package plextv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

func matchesV2Invited(u v2InvitedUser, fallbackID int64, fallbackEmail, usernameOrEmail string, userID int64) bool {
	if userID > 0 && (u.ID == userID || fallbackID == userID) {
		return true
	}
	if usernameOrEmail == "" {
		return false
	}
	return strings.EqualFold(u.Username, usernameOrEmail) ||
		strings.EqualFold(u.Email, usernameOrEmail) ||
		strings.EqualFold(fallbackEmail, usernameOrEmail) ||
		strings.EqualFold(u.Title, usernameOrEmail) ||
		strings.EqualFold(u.FriendlyName, usernameOrEmail)
}

func (c *Client) findV2Share(ctx context.Context, ownerToken, usernameOrEmail string, userID int64) (*v2SharedServer, error) {
	respAcc, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/v2/shared_servers/owned/accepted",
		token:  ownerToken,
		accept: "application/json",
	})
	if err == nil {
		var accepted []v2SharedServer
		if json.Unmarshal(respAcc.body, &accepted) == nil {
			for _, s := range accepted {
				if s.MachineIdentifier == c.id.ClientIdentifier && matchesV2Invited(s.Invited, s.InvitedID, s.InvitedEmail, usernameOrEmail, userID) {
					return &s, nil
				}
			}
		}
	}

	respPend, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/v2/shared_servers/invites/owned/pending",
		token:  ownerToken,
		accept: "application/json",
	})
	if err == nil {
		var pending []v2PendingInvite
		if json.Unmarshal(respPend.body, &pending) == nil {
			for _, inv := range pending {
				matchUser := matchesV2Invited(inv.Invited, inv.InvitedID, inv.InvitedEmail, usernameOrEmail, userID)
				for _, s := range inv.SharedServers {
					if s.MachineIdentifier == c.id.ClientIdentifier && (matchUser || matchesV2Invited(s.Invited, s.InvitedID, s.InvitedEmail, usernameOrEmail, userID)) {
						return &s, nil
					}
				}
			}
		}
	}

	return nil, nil
}

// SharedServerUsers lists invited Plex accounts from the owner's accepted and
// pending v2 server shares across all owned servers.
func (c *Client) SharedServerUsers(ctx context.Context, ownerToken string) ([]SharedUser, error) {
	byID := make(map[int64]int)
	var out []SharedUser
	merge := func(u v2InvitedUser, fallbackID int64, fallbackEmail, machineID string) {
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
		if idx, ok := byID[uid]; ok {
			dst := &out[idx]
			if dst.UUID == "" {
				dst.UUID = u.UUID
			}
			if dst.Username == "" {
				dst.Username = u.Username
			}
			if dst.Title == "" {
				dst.Title = u.Title
			}
			if dst.FriendlyName == "" {
				dst.FriendlyName = u.FriendlyName
			}
			if dst.Email == "" {
				dst.Email = email
			}
			if dst.Thumb == "" {
				dst.Thumb = u.Thumb
			}
			if machineID != "" {
				found := false
				for _, m := range dst.Servers {
					if m == machineID {
						found = true
						break
					}
				}
				if !found {
					dst.Servers = append(dst.Servers, machineID)
				}
			}
			return
		}
		var servers []string
		if machineID != "" {
			servers = []string{machineID}
		}
		byID[uid] = len(out)
		out = append(out, SharedUser{
			PlexID:       uid,
			UUID:         u.UUID,
			Username:     u.Username,
			Title:        u.Title,
			FriendlyName: u.FriendlyName,
			Email:        email,
			Thumb:        u.Thumb,
			Servers:      servers,
		})
	}

	var firstErr error
	var okCount int
	if respAcc, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/v2/shared_servers/owned/accepted",
		token:  ownerToken,
		accept: "application/json",
	}); err == nil {
		var accepted []v2SharedServer
		if derr := json.Unmarshal(respAcc.body, &accepted); derr == nil {
			okCount++
			for _, s := range accepted {
				merge(s.Invited, s.InvitedID, s.InvitedEmail, s.MachineIdentifier)
			}
		} else if firstErr == nil {
			firstErr = fmt.Errorf("plextv: decode accepted shared servers: %w", derr)
		}
	} else {
		firstErr = err
	}

	if respPend, err := c.do(ctx, request{
		method: http.MethodGet,
		path:   "/api/v2/shared_servers/invites/owned/pending",
		token:  ownerToken,
		accept: "application/json",
	}); err == nil {
		var pending []v2PendingInvite
		if derr := json.Unmarshal(respPend.body, &pending); derr == nil {
			okCount++
			for _, inv := range pending {
				if len(inv.SharedServers) == 0 {
					merge(inv.Invited, inv.InvitedID, inv.InvitedEmail, "")
					continue
				}
				for _, s := range inv.SharedServers {
					u := s.Invited
					if u.ID <= 0 {
						u = inv.Invited
					} else {
						if u.UUID == "" {
							u.UUID = inv.Invited.UUID
						}
						if u.Username == "" {
							u.Username = inv.Invited.Username
						}
						if u.Email == "" {
							u.Email = inv.Invited.Email
						}
						if u.Title == "" {
							u.Title = inv.Invited.Title
						}
						if u.FriendlyName == "" {
							u.FriendlyName = inv.Invited.FriendlyName
						}
						if u.Thumb == "" {
							u.Thumb = inv.Invited.Thumb
						}
					}
					fid := s.InvitedID
					if fid <= 0 {
						fid = inv.InvitedID
					}
					femail := s.InvitedEmail
					if femail == "" {
						femail = inv.InvitedEmail
					}
					merge(u, fid, femail, s.MachineIdentifier)
				}
			}
		} else if firstErr == nil {
			firstErr = fmt.Errorf("plextv: decode pending invites: %w", derr)
		}
	} else if firstErr == nil {
		firstErr = err
	}

	if okCount == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func (c *Client) resolveFriendByName(ctx context.Context, ownerToken, name string) (int64, string) {
	type candidate struct {
		id       int64
		username string
		titles   []string
		email    string
	}
	byID := map[int64]int{}
	byUser := map[string]int{}
	var cands []candidate

	compact := func(s string) string {
		return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
	}
	pickHandle := func(cand candidate) string {
		if em := strings.TrimSpace(cand.email); em != "" && !strings.Contains(em, " ") {
			return em
		}
		if un := strings.TrimSpace(cand.username); un != "" && !strings.Contains(un, " ") {
			return un
		}
		return ""
	}
	mergeInto := func(idx int, id int64, username, title, email string) {
		if cands[idx].id <= 0 && id > 0 {
			cands[idx].id = id
			byID[id] = idx
		}
		if cands[idx].username == "" && username != "" {
			cands[idx].username = username
			byUser[strings.ToLower(username)] = idx
		}
		if cands[idx].email == "" && email != "" {
			cands[idx].email = email
		}
		if title != "" {
			cands[idx].titles = append(cands[idx].titles, title)
		}
	}
	add := func(id int64, username, title, email string) {
		username = strings.TrimSpace(username)
		title = strings.TrimSpace(title)
		email = strings.TrimSpace(email)
		if id > 0 {
			if idx, ok := byID[id]; ok {
				mergeInto(idx, id, username, title, email)
				return
			}
		}
		if username != "" {
			if idx, ok := byUser[strings.ToLower(username)]; ok {
				mergeInto(idx, id, username, title, email)
				return
			}
		}
		if id <= 0 && username == "" {
			return
		}
		idx := len(cands)
		var titles []string
		if title != "" {
			titles = []string{title}
		}
		cands = append(cands, candidate{id: id, username: username, titles: titles, email: email})
		if id > 0 {
			byID[id] = idx
		}
		if username != "" {
			byUser[strings.ToLower(username)] = idx
		}
	}

	wantCompact := compact(name)
	match := func() (int64, string, bool) {
		var compactMatch candidate
		compactCount := 0
		for _, cand := range cands {
			if strings.EqualFold(cand.username, name) || strings.EqualFold(cand.email, name) {
				if cand.id > 0 || pickHandle(cand) != "" {
					return cand.id, pickHandle(cand), true
				}
			}
			for _, t := range cand.titles {
				if strings.EqualFold(t, name) {
					if cand.id > 0 || pickHandle(cand) != "" {
						return cand.id, pickHandle(cand), true
					}
				}
			}
			matchedCompact := compact(cand.username) == wantCompact
			if !matchedCompact {
				for _, t := range cand.titles {
					if compact(t) == wantCompact {
						matchedCompact = true
						break
					}
				}
			}
			if matchedCompact && (cand.id > 0 || pickHandle(cand) != "") {
				compactMatch = cand
				compactCount++
			}
		}
		if compactCount == 1 {
			return compactMatch.id, pickHandle(compactMatch), true
		}
		return 0, "", false
	}

	addSharedUsers := func(users []SharedUser) {
		for _, u := range users {
			if u.PlexID <= 0 {
				continue
			}
			add(u.PlexID, u.Username, u.Title, u.Email)
			if u.FriendlyName != "" && !strings.EqualFold(u.FriendlyName, u.Title) {
				add(u.PlexID, u.Username, u.FriendlyName, u.Email)
			}
		}
	}

	if users, err := c.AccountUsers(ctx, ownerToken); err == nil {
		addSharedUsers(users)
		if id, handle, ok := match(); ok {
			c.log.Debug("resolve friend by name result", "candidates", len(cands), "matched", true, "source", "account_users")
			return id, handle
		}
	}
	if users, err := c.Users(ctx, ownerToken); err == nil {
		addSharedUsers(users)
		if id, handle, ok := match(); ok {
			c.log.Debug("resolve friend by name result", "candidates", len(cands), "matched", true, "source", "server_users")
			return id, handle
		}
	}
	if users, err := c.SharedServerUsers(ctx, ownerToken); err == nil {
		addSharedUsers(users)
		if id, handle, ok := match(); ok {
			c.log.Debug("resolve friend by name result", "candidates", len(cands), "matched", true, "source", "shared_servers")
			return id, handle
		}
	}
	if homeUsers, err := c.HomeUsers(ctx, ownerToken); err == nil {
		for _, u := range homeUsers {
			if u.ID <= 0 {
				continue
			}
			add(u.ID, u.Username, u.Title, "")
			if u.Friendly != "" && !strings.EqualFold(u.Friendly, u.Title) {
				add(u.ID, u.Username, u.Friendly, "")
			}
		}
		if id, handle, ok := match(); ok {
			c.log.Debug("resolve friend by name result", "candidates", len(cands), "matched", true, "source", "home_users")
			return id, handle
		}
	}
	if friends, err := c.AccessTokens(ctx, ownerToken); err == nil {
		for _, f := range friends {
			if !f.Owned && f.PlexID > 0 {
				add(f.PlexID, f.Username, f.Title, f.Email)
			}
		}
		if id, handle, ok := match(); ok {
			c.log.Debug("resolve friend by name result", "candidates", len(cands), "matched", true, "source", "access_tokens")
			return id, handle
		}
	}
	if c.base != nil && c.base.String() == DefaultBaseURL {
		cFriends, err := (&CommunityClient{Client: c.hc, MinInterval: time.Millisecond, MaxRetries: -1}).Friends(ctx, ownerToken)
		if err != nil {
			c.log.Debug("community friends lookup failed", "err", err)
		} else {
			for _, f := range cFriends {
				id, _ := strconv.ParseInt(f.ID, 10, 64)
				add(id, f.Username, f.DisplayName, "")
			}
			if id, handle, ok := match(); ok {
				c.log.Debug("resolve friend by name result", "candidates", len(cands), "matched", true, "source", "community")
				return id, handle
			}
		}
	}
	c.log.Debug("resolve friend by name result", "candidates", len(cands), "matched", false)
	return 0, ""
}

func (c *Client) postV2Share(ctx context.Context, ownerToken string, allLibraries bool, sectionIDs []int64, userID int64, usernameOrEmail string, skipFriendship bool) error {
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
		return fmt.Errorf("plextv: marshal share payload: %w", err)
	}
	_, err = c.do(ctx, request{
		method:      http.MethodPost,
		path:        "/api/v2/shared_servers",
		token:       ownerToken,
		body:        bodyV2,
		contentType: "application/json",
		accept:      "application/json",
	})
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
	usernameOrEmail = strings.TrimSpace(usernameOrEmail)
	if userID <= 0 && usernameOrEmail != "" {
		if parsedID, err := strconv.ParseInt(usernameOrEmail, 10, 64); err == nil && parsedID > 0 {
			userID = parsedID
		}
	}
	if usernameOrEmail == "" && userID <= 0 {
		return errors.New("plextv: username, email or user ID required")
	}
	if c.id.ClientIdentifier == "" {
		return errors.New("plextv: client identifier required")
	}
	if userID <= 0 && usernameOrEmail != "" {
		resolvedID, resolvedHandle := c.resolveFriendByName(ctx, ownerToken, usernameOrEmail)
		if resolvedID > 0 {
			userID = resolvedID
		}
		if resolvedHandle != "" {
			usernameOrEmail = resolvedHandle
		}
		if userID <= 0 && strings.Contains(usernameOrEmail, " ") {
			return fmt.Errorf("plextv: %q contains spaces and could not be matched to a Plex friend; enter their Plex username or email instead", usernameOrEmail)
		}
	}

	allLibraries := len(sectionIDs) == 0
	if allLibraries {
		sectionIDs = []int64{}
	}

	// 1. Try modern Plex v2 API
	postV2Err := c.postV2Share(ctx, ownerToken, allLibraries, sectionIDs, userID, usernameOrEmail, true)
	if postV2Err == nil {
		return nil
	}

	// If v2 reported already sharing (422 / 400), check existing share via v2.
	// Keep existing library grants and content restrictions; only widen access
	// when the owner explicitly passed sectionIDs.
	existing, _ := c.findV2Share(ctx, ownerToken, usernameOrEmail, userID)
	if existing != nil && existing.ID > 0 {
		targetAll := existing.AllLibraries
		targetSections := existing.LibrarySectionIDs
		if len(sectionIDs) > 0 {
			targetAll = false
			targetSections = sectionIDs
		} else if !targetAll && len(targetSections) == 0 {
			targetAll = true
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
				return nil
			}
		}
	} else if userID > 0 {
		var ae *APIError
		isAPI := errors.As(postV2Err, &ae)
		if isAPI && (ae.Status == http.StatusBadRequest || ae.Status == http.StatusUnprocessableEntity) {
			if err := c.postV2Share(ctx, ownerToken, allLibraries, sectionIDs, userID, "", false); err == nil {
				return nil
			}
		}
		if usernameOrEmail != "" && !strings.Contains(usernameOrEmail, " ") && isAPI &&
			(ae.Status == http.StatusBadRequest || ae.Status == http.StatusNotFound || ae.Status == http.StatusUnprocessableEntity) {
			if err := c.postV2Share(ctx, ownerToken, allLibraries, sectionIDs, 0, usernameOrEmail, true); err == nil {
				return nil
			}
		}
	}

	// 2. Fall back to v1 XML API (for test servers or legacy environments)
	v1SectionIDs := sectionIDs
	if len(v1SectionIDs) == 0 {
		v1SectionIDs, _ = c.ServerSectionIDs(ctx, ownerToken)
	}

	v1Target := usernameOrEmail
	if (v1Target == "" || strings.Contains(v1Target, " ")) && userID > 0 {
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
		return fmt.Errorf("plextv: marshal v1 share payload: %w", err)
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
		return nil
	}

	// v1 PUT fallback
	shares, listErr := c.listRawShares(ctx, ownerToken)
	if listErr != nil {
		return postV2Err
	}
	var v1ShareID int64
	for _, s := range shares {
		if (usernameOrEmail != "" && (strings.EqualFold(s.username, usernameOrEmail) || strings.EqualFold(s.email, usernameOrEmail))) ||
			(userID > 0 && s.userID == userID) {
			v1ShareID = s.id
			break
		}
	}
	if v1ShareID == 0 {
		return postV2Err
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
		return fmt.Errorf("plextv: update share %d: %w", v1ShareID, putV1Err)
	}
	return nil
}

// SharedUser is an entry of the owner's friends/home list (v2 server users).
type SharedUser struct {
	PlexID        int64
	UUID          string
	Username      string
	Title         string
	FriendlyName  string
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
	return parseSharedUsers(resp.body)
}

// AccountUsers lists all friends and Plex Home users on the owner's plex.tv
// account (GET /api/users), including users not yet shared with this server.
func (c *Client) AccountUsers(ctx context.Context, ownerToken string) ([]SharedUser, error) {
	resp, err := c.do(ctx, request{method: http.MethodGet, path: "/api/users", token: ownerToken, accept: "application/xml"})
	if err != nil {
		return nil, err
	}
	return parseSharedUsers(resp.body)
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
