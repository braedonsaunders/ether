package plexwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/retryafter"
)

// PMS HTTP API used for server-to-server sync. Shapes and their provenance.
// "Recorded" is testdata/conform/plex (PMS 1.43.4). "Probed" is a read-only
// probe of a real PMS 1.43.5 on 2026-11-04; the fake server in pms_test.go
// reproduces its shapes with synthetic values.
//
//   - GET /identity → MediaContainer.machineIdentifier, version. Recorded.
//     It answers without a token, so it does not prove a token works.
//   - GET /myplex/account → MyPlex.username. Recorded and probed. Only the
//     server's admin token reads it.
//   - GET /accounts → Account[].id (number), name. Probed: the owner is id 1,
//     and an unnamed id 0 row exists that is not a person.
//   - GET /status/sessions/history/all?accountID=&sort=viewedAt:asc&viewedAt>=
//     with X-Plex-Container-Start/Size → Metadata[].ratingKey (string), type,
//     viewedAt and accountID (numbers), grandparentKey (no
//     grandparentRatingKey), index, parentIndex; no Guid array. Probed: the
//     account filter holds, "viewedAt>=N" (query key "viewedAt>") is strictly
//     greater, rows are oldest first and paging works. Item ids therefore
//     need one /library/metadata read per distinct item, cached per server.
//   - GET /library/metadata/{id}?includeGuids=1 → guid, Guid[].id (imdb://,
//     tmdb://, tvdb://), grandparentRatingKey (string), index, parentIndex,
//     viewCount, lastViewedAt, viewOffset. Recorded and probed: a finished
//     item omits viewOffset, which reads as no resume position.
//   - GET /library/sections → Directory[].key, type. Recorded.
//   - GET /library/sections/{id}/all?type=&guid=plex://…&includeGuids=1 → the
//     one item with that primary GUID; an unknown GUID gives size 0 and no
//     Metadata. Probed for a movie (type=1) and an episode (type=4); the
//     episode carries grandparentRatingKey, index and parentIndex.
//   - GET /:/scrobble, /:/unscrobble, /:/progress with key=<ratingKey>,
//     identifier=com.plexapp.plugins.library (and time= in milliseconds).
//     Writes, deliberately not probed; documented routes as python-plexapi
//     calls them. Whether a scrobble adds a history row is unconfirmed; the
//     echo guard assumes it and is harmless if it does not.

const (
	pmsIdentifier = "com.plexapp.plugins.library"
	pmsMaxBody    = 4 << 20
)

// ErrPMSToken means the Plex Media Server refused the token.
var ErrPMSToken = errors.New("plexwatch: the Plex Media Server refused this token")

// ErrPMSNotOwner means the token is valid but is not the server owner's.
var ErrPMSNotOwner = errors.New("plexwatch: this token is not the server owner's")

// ErrPMSAddress means the server address is not an http(s) origin.
var ErrPMSAddress = errors.New("plexwatch: enter the server address, like http://192.168.1.10:32400")

// ErrPMSUnreachable means the server could not be reached or did not answer as a PMS.
var ErrPMSUnreachable = errors.New("plexwatch: the Plex Media Server could not be reached")

type pmsClient struct {
	enabled  func() bool
	base     *url.URL
	http     *http.Client
	clientID string
	now      func() time.Time
}

func newPMSClient(base string, hc *http.Client, clientID string, now func() time.Time) (*pmsClient, error) {
	u, err := parsePMSURL(base)
	if err != nil {
		return nil, err
	}
	return &pmsClient{base: u, http: hc, clientID: clientID, now: now}, nil
}

// parsePMSURL accepts an http(s) origin with an optional path prefix and no
// credentials, query or fragment.
func parsePMSURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, ErrPMSAddress
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u, nil
}

// flexInt decodes PMS numbers sent either as JSON numbers or as strings.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		if v, ferr := strconv.ParseFloat(s, 64); ferr == nil {
			*f = flexInt(v)
			return nil
		}
		return err
	}
	*f = flexInt(n)
	return nil
}

type pmsMetadata struct {
	RatingKey        flexInt `json:"ratingKey"`
	Type             string  `json:"type"`
	Title            string  `json:"title"`
	GrandparentTitle string  `json:"grandparentTitle"`
	GUID             string  `json:"guid"`
	GUIDs            []struct {
		ID string `json:"id"`
	} `json:"Guid"`
	GrandparentRatingKey flexInt `json:"grandparentRatingKey"`
	GrandparentKey       string  `json:"grandparentKey"`
	Index                flexInt `json:"index"`
	ParentIndex          flexInt `json:"parentIndex"`
	ViewCount            flexInt `json:"viewCount"`
	ViewOffset           flexInt `json:"viewOffset"`
	LastViewedAt         flexInt `json:"lastViewedAt"`
	ViewedAt             flexInt `json:"viewedAt"`
	AccountID            flexInt `json:"accountID"`
}

func (m pmsMetadata) showKey() int64 {
	if m.GrandparentRatingKey > 0 {
		return int64(m.GrandparentRatingKey)
	}
	s := strings.TrimSuffix(strings.TrimPrefix(m.GrandparentKey, "/library/metadata/"), "/children")
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func (m pmsMetadata) displayTitle() string {
	if m.Type == "episode" && m.GrandparentTitle != "" {
		return fmt.Sprintf("%s S%dE%d %s", m.GrandparentTitle, m.ParentIndex, m.Index, m.Title)
	}
	return m.Title
}

type pmsContainer struct {
	MediaContainer struct {
		MachineIdentifier string        `json:"machineIdentifier"`
		Version           string        `json:"version"`
		Size              flexInt       `json:"size"`
		TotalSize         flexInt       `json:"totalSize"`
		Metadata          []pmsMetadata `json:"Metadata"`
		Directory         []struct {
			Key  string `json:"key"`
			Type string `json:"type"`
		} `json:"Directory"`
		Account []struct {
			ID   flexInt `json:"id"`
			Name string  `json:"name"`
		} `json:"Account"`
	} `json:"MediaContainer"`
	MyPlex struct {
		Username    string `json:"username"`
		SignInState string `json:"signInState"`
	} `json:"MyPlex"`
}

// get calls one PMS route. The token travels only in X-Plex-Token, never in
// the URL, so transport errors cannot leak it.
func (c *pmsClient) get(ctx context.Context, path string, q url.Values, token string, dest any) error {
	if c.enabled != nil && !c.enabled() {
		return errors.New("plexwatch: server watch sync is disabled")
	}
	u := *c.base
	u.Path = c.base.Path + path
	if q != nil {
		u.RawQuery = encodePMSQuery(q)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Plex-Product", "Ether")
	req.Header.Set("X-Plex-Client-Identifier", c.clientID)
	if token != "" {
		req.Header.Set("X-Plex-Token", token)
	}
	// PMS honours paging as headers; some versions ignore the query form.
	for _, name := range []string{"X-Plex-Container-Start", "X-Plex-Container-Size"} {
		if v := q.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrPMSUnreachable, redactURL(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, pmsMaxBody+1))
	if err != nil {
		return fmt.Errorf("%w: read", ErrPMSUnreachable)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		delay, _ := retryafter.Parse(resp.Header.Get("Retry-After"), c.now(), 0)
		return &StatusError{Status: resp.StatusCode, RetryAfter: delay, Op: "GET " + path}
	}
	if len(body) > pmsMaxBody {
		return fmt.Errorf("%w: response too large", ErrPMSUnreachable)
	}
	if dest != nil && len(bytes.TrimSpace(body)) > 0 {
		if err := json.Unmarshal(body, dest); err != nil {
			return fmt.Errorf("%w: not a Plex Media Server response", ErrPMSUnreachable)
		}
	}
	return nil
}

// encodePMSQuery keeps PMS filter operators ("viewedAt>") readable; url.Values
// would escape ">" and PMS expects it literal in the key.
func encodePMSQuery(q url.Values) string {
	var b strings.Builder
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable order keeps fakes and logs deterministic
	for _, k := range keys {
		for _, v := range q[k] {
			if b.Len() > 0 {
				b.WriteByte('&')
			}
			b.WriteString(strings.ReplaceAll(url.QueryEscape(k), "%3E", ">"))
			b.WriteByte('=')
			b.WriteString(url.QueryEscape(v))
		}
	}
	return b.String()
}

func (c *pmsClient) identity(ctx context.Context, token string) (machine, version string, err error) {
	var out pmsContainer
	if err := c.get(ctx, "/identity", nil, token, &out); err != nil {
		return "", "", err
	}
	if out.MediaContainer.MachineIdentifier == "" {
		return "", "", fmt.Errorf("%w: no machine identifier", ErrPMSUnreachable)
	}
	return out.MediaContainer.MachineIdentifier, out.MediaContainer.Version, nil
}

func (c *pmsClient) ownerAccount(ctx context.Context, token string) (string, error) {
	var out pmsContainer
	err := c.get(ctx, "/myplex/account", nil, token, &out)
	var st *StatusError
	if errors.As(err, &st) && (st.Status == http.StatusUnauthorized || st.Status == http.StatusForbidden) {
		if st.Status == http.StatusForbidden {
			return "", ErrPMSNotOwner
		}
		return "", ErrPMSToken
	}
	if err != nil {
		return "", err
	}
	return out.MyPlex.Username, nil
}

type pmsAccount struct {
	ID   int64
	Name string
}

func (c *pmsClient) accounts(ctx context.Context, token string) ([]pmsAccount, error) {
	var out pmsContainer
	if err := c.get(ctx, "/accounts", nil, token, &out); err != nil {
		return nil, err
	}
	list := make([]pmsAccount, 0, len(out.MediaContainer.Account))
	for _, a := range out.MediaContainer.Account {
		if a.ID > 0 && len(list) < pmsMaxAccounts {
			list = append(list, pmsAccount{ID: int64(a.ID), Name: a.Name})
		}
	}
	return list, nil
}

// history reads one page of an account's views after the cursor second,
// oldest first.
func (c *pmsClient) history(ctx context.Context, token string, accountID, after int64, start, size int) ([]pmsMetadata, error) {
	q := url.Values{}
	q.Set("accountID", strconv.FormatInt(accountID, 10))
	q.Set("sort", "viewedAt:asc")
	q.Set("viewedAt>", strconv.FormatInt(after, 10))
	q.Set("X-Plex-Container-Start", strconv.Itoa(start))
	q.Set("X-Plex-Container-Size", strconv.Itoa(size))
	var out pmsContainer
	if err := c.get(ctx, "/status/sessions/history/all", q, token, &out); err != nil {
		return nil, err
	}
	items := out.MediaContainer.Metadata
	if len(items) > size {
		items = items[:size]
	}
	return items, nil
}

func (c *pmsClient) metadata(ctx context.Context, token string, key int64) (pmsMetadata, error) {
	q := url.Values{}
	q.Set("includeGuids", "1")
	var out pmsContainer
	if err := c.get(ctx, "/library/metadata/"+strconv.FormatInt(key, 10), q, token, &out); err != nil {
		return pmsMetadata{}, err
	}
	if len(out.MediaContainer.Metadata) == 0 {
		return pmsMetadata{}, &StatusError{Status: http.StatusNotFound, Op: "GET /library/metadata"}
	}
	return out.MediaContainer.Metadata[0], nil
}

type pmsSection struct{ Key, Type string }

func (c *pmsClient) sections(ctx context.Context, token string) ([]pmsSection, error) {
	var out pmsContainer
	if err := c.get(ctx, "/library/sections", nil, token, &out); err != nil {
		return nil, err
	}
	var list []pmsSection
	for _, d := range out.MediaContainer.Directory {
		if (d.Type == "movie" || d.Type == "show") && d.Key != "" && len(list) < 64 {
			list = append(list, pmsSection{Key: d.Key, Type: d.Type})
		}
	}
	return list, nil
}

// byGUID asks one section for the item whose primary GUID is guid. libType is
// 1 for movies and 4 for episodes.
func (c *pmsClient) byGUID(ctx context.Context, token, section string, libType int, guid string) ([]pmsMetadata, error) {
	q := url.Values{}
	q.Set("type", strconv.Itoa(libType))
	q.Set("guid", guid)
	q.Set("includeGuids", "1")
	q.Set("X-Plex-Container-Start", "0")
	q.Set("X-Plex-Container-Size", "5")
	var out pmsContainer
	if err := c.get(ctx, "/library/sections/"+url.PathEscape(section)+"/all", q, token, &out); err != nil {
		return nil, err
	}
	return out.MediaContainer.Metadata, nil
}

func (c *pmsClient) mark(ctx context.Context, token string, key int64, watched bool) error {
	q := url.Values{}
	q.Set("key", strconv.FormatInt(key, 10))
	q.Set("identifier", pmsIdentifier)
	path := "/:/unscrobble"
	if watched {
		path = "/:/scrobble"
	}
	return c.get(ctx, path, q, token, nil)
}

func (c *pmsClient) progress(ctx context.Context, token string, key int64, offset time.Duration) error {
	q := url.Values{}
	q.Set("key", strconv.FormatInt(key, 10))
	q.Set("identifier", pmsIdentifier)
	q.Set("time", strconv.FormatInt(offset.Milliseconds(), 10))
	q.Set("state", "stopped")
	return c.get(ctx, "/:/progress", q, token, nil)
}
