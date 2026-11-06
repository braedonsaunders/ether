package plex

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/plex/model"
)

// PMS exposes these identifiers in profile-file order. Names are protocol
// identifiers, independent of Ether's capability registry and owner overrides.
var clientProfileNames = strings.Split(`Android
Asus Transformer Prime TF201
Chrome
Chromecast
Firefox
Generic
HTML TV App
Internet Explorer
Konvergo
Microsoft Edge
Mystery 3
Mystery 4
Mystery 5
Opera
Opera TV App
Panasonic Viera 3D TV
PlayStation 3
PlayStation 3 App
PlayStation 4 App
Plex Desktop
Plex Home Theater
Plex Home Theater-RaspberryPi
Plexamp
Popcorn Hour
Roku
Roku-3.1
Roku-5.x
Roku-5000X-6.0
Roku-6.x
Roku-7.x
RokuDLNA-3.1
RokuDLNA-4400X-7.x
RokuDLNA-5.1
RokuDLNA-5000X-6.x
RokuDLNA-5000X-7.x
RokuDLNA-6.x
RokuDLNA-6000X-7.x
Safari
Samsung
Samsung SmartTV
Samsung Tizen
Sonos
Sony Blu-ray
Sony Blu-ray 2013
Sony Bravia 2010
Sony Bravia 2011
Sony Bravia 2012
Sony Bravia 2013
Sony Streaming Player
Universal Mobile
Universal Recording
Universal TV
Unknown Browser
Vivaldi
Web
Western Digital TV Live
Wii U
Windows
Windows 10 Desktop
Windows 10 Mobile
Windows Phone
Xbox 360
Xbox 360 App
Xbox One
ZP
iOS
iOS-iPad-1,1
iOS-iPhone-1,1
iOS-iPhone-1,2
iOS-iPhone-2,1
iOS-iPhone-3,1
iOS-iPhone-3,2
iOS-iPhone-3,3
iOS-iPod-1,1
iOS-iPod-2,1
iOS-iPod-3,1
iOS-iPod-4,1
tvOS`, "\n")

var clientProfileAliases = map[string]string{
	"Chrome": "Web", "Internet Explorer": "Web", "Opera": "Web", "Safari": "Web", "Unknown Browser": "Web", "Vivaldi": "Web",
	"Mystery 3": "PlayStation 4 App", "Mystery 4": "PlayStation 4 App", "Mystery 5": "Xbox 360 App", "PlayStation 3 App": "PlayStation 4 App", "Opera TV App": "HTML TV App",
	"Plex Home Theater-RaspberryPi": "Raspberry PI", "Roku": "Roku-7.x", "Roku-6.x": "Roku-7.x",
	"iOS-iPad-1,1": "iOS-iPhone-3,1", "iOS-iPhone-1,1": "iOS-iPhone-2,1", "iOS-iPhone-1,2": "iOS-iPhone-2,1", "iOS-iPhone-3,2": "iOS-iPhone-3,1", "iOS-iPhone-3,3": "iOS-iPhone-3,1",
	"iOS-iPod-1,1": "iOS-iPhone-2,1", "iOS-iPod-2,1": "iOS-iPhone-2,1", "iOS-iPod-3,1": "iOS-iPhone-2,1", "iOS-iPod-4,1": "iOS-iPhone-3,1",
}

// profileParam preserves an explicitly empty header: it suppresses the query
// value for this name, while resolution may continue with platform and device.
func profileParam(r *http.Request, name string) string {
	if values, ok := r.Header[http.CanonicalHeaderKey(name)]; ok {
		if len(values) > 0 {
			return values[0]
		}
		return ""
	}
	for k, values := range r.URL.Query() {
		if strings.EqualFold(k, name) && len(values) > 0 {
			return values[len(values)-1]
		}
	}
	return ""
}

func resolvedPlayerProfile(pr *Request) string {
	if pr == nil {
		return ""
	}
	for _, candidate := range []string{pr.ProfileName, pr.Platform, pr.Device} {
		found := false
		for _, name := range clientProfileNames {
			if name == candidate {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		for i := 0; i < len(clientProfileNames); i++ {
			alias, ok := clientProfileAliases[candidate]
			if !ok {
				return candidate
			}
			candidate = alias
		}
	}
	return ""
}

func (s *Server) handleClientProfiles(w http.ResponseWriter, r *http.Request) {
	local := false
	if values, ok := r.URL.Query()["type"]; ok {
		raw := values[len(values)-1]
		n, err := strconv.Atoi(raw)
		if err != nil || n != 0 && n != 42 {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		local = n == 42
	}
	mc := &model.MediaContainer{}
	for _, name := range clientProfileNames {
		if local {
			switch name {
			case "Android", "iOS", "Universal Mobile", "Universal TV", "Windows Phone", "Windows", "Xbox One":
			default:
				continue
			}
		}
		title := name
		if name == "Plex Home Theater-RaspberryPi" {
			title = "Raspberry PI"
		}
		profile := model.ClientProfile{Key: name, Title: title}
		if local {
			profile.Selected = model.Ptr(name == "Universal TV")
		}
		mc.Profile = append(mc.Profile, profile)
	}
	mc.Size = len(mc.Profile)
	s.write(w, r, mc)
}
