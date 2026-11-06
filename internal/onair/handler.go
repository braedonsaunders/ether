package onair

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-chi/chi/v5"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Handler must be mounted behind the application's owner authentication and CSRF
// guard, like livetv.Handler. MCP exclusions: these routes are console designer
// transport. Binary assets, multipart clips and previews cannot be represented by
// JSON MCP tools; look/reel transport accompanies the visual stage and explicit Save.
// Agent look drafts/saves use the separate proposal-backed builtin tools.
// Bindings is an ephemeral stage preview; it does not mutate server state.
// ResourceRoutes mounts W6's font and family resource handlers on this router.
func (s *Service) Handler(resources ...func(chi.Router)) http.Handler {
	r := chi.NewRouter()
	s.StudioRoutes(r)
	r.Post("/assets/import", s.importHandler(false))
	r.Post("/reels/{id}/clips/import", s.importHandler(true))
	r.Get("/fonts/{id}", func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("id", chi.URLParam(r, "id"))
		ServeFont(w, r)
	})

	r.Get("/system-screens", ServeSystemScreens)
	r.Get("/templates", func(w http.ResponseWriter, r *http.Request) { v, e := s.Templates(r.Context()); result(w, v, e) })
	r.Post("/scenes/validate", func(w http.ResponseWriter, r *http.Request) {
		var sc StyleScene
		if !body(w, r, &sc) {
			return
		}
		e := sc.Document.Validate()
		if e == nil && len(sc.Sounds) > 4 {
			e = fmt.Errorf("scene %q: use at most four sounds", sc.Name)
		}
		result(w, sc, e)
	})
	for _, mount := range resources {
		mount(r)
	}
	r.Get("/looks", func(w http.ResponseWriter, r *http.Request) { v, e := s.Looks(r.Context()); result(w, v, e) })
	r.Post("/looks", func(w http.ResponseWriter, r *http.Request) {
		var l Look
		if !body(w, r, &l) {
			return
		}
		l.ID = ""
		v, e := s.SaveLook(r.Context(), l)
		result(w, v, e)
	})
	r.Get("/looks/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.Look(r.Context(), chi.URLParam(r, "id"))
		if e == nil {
			s.mu.Lock()
			p := s.progress[v.ID]
			if p != nil {
				cp := *p
				v.Render = &cp
			}
			s.mu.Unlock()
		}
		result(w, v, e)
	})
	r.Put("/looks/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if _, e := s.Look(r.Context(), id); e != nil {
			result(w, nil, e)
			return
		}
		var l Look
		if !body(w, r, &l) {
			return
		}
		l.ID = id
		v, e := s.SaveLook(r.Context(), l)
		result(w, v, e)
	})
	r.Delete("/looks/{id}", func(w http.ResponseWriter, r *http.Request) {
		result(w, nil, s.DeleteLook(r.Context(), chi.URLParam(r, "id")))
	})
	for _, a := range []string{"duplicate", "detach"} {
		r.Post("/looks/{id}/"+a, func(w http.ResponseWriter, r *http.Request) {
			l, e := s.Look(r.Context(), chi.URLParam(r, "id"))
			if e != nil {
				result(w, nil, e)
				return
			}
			if a == "detach" && l.Pack == nil {
				result(w, l, nil)
				return
			}
			l.ID = ""
			l.Name += " copy"
			l.Pack = nil
			l.ChannelIDs = []string{}
			v, e := s.SaveLook(r.Context(), l)
			result(w, v, e)
		})
	}
	r.Get("/bindings", func(w http.ResponseWriter, r *http.Request) {
		if s.o.Bindings == nil {
			result(w, map[string]any{}, nil)
			return
		}
		at := time.Now()
		var e error
		if q := r.URL.Query().Get("at"); q != "" {
			at, e = time.Parse(time.RFC3339, q)
			if e != nil {
				result(w, nil, fmt.Errorf("invalid at time"))
				return
			}
		}
		v, e := s.o.Bindings(r.Context(), r.URL.Query().Get("channel"), r.URL.Query().Get("viewer"), at)
		result(w, v, e)
	})
	s.LibraryRoutes(r)
	r.Get("/assets", func(w http.ResponseWriter, r *http.Request) { v, e := s.Library(r.Context()); result(w, v, e) })
	r.Post("/assets", func(w http.ResponseWriter, r *http.Request) {
		if !multipart(w, r, assetLimit) {
			return
		}
		defer r.MultipartForm.RemoveAll()
		f, h, e := r.FormFile("file")
		if e != nil {
			result(w, nil, e)
			return
		}
		defer f.Close()
		a, e := s.UploadAsset(r.Context(), h.Filename, r.FormValue("kind"), r.FormValue("license"), f)
		result(w, a, e)
	})
	r.Get("/assets/{id}", func(w http.ResponseWriter, r *http.Request) {
		var b []byte
		var mime string
		e := s.db.QueryRowContext(r.Context(), "SELECT content,mime FROM onair_assets WHERE digest=?", chi.URLParam(r, "id")).Scan(&b, &mime)
		if e != nil {
			result(w, nil, ErrNotFound)
			return
		}
		w.Header().Set("ETag", `"`+chi.URLParam(r, "id")+`"`)
		w.Header().Set("Content-Type", mime)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		http.ServeContent(w, r, "asset", time.Time{}, bytes.NewReader(b))
	})
	r.Delete("/assets/{id}", func(w http.ResponseWriter, r *http.Request) {
		result(w, nil, s.DeleteAsset(r.Context(), chi.URLParam(r, "id")))
	})
	r.Get("/reels", func(w http.ResponseWriter, r *http.Request) { v, e := s.Reels(r.Context()); result(w, v, e) })
	r.Post("/reels", func(w http.ResponseWriter, r *http.Request) {
		var v Reel
		if !body(w, r, &v) {
			return
		}
		v.ID = ""
		out, e := s.SaveReel(r.Context(), v)
		result(w, out, e)
	})
	r.Get("/reels/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.Reel(r.Context(), chi.URLParam(r, "id"))
		result(w, v, e)
	})
	r.Put("/reels/{id}", func(w http.ResponseWriter, r *http.Request) {
		var v Reel
		if !body(w, r, &v) {
			return
		}
		v.ID = chi.URLParam(r, "id")
		if _, e := s.Reel(r.Context(), v.ID); e != nil {
			result(w, nil, e)
			return
		}
		out, e := s.SaveReel(r.Context(), v)
		result(w, out, e)
	})
	r.Delete("/reels/{id}", func(w http.ResponseWriter, r *http.Request) {
		result(w, nil, s.DeleteReel(r.Context(), chi.URLParam(r, "id")))
	})
	r.Post("/reels/{id}/clips", func(w http.ResponseWriter, r *http.Request) {
		if !multipart(w, r, 500<<20) {
			return
		}
		defer r.MultipartForm.RemoveAll()
		f, h, e := r.FormFile("file")
		if e != nil {
			result(w, nil, e)
			return
		}
		defer f.Close()
		v, e := s.UploadClip(r.Context(), chi.URLParam(r, "id"), h.Filename, f)
		result(w, v, e)
	})
	r.Put("/reels/{id}/clips/{clip}", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.Reel(r.Context(), chi.URLParam(r, "id"))
		if e != nil {
			result(w, nil, e)
			return
		}
		for _, c := range v.Clips {
			if c.ID == chi.URLParam(r, "clip") {
				var in ReelClip
				if !body(w, r, &in) {
					return
				}
				if len(in.Name) == 0 || len(in.Name) > 255 {
					result(w, nil, fmt.Errorf("invalid clip name"))
					return
				}
				c.Name = in.Name
				result(w, c, s.setClip(r.Context(), v.ID, c))
				return
			}
		}
		result(w, nil, ErrNotFound)
	})
	r.Delete("/reels/{id}/clips/{clip}", func(w http.ResponseWriter, r *http.Request) {
		result(w, nil, s.DeleteClip(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "clip")))
	})
	r.Get("/reels/{id}/clips/{clip}/preview", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.Reel(r.Context(), chi.URLParam(r, "id"))
		if e == nil {
			for _, c := range v.Clips {
				if c.ID == chi.URLParam(r, "clip") && c.Status == "ready" {
					p := filepath.Join(s.o.Dir, "reel-"+c.ID+".mp4")
					if _, e = os.Stat(p); e == nil {
						http.ServeFile(w, r, p)
						return
					}
					s.enqueue(renderJob{Hash: "reel-" + c.ID, convert: func(ctx context.Context) error { return s.convertClip(ctx, v.ID, c) }})
					result(w, nil, fmt.Errorf("preview materializing; retry shortly"))
					return
				}
			}
		}
		result(w, nil, ErrNotFound)
	})
	return r
}
func multipart(w http.ResponseWriter, r *http.Request, max int64) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max+(1<<20))
	if e := r.ParseMultipartForm(1 << 20); e != nil {
		if r.MultipartForm != nil {
			r.MultipartForm.RemoveAll()
		}
		http.Error(w, "upload exceeds limit or invalid multipart", http.StatusRequestEntityTooLarge)
		return false
	}
	return true
}
func body(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	d := json.NewDecoder(r.Body)
	if e := d.Decode(v); e != nil {
		result(w, nil, e)
		return false
	}
	if d.Decode(new(any)) != io.EOF {
		result(w, nil, fmt.Errorf("trailing JSON"))
		return false
	}
	return true
}
func result(w http.ResponseWriter, v any, e error) {
	w.Header().Set("Content-Type", "application/json")
	if e != nil {
		code := 400
		if errors.Is(e, ErrNotFound) {
			code = 404
		}
		if errors.Is(e, ErrUsed) {
			code = 409
		}
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "onair", "message": e.Error()}})
		return
	}
	if v == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	json.NewEncoder(w).Encode(v)
}
