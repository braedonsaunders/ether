package mediabrowser

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

func (i *instance) localTrailers(w http.ResponseWriter, r *http.Request) {
	i.writeExtras(w, r, true)
}

func (i *instance) specialFeatures(w http.ResponseWriter, r *http.Request) {
	i.writeExtras(w, r, false)
}

func (i *instance) writeExtras(w http.ResponseWriter, r *http.Request, trailers bool) {
	detail, ok := i.authorizedItem(w, r)
	if !ok {
		return
	}
	items := []any{}
	for _, extra := range detail.Extras {
		if extra.Key == 0 || isTrailerExtra(extra) != trailers {
			continue
		}
		items = append(items, i.extraItem(detail, extra))
	}
	jsonReply(w, items)
}

func isTrailerExtra(extra frontend.Extra) bool {
	return extra.Kind == "trailer" || extra.Kind == "teaser"
}

func (i *instance) extraItem(parent frontend.ItemDetail, clip frontend.Extra) map[string]any {
	id := ids.RatingKeyGUID(clip.Key)
	parentKey := clip.ParentKey
	if parentKey == 0 {
		parentKey = parent.Title.Key
	}
	name := clip.Title
	if name == "" {
		name = "Trailer"
	}
	item := map[string]any{
		"Id": id, "Name": name, "Type": "Trailer", "MediaType": "Video", "IsFolder": false,
		"ServerId": i.info.ID, "ParentId": ids.RatingKeyGUID(parentKey),
	}
	if clip.Duration > 0 {
		item["RunTimeTicks"] = clip.Duration.Nanoseconds() / 100
	}
	if !clip.PublishedAt.IsZero() {
		item["PremiereDate"] = wireTime(clip.PublishedAt)
	}
	height := clip.Height
	if height <= 0 {
		height = 720
	}
	media := frontend.MediaInfo{Container: "mp4", Duration: clip.Duration, Estimated: true, Streams: []frontend.StreamInfo{
		{Index: 0, Type: "video", Codec: "h264", Width: height * 16 / 9, Height: height, Default: true},
		{Index: 1, Type: "audio", Codec: "aac", Channels: 2, Language: clip.Language, Default: true},
	}}
	source := i.mediaSourceDTO(id, media)
	playable := i.host.ExtraStreams != nil
	source["SupportsDirectPlay"] = playable
	source["SupportsDirectStream"] = playable
	source["SupportsTranscoding"] = false
	source["IsRemote"] = false
	if i.protocol == "emby" {
		i.embyMediaSourceDTO(source, id, media)
		delete(source, "ETag")
	} else {
		item["LocationType"] = "FileSystem"
		item["Etag"] = itemEtag(id)
	}
	if i.protocol == "emby" {
		item["Etag"] = embyETag(id)
	}
	i.mediaSourceDialect(source)
	item["MediaSources"] = []any{source}
	return item
}

func (i *instance) intros(w http.ResponseWriter, r *http.Request) {
	detail, ok := i.authorizedItem(w, r)
	if !ok {
		return
	}
	limit := 2
	if values := route.Query(r.URL.Query(), "Limit"); len(values) == 1 {
		n, err := strconv.Atoi(values[0])
		if err != nil || n < 0 {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		limit = n
	}
	var clips []frontend.Extra
	if i.host.Cinema != nil && limit > 0 && detail.Title.Type == "movie" {
		var err error
		clips, err = i.host.Cinema.Trailers(r.Context(), detailViewer(r, i), device(r), detail.Title.Key, limit)
		if err != nil {
			if errors.Is(err, frontend.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	items := make([]any, 0, len(clips))
	for _, clip := range clips {
		if clip.Key == 0 {
			continue
		}
		items = append(items, i.extraItem(detail, clip))
	}
	jsonReply(w, i.queryResult(items, len(items), 0))
}

func detailViewer(r *http.Request, i *instance) *frontend.Viewer {
	viewer, _, _ := i.viewer(r)
	return viewer
}

func (i *instance) authorizedItem(w http.ResponseWriter, r *http.Request) (frontend.ItemDetail, bool) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return frontend.ItemDetail{}, false
	}
	if i.host.Experience == nil {
		http.NotFound(w, r)
		return frontend.ItemDetail{}, false
	}
	key, err := ids.ParseRatingKeyGUID(r.PathValue("id"))
	if err != nil {
		scope, scopeErr := i.versionScope(r, viewer)
		if scopeErr != nil || i.host.VersionHandles == nil {
			http.NotFound(w, r)
			return frontend.ItemDetail{}, false
		}
		target, resolveErr := i.host.VersionHandles.Resolve(i.identityContext(r), scope, r.PathValue("id"))
		if resolveErr != nil || target.Item == 0 {
			http.NotFound(w, r)
			return frontend.ItemDetail{}, false
		}
		key = target.Item
	}
	detail, err := i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: key})
	if err != nil {
		http.NotFound(w, r)
		return frontend.ItemDetail{}, false
	}
	return detail, true
}

func (i *instance) extraPlayback(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, key frontend.RatingKey, playback bool) bool {
	if key.Kind() != frontend.KindExtra || i.host.ExtraStreams == nil {
		return false
	}
	clip, ok := i.host.ExtraStreams.Lookup(key)
	if !ok || clip.ParentKey == 0 || i.host.Experience == nil {
		http.NotFound(w, r)
		return true
	}
	if _, err := i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: clip.ParentKey}); err != nil {
		http.NotFound(w, r)
		return true
	}
	id := ids.RatingKeyGUID(key)
	item := i.extraItem(frontend.ItemDetail{Title: frontend.Title{Key: clip.ParentKey}}, clip)
	sources, _ := item["MediaSources"].([]any)
	if len(sources) != 1 {
		http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
		return true
	}
	source := sources[0].(map[string]any)
	if playback {
		container, rawURL, _, err := i.host.ExtraStreams.Open(r.Context(), key)
		if err != nil || !publicExtraURL(rawURL) {
			http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
			return true
		}
		if container != "" {
			source["Container"] = container
		}
		path := "/Videos/" + id + "/stream.mp4?Static=true"
		if prefix, ok := r.Context().Value(prefixKey{}).(string); ok && prefix != "" {
			path = prefix + path
		}
		source["Path"] = path
	}
	jsonReply(w, map[string]any{"MediaSources": []any{source}, "PlaySessionId": ""})
	return true
}

func (i *instance) serveExtra(w http.ResponseWriter, r *http.Request, viewer *frontend.Viewer, key frontend.RatingKey) {
	if i.host.ExtraStreams == nil {
		http.NotFound(w, r)
		return
	}
	clip, ok := i.host.ExtraStreams.Lookup(key)
	if !ok || clip.ParentKey == 0 || i.host.Experience == nil {
		http.NotFound(w, r)
		return
	}
	if _, err := i.host.Experience.Item(i.identityContext(r), frontend.ItemRequest{Viewer: viewer, Device: device(r), Key: clip.ParentKey}); err != nil {
		http.NotFound(w, r)
		return
	}
	_, rawURL, headers, err := i.host.ExtraStreams.Open(r.Context(), key)
	if err != nil || !publicExtraURL(rawURL) {
		http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
		return
	}
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, rawURL, nil)
	if err != nil {
		http.Error(w, "Playback unavailable", http.StatusServiceUnavailable)
		return
	}
	if r.Method == http.MethodHead {
		upstream.Method = http.MethodHead
	}
	if value := r.Header.Get("Range"); rangeHeader(value) {
		upstream.Header.Set("Range", value)
	}
	for name, value := range headers {
		if value == "" || strings.ContainsAny(name, "\r\n") || strings.ContainsAny(value, "\r\n") {
			continue
		}
		upstream.Header.Set(name, value)
	}
	response, err := i.extraClient.Do(upstream)
	if err != nil {
		http.Error(w, "Playback unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusPartialContent {
		http.Error(w, "Playback unavailable", http.StatusBadGateway)
		return
	}
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if value := response.Header.Get(name); value != "" && !strings.ContainsAny(value, "\r\n") {
			w.Header().Set(name, value)
		}
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "video/mp4")
	}
	w.WriteHeader(response.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, response.Body)
	}
}

func rangeHeader(value string) bool {
	if !strings.HasPrefix(value, "bytes=") || strings.ContainsAny(value, "\r\n") {
		return false
	}
	_, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	return err == nil && len(value) < 64
}

func publicExtraURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return false
	}
	host := parsed.Hostname()
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast()) {
		return false
	}
	switch strings.ToLower(host) {
	case "localhost", "metadata.google.internal":
		return false
	}
	return true
}

func newExtraHTTPClient(proxy func(*http.Request) (*url.URL, error)) *http.Client {
	if proxy == nil {
		proxy = http.ProxyFromEnvironment
	}
	return &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			Proxy:                 proxy,
			ResponseHeaderTimeout: 20 * time.Second,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !publicExtraURL(req.URL.String()) {
				return errors.New("refused trailer redirect")
			}
			return nil
		},
	}
}
