package sessions

import (
	"context"
	"encoding/xml"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontends/dlna"
	"github.com/braedonsaunders/ether/internal/store"
)

func TestDLNABrowseAndScopedMediaUseBroker(t *testing.T) {
	ctx := context.Background()
	db := store.OpenTest(t)
	selected := int64(1)
	active := func(_ context.Context, id int64) (*core.Viewer, error) {
		if id != selected {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: id}, nil
	}
	events := NewEventBus(nil, active)
	defer events.Close()
	pb := &fakePlayback{sessions: map[string]*core.PlaySession{}, limit: 2}
	lookup := func(_ context.Context, _ *core.Viewer, _ core.Device, key core.RatingKey) (*core.Title, error) {
		return &core.Title{Key: key}, nil
	}
	broker, err := NewBroker(BrokerDeps{Events: events, Playback: pb, Transcoder: fakeBrokerTranscoder{}, Progress: &fakeProgress{}, ActiveViewer: active, CanPlay: func(*core.Viewer, bool, time.Time) bool { return true }, BitrateLimit: func(*core.Viewer) int { return 0 }, OnTimeline: func(*core.Viewer, core.RatingKey, time.Duration, time.Duration, string) {}, Lookup: lookup, MediaLookup: lookup})
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close(ctx)
	clientIP := func(r *http.Request) (netip.Addr, bool) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return netip.Addr{}, false
		}
		ip, err := netip.ParseAddr(host)
		return ip, err == nil
	}
	lanViewer := func(_ context.Context, r *http.Request) (*core.Viewer, error) {
		ip, ok := clientIP(r)
		if !ok || !ip.IsPrivate() || selected == 0 {
			return nil, core.ErrUnauthorized
		}
		return &core.Viewer{ID: selected}, nil
	}
	lan, err := NewLANLazyStreams(db, broker, clientIP, lanViewer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	movie := frontend.Title{Key: core.MovieKey(42), Type: core.TypeMovie, Title: "Visible"}
	host := frontend.Host{LANViewer: lanViewer, LANStreams: lan, ClientIP: clientIP, Capabilities: frontendtest.Capabilities{}, Sessions: broker,
		SurfaceURL: func(context.Context, *http.Request) (string, error) { return "http://ether.local/dlna", nil },
		Experience: frontendtest.Experience{LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{Items: []frontend.Title{movie}, Total: 1}, nil
		}},
	}
	inst, err := (dlna.Frontend{MachineID: "machine"}).Mount(ctx, host)
	if err != nil {
		t.Fatal(err)
	}
	body := `<Browse><ObjectID>view:1</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><StartingIndex>0</StartingIndex><RequestedCount>20</RequestedCount></Browse>`
	req := httptest.NewRequest(http.MethodPost, "/dlna/ContentDirectory/control", strings.NewReader(body))
	req.RemoteAddr = "192.168.1.5:1234"
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, req)
	if w.Code != 200 || pb.prepares != 0 {
		t.Fatalf("browse %d opened=%d", w.Code, pb.prepares)
	}
	var envelope struct {
		Body struct {
			Response struct {
				Result string `xml:"Result"`
			} `xml:"BrowseResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	var didl struct {
		Items []struct {
			Resource string `xml:"res"`
		} `xml:"item"`
	}
	if err := xml.Unmarshal([]byte(envelope.Body.Response.Result), &didl); err != nil || len(didl.Items) != 1 {
		t.Fatalf("DIDL %q err=%v", envelope.Body.Response.Result, err)
	}
	media, err := url.Parse(didl.Items[0].Resource)
	if err != nil || !strings.HasPrefix(media.Path, "/dlna/sessions/lazy/") {
		t.Fatalf("resource %q %v", didl.Items[0].Resource, err)
	}
	call := func(method, peer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, media.String(), nil)
		r.RemoteAddr = net.JoinHostPort(peer, "1234")
		out := httptest.NewRecorder()
		inst.Handler().ServeHTTP(out, r)
		return out
	}
	if out := call(http.MethodHead, "192.168.1.5"); out.Code != 200 || pb.prepares != 0 {
		t.Fatalf("HEAD %d prepares=%d", out.Code, pb.prepares)
	}
	if out := call(http.MethodGet, "192.168.1.5"); out.Code != 200 || out.Body.String() != "source bytes" || pb.prepares != 1 {
		t.Fatalf("GET %d %q prepares=%d", out.Code, out.Body.String(), pb.prepares)
	}
	if out := call(http.MethodGet, "192.168.1.6"); out.Code != 404 {
		t.Fatalf("other peer %d", out.Code)
	}
	selected = 2
	if out := call(http.MethodGet, "192.168.1.5"); out.Code != 404 {
		t.Fatalf("viewer changed %d", out.Code)
	}
	selected = 0
	if out := call(http.MethodGet, "192.168.1.5"); out.Code != 403 {
		t.Fatalf("viewer removed %d", out.Code)
	}
}
