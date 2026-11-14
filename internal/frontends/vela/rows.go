package vela

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/frontend"
	"net/http"
	"strconv"
	"time"
)

const defaultRowItemLimit = 20

// The collection's limit counts rows; only a single-row request uses it as
// an item limit. Keep bootstrap independent of the number of shelves requested.
func rowItemLimit(r *http.Request) int {
	n := defaultRowItemLimit
	if r.PathValue("id") != "" {
		if requested, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && requested > 0 {
			n = min(requested, 50)
		}
	}
	return n
}

type rowCursor struct {
	Row, Scope string
	Exclude    []string
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
	page, e := views.ResolveRowView(r.Context(), frontend.RowViewPageRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", ID: id, Offset: offset, Limit: rowItemLimit(r)})
	if e != nil {
		return frontend.Row{}, e
	}
	kind := ""
	if page.Truncated {
		kind = "truncated"
	}
	return frontend.Row{Kind: kind, ID: id, Entries: page.Entries, Handle: page.ImpressionHandle, More: page.More}, nil
}
func (i *instance) rowToken(r *http.Request, v *frontend.Viewer, id string, offset int, exclude []string) string {
	return i.sealRowCursor(rowCursor{Row: id, Scope: i.cursorScope(r, v), Offset: offset, Exclude: exclude, Expires: time.Now().Add(10 * time.Minute).Unix()})
}

func (i *instance) sealRowCursor(c rowCursor) string {
	nonce := make([]byte, i.seal.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return ""
	}
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(i.seal.Seal(nonce, nonce, b, []byte("vela-row")))
}
func (i *instance) readRowToken(r *http.Request, v *frontend.Viewer, token string) (rowCursor, error) {
	var c rowCursor
	if len(token) > 4096 {
		return c, frontend.ErrRowViewStale
	}
	b, e := base64.RawURLEncoding.DecodeString(token)
	if e != nil || len(b) < i.seal.NonceSize() {
		return c, frontend.ErrRowViewStale
	}
	b, e = i.seal.Open(nil, b[:i.seal.NonceSize()], b[i.seal.NonceSize():], []byte("vela-row"))
	if e != nil || json.Unmarshal(b, &c) != nil || c.Expires < time.Now().Unix() || c.Scope != i.cursorScope(r, v) || c.Offset < 0 || c.Offset > 10000 || len(c.Exclude) > 6 {
		return c, frontend.ErrRowViewStale
	}
	for _, id := range c.Exclude {
		if len(id) > 256 {
			return rowCursor{}, frontend.ErrRowViewStale
		}
	}
	return c, nil
}
