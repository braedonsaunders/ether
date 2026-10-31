package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/remote"
)

func (h *Handler) handleOutboundCheck(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_json", "could not read request body")
		return
	}
	cfg := h.deps.Config.Get()
	o := cfg.Outbound
	if len(bytes.TrimSpace(body)) > 0 && string(bytes.TrimSpace(body)) != "null" {
		var posted config.Outbound
		if err := json.Unmarshal(body, &posted); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json", "could not read the proxy settings")
			return
		}
		if isRedacted(posted.Password) && strings.TrimSpace(posted.URL) == strings.TrimSpace(cfg.Outbound.URL) {
			posted.Password = cfg.Outbound.Password
		} else if isRedacted(posted.Password) {
			posted.Password = ""
		}
		o = posted
	}
	if probs := o.Problems(); len(probs) > 0 && (o.Enabled || strings.TrimSpace(o.URL) != "") {
		errs := make([]FieldError, len(probs))
		for i, p := range probs {
			errs[i] = FieldError{Field: p.Field, Message: p.Message}
		}
		writeValidationError(w, errs)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	res := remote.CheckEgress(ctx, egress.PolicyFrom(o), nil)
	res.RemoteAccess = cfg.Plex.RemoteAccess
	if host := strings.TrimSpace(cfg.Plex.PublicHost); host != "" {
		res.PublishedIP = host
		res.PublishedError = ""
	}
	if o.Password != "" {
		res.SourceError = strings.ReplaceAll(res.SourceError, o.Password, "")
		res.PublishedError = strings.ReplaceAll(res.PublishedError, o.Password, "")
	}
	writeJSON(w, http.StatusOK, res)
}
