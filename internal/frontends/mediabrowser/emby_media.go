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
