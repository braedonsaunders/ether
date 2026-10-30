package mediabrowser

import (
	"crypto/rand"
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// bitrateTest emits bounded random bytes without allocating the requested size.
func (i *instance) bitrateTest(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	values := route.Query(r.URL.Query(), "size")
	if len(values) > 1 {
		http.Error(w, "Invalid size", http.StatusBadRequest)
		return
	}
	size := int64(102400)
	if len(values) == 1 {
		size, err = strconv.ParseInt(values[0], 10, 64)
		if err != nil || size < 1 || size > 100000000 {
			http.Error(w, "Invalid size", http.StatusBadRequest)
			return
		}
	}
	buffer := make([]byte, 32*1024)
	if _, err = rand.Read(buffer); err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Cache-Control", "no-store, no-transform")
	for size > 0 {
		n := min(size, int64(len(buffer)))
		if _, err = w.Write(buffer[:n]); err != nil {
			return
		}
		size -= n
		if size > 0 {
			if _, err = rand.Read(buffer); err != nil {
				return
			}
		}
	}
}
