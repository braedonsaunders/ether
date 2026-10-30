package dlna

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
)

func TestLANAndContentDirectoryBrowse(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	var seen []string
	host := frontend.Host{ClientSeen: func(id string) { seen = append(seen, id) }, LANViewer: func(_ context.Context, r *http.Request) (*frontend.Viewer, error) {
		if r.Header.Get("X-Remote") == "1" {
			return nil, errors.New("remote")
		}
		return viewer, nil
	}, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.5"), true }, Sessions: frontendtest.Sessions{}, Experience: frontendtest.Experience{ViewsFunc: func(_ context.Context, v *frontend.Viewer) ([]frontend.View, error) {
		if v.ID != 7 {
			t.Fatal("wrong viewer")
		}
		return []frontend.View{{ID: "1", Name: "Movies & TV", Type: "movie"}}, nil
	}}}
	inst, err := (Frontend{MachineID: "machine"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/dlna/description.xml", nil)
	req.Header.Set("X-Remote", "1")
	inst.Handler().ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatalf("remote description %d", w.Code)
	}
	if len(seen) != 0 {
		t.Fatal("unauthorized peer counted")
	}
	query := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><u:Browse xmlns:u="urn:schemas-upnp-org:service:ContentDirectory:1"><ObjectID>0</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><StartingIndex>0</StartingIndex><RequestedCount>20</RequestedCount></u:Browse></s:Body></s:Envelope>`
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/dlna/ContentDirectory/control", strings.NewReader(query))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	inst.Handler().ServeHTTP(w, req)
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "dlna-") {
		t.Fatalf("clients: %+v", seen)
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Movies &amp;amp; TV") || !strings.Contains(w.Body.String(), "NumberReturned&gt;") && !strings.Contains(w.Body.String(), "<NumberReturned>1</NumberReturned>") {
		t.Fatalf("browse %d %s", w.Code, w.Body.String())
	}
	decoder := xml.NewDecoder(strings.NewReader(w.Body.String()))
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("invalid SOAP XML", err)
		}
	}
}

func TestSSDPSearchTargets(t *testing.T) {
	probe := "M-SEARCH * HTTP/1.1\r\nMAN: \"ssdp:discover\"\r\nST: urn:schemas-upnp-org:device:MediaServer:1\r\n\r\n"
	targets := searchTargets(probe, "fixture")
	if len(targets) != 1 || targets[0] != "urn:schemas-upnp-org:device:MediaServer:1" {
		t.Fatal(targets)
	}
	if got := searchTargets(strings.Replace(probe, "MediaServer", "Other", 1), "fixture"); len(got) != 0 {
		t.Fatal(got)
	}
	all := searchTargets(strings.Replace(probe, "urn:schemas-upnp-org:device:MediaServer:1", "ssdp:all", 1), "fixture")
	if len(all) != 5 || all[1] != "uuid:fixture" {
		t.Fatal(all)
	}
	one := searchTargets(strings.Replace(probe, "urn:schemas-upnp-org:device:MediaServer:1", "UUID:FIXTURE", 1), "fixture")
	if len(one) != 1 || one[0] != "uuid:fixture" {
		t.Fatal(one)
	}
}

func TestServiceDescriptionsDeclareActionArguments(t *testing.T) {
	for _, tc := range []struct {
		name     string
		value    serviceDescription
		expected map[string]int
	}{
		{"ContentDirectory", contentService(), map[string]int{"Browse": 10, "Search": 10, "GetSearchCapabilities": 1, "GetSortCapabilities": 1, "GetSystemUpdateID": 1}},
		{"ConnectionManager", connectionService(), map[string]int{"GetProtocolInfo": 2, "GetCurrentConnectionIDs": 1, "GetCurrentConnectionInfo": 8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			writeService(w, tc.value)
			if w.Code != http.StatusOK {
				t.Fatalf("description: %d %s", w.Code, w.Body.String())
			}
			var parsed serviceDescription
			if err := xml.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
				t.Fatal(err)
			}
			states := make(map[string]bool)
			for _, v := range parsed.Variables {
				if v.Name == "" || v.Type == "" {
					t.Fatalf("invalid state: %+v", v)
				}
				states[v.Name] = true
			}
			for _, action := range parsed.Actions {
				count, ok := tc.expected[action.Name]
				if !ok || len(action.Arguments) != count {
					t.Fatalf("unexpected action: %+v", action)
				}
				for _, argument := range action.Arguments {
					if !states[argument.Variable] || (argument.Direction != "in" && argument.Direction != "out") {
						t.Fatalf("invalid argument: %+v", argument)
					}
				}
			}
			if len(parsed.Actions) != len(tc.expected) {
				t.Fatalf("missing actions: %+v", parsed.Actions)
			}
		})
	}
}

func TestContentDirectoryRejectsUnsupportedSortAndMismatchedBody(t *testing.T) {
	host := frontend.Host{LANViewer: func(context.Context, *http.Request) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 1}, nil }, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.5"), true }, Experience: frontendtest.Experience{}, Sessions: frontendtest.Sessions{}}
	inst, err := (Frontend{MachineID: "machine"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body string
		code string
	}{
		{`<Browse><ObjectID>0</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><SortCriteria>+dc:title</SortCriteria></Browse>`, "709"},
		{`<Search><ContainerID>0</ContainerID></Search>`, "402"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/dlna/ContentDirectory/control", strings.NewReader(tc.body))
		req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
		rec := httptest.NewRecorder()
		inst.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "<errorCode>"+tc.code+"</errorCode>") {
			t.Fatalf("unexpected response: %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestSearchViewRejectsIncompleteLibrary(t *testing.T) {
	experience := frontendtest.Experience{LibraryItemsFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, _ string, q frontend.ItemQuery) (frontend.Page, error) {
		if q.Offset == 0 {
			return frontend.Page{Items: []frontend.Title{{Title: "match"}}, Total: 2}, nil
		}
		return frontend.Page{Total: 2}, nil
	}}
	i := &instance{host: frontend.Host{Experience: experience}}
	_, err := i.searchView(httptest.NewRequest("POST", "/", nil), &frontend.Viewer{ID: 7}, frontend.Device{}, "movies", "match", frontend.ItemQuery{Limit: 10})
	if err == nil {
		t.Fatal("incomplete listing was reported as complete")
	}
}

func TestSearchStaysInSelectedLibraryAndPagesAfterMatching(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	host := frontend.Host{LANViewer: func(context.Context, *http.Request) (*frontend.Viewer, error) { return viewer, nil }, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.5"), true }, Sessions: frontendtest.Sessions{}, Experience: frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "one", Type: "movie"}, {ID: "two", Type: "movie"}}, nil
		},
		SearchFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.SearchPage, error) {
			t.Fatal("scoped search escaped its library")
			return frontend.SearchPage{}, nil
		},
		LibraryItemsFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, id string, q frontend.ItemQuery) (frontend.Page, error) {
			if id != "one" || q.Offset != 0 {
				t.Fatalf("wrong library: %s %+v", id, q)
			}
			return frontend.Page{Items: []frontend.Title{{Key: 1, Title: "Match one", Type: "movie"}, {Key: 2, Title: "Other", Type: "movie"}, {Key: 3, Title: "Match two", Type: "movie"}}, Total: 3}, nil
		},
	}}
	inst, err := (Frontend{MachineID: "machine"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	body := `<Search><ContainerID>view:one</ContainerID><SearchCriteria>dc:title contains "match"</SearchCriteria><StartingIndex>1</StartingIndex><RequestedCount>1</RequestedCount></Search>`
	req := httptest.NewRequest(http.MethodPost, "/dlna/ContentDirectory/control", strings.NewReader(body))
	req.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Search"`)
	rec := httptest.NewRecorder()
	inst.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Match two") || strings.Contains(rec.Body.String(), "Match one") || !strings.Contains(rec.Body.String(), "<TotalMatches>2</TotalMatches>") {
		t.Fatalf("search: %d %s", rec.Code, rec.Body.String())
	}
}

func TestAdvertisementsFollowSelectedLANViewer(t *testing.T) {
	enabled := false
	var sent []bool
	s := ssdp{host: frontend.Host{
		LANViewer: func(_ context.Context, r *http.Request) (*frontend.Viewer, error) {
			if !strings.HasPrefix(r.RemoteAddr, "192.168.1.8:") {
				t.Fatalf("unverified interface %q", r.RemoteAddr)
			}
			if !enabled {
				return nil, errors.New("no viewer")
			}
			return &frontend.Viewer{ID: 7}, nil
		},
		SurfaceURL: func(context.Context, *http.Request) (string, error) { return "http://192.168.1.8:32400/dlna", nil },
	}, addresses: func() ([]net.Addr, error) {
		_, private, _ := net.ParseCIDR("192.168.1.8/24")
		private.IP = net.ParseIP("192.168.1.8")
		_, public, _ := net.ParseCIDR("203.0.113.1/24")
		public.IP = net.ParseIP("203.0.113.1")
		return []net.Addr{private, public}, nil
	}, emit: func(item advertisement, alive bool) {
		if item.location != "http://192.168.1.8:32400/dlna/description.xml" {
			t.Fatal(item.location)
		}
		sent = append(sent, alive)
	}}
	s.refreshAdvertisements(context.Background())
	if len(sent) != 0 {
		t.Fatal("announced without selected viewer")
	}
	enabled = true
	s.refreshAdvertisements(context.Background())
	if len(sent) != 1 || !sent[0] {
		t.Fatalf("missing alive: %+v", sent)
	}
	enabled = false
	s.refreshAdvertisements(context.Background())
	if len(sent) != 2 || sent[1] {
		t.Fatalf("missing withdrawal: %+v", sent)
	}
}

func TestNotificationPacketsAdvertiseAndWithdrawEveryTarget(t *testing.T) {
	alive := notificationPackets("fixture", "http://192.168.1.8/dlna/description.xml", true)
	bye := notificationPackets("fixture", "", false)
	if len(alive) != 5 || len(bye) != len(alive) {
		t.Fatal(len(alive), len(bye))
	}
	for j := range alive {
		for _, header := range []string{"NOTIFY * HTTP/1.1", "HOST: 239.255.255.250:1900", "NTS: ssdp:alive", "LOCATION: http://192.168.1.8/dlna/description.xml", "USN:"} {
			if !strings.Contains(alive[j], header) {
				t.Fatalf("alive missing %q: %q", header, alive[j])
			}
		}
		if !strings.Contains(bye[j], "NTS: ssdp:byebye") || strings.Contains(bye[j], "LOCATION:") || !strings.HasSuffix(bye[j], "\r\n\r\n") {
			t.Fatalf("invalid withdrawal: %q", bye[j])
		}
	}
}

type lanStreamFixture struct {
	issued int
	served int
}

func (f *lanStreamFixture) IssueLAN(_ context.Context, _ *http.Request, req frontend.PlanRequest) (string, error) {
	if req.Viewer == nil || req.Viewer.ID != 7 || req.Protocol != "dlna" || req.Device.ID == "" || req.Item != frontend.RatingKey(1000000000000042) || !req.Intent.Options.DirectPlay {
		return "", errors.New("wrong scope")
	}
	f.issued++
	return "/sessions/lazy/opaque/file", nil
}
func (f *lanStreamFixture) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	f.served++
	_, _ = w.Write([]byte("media"))
}

func TestBrowseIssuesOnlyViewerScopedLANMedia(t *testing.T) {
	streams := &lanStreamFixture{}
	viewer := &frontend.Viewer{ID: 7}
	movie := frontend.Title{Key: frontend.RatingKey(1000000000000042), Type: "movie", Title: "Visible"}
	host := frontend.Host{
		LANViewer: func(_ context.Context, r *http.Request) (*frontend.Viewer, error) {
			if r.Header.Get("X-Remote") == "1" {
				return nil, errors.New("remote")
			}
			return viewer, nil
		},
		ClientIP:     func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.5"), true },
		SurfaceURL:   func(context.Context, *http.Request) (string, error) { return "http://ether.local/dlna", nil },
		Capabilities: frontendtest.Capabilities{}, LANStreams: streams, Sessions: frontendtest.Sessions{},
		Experience: frontendtest.Experience{LibraryItemsFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, _ string, _ frontend.ItemQuery) (frontend.Page, error) {
			if v.ID != 7 {
				t.Fatal("wrong viewer")
			}
			return frontend.Page{Items: []frontend.Title{movie}, Total: 1}, nil
		}},
	}
	inst, err := (Frontend{MachineID: "machine"}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	body := `<Browse><ObjectID>view:1</ObjectID><BrowseFlag>BrowseDirectChildren</BrowseFlag><StartingIndex>0</StartingIndex><RequestedCount>20</RequestedCount></Browse>`
	r := httptest.NewRequest(http.MethodPost, "/dlna/ContentDirectory/control", strings.NewReader(body))
	r.Header.Set("SOAPAction", `"urn:schemas-upnp-org:service:ContentDirectory:1#Browse"`)
	w := httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 200 || streams.issued != 1 || !strings.Contains(w.Body.String(), "http://ether.local/dlna/sessions/lazy/opaque/file") || strings.Contains(w.Body.String(), "Hidden") {
		t.Fatalf("browse %d %q issued=%d", w.Code, w.Body.String(), streams.issued)
	}
	w = httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/dlna/sessions/lazy/opaque/file", nil))
	if w.Code != 200 || w.Body.String() != "media" || streams.served != 1 {
		t.Fatalf("media %d %q", w.Code, w.Body.String())
	}
	r = httptest.NewRequest(http.MethodGet, "/dlna/sessions/lazy/opaque/file", nil)
	r.Header.Set("X-Remote", "1")
	w = httptest.NewRecorder()
	inst.Handler().ServeHTTP(w, r)
	if w.Code != 403 || streams.served != 1 {
		t.Fatalf("remote media %d", w.Code)
	}
}
