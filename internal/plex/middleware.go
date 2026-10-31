package plex

import (
	"bufio"
	"compress/gzip"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/identity"

	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/faillog"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type acceptanceContextKey struct{}

func withAcceptance(ctx context.Context, a auth.Acceptance) context.Context {
	return context.WithValue(ctx, acceptanceContextKey{}, a)
}

func acceptanceFrom(ctx context.Context) (auth.Acceptance, bool) {
	a, ok := ctx.Value(acceptanceContextKey{}).(auth.Acceptance)
	return a, ok && a.Valid()
}

const corsAllowHeaders = "Accept, Accept-Language, Content-Type, Range, X-Plex-Token, X-Plex-Client-Identifier, X-Plex-Product, " +
	"X-Plex-Version, X-Plex-Platform, X-Plex-Platform-Version, X-Plex-Device, X-Plex-Device-Name, X-Plex-Device-Vendor, " +
	"X-Plex-Device-Screen-Resolution, X-Plex-Model, X-Plex-Language, X-Plex-Features, X-Plex-Provides, X-Plex-Drm, " +
	"X-Plex-Container-Start, X-Plex-Container-Size, X-Plex-Client-Profile-Extra, X-Plex-Client-Profile-Name, " +
	"X-Plex-Client-Capabilities, X-Plex-Session-Identifier, X-Plex-Pms-Api-Version, X-Plex-Text-Format, X-Plex-Restriction-Profile, " +
	"X-Plex-Marketplace, X-Plex-Sync-Version, X-Plex-Http-Pipeline"

const corsExposeHeaders = "Content-Length, Content-Range, Content-Type, Date, Location, X-Plex-Protocol, " +
	"X-Plex-Container-Start, X-Plex-Container-Total-Size, X-Plex-Content-Original-Length, X-Plex-Content-Compressed-Length, X-Plex-Activity"

// commonHeaders sets X-Plex-Protocol and CORS on every response and answers
// preflights.
func (s *Server) commonHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Plex-Protocol", "1.0")
		// Plex routes serve data and upstream media, never the owner SPA
		// (which is mounted separately at /ether/). A provider can return
		// HTML/SVG even from a media URL; direct navigation must not execute
		// it on the origin that holds the owner's localStorage credential.
		// Leave media/image sources unrestricted for ordinary playback.
		// Artwork handlers replace this with their stricter image policy.
		h.Set("Content-Security-Policy", "script-src 'none'; object-src 'none'; base-uri 'none'; sandbox")
		h.Set("X-Content-Type-Options", "nosniff")
		// Like PMS, CORS headers are only sent to cross-origin (browser)
		// requests.
		if origin := r.Header.Get("Origin"); origin != "" {
			h.Set("Access-Control-Allow-Origin", origin)
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
		}
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, HEAD, OPTIONS")
			if req := r.Header.Get("Access-Control-Request-Headers"); req != "" {
				h.Set("Access-Control-Allow-Headers", req)
			} else {
				h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
			}
			h.Set("Access-Control-Max-Age", "1209600")
			h.Set("Content-Length", "0")
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// recoverer turns handler panics into 500s.
func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("handler panic", "path", r.URL.Path, "panic", v)
				writeHTTPError(w, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// statusRecorder captures the status code for logging.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (sr *statusRecorder) WriteHeader(code int) {
	if sr.status == 0 {
		sr.status = code
	}
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Write(b []byte) (int, error) {
	if sr.status == 0 {
		sr.status = http.StatusOK
	}
	n, err := sr.ResponseWriter.Write(b)
	sr.written += int64(n)
	return n, err
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := sr.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("hijack not supported")
}

func (sr *statusRecorder) Unwrap() http.ResponseWriter { return sr.ResponseWriter }

// requestLog logs requests at debug level and summarizes noisy 401 failures.
func (s *Server) requestLog(next http.Handler) http.Handler {
	log := s.log
	if log == nil {
		log = slog.Default()
	}
	failures := faillog.New(log.With("frontend", "plex"), "authentication")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(sr, r)
		if sr.status == http.StatusUnauthorized {
			failures.Fail(errors.New("unauthorized"))
			return
		}
		pr := FromContext(r.Context())
		s.log.Debug("request", "method", r.Method, "path", r.URL.Path, "status", sr.status,
			"dur", time.Since(start).Round(time.Millisecond), "product", pr.Product, "client", pr.ClientID,
			"range", r.Header.Get("Range"), "contentRange", sr.Header().Get("Content-Range"),
			"pageStart", pr.Start, "pageSize", pr.Size, "paged", pr.HasPage, "format", pr.Format, "includeChildren", r.URL.Query().Get("includeChildren"),
			"model", pr.Model, "device", pr.Device, "version", pr.Version, "platformVersion", pr.PlatformVersion, "peer", pr.RemoteIP.String())
		s.observeRequest(r, sr)
	})
}

// parse attaches the parsed request context.
func (s *Server) parse(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pr := ParseRequest(r, identity.ProxyPrefixes(s.cfg().Server.TrustedProxyCIDRs)...)
		next.ServeHTTP(w, r.WithContext(withRequest(r.Context(), pr)))
	})
}

// publicPaths need no token.
var publicPaths = map[string]bool{
	"/identity": true,
}

// authenticate maps the token to a user. Requests without a valid token get
// PMS's 401 body.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPaths[trimSlash(r.URL.Path)] {
			next.ServeHTTP(w, r)
			return
		}
		pr := FromContext(r.Context())
		if pr.Token == "" && (s.authenticateStreamGrant(r, pr) || s.authenticateLiveGrant(r, pr)) {
			if !s.acceptanceCurrent(r) {
				writeHTTPError(w, http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if pr.Token == "" || s.d.Auth == nil {
			writeHTTPError(w, http.StatusUnauthorized)
			return
		}
		u, receipt, err := s.acceptToken(r.Context(), pr.Token)
		switch {
		case err == nil && u != nil:
			if policy, ok := s.d.AccessPolicy.(core.UserPolicySource); ok {
				u = policy.UserPolicy(u)
			}
			if id := transcodeSessionID(r.URL.Path); id != "" {
				e, ok := s.transcodes.get(id)
				if !ok {
					writeHTTPError(w, http.StatusNotFound)
					return
				}
				if e.userID != u.ID {
					writeHTTPError(w, http.StatusForbidden)
					return
				}
			}
			pr.User = u
			if receipt.Valid() {
				r = r.WithContext(withAcceptance(r.Context(), receipt))
			}
			if !s.acceptanceCurrent(r) {
				writeHTTPError(w, http.StatusUnauthorized)
				return
			}
			if s.d.Hooks.OnClientSeen != nil {
				s.d.Hooks.OnClientSeen(pr.ClientID)
			}
			next.ServeHTTP(w, r)
		case err == nil, errors.Is(err, core.ErrUnauthorized), errors.Is(err, core.ErrNotFound):
			writeHTTPError(w, http.StatusUnauthorized)
		case errors.Is(err, r.Context().Err()):
			// client went away
		default:
			s.log.Warn("authentication failed", "err", err)
			writeHTTPError(w, http.StatusServiceUnavailable)
		}
	})
}

type tokenAcceptor interface {
	Accept(context.Context, string) (*core.User, auth.Acceptance, error)
	ReceiptCurrent(context.Context, auth.Acceptance) error
}

func (s *Server) acceptToken(ctx context.Context, token string) (*core.User, auth.Acceptance, error) {
	if acc, ok := s.d.Auth.(tokenAcceptor); ok {
		return acc.Accept(ctx, token)
	}
	u, err := s.d.Auth.Authenticate(ctx, token)
	return u, auth.Acceptance{}, err
}

// acceptanceCurrent rechecks a receipt minted for this request. Requests that
// did not mint one, including stream grants, stay on their own check.
func (s *Server) acceptanceCurrent(r *http.Request) bool {
	receipt, ok := acceptanceFrom(r.Context())
	if !ok {
		return true
	}
	checker, ok := s.d.Auth.(tokenAcceptor)
	if !ok {
		return false
	}
	return checker.ReceiptCurrent(r.Context(), receipt) == nil
}

func (s *Server) authenticateLiveGrant(r *http.Request, pr *Request) bool {
	if s.d.LiveTV == nil {
		return false
	}
	u, ok := s.d.LiveTV.AuthenticateMediaGrant(r, s.activeViewer)
	if !ok {
		return false
	}
	if policy, ok := s.d.AccessPolicy.(core.UserPolicySource); ok {
		u = policy.UserPolicy(u)
	}
	if fresh, ok := s.d.AccessPolicy.(interface {
		ConfirmPlayback(context.Context, *core.User, bool, time.Time) (*core.User, error)
	}); ok {
		confirmed, err := fresh.ConfirmPlayback(r.Context(), u, pr.ClientCaps(0).Remote, s.now())
		if err != nil && !errors.Is(err, core.ErrPolicyUnconfirmed) {
			return false
		}
		if err == nil {
			if confirmed == nil || confirmed.ID != u.ID {
				return false
			}
			u = confirmed
		} else if policy, ok := s.d.AccessPolicy.(core.PlaybackAccessPolicy); ok && !policy.CanPlay(u, pr.ClientCaps(0).Remote, s.now()) {
			return false
		}
	} else if policy, ok := s.d.AccessPolicy.(core.PlaybackAccessPolicy); ok && !policy.CanPlay(u, pr.ClientCaps(0).Remote, s.now()) {
		return false
	}
	pr.User = u
	return true
}

// ownerOnly restricts a handler to the server owner.
func (s *Server) ownerOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u := FromContext(r.Context()).User; u == nil || !u.IsOwner {
			writeHTTPError(w, http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

// writeHTTPError writes PMS's minimal HTML error body.
func writeHTTPError(w http.ResponseWriter, code int) {
	text := http.StatusText(code)
	body := "<html><head><title>" + text + "</title></head><body><h1>" + strconv.Itoa(code) + " " + text + "</h1></body></html>"
	h := w.Header()
	h.Set("Content-Type", "text/html")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Del("Content-Encoding")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}

// write encodes a response model in the client's format, gzip-compressed
// when the client accepts it and the body is worth compressing.
func (s *Server) write(w http.ResponseWriter, r *http.Request, v any) {
	s.writeStatus(w, r, http.StatusOK, v)
}

func (s *Server) writeStatus(w http.ResponseWriter, r *http.Request, status int, v any) {
	pr := FromContext(r.Context())
	// allowSync on library containers follows the viewer's download
	// permission; containers that don't carry the attribute keep not
	// carrying it.
	if mc, ok := v.(*model.MediaContainer); ok && mc.AllowSync != nil {
		*mc.AllowSync = s.downloadsAllowed(pr.User)
	}
	body, err := model.Marshal(v, pr.Format)
	if err != nil {
		s.log.Error("encode response", "path", r.URL.Path, "err", err)
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", pr.Format.ContentType())
	if h.Get("Cache-Control") == "" {
		h.Set("Cache-Control", "no-cache")
	}
	if mc, ok := v.(*model.MediaContainer); ok && mc.Offset != nil {
		h.Set("X-Plex-Container-Start", strconv.Itoa(*mc.Offset))
		if mc.TotalSize != nil {
			h.Set("X-Plex-Container-Total-Size", strconv.Itoa(*mc.TotalSize))
		}
	}
	if r.Method == http.MethodHead {
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		return
	}
	if len(body) > 1024 && acceptsGzip(r) {
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
		h.Set("X-Plex-Content-Original-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(body)
		_ = gz.Close()
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func acceptsGzip(r *http.Request) bool {
	for _, v := range r.Header.Values("Accept-Encoding") {
		for _, p := range strings.Split(v, ",") {
			name, params, _ := strings.Cut(strings.TrimSpace(p), ";")
			if strings.EqualFold(strings.TrimSpace(name), "gzip") && !zeroQuality(params) {
				return true
			}
		}
	}
	return false
}

// zeroQuality reports whether Accept-Encoding parameters carry q=0 (the
// coding is refused); "q=0.5" still accepts it.
func zeroQuality(params string) bool {
	for _, p := range strings.Split(params, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "q") {
			continue
		}
		q, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return err == nil && q == 0
	}
	return false
}

// container returns a MediaContainer with the attributes PMS puts on
// library responses.
func libraryContainer() *model.MediaContainer {
	return &model.MediaContainer{
		AllowSync:       model.Ptr(false),
		Identifier:      model.LibraryIdentifier,
		MediaTagPrefix:  model.MediaTagPrefix,
		MediaTagVersion: model.MediaTagVersion,
	}
}

// hubContainer is the container PMS uses for hub lists.
func hubContainer() *model.MediaContainer {
	return &model.MediaContainer{AllowSync: model.Ptr(false), Identifier: model.LibraryIdentifier}
}

// writeEmpty writes an empty MediaContainer.
func (s *Server) writeEmpty(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, &model.MediaContainer{})
}

// notAvailableRetryAfter is the Retry-After (seconds) sent with a 503 for a
// title that cannot play right now.
const notAvailableRetryAfter = "30"

// writeErr maps a component error to an HTTP status.
func (s *Server) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeHTTPError(w, http.StatusNotFound)
	case errors.Is(err, core.ErrUnauthorized):
		writeHTTPError(w, http.StatusUnauthorized)
	case errors.Is(err, core.ErrStreamLimit), errors.Is(err, transcode.ErrTooManyTranscodes):
		w.Header().Set("Retry-After", "5")
		writeHTTPError(w, http.StatusServiceUnavailable)
	case errors.Is(err, core.ErrNotAvailable), errors.Is(err, core.ErrUnreleased), errors.Is(err, core.ErrDebridAuth):
		// The title exists; nothing can play it right now (no release yet,
		// nothing cached, no sources, a rejected debrid key). A 404 here
		// reads as "this item is gone" and Plex apps show a bare "404
		// error"; 503 says "temporarily", and the reason travels in the
		// decision response the client fetched first.
		w.Header().Set("Retry-After", notAvailableRetryAfter)
		writeHTTPError(w, http.StatusServiceUnavailable)
	case errors.Is(err, r.Context().Err()):
		// client gone; nothing useful to write
	default:
		s.log.Warn("request failed", "path", r.URL.Path, "err", err)
		writeHTTPError(w, http.StatusInternalServerError)
	}
}
