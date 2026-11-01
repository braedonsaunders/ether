package plex

import (
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// PMS's channel-tune route serializes a rolling subscription, rather than an
// ordinary play queue. Current native clients read grab.Metadata directly,
// then map its Media array. Keep that client contract separate from the Web
// bootstrap and from the older PMS binary's Video/Media serialization.

type nativeLiveGrab struct {
	Key            string         `plex:"key"`
	ID             string         `plex:"id"`
	SubscriptionID int64          `plex:"mediaSubscriptionID"`
	MediaIndex     int            `plex:"mediaIndex"`
	Status         string         `plex:"status"`
	Rolling        bool           `plex:"rolling"`
	Provider       string         `plex:"provider"`
	Metadata       model.Metadata `plex:"Metadata,object"`
}

type nativeLiveSubscription struct {
	Key      string           `plex:"key"`
	Type     int              `plex:"type"`
	Title    string           `plex:"title"`
	Channel  string           `plex:"channelIdentifier"`
	Grab     []nativeLiveGrab `plex:"MediaGrabOperation"`
	Timeline liveTimeline     `plex:"Timeline,object"`
}

type nativeLiveContainer struct {
	Size          int                      `plex:"size"`
	Subscriptions []nativeLiveSubscription `plex:"MediaSubscription"`
	Grabs         []nativeLiveGrab         `plex:"MediaGrabOperation"`
}

func nativeLiveRequest(r *http.Request) bool {
	pr := FromContext(r.Context())
	// PMS serves Apple, iOS and Android a channel-tune payload instead of the
	// web bootstrap. The match is the registry quirk plex-native-live-tune.
	if pr == nil {
		return false
	}
	return transcode.HasQuirk(pr.Product, pr.Platform, pr.Device, pr.Model, pr.Version, "plex-native-live-tune")
}

func (s *Server) nativeLiveSubscriptionFor(info livetv.SessionInfo) nativeLiveSubscription {
	md := s.liveSessionMetadata(info)
	md.Extra = append(md.Extra, model.A("source", "server://"+s.identity().MachineIdentifier), model.A("itemType", "live"))
	web := s.liveSubscriptionFor(info)
	return nativeLiveSubscription{
		Key: strconv.FormatInt(info.SubscriptionID, 10), Type: 4,
		Title: info.Channel.Name, Channel: info.Channel.ID, Timeline: web.Timeline,
		Grab: []nativeLiveGrab{{
			Key: "/media/grabbers/operations/" + info.ID, ID: info.ID,
			SubscriptionID: info.SubscriptionID, Status: "inprogress", Rolling: true,
			Provider: s.identity().MachineIdentifier, Metadata: md,
		}},
	}
}

func (s *Server) writeNativeLiveTune(w http.ResponseWriter, r *http.Request, info livetv.SessionInfo) {
	w.Header().Set("X-Plex-Activity", info.ActivityID)
	s.write(w, r, &nativeLiveContainer{Size: 1, Subscriptions: []nativeLiveSubscription{s.nativeLiveSubscriptionFor(info)}})
}

// A subscription key is not a stream UUID. Resolve it only through sessions the
// authenticated viewer can access; knowing a numeric ID never grants access.
func (s *Server) liveSubscriptionSession(r *http.Request, key string) (livetv.SessionInfo, error) {
	user := FromContext(r.Context()).User
	if id, err := strconv.ParseInt(key, 10, 64); err == nil {
		for _, info := range s.d.LiveTV.Sessions(r.Context(), user) {
			if info.SubscriptionID == id {
				return info, nil
			}
		}
		return livetv.SessionInfo{}, core.ErrNotFound
	}
	return s.d.LiveTV.Session(r.Context(), key, user)
}
