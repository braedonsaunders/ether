package vela

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

type liveCursor struct {
	Scope, Query, Snapshot string
	Offset                 int
	Expires                int64
}

func (i *instance) livePageScope(r *http.Request, v *frontend.Viewer, token string) (string, error) {
	family, err := i.grantScope(r.Context(), token, requestDevice(r))
	if err != nil {
		return "", err
	}
	return hashJSON([]any{i.cursorScope(r, v), family}), nil
}

func (i *instance) readLiveCursor(r *http.Request, scope, query string) (liveCursor, error) {
	c := liveCursor{Scope: scope, Query: query, Expires: time.Now().Add(10 * time.Minute).Unix()}
	token := r.URL.Query().Get("cursor")
	if token == "" {
		return c, nil
	}
	if len(token) > 4096 {
		return c, frontend.ErrLiveGuideStale
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(b) < i.seal.NonceSize() {
		return c, frontend.ErrLiveGuideStale
	}
	b, err = i.seal.Open(nil, b[:i.seal.NonceSize()], b[i.seal.NonceSize():], []byte("vela-live"))
	if err != nil || json.Unmarshal(b, &c) != nil || c.Scope != scope || c.Query != query || c.Expires <= time.Now().Unix() || c.Offset <= 0 || c.Snapshot == "" {
		return c, frontend.ErrLiveGuideStale
	}
	return c, nil
}

func (i *instance) nextLiveCursor(c liveCursor, offset int, snapshot string) (string, error) {
	c.Offset, c.Snapshot = offset, snapshot
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, i.seal.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(i.seal.Seal(nonce, nonce, b, []byte("vela-live"))), nil
}
