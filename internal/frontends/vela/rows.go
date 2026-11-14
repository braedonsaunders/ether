package vela

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http"
	"time"
)

type rowCursor struct {
	Row, Scope string
	Offset     int
	Expires    int64
}

func (i *instance) cursorScope(r *http.Request, v *frontend.Viewer) string {
	return hashJSON([]any{v, requestDevice(r).ID})
}
func (i *instance) rowPage(r *http.Request, v *frontend.Viewer, id string, offset int) (frontend.Row, error) {
	views, ok := i.host.Experience.(frontend.RowViews)
	if !ok {
		return frontend.Row{}, frontend.ErrNotAvailable
	}
	page, e := views.ResolveRowView(r.Context(), frontend.RowViewPageRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", ID: id, Offset: offset, Limit: min(limit(r), 50)})
	if e != nil {
		return frontend.Row{}, e
	}
	kind := ""
	if page.Truncated {
		kind = "truncated"
	}
	return frontend.Row{Kind: kind, ID: id, Entries: page.Entries, Handle: page.ImpressionHandle, More: page.More}, nil
}
func (i *instance) rowToken(r *http.Request, v *frontend.Viewer, id string, offset int) string {
	nonce := make([]byte, i.seal.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return ""
	}
	b, _ := json.Marshal(rowCursor{Row: id, Scope: i.cursorScope(r, v), Offset: offset, Expires: time.Now().Add(10 * time.Minute).Unix()})
	return base64.RawURLEncoding.EncodeToString(i.seal.Seal(nonce, nonce, b, []byte("vela-row")))
}
func (i *instance) readRowToken(r *http.Request, v *frontend.Viewer, token string) (rowCursor, error) {
	var c rowCursor
	b, e := base64.RawURLEncoding.DecodeString(token)
	if e != nil || len(b) < i.seal.NonceSize() {
		return c, frontend.ErrRowViewStale
	}
	b, e = i.seal.Open(nil, b[:i.seal.NonceSize()], b[i.seal.NonceSize():], []byte("vela-row"))
	if e != nil || json.Unmarshal(b, &c) != nil || c.Expires < time.Now().Unix() || c.Scope != i.cursorScope(r, v) || c.Offset < 0 || c.Offset > 10000 {
		return c, frontend.ErrRowViewStale
	}
	return c, nil
}
