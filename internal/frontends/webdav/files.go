package webdav

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/caps"
	"github.com/braedonsaunders/ether/internal/frontend/kit/libpath"
)

var errNotFound = errors.New("webdav: path not found")

type resource struct {
	name  string
	dir   bool
	title frontend.Title
	kind  string
}

func (i *instance) library(r *http.Request, v *frontend.Viewer, d frontend.Device, typ string) ([]frontend.Title, error) {
	views, err := i.host.Experience.Views(r.Context(), v)
	if err != nil {
		return nil, err
	}
	var out []frontend.Title
	for _, view := range views {
		if string(view.Type) != typ {
			continue
		}
		for offset := 0; offset < 100000; {
			page, err := i.host.Experience.LibraryItems(r.Context(), v, d, view.ID, frontend.ItemQuery{Offset: offset, Limit: 250})
			if err != nil {
				return nil, err
			}
			out = append(out, page.Items...)
			offset += len(page.Items)
			if offset >= page.Total {
				break
			}
			if len(page.Items) == 0 {
				return nil, fmt.Errorf("incomplete library listing")
			}
			if offset >= 100000 {
				return nil, fmt.Errorf("library exceeds browse bound")
			}
		}
	}
	return out, nil
}

func (i *instance) children(r *http.Request, viewer *frontend.Viewer, device frontend.Device, node resource) ([]resource, error) {
	switch node.kind {
	case "root":
		return []resource{{name: "Movies", dir: true, kind: "movies"}, {name: "TV", dir: true, kind: "tv"}, {name: "Collections", dir: true, kind: "collections"}}, nil
	case "movies", "tv":
		typ := "movie"
		if node.kind == "tv" {
			typ = "show"
		}
		titles, err := i.library(r, viewer, device, typ)
		if err != nil {
			return nil, err
		}
		var out []resource
		for _, title := range titles {
			if string(title.Type) != typ {
				continue
			}
			name, kind := libpath.MovieName(title), "movie"
			if typ == "show" {
				name, kind = libpath.Safe(title.Title), "show"
			}
			out = append(out, resource{name: name, dir: true, title: title, kind: kind})
		}
		return unique(out), nil
	case "movie":
		return []resource{{name: node.name + ".mkv", title: node.title, kind: "file"}}, nil
	case "show", "season", "collection":
		var titles []frontend.Title
		for offset := 0; offset < 100000; {
			page, err := i.host.Experience.Children(r.Context(), viewer, device, node.title.Key, frontend.ItemQuery{Offset: offset, Limit: 500})
			if err != nil {
				return nil, err
			}
			titles = append(titles, page.Items...)
			offset += len(page.Items)
			if offset >= page.Total {
				break
			}
			if len(page.Items) == 0 {
				return nil, fmt.Errorf("incomplete child listing")
			}
			if offset >= 100000 {
				return nil, fmt.Errorf("child listing exceeds browse bound")
			}
		}
		var out []resource
		for _, title := range titles {
			if node.kind == "show" && string(title.Type) == "season" {
				out = append(out, resource{name: fmt.Sprintf("Season %02d", title.Season), dir: true, title: title, kind: "season"})
			} else if node.kind == "season" && string(title.Type) == "episode" {
				out = append(out, resource{name: path.Base(libpath.Episode(title)), title: title, kind: "file"})
			} else if node.kind == "collection" && string(title.Type) == "movie" {
				out = append(out, resource{name: libpath.MovieName(title) + ".mkv", title: title, kind: "file"})
			} else if node.kind == "collection" && string(title.Type) == "show" {
				out = append(out, resource{name: libpath.Safe(title.Title), dir: true, title: title, kind: "show"})
			}
		}
		return unique(out), nil
	case "collections":
		var out []resource
		for offset := 0; offset < 100000; {
			page, err := i.host.Experience.Collections(r.Context(), viewer, device, frontend.CollectionQuery{Offset: offset, Limit: 500})
			if err != nil {
				return nil, err
			}
			for _, collection := range page.Items {
				out = append(out, resource{name: libpath.Safe(collection.Title), dir: true, title: frontend.Title{Key: collection.Key}, kind: "collection"})
			}
			offset += len(page.Items)
			if offset >= page.Total {
				return unique(out), nil
			}
			if len(page.Items) == 0 {
				return nil, fmt.Errorf("incomplete collection listing")
			}
		}
		return nil, fmt.Errorf("collection listing exceeds browse bound")
	}
	return nil, nil
}

// Use the shared deterministic collision policy so other frontends can
// resolve exactly the same paths for an authorized title.
func unique(nodes []resource) []resource {
	items := make([]libpath.NamedKey, 0, len(nodes))
	for _, node := range nodes {
		name := node.name
		if node.kind == "file" {
			name = strings.TrimSuffix(name, ".mkv")
		}
		items = append(items, libpath.NamedKey{Name: name, Key: node.title.Key})
	}
	names := libpath.Disambiguate(items)
	seen := make(map[frontend.RatingKey]bool, len(nodes))
	out := nodes[:0]
	for _, node := range nodes {
		if seen[node.title.Key] {
			continue
		}
		name := names[node.title.Key]
		if name == "" {
			continue
		}
		seen[node.title.Key] = true
		node.name = name
		if node.kind == "file" {
			node.name += ".mkv"
		}
		out = append(out, node)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].name < out[b].name })
	return out
}

func (i *instance) resolve(r *http.Request, viewer *frontend.Viewer, device frontend.Device) (resource, error) {
	node := resource{dir: true, kind: "root"}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return node, nil
	}
	if len(parts) > 5 {
		return resource{}, errNotFound
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || !node.dir {
			return resource{}, errNotFound
		}
		children, err := i.children(r, viewer, device, node)
		if err != nil {
			return resource{}, err
		}
		found := false
		for _, child := range children {
			if child.name == part {
				node, found = child, true
				break
			}
		}
		if !found {
			return resource{}, errNotFound
		}
	}
	return node, nil
}

type davType struct {
	Collection *struct{} `xml:"collection,omitempty"`
}
type davProp struct {
	Display     string  `xml:"displayname"`
	Resource    davType `xml:"resourcetype"`
	Size        *int64  `xml:"getcontentlength,omitempty"`
	ContentType string  `xml:"getcontenttype,omitempty"`
}
type davPropstat struct {
	Prop   davProp `xml:"prop"`
	Status string  `xml:"status"`
}
type davResponse struct {
	Href     string      `xml:"href"`
	Propstat davPropstat `xml:"propstat"`
}
type davMulti struct {
	XMLName   xml.Name      `xml:"multistatus"`
	XMLNS     string        `xml:"xmlns,attr"`
	Responses []davResponse `xml:"response"`
}

func (i *instance) estimate(r *http.Request, viewer *frontend.Viewer, device frontend.Device, key frontend.RatingKey) *int64 {
	if i.host.Capabilities == nil || i.host.ClientIP == nil {
		return nil
	}
	ip, ok := i.host.ClientIP(r)
	if !ok {
		return nil
	}
	ctx := frontend.WithClientIP(r.Context(), ip)
	profile, err := i.host.Capabilities.Build(ctx, frontend.CapabilityRequest{Viewer: viewer, Device: caps.DeviceInfo{Protocol: "webdav", ClientID: device.ID, Product: device.Product}, UserAgent: r.UserAgent()})
	if err != nil {
		return nil
	}
	plan, err := i.host.Sessions.Plan(ctx, frontend.PlanRequest{Viewer: viewer, Device: device, Protocol: "webdav", Item: key, Caps: profile.Client, Intent: frontend.PlayIntent{Estimate: true, Options: frontend.DecideOptions{DirectPlay: true}}})
	if err != nil || !plan.Estimated || plan.Media.Size <= 0 {
		return nil
	}
	return &plan.Media.Size
}

func davItem(prefix string, node resource, size *int64) davResponse {
	u := url.URL{Path: prefix}
	if node.dir && !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
	}
	props := davProp{Display: node.name, Size: size}
	if node.dir {
		props.Resource.Collection = &struct{}{}
	} else {
		props.ContentType = "application/octet-stream"
	}
	return davResponse{Href: u.EscapedPath(), Propstat: davPropstat{Prop: props, Status: "HTTP/1.1 200 OK"}}
}

func (i *instance) handleFile(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, device frontend.Device, credential string) {
	node, err := i.resolve(r, viewer, device)
	if err != nil {
		if errors.Is(err, errNotFound) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		}
		return
	}
	if r.Method == "PROPFIND" {
		depth := r.Header.Get("Depth")
		if depth != "0" && depth != "1" {
			http.Error(w, "Depth not supported", http.StatusForbidden)
			return
		}
		prefix := "/webdav" + r.URL.Path
		var size *int64
		if !node.dir {
			size = i.estimate(r, viewer, device, node.title.Key)
		}
		result := davMulti{XMLNS: "DAV:", Responses: []davResponse{davItem(prefix, node, size)}}
		if depth == "1" && node.dir {
			children, err := i.children(r, viewer, device, node)
			if err != nil {
				http.Error(w, "Unavailable", http.StatusServiceUnavailable)
				return
			}
			for _, child := range children {
				var size *int64
				if !child.dir {
					size = i.estimate(r, viewer, device, child.title.Key)
				}
				result.Responses = append(result.Responses, davItem(strings.TrimRight(prefix, "/")+"/"+child.name, child, size))
			}
		}
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.WriteHeader(207)
		_, _ = w.Write([]byte(xml.Header))
		_ = xml.NewEncoder(w).Encode(result)
		return
	}
	if node.dir {
		http.Error(w, "Not a file", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "application/octet-stream")
		if size := i.estimate(r, viewer, device, node.title.Key); size != nil {
			w.Header().Set("Content-Length", fmt.Sprint(*size))
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Error(w, "Unavailable", http.StatusNotImplemented)
}
