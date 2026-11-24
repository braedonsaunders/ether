package admin

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// PlexExtras manages extra Plex servers: other Plex accounts the owner
// connects to this Ether, each served on the same port as the main server
// (config.PlexExtraServer). Off by default and capped.
type PlexExtras interface {
	// Status lists the servers, their viewers and any blocked server.
	Status(ctx context.Context) any
	// StartPIN begins a plex.tv sign-in for another account.
	StartPIN(ctx context.Context) (id, code, authURL string, err error)
	// PollPIN reports whether the sign-in finished; when it did, the account
	// is stored as a standby server and server describes it.
	PollPIN(ctx context.Context, id string) (done bool, server any, err error)
	// Start claims and publishes a standby server now.
	Start(ctx context.Context, id string) error
	// Rename sets a server's name; empty inherits the main server's name.
	Rename(id, name string) error
	// Remove signs a server out of plex.tv and forgets it.
	Remove(ctx context.Context, id string) error
	// Retire stops (or resumes) new invites on a server ("main" is the main
	// server), for a ban or to empty it.
	Retire(id string, retired bool) error
	// Move shares viewers on another server (to "auto", "main" or an id) and,
	// when removeOld, takes them off the servers they were on.
	Move(ctx context.Context, userIDs []int64, to string, removeOld bool) (any, error)
	// Members maps viewer ids to the Plex servers that share with them.
	Members(ctx context.Context) any
	// Invite shares the first server with room with a Plex user.
	Invite(ctx context.Context, handle string, plexID int64) (any, error)
}

// ErrPlexAccountInUse marks a sign-in with an account that already owns one
// of the servers.
var ErrPlexAccountInUse = errors.New("plex account already used")

func (h *Handler) plexExtras(w http.ResponseWriter) (PlexExtras, bool) {
	if h.deps.PlexExtras == nil {
		writeError(w, http.StatusNotImplemented, "unavailable", "Extra Plex servers are not available on this server.")
		return nil, false
	}
	return h.deps.PlexExtras, true
}

func (h *Handler) handlePlexExtras(w http.ResponseWriter, r *http.Request) {
	ps, ok := h.plexExtras(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, ps.Status(r.Context()))
}

func (h *Handler) handlePlexExtraPINStart(w http.ResponseWriter, r *http.Request) {
	ps, ok := h.plexExtras(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.TestTimeout)
	defer cancel()
	id, code, authURL, err := ps.StartPIN(ctx)
	if err != nil {
		writeError(w, http.StatusBadGateway, "plex_pin_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "code": code, "authUrl": authURL})
}

func (h *Handler) handlePlexExtraPINPoll(w http.ResponseWriter, r *http.Request) {
	ps, ok := h.plexExtras(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.TestTimeout)
	defer cancel()
	done, server, err := ps.PollPIN(ctx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, ErrPlexAccountInUse) {
			writeError(w, http.StatusConflict, "plex_account_in_use", err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, "plex_pin_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"done": done, "server": server})
}

func (h *Handler) handlePlexExtraStart(w http.ResponseWriter, r *http.Request) {
	ps, ok := h.plexExtras(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*h.deps.TestTimeout)
	defer cancel()
	if err := ps.Start(ctx, r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadGateway, "plex_server_start_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ps.Status(r.Context()))
}

func (h *Handler) handlePlexExtraRename(w http.ResponseWriter, r *http.Request) {
	ps, ok := h.plexExtras(w)
	if !ok {
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if err := ps.Rename(r.PathValue("id"), body.Name); err != nil {
		writeError(w, http.StatusBadRequest, "plex_server_rename_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ps.Status(r.Context()))
}

func (h *Handler) handlePlexExtraRemove(w http.ResponseWriter, r *http.Request) {
	ps, ok := h.plexExtras(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.deps.TestTimeout)
	defer cancel()
	if err := ps.Remove(ctx, r.PathValue("id")); err != nil {
		writeError(w, http.StatusBadGateway, "plex_server_remove_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ps.Status(r.Context()))
}

func (h *Handler) handlePlexExtraRetire(w http.ResponseWriter, r *http.Request) {
	ps, ok := h.plexExtras(w)
	if !ok {
		return
	}
	var body struct {
		Retired bool `json:"retired"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if err := ps.Retire(r.PathValue("id"), body.Retired); err != nil {
		writeError(w, http.StatusBadRequest, "plex_server_retire_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ps.Status(r.Context()))
}

func (h *Handler) handlePlexExtraMove(w http.ResponseWriter, r *http.Request) {
	ps, ok := h.plexExtras(w)
	if !ok {
		return
	}
	var body struct {
		UserIDs   []int64 `json:"userIds"`
		To        string  `json:"to"`
		RemoveOld bool    `json:"removeOld"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	if len(body.UserIDs) == 0 || len(body.UserIDs) > 1000 {
		writeError(w, http.StatusBadRequest, "invalid", "pick between 1 and 1000 viewers")
		return
	}
	if body.To == "" {
		body.To = "auto"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	results, err := ps.Move(ctx, body.UserIDs, body.To, body.RemoveOld)
	if err != nil {
		writeError(w, http.StatusBadRequest, "plex_move_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (h *Handler) handlePlexExtraMembers(w http.ResponseWriter, r *http.Request) {
	ps, ok := h.plexExtras(w)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, ps.Members(r.Context()))
}

func (h *Handler) handlePlexExtraInvite(w http.ResponseWriter, r *http.Request) {
	ps, ok := h.plexExtras(w)
	if !ok {
		return
	}
	var body struct {
		PlexID   int64  `json:"plexId"`
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", err.Error())
		return
	}
	handle := body.Username
	if handle == "" {
		handle = body.Email
	}
	if handle == "" && body.PlexID <= 0 {
		writeError(w, http.StatusBadRequest, "invalid", "a Plex username, email or id is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*h.deps.TestTimeout)
	defer cancel()
	res, err := ps.Invite(ctx, handle, body.PlexID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "plex_invite_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}
