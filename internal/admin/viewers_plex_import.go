package admin

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
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

// findExactViewer checks for an unambiguous existing match:
// 1. An account already mapped to a viewer (acct.UserID > 0).
// 2. The PMS server owner matched to the owner viewer.
// 3. Exact stable PlexID match (when known).
func findExactViewer(viewers []ViewerRecord, acct plexwatch.PMSAccount) *ViewerRecord {
	// 1. Existing mapping
	if acct.UserID > 0 {
		for i := range viewers {
			if viewers[i].ID == acct.UserID {
				return &viewers[i]
			}
		}
	}
	// 2. Server owner
	if acct.Owner {
		for i := range viewers {
			if viewers[i].IsOwner {
				return &viewers[i]
			}
		}
	}
	// 3. Stable Plex ID
	if !acct.Owner && acct.ID > 0 {
		for i := range viewers {
			if viewers[i].PlexID > 0 && viewers[i].PlexID == acct.ID {
				return &viewers[i]
			}
		}
	}
	return nil
}

// suggestMatchingViewer checks in strict priority order:
// 1. Exact match (owner or mapped).
// 2. Exact stable Plex ID match across all viewers.
// 3. Name or email heuristics (title, username, email).
// Heuristics are ONLY used to populate candidate suggestions for the owner to review and confirm,
// NEVER to automatically adopt or merge viewers during import.
func suggestMatchingViewer(viewers []ViewerRecord, acct plexwatch.PMSAccount) *ViewerRecord {
	if exact := findExactViewer(viewers, acct); exact != nil {
		return exact
	}
	targetName := strings.TrimSpace(acct.Name)
	if targetName == "" {
		return nil
	}
	for i := range viewers {
		v := &viewers[i]
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
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func compactName(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), " ", ""))
}

// resolvePlexUser matches a PMS account against the owner's plex.tv friends and
// home users in strict priority order:
// 1. Exact PlexID match (when !acct.Owner && acct.ID > 0).
// 2. Unique case-insensitive Username or Email match.
// 3. Unique case-insensitive Title or FriendlyName match.
// 4. Unique space-insensitive Username, Title, or FriendlyName match.
func resolvePlexUser(acct plexwatch.PMSAccount, plexUsers []plextv.SharedUser) *plextv.SharedUser {
	if acct.Owner || len(plexUsers) == 0 {
		return nil
	}
	if acct.ID > 0 {
		for i := range plexUsers {
			if plexUsers[i].PlexID == acct.ID {
				return &plexUsers[i]
			}
		}
	}
	target := strings.TrimSpace(acct.Name)
	if target == "" {
		return nil
	}
	var byLogin []*plextv.SharedUser
	for i := range plexUsers {
		u := &plexUsers[i]
		if (u.Username != "" && strings.EqualFold(strings.TrimSpace(u.Username), target)) ||
			(u.Email != "" && strings.EqualFold(strings.TrimSpace(u.Email), target)) {
			byLogin = append(byLogin, u)
		}
	}
	if len(byLogin) == 1 {
		return byLogin[0]
	}
	if len(byLogin) > 1 {
		return nil
	}
	var byTitle []*plextv.SharedUser
	for i := range plexUsers {
		u := &plexUsers[i]
		if (u.Title != "" && strings.EqualFold(strings.TrimSpace(u.Title), target)) ||
			(u.FriendlyName != "" && strings.EqualFold(strings.TrimSpace(u.FriendlyName), target)) {
			byTitle = append(byTitle, u)
		}
	}
	if len(byTitle) == 1 {
		return byTitle[0]
	}
	if len(byTitle) > 1 {
		return nil
	}
	wantCompact := compactName(target)
	var byCompact []*plextv.SharedUser
	for i := range plexUsers {
		u := &plexUsers[i]
		if (u.Username != "" && compactName(u.Username) == wantCompact) ||
			(u.Title != "" && compactName(u.Title) == wantCompact) ||
			(u.FriendlyName != "" && compactName(u.FriendlyName) == wantCompact) {
			byCompact = append(byCompact, u)
		}
	}
	if len(byCompact) == 1 {
		return byCompact[0]
	}
	return nil
}

// minGlobalPlexAccountID guards against treating PMS local/synthetic sequential
// account IDs (e.g. owner ID 1 or small test IDs) as global plex.tv user IDs.
// Real non-owner plex.tv accounts stored in PMS /accounts carry their global plex.tv ID.
const minGlobalPlexAccountID int64 = 10000

func fallbackPMSAccountPlexID(acct plexwatch.PMSAccount) int64 {
	if acct.Owner || acct.ID < minGlobalPlexAccountID {
		return 0
	}
	return acct.ID
}

func (h *Handler) ensurePlexViewer(ctx context.Context, viewers []ViewerRecord, friend plextv.SharedUser) (viewerID int64, fromExisting bool) {
	if friend.PlexID <= 0 {
		return 0, false
	}
	for i := range viewers {
		if viewers[i].PlexID == friend.PlexID {
			return viewers[i].ID, true
		}
	}
	if h.deps.EnsurePlexFriend == nil {
		return 0, false
	}
	id, err := h.deps.EnsurePlexFriend(ctx, friend)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, false
}

func findSyncedFriendViewer(viewers []ViewerRecord, acct plexwatch.PMSAccount, resolved *plextv.SharedUser) *ViewerRecord {
	if m := findExactViewer(viewers, acct); m != nil {
		return m
	}
	if resolved != nil && resolved.PlexID > 0 {
		for i := range viewers {
			if viewers[i].PlexID == resolved.PlexID {
				return &viewers[i]
			}
		}
	}
	target := strings.TrimSpace(acct.Name)
	for i := range viewers {
		v := &viewers[i]
		if v.Origin != "friend" {
			continue
		}
		if target != "" && (strings.EqualFold(strings.TrimSpace(v.Username), target) || strings.EqualFold(strings.TrimSpace(v.Email), target)) {
			return v
		}
		if resolved != nil && ((resolved.Username != "" && strings.EqualFold(strings.TrimSpace(v.Username), strings.TrimSpace(resolved.Username))) ||
			(resolved.Email != "" && strings.EqualFold(strings.TrimSpace(v.Email), strings.TrimSpace(resolved.Email)))) {
			return v
		}
	}
	if target != "" {
		var byTitle []*ViewerRecord
		for i := range viewers {
			v := &viewers[i]
			if v.Origin == "friend" && v.Title != "" && strings.EqualFold(strings.TrimSpace(v.Title), target) {
				byTitle = append(byTitle, v)
			}
		}
		if len(byTitle) == 1 {
			return byTitle[0]
		}
	}
	return nil
}

func (h *Handler) listPlexUsers(ctx context.Context) []plextv.SharedUser {
	if h.deps.ListPlexUsers == nil {
		return nil
	}
	users, err := h.deps.ListPlexUsers(ctx)
	if err != nil {
		h.log.Warn("list plex.tv users error", "err", err)
		return nil
	}
	return users
}

func (h *Handler) shareViewerPlex(ctx context.Context, usernameOrEmail string, userID int64, batch bool) error {
	return h.shareViewerPlexTarget(ctx, usernameOrEmail, userID, batch)
}

func (h *Handler) shareViewerPlexViewer(ctx context.Context, v *ViewerRecord, fallbackName string, batch bool) error {
	if v == nil {
		return h.shareViewerPlexTarget(ctx, fallbackName, 0, batch)
	}
	target := firstNonEmpty(v.Email, v.Username, v.Title, fallbackName)
	return h.shareViewerPlexTarget(ctx, target, v.PlexID, batch)
}

func (h *Handler) shareViewerPlexTarget(ctx context.Context, usernameOrEmail string, userID int64, batch bool) error {
	usernameOrEmail = strings.TrimSpace(usernameOrEmail)
	if usernameOrEmail == "" && userID <= 0 {
		return errors.New("plextv: username, email or user ID required")
	}
	shareFn := h.deps.SharePlexViewer
	if batch && h.deps.SharePlexViewerNoSync != nil {
		shareFn = h.deps.SharePlexViewerNoSync
	}
	if shareFn != nil {
		err := shareFn(ctx, usernameOrEmail, userID)
		if err != nil {
			h.log.Warn("share plex library error", "account", usernameOrEmail, "userId", userID, "err", err)
		} else {
			h.log.Info("shared server libraries on plex.tv", "account", usernameOrEmail, "userId", userID)
		}
		return err
	}
	if h.deps.Config == nil {
		return errors.New("plextv: config dependency not available")
	}
	cfg := h.deps.Config.Get()
	token := cfg.Plex.Token
	machineID := cfg.Server.MachineIdentifier
	if token == "" {
		return errors.New("plextv: plex token not configured")
	}
	if machineID == "" {
		return errors.New("plextv: server machine identifier not configured")
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
		h.log.Warn("share plex library error", "account", usernameOrEmail, "userId", userID, "err", err)
	} else {
		h.log.Info("shared server libraries on plex.tv", "account", usernameOrEmail, "userId", userID)
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
				if suggested := suggestMatchingViewer(viewers, acct); suggested != nil {
					suggestedID = suggested.ID
					suggestedName = suggested.Title
					if suggestedName == "" {
						suggestedName = suggested.Username
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
		status      = "imported"
		sharedPlex  bool
		shareErr    string
	)
	if acct.Owner {
		direction = 3 // Bidirectional for owner
	}

	var (
		matched  *ViewerRecord
		resolved *plextv.SharedUser
	)
	// 1. Check if an existing viewer matches EXACTLY (mapping, owner, stable Plex ID)
	if m := findExactViewer(viewers, *acct); m != nil {
		matched = m
		newViewerID = m.ID
		status = "matched"
	}

	// 2. If no exact match and not owner, resolve against Plex.tv users (when available)
	// and attempt to grant library access on Plex.tv first.
	// Sharing on Plex.tv synchronizes authoritative Plex friend membership into auth_users,
	// creating/updating the Plex user record with their global Plex ID before we fall back
	// to creating a local tokenless viewer.
	if newViewerID == 0 && !acct.Owner {
		resolved = resolvePlexUser(*acct, h.listPlexUsers(r.Context()))
		shareTarget := acct.Name
		shareUserID := fallbackPMSAccountPlexID(*acct)
		if resolved != nil {
			shareUserID = resolved.PlexID
			shareTarget = firstNonEmpty(resolved.Email, resolved.Username, acct.Name)
		}
		if shareUserID > 0 || strings.TrimSpace(shareTarget) != "" {
			if err := h.shareViewerPlex(r.Context(), shareTarget, shareUserID, false); err != nil {
				shareErr = err.Error()
			} else {
				sharedPlex = true
				if reloaded, err := h.storedViewers(r.Context()); err == nil {
					viewers = reloaded
					if synced := findSyncedFriendViewer(viewers, *acct, resolved); synced != nil {
						newViewerID = synced.ID
						status = "imported"
					}
				}
				if newViewerID == 0 && resolved == nil {
					resolved = resolvePlexUser(*acct, h.listPlexUsers(r.Context()))
				}
			}
		}
		// If resolved on Plex.tv (or verified by a successful share via global PMS ID)
		// but not yet in auth_users, create/update the friend viewer with full Plex profile
		// instead of a blank local invite.
		if newViewerID == 0 && resolved != nil {
			friend := *resolved
			if strings.TrimSpace(friend.Title) == "" {
				friend.Title = strings.TrimSpace(friend.FriendlyName)
			}
			if id, _ := h.ensurePlexViewer(r.Context(), viewers, friend); id > 0 {
				newViewerID = id
				status = "imported"
			}
		}
		if newViewerID == 0 && sharedPlex && shareUserID > 0 {
			if id, _ := h.ensurePlexViewer(r.Context(), viewers, plextv.SharedUser{
				PlexID: shareUserID,
				Title:  displayName,
			}); id > 0 {
				newViewerID = id
				status = "imported"
			}
		}
	}

	// 3. If still no viewer ID (e.g. local managed user without Plex.tv account, share failure,
	// or offline/test environment), create a local passwordless viewer via invite.
	if newViewerID == 0 {
		id, secret, err := h.deps.Invites.CreateViewer(r.Context(), displayName)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid", "could not create viewer: "+err.Error())
			return
		}
		newViewerID = id
		inviteURL = h.inviteURL(secret)
	}

	// 4. Map account in plexwatch.Servers
	_, err = h.deps.PlexServers.MapAccount(r.Context(), body.ServerID, body.AccountID, plexwatch.PMSMappingInput{
		UserID:    newViewerID,
		Direction: direction,
		Interval:  10,
	})
	if err != nil {
		h.plexServerError(w, err)
		return
	}

	// 5. If matched in step 1 and not yet shared on Plex.tv, share library access now.
	// Only share if the matched viewer is a Plex-authenticated user (PlexID > 0).
	// For local viewers (PlexID == 0), sharing on Plex.tv would sync a duplicate friend into auth_users.
	if !acct.Owner && (matched != nil && matched.PlexID > 0) && !sharedPlex && shareErr == "" {
		if err := h.shareViewerPlexViewer(r.Context(), matched, acct.Name, false); err != nil {
			shareErr = err.Error()
		} else {
			sharedPlex = true
		}
	}

	if err := h.deps.RefreshViewers(r.Context()); err != nil {
		h.log.Warn("refresh viewers error", "err", err)
	}

	res := map[string]any{
		"id":         newViewerID,
		"name":       displayName,
		"inviteUrl":  inviteURL,
		"serverId":   body.ServerID,
		"accountId":  body.AccountID,
		"status":     status,
		"sharedPlex": sharedPlex,
	}
	if shareErr != "" {
		res["shareError"] = shareErr
	}
	writeJSON(w, http.StatusCreated, res)
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
		found    bool
	)
	for _, a := range accounts {
		if a.ID == body.AccountID {
			isOwner = a.Owner
			acctName = a.Name
			found = true
			break
		}
	}
	if !found {
		writeError(w, http.StatusNotFound, "not_found", "account not found on server")
		return
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

	// Grant access on Plex.tv if not owner and the viewer is a Plex-authenticated user (PlexID > 0).
	// For local viewers (PlexID == 0), sharing on Plex.tv would sync a duplicate friend into auth_users.
	var (
		sharedPlex bool
		shareErr   string
	)
	var matchedViewer *ViewerRecord
	if viewers, err := h.storedViewers(r.Context()); err == nil {
		for i := range viewers {
			if viewers[i].ID == body.ViewerID {
				matchedViewer = &viewers[i]
				break
			}
		}
	}
	if !isOwner && matchedViewer != nil && matchedViewer.PlexID > 0 {
		if err := h.shareViewerPlexViewer(r.Context(), matchedViewer, acctName, false); err != nil {
			shareErr = err.Error()
		} else {
			sharedPlex = true
		}
	}

	if err := h.deps.RefreshViewers(r.Context()); err != nil {
		h.log.Warn("refresh viewers error", "err", err)
	}
	res := map[string]any{
		"ok":         true,
		"viewerId":   body.ViewerID,
		"serverId":   body.ServerID,
		"accountId":  body.AccountID,
		"sharedPlex": sharedPlex,
	}
	if shareErr != "" {
		res["shareError"] = shareErr
	}
	writeJSON(w, http.StatusOK, res)
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

	type bulkImportResultItem struct {
		ID         int64  `json:"id,omitempty"`
		Name       string `json:"name"`
		InviteURL  string `json:"inviteUrl,omitempty"`
		ServerID   int64  `json:"serverId"`
		AccountID  int64  `json:"accountId"`
		Status     string `json:"status"` // "imported", "matched", "skipped", "failed"
		Reason     string `json:"reason,omitempty"`
		SharedPlex bool   `json:"sharedPlex"`
		ShareError string `json:"shareError,omitempty"`
		Error      string `json:"error,omitempty"`
	}

	var results []bulkImportResultItem
	viewers, _ := h.storedViewers(r.Context())
	plexUsers := h.listPlexUsers(r.Context())
	sharedAny := false

	for _, srv := range servers {
		accounts, err := h.deps.PlexServers.Accounts(r.Context(), srv.ID)
		if err != nil {
			h.log.Warn("list server accounts error during bulk import", "serverId", srv.ID, "err", err)
			continue
		}
		for _, acct := range accounts {
			name := cleanDisplayName(acct.Name)
			if acct.UserID > 0 {
				results = append(results, bulkImportResultItem{
					ID:        acct.UserID,
					Name:      name,
					ServerID:  srv.ID,
					AccountID: acct.ID,
					Status:    "skipped",
					Reason:    "already_mapped",
				})
				continue
			}

			var (
				vID        int64
				inviteURL  string
				status     = "imported"
				direction  = 2
				sharedPlex bool
				shareErr   string
			)
			if acct.Owner {
				direction = 3
			}

			var (
				matched  *ViewerRecord
				resolved *plextv.SharedUser
			)
			// 1. Check for EXACT match among existing viewers (mapping, owner, stable Plex ID).
			if m := findExactViewer(viewers, acct); m != nil {
				matched = m
				vID = m.ID
				status = "matched"
			}

			// 2. If not matched and not owner, resolve against Plex.tv users and grant library access on Plex.tv.
			if vID == 0 && !acct.Owner {
				resolved = resolvePlexUser(acct, plexUsers)
				shareTarget := acct.Name
				shareUserID := fallbackPMSAccountPlexID(acct)
				if resolved != nil {
					shareUserID = resolved.PlexID
					shareTarget = firstNonEmpty(resolved.Email, resolved.Username, acct.Name)
				}
				if shareUserID > 0 || strings.TrimSpace(shareTarget) != "" {
					if err := h.shareViewerPlex(r.Context(), shareTarget, shareUserID, true); err != nil {
						shareErr = err.Error()
					} else {
						sharedPlex = true
						sharedAny = true
						// When using a legacy single-call SharePlexViewer hook (e.g. tests),
						// check ViewerDirectory for a newly synced friend row.
						if h.deps.SharePlexViewerNoSync == nil {
							if reloaded, err := h.storedViewers(r.Context()); err == nil {
								viewers = reloaded
								if synced := findSyncedFriendViewer(viewers, acct, resolved); synced != nil {
									vID = synced.ID
									status = "imported"
								}
							}
						}
					}
				}
				if vID == 0 && resolved == nil && sharedPlex {
					plexUsers = h.listPlexUsers(r.Context())
					resolved = resolvePlexUser(acct, plexUsers)
				}
				if vID == 0 && resolved != nil {
					friend := *resolved
					if strings.TrimSpace(friend.Title) == "" {
						friend.Title = strings.TrimSpace(friend.FriendlyName)
					}
					if id, existed := h.ensurePlexViewer(r.Context(), viewers, friend); id > 0 {
						vID = id
						status = "imported"
						if !existed {
							viewers = append(viewers, ViewerRecord{
								User: core.User{
									ID:       id,
									PlexID:   resolved.PlexID,
									Username: resolved.Username,
									Title:    firstNonEmpty(resolved.Title, resolved.FriendlyName, resolved.Username, name),
									Email:    resolved.Email,
								},
								Origin:  "friend",
								HasPlex: true,
							})
						}
					}
				}
				if vID == 0 && sharedPlex && shareUserID > 0 {
					if id, existed := h.ensurePlexViewer(r.Context(), viewers, plextv.SharedUser{
						PlexID: shareUserID,
						Title:  name,
					}); id > 0 {
						vID = id
						status = "imported"
						if !existed {
							viewers = append(viewers, ViewerRecord{
								User: core.User{
									ID:     id,
									PlexID: shareUserID,
									Title:  name,
								},
								Origin:  "friend",
								HasPlex: true,
							})
						}
					}
				}
			}

			// 3. Fall back to local invite viewer if not matched/resolved.
			if vID == 0 {
				id, secret, err := h.deps.Invites.CreateViewer(r.Context(), name)
				if err != nil {
					results = append(results, bulkImportResultItem{
						Name:      name,
						ServerID:  srv.ID,
						AccountID: acct.ID,
						Status:    "failed",
						Error:     "could not create viewer: " + err.Error(),
					})
					continue
				}
				vID = id
				inviteURL = h.inviteURL(secret)
				viewers = append(viewers, ViewerRecord{User: core.User{ID: id, Title: name}})
			}

			_, err = h.deps.PlexServers.MapAccount(r.Context(), srv.ID, acct.ID, plexwatch.PMSMappingInput{
				UserID:    vID,
				Direction: direction,
				Interval:  10,
			})
			if err != nil {
				results = append(results, bulkImportResultItem{
					ID:        vID,
					Name:      name,
					ServerID:  srv.ID,
					AccountID: acct.ID,
					Status:    "failed",
					Error:     "map account failed: " + err.Error(),
				})
				continue
			}

			// 4. If matched in step 1 and not yet shared on Plex.tv, share now.
			// Only share if the matched viewer is a Plex-authenticated user (PlexID > 0).
			// For local viewers (PlexID == 0), sharing on Plex.tv would sync a duplicate friend into auth_users.
			if !acct.Owner && (matched != nil && matched.PlexID > 0) && !sharedPlex && shareErr == "" {
				if err := h.shareViewerPlexViewer(r.Context(), matched, acct.Name, true); err != nil {
					shareErr = err.Error()
				} else {
					sharedPlex = true
					sharedAny = true
				}
			}

			results = append(results, bulkImportResultItem{
				ID:         vID,
				Name:       name,
				InviteURL:  inviteURL,
				ServerID:   srv.ID,
				AccountID:  acct.ID,
				Status:     status,
				SharedPlex: sharedPlex,
				ShareError: shareErr,
			})
		}
	}

	if sharedAny && h.deps.SyncPlexFriends != nil {
		if err := h.deps.SyncPlexFriends(r.Context()); err != nil {
			h.log.Warn("sync plex friends error after bulk import", "err", err)
		}
	}

	if err := h.deps.RefreshViewers(r.Context()); err != nil {
		h.log.Warn("refresh viewers error after bulk import", "err", err)
	}
	if results == nil {
		results = []bulkImportResultItem{}
	}
	writeJSON(w, http.StatusOK, results)
}
