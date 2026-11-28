// Package xtream provides a viewer-scoped API for IPTV player apps.
package xtream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/lineup"
	"github.com/braedonsaunders/ether/internal/frontend/kit/listcache"
	"github.com/braedonsaunders/ether/internal/frontend/kit/lru"
	"github.com/braedonsaunders/ether/internal/frontend/kit/mediasafety"
	"github.com/braedonsaunders/ether/internal/frontend/kit/reqmemo"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

type Frontend struct{}

func (Frontend) Describe() frontend.Descriptor {
	return frontend.Descriptor{ID: "xtream", Name: "IPTV apps (Xtream)", Version: "1.0", SettingsSchema: frontend.AccountSchema("xtream")}
}
func (Frontend) Mount(_ context.Context, host frontend.Host) (frontend.Instance, error) {
	if _, ok := host.Identity.(frontend.AppPasswordIdentity); !ok || host.Experience == nil || host.LiveTV == nil || host.Sessions == nil {
		return nil, errors.New("xtream requires app passwords, experience, live TV and sessions")
	}
	i := &instance{host: host, exports: listcache.New(host.CacheDir)}
	if err := i.registerRoutes(); err != nil {
		i.exports.Close()
		return nil, err
	}
	i.lineups = i.openLineups()
	return i, nil
}

type instance struct {
	host       frontend.Host
	router     *route.Router
	xmltvMu    sync.Mutex
	xmltvCache *lru.Cache[xmltvGuideKey, []frontend.LiveProgramme]
	// exports keeps encoded whole-library listings; nil encodes each one.
	exports *listcache.Store
	lineups *lineup.Store
}

func (i *instance) registerRoutes() error {
	i.router = &route.Router{}
	for _, p := range []string{"/player_api.php", "/xmltv.php", "/get.php"} {
		if err := i.router.Handle("GET", p, http.HandlerFunc(i.serve)); err != nil {
			return err
		}
	}
	for _, kind := range []string{"live", "movie", "series"} {
		for _, method := range []string{"GET", "HEAD"} {
			if err := i.router.Handle(method, "/"+kind+"/{username}/{password}/{file}", http.HandlerFunc(i.serve)); err != nil {
				return err
			}
		}
	}
	for _, method := range []string{"GET", "HEAD"} {
		for _, p := range []string{"/timeshift/{username}/{password}/{duration}/{start}/{file}", "/streaming/timeshift.php"} {
			if err := i.router.Handle(method, p, http.HandlerFunc(i.serve)); err != nil {
				return err
			}
		}
	}
	for _, p := range []string{"/sessions/{tail...}", "/image/{secret}"} {
		for _, method := range []string{"GET", "HEAD"} {
			if err := i.router.Handle(method, p, http.HandlerFunc(i.media)); err != nil {
				return err
			}
		}
	}
	return nil
}
func RegisteredRoutes() ([]route.Route, error) {
	i := &instance{}
	err := i.registerRoutes()
	return i.router.Routes(), err
}
func (i *instance) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mediasafety.SetHeaders(w)
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		ctx := r.Context()
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			if !reqmemo.Active(ctx) {
				ctx = reqmemo.With(ctx)
			}
			if frontend.RequestMemoFrom(ctx) == nil {
				ctx = frontend.WithRequestMemo(ctx)
			}
		}
		r = r.Clone(ctx)
		u := *r.URL
		u.Path = strings.TrimPrefix(u.Path, "/xtream")
		r.URL = &u
		i.router.ServeHTTP(w, r)
	})
}
func (*instance) Listeners() []frontend.ListenerSpec {
	return []frontend.ListenerSpec{{Shared: true, PathPrefix: "/xtream"}}
}
func (*instance) Discovery() []frontend.Announcer { return nil }
func (*instance) Status(context.Context) frontend.Status {
	return frontend.Status{TargetVersion: "Xtream", Conformance: "partial"}
}
func (i *instance) Close(context.Context) error {
	i.lineups.Close()
	i.exports.Close()
	return nil
}
func (i *instance) Routes() []route.Route { return i.router.Routes() }

// BackgroundBusy counts listing builds and compressions in flight.
func (i *instance) BackgroundBusy() int { return i.exports.Busy() + i.lineups.Busy() }

type login struct {
	viewer             *frontend.Viewer
	device             frontend.Device
	password, username string
	credential         string
	artwork            map[frontend.RatingKey]string
	ids                map[frontend.RatingKey]int32
	channels           map[string]int32
	groups             map[string]int32
	// base is the request's media origin once hasBase is set; listings sign
	// artwork under it instead of asking for it again per page.
	base    string
	hasBase bool
	// keep, when set, receives each listing page's signed artwork.
	keep func([]frontend.ImageBatchItem) error
}

// detached is l with maps of its own, for work that outlives the request.
func (l login) detached() login {
	l.artwork, l.ids = make(map[frontend.RatingKey]string), make(map[frontend.RatingKey]int32)
	l.channels, l.groups = make(map[string]int32), make(map[string]int32)
	return l
}

// forPage is l with artwork and id maps of its own, for one listing page
// signed while another is written.
func (l login) forPage() login {
	l.artwork, l.ids = make(map[frontend.RatingKey]string, pageSize), make(map[frontend.RatingKey]int32, pageSize)
	return l
}

func (i *instance) authenticate(r *http.Request) (login, error) {
	u, p := r.PathValue("username"), r.PathValue("password")
	if u == "" {
		u, p = r.URL.Query().Get("username"), r.URL.Query().Get("password")
	}
	if i.host.ClientIP == nil {
		return login{}, errors.New("unauthorized")
	}
	ip, ok := i.host.ClientIP(r)
	if !ok {
		return login{}, errors.New("unauthorized")
	}
	v, err := i.host.Identity.(frontend.AppPasswordIdentity).AuthenticateAppPassword(frontend.WithClientIP(r.Context(), ip), u, p)
	if err != nil {
		return login{}, err
	}
	if v == nil {
		return login{}, errors.New("unauthorized")
	}
	credential, short := frontend.XtreamGrantCredential(u, p)
	if !short {
		credential = p
	}
	sum := sha256.Sum256([]byte(credential))
	d := frontend.Device{ID: "xtream-" + hex.EncodeToString(sum[:16]), Client: "IPTV player apps", Product: "Xtream"}
	if i.host.ClientSeen != nil {
		i.host.ClientSeen(d.ID)
	}
	return login{viewer: v, device: d, password: p, username: u, credential: credential, artwork: make(map[frontend.RatingKey]string), ids: make(map[frontend.RatingKey]int32), channels: make(map[string]int32), groups: make(map[string]int32)}, nil
}
func jsonResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}
func (i *instance) serve(w http.ResponseWriter, r *http.Request) {
	l, err := i.authenticate(r)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		status := http.StatusUnauthorized
		if errors.Is(err, frontend.ErrRateLimited) {
			status = http.StatusTooManyRequests
			w.Header().Set("Retry-After", "900")
		}
		w.WriteHeader(status)
		jsonResponse(w, map[string]any{"user_info": map[string]int{"auth": 0}})
		return
	}
	switch strings.ToLower(r.URL.Path) {
	case "/player_api.php":
		i.api(w, r, l)
	case "/get.php":
		i.playlist(w, r, l)
	case "/xmltv.php":
		i.xmltv(w, r, l)
	case "/streaming/timeshift.php":
		i.catchUp(w, r, l)
	default:
		if r.PathValue("duration") != "" {
			i.catchUp(w, r, l)
			return
		}
		i.play(w, r, l)
	}
}
func (i *instance) base(r *http.Request) string {
	if i.host.SurfaceURL == nil {
		return ""
	}
	s, err := i.host.SurfaceURL(r.Context(), r)
	if err != nil {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ""
	}
	// Shared-listener hosts can return the reachable origin instead of the
	// frontend prefix. All media and artwork routes belong under /xtream.
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/xtream") {
		u.Path += "/xtream"
	}
	u.RawPath = ""
	return u.String()
}
func (i *instance) api(w http.ResponseWriter, r *http.Request, l login) {
	action := r.URL.Query().Get("action")
	switch action {
	case "":
		base, err := url.Parse(i.base(r))
		if err != nil || base.Host == "" {
			http.Error(w, "Unavailable", 503)
			return
		}
		port := base.Port()
		if port == "" {
			port = "80"
			if base.Scheme == "https" {
				port = "443"
			}
		}
		now := time.Now().UTC()
		jsonResponse(w, map[string]any{"user_info": map[string]any{"username": l.username, "password": l.password, "auth": 1, "status": "Active", "message": "Ether", "exp_date": nil, "is_trial": "0", "active_cons": strconv.Itoa(i.active(r, l)), "created_at": "0", "max_connections": strconv.Itoa(i.connectionLimit(r, l)), "allowed_output_formats": []string{"m3u8", "ts"}}, "server_info": map[string]any{"url": base.Hostname(), "port": port, "https_port": "443", "server_protocol": base.Scheme, "rtmp_port": "0", "timezone": "UTC", "timestamp_now": now.Unix(), "time_now": now.Format("2006-01-02 15:04:05")}})
	case "get_live_categories", "get_live_streams":
		i.live(w, r, l, action)
	case "get_short_epg", "get_simple_data_table":
		i.epg(w, r, l, action)
	case "get_vod_categories", "get_series_categories", "get_vod_streams", "get_series", "get_vod_info", "get_series_info":
		i.catalog(w, r, l, action)
	default:
		http.Error(w, "Unknown action", 400)
	}
}
func (i *instance) media(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(strings.ToLower(r.URL.Path), "/image/") {
		if i.host.ImageGrants != nil {
			i.host.ImageGrants.Serve(w, r, r.PathValue("secret"))
			return
		}
	} else if i.host.LazyStreams != nil {
		if i.host.ClientIP != nil {
			if ip, ok := i.host.ClientIP(r); ok {
				r = r.WithContext(frontend.WithClientIP(r.Context(), ip))
			}
		}
		i.host.LazyStreams.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}
func (i *instance) image(r *http.Request, l login, key frontend.RatingKey, channel string, images []frontend.Image) string {
	if len(images) == 0 || i.host.ImageGrants == nil {
		return ""
	}
	base := i.base(r)
	if base == "" {
		return ""
	}
	secret, err := i.host.ImageGrants.Issue(r.Context(), frontend.ImageGrantRequest{Credential: l.credential, Device: l.device, Item: key, Channel: channel, Image: images[0]})
	if err != nil {
		return ""
	}
	return base + "/image/" + secret
}
