package livetv

import (
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/go-chi/chi/v5"
)

// Handler is the owner management API. Mount behind the application's owner
// authentication and CSRF middleware; it deliberately cannot authenticate Plex
// tokens on its own. Streaming uses the separately authenticated Plex adapter.
func (s *Service) Handler() http.Handler {
	r := chi.NewRouter()
	r.Get("/channels/{id}/timeline", s.timelineRoute)
	r.Get("/channels/{id}/rundown", func(w http.ResponseWriter, r *http.Request) {
		viewer, err := strconv.ParseInt(r.URL.Query().Get("viewer"), 10, 64)
		if err != nil {
			ownerResult(w, nil, fmt.Errorf("viewer must be positive"))
			return
		}
		v, e := s.Rundown(r.Context(), chi.URLParam(r, "id"), viewer)
		ownerResult(w, v, e)
	})
	owner := &core.User{IsOwner: true}
	r.Patch("/channels/bulk", func(w http.ResponseWriter, r *http.Request) {
		var change ChannelBatch
		if !ownerBody(w, r, &change) {
			return
		}
		n, err := s.UpdateChannels(r.Context(), change)
		ownerResult(w, map[string]int{"updated": n}, err)
	})
	r.Get("/channels", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.Channels(r.Context())
		if e == nil {
			s.redactAddonChannels(r.Context(), v)
		}
		ownerResult(w, v, e)
	})
	r.Get("/channels/summary", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.ChannelSummaries(r.Context())
		ownerResult(w, v, e)
	})
	r.Get("/channels/page", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		v, e := s.ChannelPage(r.Context(), ChannelPageQuery{
			Limit: limit, Offset: offset, Q: q.Get("q"), Filter: q.Get("filter"), Source: q.Get("source"),
		})
		if e == nil {
			s.redactAddonChannels(r.Context(), v.Channels)
		}
		ownerResult(w, v, e)
	})
	r.Get("/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.Channel(r.Context(), chi.URLParam(r, "id"))
		if e == nil {
			one := []Channel{v}
			s.redactAddonChannels(r.Context(), one)
			v = one[0]
		}
		ownerResult(w, v, e)
	})
	r.Get("/channels/{id}/sources/status", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.StreamSourceStatuses(r.Context(), chi.URLParam(r, "id"))
		ownerResult(w, v, e)
	})
	save := func(w http.ResponseWriter, r *http.Request) {
		var c Channel
		if !ownerBody(w, r, &c) {
			return
		}
		if id := chi.URLParam(r, "id"); id != "" {
			c.ID = id
		}
		if e := s.restoreAddonChannel(r.Context(), &c); e != nil {
			ownerResult(w, nil, e)
			return
		}
		v, e := s.SaveChannel(r.Context(), c)
		if e == nil {
			one := []Channel{v}
			s.redactAddonChannels(r.Context(), one)
			v = one[0]
		}
		ownerResult(w, v, e)
	}
	r.Post("/channels", save)
	r.Put("/channels/{id}", save)
	r.Put("/channels/{id}/viewers", func(w http.ResponseWriter, r *http.Request) {
		var change ChannelViewers
		if !ownerBody(w, r, &change) {
			return
		}
		ownerResult(w, map[string]bool{"ok": true}, s.SetChannelViewers(r.Context(), chi.URLParam(r, "id"), change))
	})
	r.Delete("/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		ownerResult(w, map[string]bool{"ok": true}, s.DeleteChannel(r.Context(), chi.URLParam(r, "id")))
	})
	r.Get("/sources", func(w http.ResponseWriter, r *http.Request) { v, e := s.Sources(r.Context()); ownerResult(w, v, e) })
	r.Get("/usage", func(w http.ResponseWriter, r *http.Request) { v, e := s.LiveUsage(r.Context()); ownerResult(w, v, e) })
	r.Get("/sources/progress/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, ok := s.ImportProgress(chi.URLParam(r, "id"))
		if !ok {
			ownerResult(w, nil, core.ErrNotFound)
			return
		}
		ownerResult(w, v, nil)
	})
	importSource := func(w http.ResponseWriter, r *http.Request) {
		var src Source
		if !ownerBody(w, r, &src) {
			return
		}
		if id := chi.URLParam(r, "id"); id != "" {
			src.ID = id
		}
		pending := src.Kind != "plex" && r.URL.Query().Get("pending") == "1" && src.Playlist == "" && src.PlaylistURL == "" && src.Guide == "" && src.GuideURL == ""
		ctx := r.Context()
		if id := r.URL.Query().Get("progress"); id != "" {
			ctx = withImportProgress(ctx, s.beginImportProgress(id))
			defer s.endImportProgress(id)
		}
		var v Source
		var e error
		if pending {
			v, e = s.SavePendingSource(ctx, src)
		} else {
			v, e = s.ImportSource(ctx, src)
		}
		ownerResult(w, v, e)
	}
	r.Post("/sources", importSource)
	r.Put("/sources/{id}", importSource)
	upload := func(part string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			max := int64(maxPlaylistBytes)
			if part == "guide" {
				max = maxGuideBytes
			}
			r.Body = http.MaxBytesReader(w, r.Body, max)
			v, err := s.ImportUpload(r.Context(), chi.URLParam(r, "id"), part, r.Body)
			if err != nil {
				var maxed *http.MaxBytesError
				if errors.As(err, &maxed) {
					kind := "playlist"
					if part == "guide" {
						kind = "guide"
					}
					err = limitf(kind, max, false)
				}
			}
			ownerResult(w, v, err)
		}
	}
	r.Post("/sources/{id}/playlist", upload("playlist"))
	r.Post("/sources/{id}/guide", upload("guide"))
	r.Post("/sources/{id}/refresh", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.RefreshSource(r.Context(), chi.URLParam(r, "id"))
		ownerResult(w, v, e)
	})
	r.Delete("/sources/{id}", func(w http.ResponseWriter, r *http.Request) {
		ownerResult(w, map[string]bool{"ok": true}, s.DeleteSource(r.Context(), chi.URLParam(r, "id")))
	})
	r.Get("/guide", func(w http.ResponseWriter, r *http.Request) {
		from, until, e := s.guideWindow(r)
		if e != nil {
			ownerResult(w, nil, e)
			return
		}
		ids := guideChannelIDs(r)
		if len(ids) > 4096 {
			ownerResult(w, nil, fmt.Errorf("too many guide channels"))
			return
		}
		v, e := s.GuideForChannels(r.Context(), owner, from, until, ids)
		ownerResult(w, v, e)
	})
	r.Get("/plex-guide/lineups", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.PlexLineups(r.Context(), r.URL.Query().Get("country"), r.URL.Query().Get("postalCode"))
		ownerResult(w, v, e)
	})
	s.guideRoutes(r)
	s.previewRoutes(r, owner)
	r.Get("/guide.xml", func(w http.ResponseWriter, r *http.Request) { s.ServeXMLTV(w, r, owner) })
	r.Get("/policies", func(w http.ResponseWriter, r *http.Request) { v, e := s.Policies(r.Context()); ownerResult(w, v, e) })
	r.Get("/policies/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if e != nil || id <= 0 {
			ownerResult(w, nil, fmt.Errorf("invalid viewer id"))
			return
		}
		v, e := s.Policy(r.Context(), id)
		ownerResult(w, v, e)
	})
	r.Put("/policies/{id}", func(w http.ResponseWriter, r *http.Request) {
		var p Policy
		if !ownerBody(w, r, &p) {
			return
		}
		id, e := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if e != nil {
			ownerResult(w, nil, fmt.Errorf("invalid viewer id"))
			return
		}
		p.UserID = id
		p.normalize()
		ownerResult(w, p, s.SavePolicy(r.Context(), p))
	})
	r.Post("/preview", func(w http.ResponseWriter, r *http.Request) {
		var c Channel
		if !ownerBody(w, r, &c) {
			return
		}
		if e := s.expand(r.Context(), &c); e != nil {
			ownerResult(w, nil, e)
			return
		}
		if e := validateChannel(&c); e != nil {
			ownerResult(w, nil, e)
			return
		}
		from, until := s.o.Now(), s.o.Now().Add(6*time.Hour)
		if raw := r.URL.Query().Get("from"); raw != "" {
			parsed, e := time.Parse(time.RFC3339, raw)
			if e != nil {
				ownerResult(w, nil, fmt.Errorf("from must be RFC3339"))
				return
			}
			from = parsed
		}
		if raw := r.URL.Query().Get("until"); raw != "" {
			parsed, e := time.Parse(time.RFC3339, raw)
			if e != nil {
				ownerResult(w, nil, fmt.Errorf("until must be RFC3339"))
				return
			}
			until = parsed
		}
		if until.Sub(from) > 8*24*time.Hour {
			until = from.Add(8 * 24 * time.Hour)
		}
		if c.Kind == "iptv" {
			// An unsaved mapping may preview a Plex channel before it is imported.
			ps, e := s.previewProgrammes(r.Context(), c, from, until)
			ownerResult(w, ps, e)
			return
		}
		ownerResult(w, Schedule(s.graphicsChannel(c), from, until), nil)
	})
	return gzipResponses(r)
}

// guideChannelIDs reads the optional channels filter. The whole lineup stays
// the default so existing callers keep their response.
func guideChannelIDs(r *http.Request) []string {
	raw := r.URL.Query()["channels"]
	if len(raw) == 0 {
		return nil
	}
	seen := map[string]bool{}
	ids := []string{}
	for _, value := range raw {
		for _, id := range strings.Split(value, ",") {
			id = strings.TrimSpace(id)
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return ids
}

// gzipResponses compresses the JSON owner API. The lineup and coverage
// payloads are tens of megabytes before compression; browsers negotiate this
// on their own and clients that do not ask for gzip see the old bytes.
func gzipResponses(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		w.Header().Del("Content-Length")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, zw: gz}, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	zw          *gzip.Writer
	wroteHeader bool
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.zw.Write(p)
}

func (w *gzipResponseWriter) Flush() {
	w.zw.Flush()
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func ownerBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 80<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		ownerResult(w, nil, fmt.Errorf("invalid request body: %w", err))
		return false
	}
	return true
}
func ownerResult(w http.ResponseWriter, v any, err error) {
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		status := 400
		if errors.Is(err, core.ErrNotFound) {
			status = 404
		}
		if errors.Is(err, core.ErrNotAvailable) {
			status = http.StatusServiceUnavailable
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "livetv", "message": err.Error()}})
		return
	}
	json.NewEncoder(w).Encode(v)
}
func (s *Service) guideWindow(r *http.Request) (time.Time, time.Time, error) {
	from := s.o.Now()
	until := from.Add(24 * time.Hour)
	var err error
	if raw := r.URL.Query().Get("from"); raw != "" {
		from, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return from, until, fmt.Errorf("from must be RFC3339")
		}
	}
	if raw := r.URL.Query().Get("until"); raw != "" {
		until, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return from, until, fmt.Errorf("until must be RFC3339")
		}
	} else {
		until = from.Add(24 * time.Hour)
	}
	return from, until, nil
}
func (s *Service) ServeXMLTV(w http.ResponseWriter, r *http.Request, u *core.User) {
	from, until, err := s.guideWindow(r)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	ps, err := s.Guide(r.Context(), u, from, until)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	cs, err := s.AllowedChannels(r.Context(), u)
	if err != nil {
		writeStreamError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	if r.Method == "HEAD" {
		return
	}
	fmt.Fprint(w, xml.Header)
	enc := xml.NewEncoder(w)
	root := xml.StartElement{Name: xml.Name{Local: "tv"}, Attr: []xml.Attr{{Name: xml.Name{Local: "generator-info-name"}, Value: "Ether"}}}
	enc.EncodeToken(root)
	for _, c := range cs {
		v := xmlChannel{ID: c.ID, Name: []xmlText{{Value: c.Name}}}
		v.Icon.Src = c.Logo
		enc.EncodeElement(v, xml.StartElement{Name: xml.Name{Local: "channel"}})
	}
	for _, p := range ps {
		v := xmlProgramme{Channel: p.ChannelID, Start: p.Start.UTC().Format("20060102150405 -0700"), Stop: p.End.UTC().Format("20060102150405 -0700"), Title: []xmlText{{Value: p.Item.Title}}, Desc: []xmlText{{Value: p.Item.Summary}}}
		v.Icon.Src = p.Item.Thumb
		if p.Item.EpisodeTitle != "" {
			v.SubTitle = []xmlText{{Value: p.Item.EpisodeTitle}}
		}
		for _, g := range p.Item.Genres {
			v.Category = append(v.Category, xmlText{Value: g})
		}
		if p.Item.ReleasedMS > 0 {
			v.Date = time.UnixMilli(p.Item.ReleasedMS).UTC().Format("20060102")
		}
		if p.Item.Season > 0 && p.Item.Episode > 0 {
			v.EpisodeNum = append(v.EpisodeNum, xmlEpisodeNum{System: "xmltv_ns", Value: fmt.Sprintf("%d.%d.", p.Item.Season-1, p.Item.Episode-1)})
		}
		if strings.HasPrefix(p.Item.GUID, "plex://") {
			v.EpisodeNum = append(v.EpisodeNum, xmlEpisodeNum{System: "plex", Value: p.Item.GUID})
		}
		if p.Item.Premiere {
			v.New = &struct{}{}
		}
		if p.Item.Rating != "" {
			v.Rating = append(v.Rating, struct {
				Value string `xml:"value"`
			}{p.Item.Rating})
		}
		enc.EncodeElement(v, xml.StartElement{Name: xml.Name{Local: "programme"}})
	}
	enc.EncodeToken(root.End())
	enc.Flush()
}
