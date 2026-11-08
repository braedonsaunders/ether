package admin

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/plexwatch"
)

type PlexCandidateAccount struct {
	ServerID              int64  `json:"serverId"`
	ServerName            string `json:"serverName"`
	AccountID             int64  `json:"accountId"`
	AccountName           string `json:"accountName"`
	IsOwner               bool   `json:"isOwner"`
	MatchedViewerID       int64  `json:"matchedViewerId"`
	MatchedViewerName     string `json:"matchedViewerName,omitempty"`
	MatchedViewerUsername string `json:"matchedViewerUsername,omitempty"`
	SuggestedViewerID     int64  `json:"suggestedViewerId,omitempty"`
	SuggestedViewerName   string `json:"suggestedViewerName,omitempty"`
}

type PlexCandidateServer struct {
	ServerID   int64                  `json:"serverId"`
	ServerName string                 `json:"serverName"`
	Accounts   []PlexCandidateAccount `json:"accounts"`
}

type ImportPlexViewerRequest struct {
	ServerID  int64  `json:"serverId"`
	AccountID int64  `json:"accountId"`
	Name      string `json:"name,omitempty"`
}

type MatchPlexViewerRequest struct {
	ServerID  int64 `json:"serverId"`
	AccountID int64 `json:"accountId"`
	ViewerID  int64 `json:"viewerId"`
}

func (h *Handler) viewersPlexImportRoutes(api func(pattern string, fn http.HandlerFunc)) {
	api("GET /api/viewers/plex-candidates", h.handlePlexCandidates)
	api("POST /api/viewers/import-plex", h.handleImportPlexViewer)
	api("POST /api/viewers/match-plex", h.handleMatchPlexViewer)
	api("POST /api/viewers/import-plex/all", h.handleImportAllPlexViewers)
}

func cleanDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "Plex Viewer"
	}
	r := []rune(name)
	if len(r) > 120 {
		name = strings.TrimSpace(string(r[:120]))
	}
	if name == "" {
		return "Plex Viewer"
	}
	return name
}

func findMatchingViewer(viewers []ViewerRecord, acct plexwatch.PMSAccount) *ViewerRecord {
	targetName := strings.TrimSpace(acct.Name)
	for i := range viewers {
		v := &viewers[i]
		if acct.Owner && v.IsOwner {
			return v
		}
		if !acct.Owner && acct.ID > 0 && v.PlexID > 0 && v.PlexID == acct.ID {
			return v
		}
		if targetName != "" {
			if v.Title != "" && strings.EqualFold(strings.TrimSpace(v.Title), targetName) {
				return v
			}
			if v.Username != "" && strings.EqualFold(strings.TrimSpace(v.Username), targetName) {
				return v
			}
			if v.Email != "" && strings.EqualFold(strings.TrimSpace(v.Email), targetName) {
				return v
			}
		}
	}
	return nil
}

func (h *Handler) shareViewerPlex(ctx context.Context, usernameOrEmail string, userID int64) error {
	usernameOrEmail = strings.TrimSpace(usernameOrEmail)
	if usernameOrEmail == "" && userID <= 0 {
		return nil
	}
	if h.deps.SharePlexViewer != nil {
		err := h.deps.SharePlexViewer(ctx, usernameOrEmail, userID)
		if err != nil {
			h.log.Warn("share plex library error", "account", usernameOrEmail, "userID", userID, "err", err)
		} else {
			h.log.Info("shared server libraries on plex.tv", "account", usernameOrEmail, "userID", userID)
		}
		return err
	}
	if h.deps.Config == nil {
		return nil
	}
	cfg := h.deps.Config.Get()
	token := cfg.Plex.Token
	machineID := cfg.Server.MachineIdentifier
	if token == "" || machineID == "" {
		return nil
	}
	client, err := plextv.New(plextv.Options{
		Identity:     plextv.ServerIdentity(cfg, ""),
		HTTPClient:   &http.Client{Timeout: 15 * time.Second},
		RetryBackoff: 200 * time.Millisecond,
	})
	if err != nil {
		h.log.Warn("create plextv client error", "err", err)
		return err
	}
	err = client.ShareServer(ctx, token, usernameOrEmail, userID, nil)
	if err != nil {
		h.log.Warn("share plex library error", "account", usernameOrEmail, "userID", userID, "err", err)
	} else {
		h.log.Info("shared server libraries on plex.tv", "account", usernameOrEmail, "userID", userID)
	}
	return err
}

func (h *Handler) handlePlexCandidates(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner {
		writeError(w, http.StatusForbidden, "forbidden", "owner required")
		return
	}
	if h.deps.PlexServers == nil {
		writeJSON(w, http.StatusOK, []PlexCandidateServer{})
		return
	}
	servers, err := h.deps.PlexServers.List(r.Context())
	if err != nil {
		h.plexServerError(w, err)
		return
	}

	viewers, _ := h.storedViewers(r.Context())
	viewerByID := make(map[int64]ViewerRecord, len(viewers))
	for _, v := range viewers {
		viewerByID[v.ID] = v
	}

	var result []PlexCandidateServer
	for _, srv := range servers {
		accounts, err := h.deps.PlexServers.Accounts(r.Context(), srv.ID)
		if err != nil {
			h.log.Warn("plex candidates accounts error", "server", srv.ID, "err", err)
			continue
		}
		var list []PlexCandidateAccount
		for _, acct := range accounts {
			matchedID := acct.UserID
			var (
				suggestedID     int64
				suggestedName   string
				matchedName     string
				matchedUsername string
			)

			if matchedID > 0 {
				if v, ok := viewerByID[matchedID]; ok {
					matchedName = v.Title
					if matchedName == "" {
						matchedName = v.Username
					}
					matchedUsername = v.Username
				}
			} else {
				// Pure in-memory heuristic suggestion without GET side-effects.
				if matched := findMatchingViewer(viewers, acct); matched != nil {
					suggestedID = matched.ID
					suggestedName = matched.Title
					if suggestedName == "" {
						suggestedName = matched.Username
					}
				}
			}

			list = append(list, PlexCandidateAccount{
				ServerID:              srv.ID,
				ServerName:            srv.Name,
				AccountID:             acct.ID,
				AccountName:           acct.Name,
				IsOwner:               acct.Owner,
				MatchedViewerID:       matchedID,
				MatchedViewerName:     matchedName,
				MatchedViewerUsername: matchedUsername,
				SuggestedViewerID:     suggestedID,
				SuggestedViewerName:   suggestedName,
			})
		}
		result = append(result, PlexCandidateServer{
			ServerID:   srv.ID,
			ServerName: srv.Name,
			Accounts:   list,
		})
	}
	if result == nil {
		result = []PlexCandidateServer{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *Handler) handleImportPlexViewer(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner || h.deps.PlexServers == nil || h.deps.Invites == nil || h.deps.RefreshViewers == nil {
		writeError(w, http.StatusForbidden, "forbidden", "owner and required services required")
		return
	}
	var body ImportPlexViewerRequest
	if decodeJSON(r, &body) != nil || body.ServerID <= 0 || body.AccountID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid", "invalid server or account ID")
		return
	}
	accounts, err := h.deps.PlexServers.Accounts(r.Context(), body.ServerID)
	if err != nil {
		h.plexServerError(w, err)
		return
	}
	var acct *plexwatch.PMSAccount
	for i := range accounts {
		if accounts[i].ID == body.AccountID {
			acct = &accounts[i]
			break
		}
	}
	if acct == nil {
		writeError(w, http.StatusNotFound, "not_found", "account not found on server")
		return
	}

	displayName := cleanDisplayName(body.Name)
	if displayName == "Plex Viewer" && strings.TrimSpace(acct.Name) != "" {
		displayName = cleanDisplayName(acct.Name)
	}

	viewers, _ := h.storedViewers(r.Context())

	var (
		newViewerID int64
		inviteURL   string
		direction   = 2 // Plex -> Ether
	)
	if acct.Owner {
		direction = 3 // Bidirectional for owner
	}

	// 1. Check if an existing viewer matches
	if matched := findMatchingViewer(viewers, *acct); matched != nil {
		newViewerID = matched.ID
	}

	// 2. If no match, create new viewer
	if newViewerID == 0 {
		id, secret, err := h.deps.Invites.CreateViewer(r.Context(), displayName)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "could not create viewer: "+err.Error())
			return
		}
		newViewerID = id
		inviteURL = "/ether/me#invite=" + secret
	}

	// 3. Map account in plexwatch.Servers
	_, err = h.deps.PlexServers.MapAccount(r.Context(), body.ServerID, body.AccountID, plexwatch.PMSMappingInput{
		UserID:    newViewerID,
		Direction: direction,
		Interval:  10,
	})
	if err != nil {
		h.plexServerError(w, err)
		return
	}

	// 4. Grant access on Plex.tv right away if not owner
	var sharedPlex bool
	if !acct.Owner {
		if err := h.shareViewerPlex(r.Context(), acct.Name, acct.ID); err == nil {
			sharedPlex = true
		}
	}

	_ = h.deps.RefreshViewers(r.Context())

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":         newViewerID,
		"name":       displayName,
		"inviteUrl":  inviteURL,
		"serverId":   body.ServerID,
		"accountId":  body.AccountID,
		"sharedPlex": sharedPlex,
	})
}

func (h *Handler) handleMatchPlexViewer(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner || h.deps.PlexServers == nil || h.deps.RefreshViewers == nil {
		writeError(w, http.StatusForbidden, "forbidden", "owner required")
		return
	}
	var body MatchPlexViewerRequest
	if decodeJSON(r, &body) != nil || body.ServerID <= 0 || body.AccountID <= 0 || body.ViewerID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid", "invalid IDs")
		return
	}

	accounts, err := h.deps.PlexServers.Accounts(r.Context(), body.ServerID)
	if err != nil {
		h.plexServerError(w, err)
		return
	}
	var (
		isOwner  bool
		acctName string
	)
	for _, a := range accounts {
		if a.ID == body.AccountID {
			isOwner = a.Owner
			acctName = a.Name
			break
		}
	}
	direction := 2
	if isOwner {
		direction = 3 // Bidirectional for owner
	}

	_, err = h.deps.PlexServers.MapAccount(r.Context(), body.ServerID, body.AccountID, plexwatch.PMSMappingInput{
		UserID:    body.ViewerID,
		Direction: direction,
		Interval:  10,
	})
	if err != nil {
		h.plexServerError(w, err)
		return
	}

	// Grant access on Plex.tv right away if not owner
	if !isOwner && (acctName != "" || body.AccountID > 0) {
		_ = h.shareViewerPlex(r.Context(), acctName, body.AccountID)
	}

	_ = h.deps.RefreshViewers(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *Handler) handleImportAllPlexViewers(w http.ResponseWriter, r *http.Request) {
	access, _ := RequestAccess(r.Context())
	if !access.Owner || h.deps.PlexServers == nil || h.deps.Invites == nil || h.deps.RefreshViewers == nil {
		writeError(w, http.StatusForbidden, "forbidden", "owner required")
		return
	}
	servers, err := h.deps.PlexServers.List(r.Context())
	if err != nil {
		h.plexServerError(w, err)
		return
	}

	type importedItem struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		InviteURL string `json:"inviteUrl,omitempty"`
		ServerID  int64  `json:"serverId"`
		AccountID int64  `json:"accountId"`
	}

	var imported []importedItem
	viewers, _ := h.storedViewers(r.Context())

	for _, srv := range servers {
		accounts, err := h.deps.PlexServers.Accounts(r.Context(), srv.ID)
		if err != nil {
			continue
		}
		for _, acct := range accounts {
			if acct.UserID > 0 {
				continue // already mapped
			}
			name := cleanDisplayName(acct.Name)
			var (
				vID       int64
				inviteURL string
				direction = 2
			)
			if acct.Owner {
				direction = 3
			}

			// Check for match among existing viewers
			if matched := findMatchingViewer(viewers, acct); matched != nil {
				vID = matched.ID
			} else {
				id, secret, err := h.deps.Invites.CreateViewer(r.Context(), name)
				if err != nil {
					continue
				}
				vID = id
				inviteURL = "/ether/me#invite=" + secret
				viewers = append(viewers, ViewerRecord{ID: id, Title: name})
			}

			_, err = h.deps.PlexServers.MapAccount(r.Context(), srv.ID, acct.ID, plexwatch.PMSMappingInput{
				UserID:    vID,
				Direction: direction,
				Interval:  10,
			})
			if err != nil {
				continue
			}

			// Share library access on Plex.tv
			if !acct.Owner {
				_ = h.shareViewerPlex(r.Context(), acct.Name, acct.ID)
			}

			imported = append(imported, importedItem{
				ID:        vID,
				Name:      name,
				InviteURL: inviteURL,
				ServerID:  srv.ID,
				AccountID: acct.ID,
			})
		}
	}

	_ = h.deps.RefreshViewers(r.Context())
	if imported == nil {
		imported = []importedItem{}
	}
	writeJSON(w, http.StatusOK, imported)
}
