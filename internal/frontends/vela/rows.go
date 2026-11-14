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
func (i *instance) rowPage(r *http.Request, v *frontend.Viewer, id string, offset, n int) (frontend.Row, error) {
	views, ok := i.host.Experience.(frontend.RowViews)
	if !ok {
		return frontend.Row{}, frontend.ErrNotAvailable
	}
	page, e := views.ResolveRowView(r.Context(), frontend.RowViewPageRequest{Viewer: v, Device: requestDevice(r), Protocol: "vela", ID: id, Offset: offset, Limit: n})
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

// backfillShelf consumes only the missing slots from the same immutable visit.
// At most six extra pages (one per possible featured family) are fetched. A
// family can contain many episodes, so hitting the bound retains honest "more"
// rather than claiming exhaustion or scanning the entire source.
func (i *instance) backfillShelf(r *http.Request, v *frontend.Viewer, token string, shelf object, wanted int) error {
	for attempt := 0; attempt < 6; attempt++ {
		items := shelf["items"].([]object)
		if len(items) >= wanted || shelf["continuation"] != "more" {
			break
		}
		cursor, err := i.readRowToken(r, v, shelf["nextCursor"].(string))
		if err != nil {
			return err
		}
		if len(cursor.Exclude) == 0 {
			break
		}
		row, err := i.rowPage(r, v, cursor.Row, cursor.Offset, wanted-len(items))
		if err != nil {
			return err
		}
		page, err := i.rowWire(r, v, token, row, cursor.Offset, cursor.Exclude, true)
		if err != nil {
			return err
		}
		shelf["items"] = append(items, page["items"].([]object)...)
		shelf["continuation"] = page["continuation"]
		delete(shelf, "nextCursor")
		if next, ok := page["nextCursor"]; ok {
			shelf["nextCursor"] = next
		}
		if len(row.Entries) == 0 {
			break // A non-progressing source must not spin.
		}
	}
	return nil
}
