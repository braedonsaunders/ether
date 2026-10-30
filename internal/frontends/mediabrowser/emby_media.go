package mediabrowser

import (
	"net/http"
	"strings"
)

func (i *instance) embyMediaHeaders(w http.ResponseWriter, r *http.Request) {
	// Broker direct-file URLs are grant-bound. Keep responses private while
	// allowing the client to cache bytes without intermediary transformations.
	if strings.HasSuffix(r.URL.Path, "/file") {
		w.Header().Set("Cache-Control", "private, no-transform")
	}
}

// embyStaticResponse applies the recorded static-video policy only after the
// broker has chosen a status. Authorization, source binding and range handling
// still belong to the broker. Denials and redirects must not be stored.
type embyStaticResponse struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *embyStaticResponse) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.wroteHeader = true
	switch status {
	case http.StatusOK, http.StatusPartialContent, http.StatusNotModified:
		// Preserve an explicit broker policy, including a stricter no-store policy.
		if w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", "private, no-transform")
		}
	default:
		w.Header().Set("Cache-Control", "no-store")
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *embyStaticResponse) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *embyStaticResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
