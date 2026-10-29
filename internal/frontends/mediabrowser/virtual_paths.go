package mediabrowser

import (
	"net/http"
	"path"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// virtualPaths returns only host-authorized names from the actual WebDAV tree.
// Bounded batches share the host's collision lookup and avoid per-item scans.
func (i *instance) virtualPaths(r *http.Request, viewer *frontend.Viewer, keys []frontend.RatingKey) map[frontend.RatingKey]string {
	result := map[frontend.RatingKey]string{}
	if i.host.VirtualPaths == nil || viewer == nil {
		return result
	}
	for start := 0; start < len(keys); start += 50 {
		end := min(start+50, len(keys))
		requested := make(map[frontend.RatingKey]bool, end-start)
		for _, key := range keys[start:end] {
			requested[key] = true
		}
		batch, err := i.host.VirtualPaths.Paths(i.identityContext(r), viewer, device(r), keys[start:end])
		if err != nil {
			continue
		}
		for key, relative := range batch {
			if !requested[key] || relative == "" || strings.HasPrefix(relative, "/") || strings.ContainsAny(relative, "\\\r\n") || path.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, "../") {
				continue
			}
			result[key] = "/" + relative
		}
	}
	return result
}
