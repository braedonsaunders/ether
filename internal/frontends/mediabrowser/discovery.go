package mediabrowser

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// RespondDiscovery describes only this enabled surface; the host owns the
// shared UDP socket and supplies a verified, reachable endpoint.
func (i *instance) RespondDiscovery(_ context.Context, request frontend.DiscoveryRequest) ([]byte, bool) {
	expected := "who is JellyfinServer?"
	if i.protocol == "emby" {
		expected = "who is EmbyServer?"
	}
	if !strings.EqualFold(string(request.Data), expected) {
		return nil, false
	}
	settings := frontend.Settings{}
	if i.host.Config != nil {
		settings = i.host.Config()
	}
	if enabled, ok := settings["discovery"].(bool); ok && !enabled {
		return nil, false
	}
	endpoint, err := url.Parse(request.Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, false
	}
	name := frontend.AdvertisedName(settings)
	response := map[string]string{"Address": request.Endpoint, "Id": i.info.ID, "Name": name}
	if i.protocol == "jellyfin" {
		response["EndpointAddress"] = endpoint.Hostname()
	}
	encoded, err := json.Marshal(response)
	return encoded, err == nil
}

var _ frontend.DiscoveryResponder = (*instance)(nil)
