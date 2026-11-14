package vela

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// tokenResult binds every complete token result, including refresh responses,
// to the validated access grant independently of a poll's idempotency key. Pairing can legitimately replay the
// same access token, but it must not mint another refresh family for that token.
// The result uses the existing encrypted durable replay store.
func (i *instance) tokenResult(w http.ResponseWriter, r *http.Request, access, refresh string, d frontend.Device, v *frontend.Viewer, expires time.Time) {
	key := hash("vela-grant-tokens\x00" + d.ID + "\x00" + access)
	digest := hash("vela-grant-tokens-v1")
	i.replayMu.Lock()
	if old := i.replays[key]; old != nil {
		i.replayMu.Unlock()
		select {
		case <-old.done:
		case <-r.Context().Done():
			return
		case <-i.ctx.Done():
			return
		}
		i.writeTokenResult(w, r, access, d, savedResponse{Status: old.status, Header: old.header, Body: old.body})
		return
	}
	if len(i.replays) >= 10000 {
		i.replayMu.Unlock()
		problem(w, 503, "capacity", "idempotency_capacity")
		return
	}
	entry := &replay{digest: digest, done: make(chan struct{})}
	i.replays[key] = entry
	i.replayMu.Unlock()
	out := httptest.NewRecorder()
	saved, conflict, err := i.claim(r.Context(), key, digest)
	if err == nil && !conflict {
		// Keep the canonical result at least as long as this access grant can
		// be accepted. A repeated app-password sign-in may extend that grant;
		// its next read must not create a second family after a 24h cache TTL.
		retainUntil := expires.Add(24 * time.Hour).UnixMilli()
		_, err = i.db.ExecContext(r.Context(), `UPDATE vela_replays SET expires=CASE WHEN expires<? THEN ? ELSE expires END WHERE key=?`, retainUntil, retainUntil, key)
	}
	switch {
	case errors.Is(err, errPending):
		problem(out, 409, "conflict", "mutation_outcome_unknown")
	case err != nil:
		fail(out, err)
	case conflict:
		problem(out, 409, "conflict", "idempotency_conflict")
	case saved != nil:
		for k, v := range saved.Header {
			out.Header()[k] = v
		}
		out.WriteHeader(saved.Status)
		_, _ = out.Write(saved.Body)
	default:
		if refresh == "" {
			refresh, err = i.host.Identity.IssueRefresh(r.Context(), access, d)
		}
		if err != nil {
			_ = i.host.Identity.Revoke(r.Context(), access)
			fail(out, err)
		} else {
			send(out, 200, object{"accessToken": access, "refreshToken": refresh, "expiresAt": expires.UTC().Format(time.RFC3339), "viewer": viewerWire(v)})
		}
		// Once issuance committed, preserve its encrypted result even when the
		// caller loses its connection. No token is issued again on a fresh poll key.
		saveCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		err = i.save(saveCtx, key, savedResponse{Status: out.Code, Header: out.Header(), Body: out.Body.Bytes()})
		cancel()
		if err != nil {
			out = httptest.NewRecorder()
			problem(out, 503, "transient", "mutation_outcome_unknown")
		}
	}
	entry.status = out.Code
	entry.header = out.Header().Clone()
	entry.body = append([]byte(nil), out.Body.Bytes()...)
	close(entry.done)
	i.replayMu.Lock()
	delete(i.replays, key)
	i.replayMu.Unlock()
	i.writeTokenResult(w, r, access, d, savedResponse{Status: entry.status, Header: entry.header, Body: entry.body})
}

func (i *instance) writeTokenResult(w http.ResponseWriter, r *http.Request, access string, d frontend.Device, response savedResponse) {
	if response.Status == 200 {
		if _, err := i.grantScope(r.Context(), access, d); err != nil {
			fail(w, err)
			return
		}
	}
	for k, v := range response.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(response.Status)
	_, _ = w.Write(response.Body)
}
