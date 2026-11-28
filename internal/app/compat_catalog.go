package app

import (
	"context"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/clientreg"
	"github.com/braedonsaunders/ether/internal/core"
)

// compatApp is one known client application from the registry catalogue,
// with built-in capabilities computed through clientreg.Playback and Policy.
type compatApp struct {
	Protocol            string   `json:"protocol"`
	Name                string   `json:"name"`
	Product             string   `json:"product,omitempty"`
	Devices             []string `json:"devices"`
	VideoCodecs         []string `json:"videoCodecs"`
	AudioCodecs         []string `json:"audioCodecs"`
	Containers          []string `json:"containers"`
	Subtitles           []string `json:"subtitles"`
	DolbyVisionProfiles []int    `json:"dolbyVisionProfiles,omitempty"`
	MaxHeight           int      `json:"maxHeight,omitempty"`
	HDR10               bool     `json:"hdr10,omitempty"`
	DolbyVision         bool     `json:"dolbyVision,omitempty"`
	DirectPlay          string   `json:"directPlay"`
	Note                string   `json:"note"`
	Certification       string   `json:"certification"`
	products            []string
}

// compatDevice shows defaults for a representative registry client identity,
// after owner overrides. Runtime app declarations and model-specific matches
// can refine these defaults; a row does not certify every model or display.
type compatDevice struct {
	ID                  string   `json:"id,omitempty"`
	Name                string   `json:"name"`
	DolbyVisionProfiles []int    `json:"dolbyVisionProfiles"`
	SubtitleCodecs      []string `json:"subtitleCodecs"`
	Notes               string   `json:"notes"`
	VideoCodecs         []string `json:"videoCodecs,omitempty"`
	MaxHeight           int      `json:"maxHeight,omitempty"`
	HDR10               bool     `json:"hdr10"`
	DolbyVision         bool     `json:"dolbyVision"`
	BuiltinAudioCodecs  []string `json:"builtinAudioCodecs"`
	BuiltinContainers   []string `json:"builtinContainers"`
	BuiltinMaxHeight    int      `json:"builtinMaxHeight,omitempty"`
	BuiltinHDR10        bool     `json:"builtinHdr10"`
	BuiltinDolbyVision  bool     `json:"builtinDolbyVision"`
}

type observedClient struct {
	Protocol string    `json:"protocol"`
	Name     string    `json:"name"`
	LastSeen time.Time `json:"lastSeen"`
}

type seenDevice struct {
	OverrideIndex    *int      `json:"overrideIndex,omitempty"`
	ClientIdentifier string    `json:"clientIdentifier"`
	Name             string    `json:"name"`
	Platform         string    `json:"platform"`
	Device           string    `json:"device"`
	Model            string    `json:"model,omitempty"`
	App              string    `json:"app"`
	Viewer           string    `json:"viewer"`
	LastSeen         time.Time `json:"lastSeen"`
	// Profile is the device profile record this client resolves to when
	// its hardware model picks a different one than its name would
	// (an Apple TV HD is "apple-tv-hd", not "apple-tv"); empty otherwise.
	Profile string `json:"profile,omitempty"`
}

type compatCatalog struct {
	SeenDevices []seenDevice                    `json:"seenDevices"`
	Apps        []compatApp                     `json:"apps"`
	Devices     []compatDevice                  `json:"devices"`
	Observed    []observedClient                `json:"observed"`
	Overrides   []core.ClientCapabilityOverride `json:"overrides"`
}

// builtinApps builds the app catalogue directly from clientreg.Apps() and
// evaluates each app's built-in capabilities through clientreg.Playback and
// clientreg.Policy so playback matching and the console table share one source
// of truth.
func builtinApps() []compatApp {
	items := clientreg.Apps()
	out := make([]compatApp, 0, len(items))
	for _, item := range items {
		product := item.Product
		if product == "" {
			product = item.Name
		}
		id := item.Identity
		if id.Product == "" {
			id.Product = product
		}
		play := clientreg.Playback(id, nil)
		policy := clientreg.Policy(id, nil)
		row := compatApp{
			Protocol:      item.Protocol,
			Name:          item.Name,
			Product:       item.Product,
			Devices:       item.Devices,
			VideoCodecs:   item.VideoCodecs,
			Subtitles:     item.Subtitles,
			DirectPlay:    item.DirectPlay,
			Note:          item.Note,
			Certification: item.Certification,
			products:      item.AllProducts(),
		}
		if play.ID != "generic" {
			if row.VideoCodecs == nil {
				row.VideoCodecs = play.Video
			}
			row.AudioCodecs = play.Audio
			row.Containers = play.Containers
			row.MaxHeight = play.MaxHeight
			row.HDR10 = play.HDR10
			row.DolbyVision = play.DolbyVision
		}
		if play.ID != "generic" || policy.ID != "generic" {
			row.DolbyVisionProfiles = policy.DolbyVisionProfiles
			if row.Subtitles == nil {
				row.Subtitles = policy.Subtitles
			}
		}
		out = append(out, row)
	}
	return out
}

func builtinDevices(overrides []core.ClientCapabilityOverride) []compatDevice {
	devices := clientreg.Devices()
	out := make([]compatDevice, 0, len(devices)+1)
	for _, item := range devices {
		id := item.Identity
		basePolicy := clientreg.Policy(id, nil)
		edited := clientreg.Policy(id, overrides)
		playBase := clientreg.Playback(id, nil)
		play := clientreg.Playback(id, overrides)
		recordID := basePolicy.ID
		if recordID == "generic" {
			recordID = playBase.ID
		}
		out = append(out, compatDevice{
			ID:                  recordID,
			Name:                item.Name,
			DolbyVisionProfiles: edited.DolbyVisionProfiles,
			SubtitleCodecs:      edited.Subtitles,
			Notes:               edited.Notes,
			VideoCodecs:         play.Video,
			MaxHeight:           play.MaxHeight,
			HDR10:               play.HDR10,
			DolbyVision:         play.DolbyVision,
			BuiltinAudioCodecs:  playBase.Audio,
			BuiltinContainers:   playBase.Containers,
			BuiltinMaxHeight:    playBase.MaxHeight,
			BuiltinHDR10:        playBase.HDR10,
			BuiltinDolbyVision:  playBase.DolbyVision,
		})
	}
	out = append(out, compatDevice{
		Name:           "DLNA TV or receiver",
		SubtitleCodecs: []string{"srt", "subrip"},
		Notes:          "The TV or receiver advertises what it can play.",
	})
	return out
}

func (a *App) compatibilityCatalog(ctx context.Context) (compatCatalog, error) {
	observed, err := a.observedClients(ctx)
	if err != nil {
		return compatCatalog{}, err
	}
	if observed == nil {
		observed = []observedClient{}
	}
	seen, err := a.seenPlaybackDevices(ctx)
	if err != nil {
		return compatCatalog{}, err
	}
	overrides := a.cfg.Read().Clients.Overrides
	if overrides == nil {
		overrides = []core.ClientCapabilityOverride{}
	}
	return compatCatalog{SeenDevices: seen, Apps: builtinApps(), Devices: builtinDevices(overrides), Observed: observed, Overrides: overrides}, nil
}

func (a *App) observedClients(ctx context.Context) ([]observedClient, error) {
	result, err := a.db.QueryContext(ctx, `SELECT surface, family, last_seen FROM frontend_client_families ORDER BY surface, family`)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	var rows []observedClient
	for result.Next() {
		var surface, family string
		var seen int64
		if err := result.Scan(&surface, &family, &seen); err != nil {
			return nil, err
		}
		rows = append(rows, observedClient{Protocol: surface, Name: family, LastSeen: time.Unix(seen, 0).UTC()})
	}
	return rows, result.Err()
}

// The registry's last-seen index supplies at most 100 rows. Each latest play
// is a primary-key lookup, independent of the size of playback history.
// seenDeviceName makes a seen client's name tell identical boxes apart.
// Clients often report only a generic name ("Apple TV" for every Apple
// TV); when that is all the name says and the hardware model is known, the
// model's product name ("Apple TV HD", "Apple TV 4K (3rd gen)") replaces
// it. A name the owner chose ("Living room") is kept.
func seenDeviceName(name, device, model string, id clientreg.Identity) string {
	id.Model = model
	product := clientreg.ModelName(clientreg.CanonicalModel(id))
	if product == "" {
		// Android TV boxes put the model code in the device field (AFTMM).
		product = clientreg.ModelName(device)
	}
	if product == "" {
		return name
	}
	trimmed := strings.TrimSpace(name)
	family := clientreg.Policy(id, nil).Family
	if trimmed == "" || strings.EqualFold(trimmed, device) || strings.EqualFold(trimmed, model) || (family != "" && strings.EqualFold(trimmed, family)) {
		return product
	}
	return name
}

func (a *App) seenPlaybackDevices(ctx context.Context) ([]seenDevice, error) {
	rows, err := a.db.QueryContext(ctx, `SELECT d.client_identifier,d.name,d.platform,COALESCE(p.device_match,''),COALESCE(p.model,''),COALESCE(p.product,''),COALESCE(p.username,''),d.last_seen FROM (SELECT * FROM stats_device_registry WHERE last_seen>0 ORDER BY last_seen DESC,id DESC LIMIT 100) d LEFT JOIN stats_plays p ON p.id=d.latest_play_id ORDER BY d.last_seen DESC,d.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []seenDevice{}
	overrides := a.cfg.Read().Clients.Overrides
	for rows.Next() {
		var d seenDevice
		var seen int64
		if err := rows.Scan(&d.ClientIdentifier, &d.Name, &d.Platform, &d.Device, &d.Model, &d.App, &d.Viewer, &seen); err != nil {
			return nil, err
		}
		d.LastSeen = time.Unix(seen, 0).UTC()
		// Version is unavailable for historical observations; do not claim a
		// version-scoped edit applies when that cannot be established.
		identity := clientreg.Identity{Product: d.App, Platform: d.Platform, Device: d.Device, DeviceName: d.Name, Model: d.Model}
		d.Name = seenDeviceName(d.Name, d.Device, d.Model, identity)
		if d.Model != "" {
			generic := identity
			generic.Model = ""
			if clientreg.Playback(identity, nil).ID != clientreg.Playback(generic, nil).ID {
				d.Profile = clientreg.Policy(identity, nil).ID
				if d.Profile == "generic" {
					d.Profile = clientreg.Playback(identity, nil).ID
				}
			}
		}
		if matched := clientreg.MatchingOverrides(identity, overrides); len(matched) > 0 {
			index := matched[len(matched)-1]
			d.OverrideIndex = &index
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
