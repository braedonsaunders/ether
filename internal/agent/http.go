package agent

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/braedonsaunders/ether/internal/httpjson"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/agent/tools"
)

// OwnerPrincipal is the console owner principal. The mount in
// internal/app/agent.go sits behind owner auth.
func OwnerPrincipal() tools.Principal {
	return tools.Principal{IsOwner: true, Surface: "console"}
}

// Handler returns the /api/agent subtree (prefix already stripped).
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /turn", s.handleTurn)
	mux.HandleFunc("GET /threads", s.handleThreads)
	mux.HandleFunc("GET /threads/{id}/messages", s.handleMessages)
	mux.HandleFunc("DELETE /threads/{id}", s.handleDeleteThread)
	mux.HandleFunc("POST /approvals/{id}", s.handleApproval)
	mux.HandleFunc("POST /secrets/{id}", s.handleSecret)
	mux.HandleFunc("POST /turns/{id}/cancel", s.handleCancel)
	mux.HandleFunc("GET /status", s.handleStatus)
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	httpjson.Write(w, status, v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg}})
}

func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

type turnRequest struct {
	ThreadID string      `json:"threadId"`
	Prompt   string      `json:"prompt"`
	Context  TurnContext `json:"context"`
}

func (s *Service) handleTurn(w http.ResponseWriter, r *http.Request) {
	var req turnRequest
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		writeErr(w, http.StatusBadRequest, "A prompt is required.")
		return
	}
	if len(req.Prompt) > 32<<10 {
		writeErr(w, http.StatusBadRequest, "Prompt is too long.")
		return
	}
	threadID := req.ThreadID
	// Resolve/create the thread up front so the header carries its id even
	// when the stream starts immediately.
	ctx := r.Context()
	if threadID == "" {
		th, err := s.store.CreateThread(ctx, threadTitle(req.Prompt))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "Could not start the conversation.")
			return
		}
		threadID = th.ID
	} else if _, ok, err := s.store.Thread(ctx, threadID); err != nil || !ok {
		writeErr(w, http.StatusNotFound, "Conversation not found.")
		return
	}
	// Catalogue AI is an ephemeral use of the collection builder. Its turn
	// remains request-bound; remove its transcript only after RunTurn unwinds,
	// including cancellation, never while a tool is still writing messages.
	if req.Context.Route == CatalogSearchRoute && req.ThreadID == "" {
		defer func() {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := s.store.DeleteThread(cleanup, threadID); err != nil {
				s.log.Warn("catalogue search transcript cleanup failed", "err", err)
			}
		}()
	}
	streamHeaders(w)
	w.Header().Set("X-Ether-Thread-Id", threadID)
	w.WriteHeader(http.StatusOK)
	out := newSSEStream(w)
	// A model thinking for minutes must never look like a dead connection
	// to the browser or a proxy in between.
	stopBeat := out.heartbeat(s.heartbeat)
	defer stopBeat()
	run := func(ctx context.Context) {
		if _, _, err := s.RunTurn(ctx, OwnerPrincipal(), threadID, req.Prompt, req.Context, out.emit); err != nil {
			msg := "The turn failed. Details are in the server log."
			if errors.Is(err, ErrThreadBusy) {
				msg = "Ether is still working in this conversation. Send again when it finishes."
			}
			_ = out.emit(chunkError(msg))
			_ = out.emit(chunkFinish("error"))
			s.log.Warn("agent turn http failed", "thread", threadID, "err", err)
		}
	}
	if !detachedRoutes[req.Context.Route] || !s.claimDetached() {
		run(ctx)
		out.done()
		return
	}
	// A detached turn outlives its request: a reload, a proxy that drops
	// the stream or a closed tab never stops the build. It still ends at
	// the turn deadline, and the console rejoins it from the stored
	// transcript while GET /threads/{id}/messages reports running.
	out.detach()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		defer s.releaseDetached()
		run(context.WithoutCancel(ctx))
	}()
	select {
	case <-finished:
		out.done()
	case <-r.Context().Done():
	}
	out.close()
}

// detachedRoutes are console pages whose turns keep running when the
// connection that started them goes away.
var detachedRoutes = map[string]bool{DescribeCollectionRoute: true}

// maxDetachedTurns bounds detached turns running at once; past it a turn
// is tied to its request like any other.
const maxDetachedTurns = 4

func (s *Service) claimDetached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detached >= maxDetachedTurns {
		return false
	}
	s.detached++
	return true
}

func (s *Service) releaseDetached() {
	s.mu.Lock()
	s.detached--
	s.mu.Unlock()
}

// Running reports whether a turn is in flight in a conversation.
func (s *Service) Running(threadID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[threadID]
}

func (s *Service) handleThreads(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	threads, err := s.store.ListThreads(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load conversations.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"threads": threads})
}

func (s *Service) handleMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok, err := s.store.Thread(r.Context(), id); err != nil || !ok {
		writeErr(w, http.StatusNotFound, "Conversation not found.")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	msgs, hasOlder, err := s.store.Messages(r.Context(), id, r.URL.Query().Get("before"), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not load messages.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"threadId": id, "messages": msgs, "hasOlder": hasOlder, "running": s.Running(id),
	})
}

func (s *Service) handleDeleteThread(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok, err := s.store.Thread(r.Context(), id); err != nil || !ok {
		writeErr(w, http.StatusNotFound, "Conversation not found.")
		return
	}
	if err := s.store.DeleteThread(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "Could not delete the conversation.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Service) handleApproval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Decision string      `json:"decision"`
		Note     string      `json:"note"`
		Context  TurnContext `json:"context"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "Invalid request body.")
		return
	}
	if body.Decision != "approved" && body.Decision != "rejected" {
		writeErr(w, http.StatusBadRequest, "Decision must be approved or rejected.")
		return
	}
	appr, err := s.DecideApproval(r.Context(), OwnerPrincipal(), r.PathValue("id"), body.Decision, strings.TrimSpace(body.Note))
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeErr(w, http.StatusNotFound, "Approval not found.")
			return
		}
		if strings.Contains(err.Error(), "already") {
			writeErr(w, http.StatusConflict, "This approval has already been decided.")
			return
		}
		writeErr(w, http.StatusInternalServerError, "Could not save the decision.")
		s.log.Warn("agent approval failed", "id", r.PathValue("id"), "err", err)
		return
	}
	out := map[string]any{
		"approvalId": appr.ID, "status": appr.Status, "decisionNote": appr.DecisionNote, "result": appr.Result,
	}
	// The card promises the agent carries on; run that turn before replying
	// so the console's refetch shows it. A failed apply does not continue.
	if (appr.Status == "approved" || appr.Status == "rejected") && appr.Input["editorChannel"] == nil {
		continued, err := s.ContinueAfterApproval(context.WithoutCancel(r.Context()), OwnerPrincipal(), appr, body.Context)
		if err != nil {
			s.log.Warn("agent approval follow-up failed", "id", appr.ID, "err", err)
		}
		out["continued"] = continued
	}
	if appr.FailureReason != "" {
		out["failureReason"] = appr.FailureReason
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Service) handleSecret(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value string `json:"value"`
	}
	if err := decodeBody(r, &body); err != nil || body.Value == "" {
		writeErr(w, http.StatusBadRequest, "A value is required.")
		return
	}
	if err := s.SubmitSecret(r.Context(), r.PathValue("id"), body.Value); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeErr(w, http.StatusNotFound, "Secret request not found.")
			return
		}
		if strings.Contains(err.Error(), "expired") || strings.Contains(err.Error(), "already") {
			writeJSON(w, http.StatusOK, map[string]any{"status": "expired"})
			return
		}
		writeErr(w, http.StatusInternalServerError, "Could not store the secret.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "stored"})
}

func (s *Service) handleCancel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cancelled := s.CancelTurn(id)
	writeJSON(w, http.StatusOK, map[string]any{"cancelled": cancelled})
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Status())
}
