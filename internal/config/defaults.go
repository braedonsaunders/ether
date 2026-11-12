package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Defaults returns a fresh config for a new install.
func Defaults(dataDir string) Config {
	return Config{
		Availability: DefaultAvailability(),
		// Preserve watch sync for installations predating the global switch.
		PlexServers: PlexServers{Enabled: true},
		Usenet:      Usenet{MinIdle: 2, Repair: Repair{AdaptiveSummaries: true, SummaryRows: 4, MaxDownloadBytes: 512 << 20, LiveWait: Duration(2 * time.Second), AheadWait: Duration(90 * time.Second)}},
		Debrid:      Debrid{CachedOnly: true, RemoveUncached: true},
		Markers:     Markers{TheIntroDB: true},
		Database:    Database{Driver: DatabaseSQLite},
		Personalize: Personalize{
			PostPlay: true, PostPlayPrebuffer: true, MicroReasons: true, SessionIntent: true,
		},
		Extras:   Extras{PrewarmBytes: 48 << 20, ResolverTimeout: Duration(20 * time.Second)},
		Outbound: Outbound{Indexers: true, Stremio: true, Debrid: true, Usenet: true},
		Server: Server{
			StartupAnimation:   true,
			Port:               DefaultPort,
			BindAddress:        "0.0.0.0",
			FriendlyName:       "Ether",
			MachineIdentifier:  randomID(20),
			LogLevel:           "info",
			DefaultStreamLimit: 2,
		},
		Plex: Plex{
			FileCheckTimeout:  Duration(8 * time.Second),
			ClientIdentifier:  randomID(16),
			UseUPnP:           true,
			SecureConnections: "preferred",
		},
		Cache: Cache{
			Dir:                  filepath.Join(dataDir, "cache"),
			BudgetBytes:          50 << 30,
			PrefetchNextEpisode:  true,
			PrefetchBytes:        64 << 20,
			KeepInProgressWarm:   true,
			NextEpisodeAt:        0.8,
			InProgressPinMaxAge:  Duration(30 * 24 * time.Hour),
			InProgressPinPercent: 50,
		},
		Catalog: Catalog{
			Language:        "en-US",
			Region:          "US",
			MinPopularity:   5,
			MinVotes:        100,
			RefreshInterval: Duration(24 * time.Hour),
		},
		Resolver: Resolver{
			HealthSamples:           12,
			SearchCacheTTL:          Duration(6 * time.Hour),
			UnavailableCooloff:      Duration(24 * time.Hour),
			MaxCandidates:           3,
			SameQualityAlternatives: 1,
		},
		Quality: DefaultQuality(),
		Transcode: Transcode{
			RTXVideo:             RTXVideo{Mode: "off", SourceMaxHeight: 720, TargetMaxHeight: 1080, Quality: 2, MaxConcurrent: 1},
			HWAccel:              "auto",
			TempDir:              filepath.Join(dataDir, "transcode"),
			MaxTranscodes:        2,
			RemoteMaxBitrateKbps: 0, // no cap away from home unless the owner sets one
			FastStart:            true,
			FastStartSegments:    2,
			FastStartSegment:     Duration(2 * time.Second),
			FastStartMaxHeight:   720,
			ABRLadder:            true,
			ABRLadderUnderCopy:   true, // legacy: abrLadder also covered video-copy sessions
			VBVReservoir:         true,
			VBVBufferSeconds:     8,
			SpikeGovernor:        false, // rewrites upcoming playlist entries; opt-in until field-proven
			SpikeMarginPercent:   125,
		},
		Downloads: Downloads{
			Enabled:              true,
			MaxConcurrent:        2,
			MaxConcurrentPerUser: 1,
			DiskBudgetBytes:      20 << 30,
			Expiry:               Duration(24 * time.Hour),
			MaxHeight:            1080,
		},
		// AutoUpdate is on for new installs. A stored config.json written
		// before it was (it may lack the key) keeps updates manual: see
		// keepStoredAutoUpdate.
		Updates: Updates{Enabled: true, Channel: "stable", AutoUpdate: true, MaintenanceStart: "03:00", MaintenanceEnd: "06:00"},
	}
}

// DefaultPort is the HTTP port of a fresh install: Plex Media Server's. When
// it is taken, the server moves to a fallback port and saves it.
const DefaultPort = 32400

// freshInstallQuality names the movie and the TV At home default
// explicitly on a brand-new install. It is not part of DefaultQuality
// because stored settings merge over the defaults key by key: a saved
// quality section without these keys must keep resolving through its own
// media-neutral default.
func freshInstallQuality(q *Quality) {
	if q.DefaultMovie == "" {
		q.DefaultMovie = q.Default
	}
	if q.DefaultTV == "" {
		q.DefaultTV = q.Default
	}
}

// DefaultQuality applies no server cap at home or away: every client gets
// the 4K profile, held to what the device itself can play (a phone stays
// at 1080p, a TV goes to 4K). The owner caps quality with a device or
// viewer rule, an "Away" profile, or a remote bitrate limit.
//
// The profiles are Radarr/Sonarr-standard (docs/quality-ranking.md): one
// quality group per resolution, highest first, and custom format scores
// for everything else. Each score level outweighs everything below it, so
// the order reads top down: the viewer's language on a foreign title,
// then a correctly labelled series year, release type, HDR, and last the
// codec, audio and how well the release plays on the device.
func DefaultQuality() Quality {
	q := Quality{
		Default: "4K",
		Remote:  "",
		Profiles: []core.QualityProfile{
			{
				Name:             "4K",
				MaxHeight:        2160,
				MinHeight:        720,
				AllowHDR:         true,
				AllowDolbyVision: true,
				MinSizeMBPerMin:  15,
				MaxSizeMBPerMin:  1000,
				QualityOrder:     ResolutionGroups(2160, 720),
				CustomFormatScores: etherScores(etherScoreSet{
					viewerLanguage: 20000, yearDiffers: -10000,
					sources: []core.Source{core.SourceRemux, core.SourceBluRay, core.SourceWEBDL, core.SourceWEBRip}, sourceStep: 2000,
					hdrStep: 500, hdr10: true,
					codecs: []string{"hevc", "av1", "h264"},
					audio:  []string{"truehd", "eac3", "dtshd", "ac3", "aac"},
				}),
			},
			{
				Name:            "1080p",
				MaxHeight:       1080,
				MinHeight:       720,
				MinSizeMBPerMin: 5,
				MaxSizeMBPerMin: 150,
				QualityOrder:    ResolutionGroups(1080, 720),
				CustomFormatScores: etherScores(etherScoreSet{
					viewerLanguage: 10000, yearDiffers: -5000,
					sources: []core.Source{core.SourceWEBDL, core.SourceBluRay, core.SourceWEBRip, core.SourceHDTV}, sourceStep: 1000,
					hdrStep: 200,
					codecs:  []string{"h264", "hevc"},
					audio:   []string{"eac3", "ac3", "aac"},
				}),
			},
			{
				Name:            "720p",
				MaxHeight:       720,
				MinHeight:       480,
				MinSizeMBPerMin: 2,
				MaxSizeMBPerMin: 60,
				QualityOrder:    ResolutionGroups(720, 480),
				CustomFormatScores: etherScores(etherScoreSet{
					viewerLanguage: 10000, yearDiffers: -5000,
					sources: []core.Source{core.SourceWEBDL, core.SourceWEBRip, core.SourceBluRay, core.SourceHDTV}, sourceStep: 1000,
					hdrStep: 200,
					codecs:  []string{"h264"},
					audio:   []string{"aac", "ac3", "eac3"},
				}),
			},
		},
	}
	// Each tier is offered for movies, then the same tiers for TV shows.
	tiers := q.Profiles
	q.Profiles = make([]core.QualityProfile, 0, len(core.ProfileMediaTypes)*len(tiers))
	for _, media := range core.ProfileMediaTypes {
		for _, p := range tiers {
			c := cloneProfile(p)
			c.MediaType = media
			c.CustomFormatScores = core.ScoresForMedia(c.CustomFormatScores, media)
			q.Profiles = append(q.Profiles, c)
		}
	}
	return q
}

// qualityHeights are the resolutions qualities are named for, highest first.
var qualityHeights = []int{2160, 1080, 720, 576, 480, 360}

// qualitySources are the release types a resolution group lists.
var qualitySources = []string{"remux", "bluray", "webdl", "webrip", "hdtv", "dvd", "unknown"}

// ResolutionGroups is the quality order of Ether's own profiles: one group
// per resolution from maxHeight down to minHeight (0 for no limit), every
// release type in each.
func ResolutionGroups(maxHeight, minHeight int) []core.QualityItem {
	var out []core.QualityItem
	for _, h := range qualityHeights {
		if (maxHeight > 0 && h > maxHeight) || h < minHeight {
			continue
		}
		item := core.QualityItem{Name: fmt.Sprintf("%dp", h)}
		for _, s := range qualitySources {
			item.Qualities = append(item.Qualities, fmt.Sprintf("%s-%d", s, h))
		}
		out = append(out, item)
	}
	return out
}

// etherScoreSet is one of Ether's own profiles' format scores: the level
// weights and the preference order within each level.
type etherScoreSet struct {
	viewerLanguage, yearDiffers int
	sources                     []core.Source // best first, sourceStep apart
	sourceStep, hdrStep         int
	hdr10                       bool // the profile allows HDR: an HDR10 layer earns a little
	codecs, audio               []string
}

// etherScores builds the score map of one of Ether's own profiles. In the
// lowest level codec preference is worth 6 points a place and audio 4;
// unlisted release types and Atmos earn 3, and the streaming formats keep
// their defaults.
func etherScores(s etherScoreSet) map[string]int {
	out := core.StreamingFormatScores()
	out[core.FormatViewerLanguage] = s.viewerLanguage
	out[core.FormatSeriesYearDiffers] = s.yearDiffers
	for _, src := range core.FormatSources {
		out[core.SourceFormat(src)] = 3
	}
	for i, src := range s.sources {
		out[core.SourceFormat(src)] = s.sourceStep * (len(s.sources) - i)
	}
	out[core.FormatDolbyVision] = s.hdrStep
	out[core.FormatHDR] = s.hdrStep
	if s.hdr10 {
		out[core.FormatHDR10] = 8
	}
	for i, c := range s.codecs {
		out[core.CodecFormat(c)] = 6 * (len(s.codecs) - i)
	}
	for i, a := range s.audio {
		out[core.AudioFormat(a)] = 4 * (len(s.audio) - i)
	}
	out[core.FormatAtmos] = 3
	out[core.FormatRepackProper] = 6
	out[core.FormatLowBitrate] = -60
	return out
}
