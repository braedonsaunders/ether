package webdav

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/frontendtest"
	"github.com/braedonsaunders/ether/internal/frontend/kit/libpath"
)

func TestOfflineClientBrowsesWithoutOpening(t *testing.T) {
	identity := frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
		if token != "grant" {
			return nil, errors.New("bad grant")
		}
		return &frontend.Viewer{ID: 7}, nil
	}}
	movie := frontend.Title{Key: frontend.RatingKey(1000000000000042), Type: "movie", Title: "A & B", Year: 2024}
	experience := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: "movie"}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, v *frontend.Viewer, _ frontend.Device, id string, _ frontend.ItemQuery) (frontend.Page, error) {
			if v.ID != 7 || id != "movies" {
				t.Fatal("wrong viewer")
			}
			return frontend.Page{Items: []frontend.Title{movie}}, nil
		},
	}
	sessions := frontendtest.Sessions{PlanFunc: func(_ context.Context, request frontend.PlanRequest) (frontend.PlayPlan, error) {
		if !request.Intent.Estimate || request.Item != movie.Key {
			t.Fatalf("unexpected plan: %+v", request)
		}
		return frontend.PlayPlan{Estimated: true, Media: frontend.MediaInfo{Size: 1234}}, nil
	}, OpenFunc: func(context.Context, frontend.OpenRequest) (frontend.Session, error) {
		t.Fatal("browse opened media")
		return frontend.Session{}, nil
	}}
	var seen []string
	host := frontend.Host{Identity: identity, Experience: experience, Sessions: sessions, Capabilities: frontendtest.Capabilities{}, ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.0.2.1"), true }, ClientSeen: func(id string) { seen = append(seen, id) }}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(inst.Handler())
	defer server.Close()
	do := func(method, pathname, depth, user, password string) (int, string, http.Header) {
		req, err := http.NewRequest(method, server.URL+pathname, nil)
		if err != nil {
			t.Fatal(err)
		}
		if depth != "" {
			req.Header.Set("Depth", depth)
		}
		req.SetBasicAuth(user, password)
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(body), res.Header
	}
	if status, body, _ := do("PROPFIND", "/webdav/Movies/", "1", "7", "grant"); status != 207 || !strings.Contains(body, "A &amp; B (2024)") {
		t.Fatalf("listing %d %s", status, body)
	}
	if status, body, _ := do("PROPFIND", "/webdav/Movies/A%20&%20B%20%282024%29/", "1", "7", "grant"); status != 207 || !strings.Contains(body, "1234") || !strings.Contains(body, ".mkv") {
		t.Fatalf("files %d %s", status, body)
	}
	if status, _, header := do("HEAD", "/webdav/Movies/A%20&%20B%20%282024%29/A%20&%20B%20%282024%29.mkv", "", "7", "grant"); status != 200 || header.Get("Content-Length") != "1234" {
		t.Fatalf("head %d %v", status, header)
	}
	if status, _, _ := do("PROPFIND", "/webdav/", "1", "8", "grant"); status != 401 {
		t.Fatal("wrong viewer accepted", status)
	}
	if len(seen) != 3 || !strings.HasPrefix(seen[0], "webdav-") {
		t.Fatalf("client observations: %+v", seen)
	}

	if status, _, _ := do("PROPFIND", "/webdav/", "infinity", "7", "grant"); status != 403 {
		t.Fatal("unbounded depth accepted", status)
	}
}

func TestDuplicateNamesRemainDistinct(t *testing.T) {
	first := frontend.Title{Key: frontend.RatingKey(1000000000000001), Type: "movie", Title: "Film", Year: 2024}
	second := frontend.Title{Key: frontend.RatingKey(1000000000000002), Type: "movie", Title: "Film", Year: 2024}
	nodes := unique([]resource{{name: libpath.MovieName(first), dir: true, kind: "movie", title: first}, {name: libpath.MovieName(second), dir: true, kind: "movie", title: second}})
	if len(nodes) != 2 || nodes[0].name == nodes[1].name || !strings.Contains(nodes[0].name, first.Key.String()) || !strings.Contains(nodes[1].name, second.Key.String()) {
		t.Fatalf("duplicates: %+v", nodes)
	}
	inst := &instance{}
	children, err := inst.children(httptest.NewRequest("PROPFIND", "/", nil), &frontend.Viewer{ID: 7}, frontend.Device{}, nodes[0])
	if err != nil || len(children) != 1 || children[0].name != nodes[0].name+".mkv" {
		t.Fatalf("movie: %+v %v", children, err)
	}
}

func TestLibraryContinuesShortPagesUntilAuthorizedTotal(t *testing.T) {
	var offsets []int
	experience := frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: "movie"}}, nil
		},
		LibraryItemsFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, _ string, q frontend.ItemQuery) (frontend.Page, error) {
			offsets = append(offsets, q.Offset)
			return frontend.Page{Items: []frontend.Title{{Key: frontend.RatingKey(q.Offset + 1), Type: "movie"}}, Total: 3}, nil
		},
	}
	i := &instance{host: frontend.Host{Experience: experience}}
	items, err := i.library(httptest.NewRequest("PROPFIND", "/", nil), &frontend.Viewer{ID: 7}, frontend.Device{}, "movie")
	if err != nil || len(items) != 3 || fmt.Sprint(offsets) != "[0 1 2]" {
		t.Fatalf("items=%v offsets=%v err=%v", items, offsets, err)
	}
}

func TestCollectionsPageFullyAndExposeShows(t *testing.T) {
	viewer := &frontend.Viewer{ID: 7}
	calls := 0
	experience := frontendtest.Experience{
		CollectionsFunc: func(_ context.Context, v *frontend.Viewer, d frontend.Device, q frontend.CollectionQuery) (frontend.CollectionPage, error) {
			if v.ID != 7 || d.ID != "device" || q.Limit != 500 || q.Offset != calls*500 {
				t.Fatalf("collection query %+v", q)
			}
			calls++
			count := min(500, 501-q.Offset)
			items := make([]frontend.Collection, count)
			for j := range items {
				items[j] = frontend.Collection{Key: frontend.RatingKey(q.Offset + j + 1), Title: fmt.Sprintf("Collection %d", q.Offset+j+1)}
			}
			return frontend.CollectionPage{Items: items, Total: 501}, nil
		},
		ChildrenFunc: func(_ context.Context, v *frontend.Viewer, d frontend.Device, k frontend.RatingKey, q frontend.ItemQuery) (frontend.Page, error) {
			if v.ID != 7 || d.ID != "device" || k != 1 {
				t.Fatal("wrong collection scope")
			}
			return frontend.Page{Items: []frontend.Title{{Key: 1000, Type: "movie", Title: "Film", Year: 2024}, {Key: 1001, Type: "show", Title: "Series"}}, Total: 2}, nil
		},
	}
	i := instance{host: frontend.Host{Experience: experience}}
	req := httptest.NewRequest("PROPFIND", "/", nil)
	device := frontend.Device{ID: "device"}
	nodes, err := i.children(req, viewer, device, resource{kind: "collections"})
	if err != nil || calls != 2 || len(nodes) != 501 {
		t.Fatalf("collections=%d calls=%d err=%v", len(nodes), calls, err)
	}
	nodes, err = i.children(req, viewer, device, resource{kind: "collection", title: frontend.Title{Key: 1}})
	if err != nil || len(nodes) != 2 {
		t.Fatalf("entries=%+v err=%v", nodes, err)
	}
	foundShow := false
	for _, node := range nodes {
		if node.name == "Series" && node.kind == "show" && node.dir {
			foundShow = true
		}
	}
	if !foundShow {
		t.Fatalf("missing show: %+v", nodes)
	}
}

func TestCollectionChildrenAreNotTruncated(t *testing.T) {
	calls := 0
	i := instance{host: frontend.Host{Experience: frontendtest.Experience{ChildrenFunc: func(_ context.Context, _ *frontend.Viewer, _ frontend.Device, _ frontend.RatingKey, q frontend.ItemQuery) (frontend.Page, error) {
		if q.Limit != 500 || q.Offset != calls*500 {
			t.Fatalf("query %+v", q)
		}
		calls++
		count := min(500, 501-q.Offset)
		items := make([]frontend.Title, count)
		for j := range items {
			items[j] = frontend.Title{Key: frontend.RatingKey(q.Offset + j + 1), Type: "movie", Title: fmt.Sprintf("Film %d", q.Offset+j+1)}
		}
		return frontend.Page{Items: items, Total: 501}, nil
	}}}}
	nodes, err := i.children(httptest.NewRequest("PROPFIND", "/", nil), &frontend.Viewer{ID: 7}, frontend.Device{ID: "device"}, resource{kind: "collection", title: frontend.Title{Key: 1}})
	if err != nil || calls != 2 || len(nodes) != 501 {
		t.Fatalf("nodes=%d calls=%d err=%v", len(nodes), calls, err)
	}
}

func TestUnknownEstimateDoesNotInventFileSize(t *testing.T) {
	movie := frontend.Title{Key: frontend.RatingKey(1000000000000042), Type: "movie", Title: "Film", Year: 2024}
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }}, Experience: frontendtest.Experience{
		ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
			return []frontend.View{{ID: "movies", Type: "movie"}}, nil
		},
		LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
			return frontend.Page{Items: []frontend.Title{movie}, Total: 1}, nil
		},
	}, Sessions: frontendtest.Sessions{}}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(inst.Handler())
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodHead, srv.URL+"/webdav/Movies/Film%20%282024%29/Film%20%282024%29.mkv", nil)
	req.SetBasicAuth("7", "grant")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 || res.Header.Get("Content-Length") != "" {
		t.Fatalf("HEAD status=%d content-length=%q", res.StatusCode, res.Header.Get("Content-Length"))
	}
}

func TestBrowseDistinguishesUnavailableFromMissing(t *testing.T) {
	fail := true
	host := frontend.Host{Identity: frontendtest.Identity{ResolveFunc: func(context.Context, string) (*frontend.Viewer, error) { return &frontend.Viewer{ID: 7}, nil }}, Sessions: frontendtest.Sessions{}, Experience: frontendtest.Experience{ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
		if fail {
			return nil, errors.New("database unavailable")
		}
		return nil, nil
	}}}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want int) {
		req := httptest.NewRequest("PROPFIND", "/webdav/Movies/missing", nil)
		req.SetBasicAuth("7", "grant")
		req.Header.Set("Depth", "0")
		rec := httptest.NewRecorder()
		inst.Handler().ServeHTTP(rec, req)
		if rec.Code != want {
			t.Fatalf("status=%d want=%d", rec.Code, want)
		}
	}
	check(http.StatusServiceUnavailable)
	fail = false
	check(http.StatusNotFound)
}

type authenticatedRanges struct {
	calls int
}

func (*authenticatedRanges) Issue(context.Context, string, frontend.PlanRequest) (string, error) {
	return "", errors.New("unexpected bearer issuance")
}
func (*authenticatedRanges) ServeHTTP(http.ResponseWriter, *http.Request) {
	panic("unexpected bearer serving")
}
func (f *authenticatedRanges) ServeAuthenticated(w http.ResponseWriter, r *http.Request, credential string, request frontend.PlanRequest) {
	f.calls++
	if credential != "grant" || request.Viewer.ID != 7 || request.Protocol != "webdav" || request.Item != frontend.RatingKey(1000000000000042) || !strings.HasPrefix(request.Device.ID, "webdav-") || !request.Intent.Options.DirectPlay || r.Header.Get("Range") != "bytes=0-2" {
		http.Error(w, "wrong scope", 500)
		return
	}
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write([]byte("abc"))
}

func TestWebDAVGETDelegatesAuthenticatedRanges(t *testing.T) {
	movie := frontend.Title{Key: frontend.RatingKey(1000000000000042), Type: "movie", Title: "Film", Year: 2024}
	ranges := &authenticatedRanges{}
	host := frontend.Host{
		Identity: frontendtest.Identity{ResolveFunc: func(_ context.Context, token string) (*frontend.Viewer, error) {
			if token != "grant" {
				return nil, errors.New("bad grant")
			}
			return &frontend.Viewer{ID: 7}, nil
		}},
		Experience: frontendtest.Experience{
			ViewsFunc: func(context.Context, *frontend.Viewer) ([]frontend.View, error) {
				return []frontend.View{{ID: "movies", Type: "movie"}}, nil
			},
			LibraryItemsFunc: func(context.Context, *frontend.Viewer, frontend.Device, string, frontend.ItemQuery) (frontend.Page, error) {
				return frontend.Page{Items: []frontend.Title{movie}, Total: 1}, nil
			},
		},
		Sessions: frontendtest.Sessions{}, LazyStreams: ranges, Capabilities: frontendtest.Capabilities{},
		ClientIP: func(*http.Request) (netip.Addr, bool) { return netip.MustParseAddr("192.168.1.5"), true },
	}
	inst, err := (Frontend{}).Mount(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(inst.Handler())
	defer server.Close()
	for _, method := range []string{http.MethodHead, "PROPFIND", http.MethodGet} {
		req, err := http.NewRequest(method, server.URL+"/webdav/Movies/Film%20%282024%29/Film%20%282024%29.mkv", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth("7", "grant")
		req.Header.Set("Range", "bytes=0-2")
		if method == "PROPFIND" {
			req.Header.Set("Depth", "0")
		}
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if method == http.MethodGet && (res.StatusCode != http.StatusPartialContent || string(body) != "abc") {
			t.Fatalf("range status=%d body=%q", res.StatusCode, body)
		}
		if method != http.MethodGet && ranges.calls != 0 {
			t.Fatal("metadata opened a stream")
		}
	}
	if ranges.calls != 1 {
		t.Fatalf("range calls: %d", ranges.calls)
	}
}

func TestRegisteredRoutesMatchMountedRouter(t *testing.T) {
	got, err := RegisteredRoutes()
	if err != nil {
		t.Fatal(err)
	}
	inst, err := (Frontend{}).Mount(context.Background(), frontend.Host{Identity: frontendtest.Identity{}, Experience: frontendtest.Experience{}, Sessions: frontendtest.Sessions{}})
	if err != nil {
		t.Fatal(err)
	}
	routes := inst.(frontend.RouteSource).Routes()
	if !slices.Equal(got, routes) || len(routes) != 4 {
		t.Fatalf("inventory=%v routes=%v", got, routes)
	}
}
