package mediabrowser

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

// capabilitiesQuery accepts the query-form declaration used by native clients.
func (i *instance) capabilitiesQuery(w http.ResponseWriter, r *http.Request) {
	viewer, _, err := i.viewer(r)
	if err != nil || viewer == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if i.host.Sessions == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	list := func(name string) []string {
		var out []string
		for _, value := range route.Query(r.URL.Query(), name) {
			for _, part := range strings.Split(value, ",") {
				if part = strings.TrimSpace(part); part != "" {
					out = append(out, part)
				}
			}
		}
		return out
	}
	flag := func(name string) (*bool, error) {
		values := route.Query(r.URL.Query(), name)
		if len(values) == 0 {
			return nil, nil
		}
		if len(values) != 1 {
			return nil, strconv.ErrSyntax
		}
		value, e := strconv.ParseBool(values[0])
		if e != nil {
			return nil, e
		}
		return &value, nil
	}
	media, err := flag("SupportsMediaControl")
	if err != nil {
		http.Error(w, "Bad Request", 400)
		return
	}
	persistent, err := flag("SupportsPersistentIdentifier")
	if err != nil {
		http.Error(w, "Bad Request", 400)
		return
	}
	remote, err := flag("SupportsRemoteControl")
	if err != nil {
		http.Error(w, "Bad Request", 400)
		return
	}
	declaration := frontend.SessionCapabilities{PlayableMediaTypes: list("PlayableMediaTypes"), SupportedCommands: list("SupportedCommands"), SupportsMediaControl: media, SupportsPersistentIdentifier: persistent, SupportsRemoteControl: remote}
	if err := i.host.Sessions.UpdateCapabilities(r.Context(), frontend.SessionAccess{Viewer: viewer, Device: device(r), Protocol: i.protocol}, declaration); err != nil {
		http.Error(w, "Unavailable", 503)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
