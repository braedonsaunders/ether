package livetv

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/go-chi/chi/v5"
)

// Handler is the owner management API. Mount behind the application's owner
// authentication and CSRF middleware; it deliberately cannot authenticate Plex
// tokens on its own. Streaming uses the separately authenticated Plex adapter.
func (s *Service) Handler() http.Handler {
	r := chi.NewRouter()
	owner := &core.User{IsOwner: true}
	r.Get("/channels", func(w http.ResponseWriter, r *http.Request) { v, e := s.Channels(r.Context()); ownerResult(w, v, e) })
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
		v, e := s.SaveChannel(r.Context(), c)
		ownerResult(w, v, e)
	}
	r.Post("/channels", save)
	r.Put("/channels/{id}", save)
	r.Delete("/channels/{id}", func(w http.ResponseWriter, r *http.Request) {
		ownerResult(w, map[string]bool{"ok": true}, s.DeleteChannel(r.Context(), chi.URLParam(r, "id")))
	})
	r.Get("/sources", func(w http.ResponseWriter, r *http.Request) { v, e := s.Sources(r.Context()); ownerResult(w, v, e) })
	importSource := func(w http.ResponseWriter, r *http.Request) {
		var src Source
		if !ownerBody(w, r, &src) {
			return
		}
		if id := chi.URLParam(r, "id"); id != "" {
			src.ID = id
		}
		pending := r.URL.Query().Get("pending") == "1" && src.Playlist == "" && src.PlaylistURL == "" && src.Guide == "" && src.GuideURL == ""
		var v Source
		var e error
		if pending {
			v, e = s.SavePendingSource(r.Context(), src)
		} else {
			v, e = s.ImportSource(r.Context(), src)
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
		v, e := s.Guide(r.Context(), owner, from, until)
		ownerResult(w, v, e)
	})
	s.guideRoutes(r)
	s.previewRoutes(r, owner)
	r.Get("/guide.xml", func(w http.ResponseWriter, r *http.Request) { s.ServeXMLTV(w, r, owner) })
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
		ownerResult(w, Schedule(c, s.o.Now(), s.o.Now().Add(6*time.Hour)), nil)
	})
	return r
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
