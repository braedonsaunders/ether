package config

import (
	"crypto/rand"
	"encoding/hex"
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
		Usenet:       Usenet{MinIdle: 2, Repair: Repair{AdaptiveSummaries: true, SummaryRows: 4, MaxDownloadBytes: 512 << 20, LiveWait: Duration(2 * time.Second), AheadWait: Duration(90 * time.Second)}},
		Debrid:       Debrid{CachedOnly: true, RemoveUncached: true},
		Markers:      Markers{TheIntroDB: true},
		Database:     Database{Driver: DatabaseSQLite},
		Personalize: Personalize{
			PostPlay: true, PostPlayPrebuffer: true, MicroReasons: true, SessionIntent: true,
		},
		Extras: Extras{PrewarmBytes: 48 << 20, ResolverTimeout: Duration(20 * time.Second)},
		Server: Server{
			Port:               DefaultPort,
			BindAddress:        "0.0.0.0",
			FriendlyName:       "Ether",
			MoviesLibraryName:  "Movies",
			TVShowsLibraryName: "TV Shows",
			MachineIdentifier:  randomID(20),
			LogLevel:           "info",
			DefaultStreamLimit: 2,
		},
		Plex: Plex{
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
			RemoteMaxBitrateKbps: 8000,
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
		Updates: Updates{Channel: "stable", AutoUpdate: false, MaintenanceStart: "03:00", MaintenanceEnd: "06:00"},
	}
}

// DefaultPort is the HTTP port of a fresh install: Plex Media Server's. When
// it is taken, the server moves to a fallback port and saves it.
const DefaultPort = 32400

func DefaultQuality() Quality {
	return Quality{
		Default: "1080p",
		Remote:  "1080p",
		Profiles: []core.QualityProfile{
			{
				Name:             "4K",
				MaxHeight:        2160,
				MinHeight:        1080,
				AllowHDR:         true,
				AllowDolbyVision: true,
				PreferredSources: []core.Source{core.SourceRemux, core.SourceBluRay, core.SourceWEBDL, core.SourceWEBRip},
				PreferredCodecs:  []string{"hevc", "av1", "h264"},
				PreferredAudio:   []string{"truehd", "eac3", "dtshd", "ac3", "aac"},
				MinSizeMBPerMin:  15,
				MaxSizeMBPerMin:  1000,
			},
			{
				Name:             "1080p",
				MaxHeight:        1080,
				MinHeight:        720,
				PreferredSources: []core.Source{core.SourceWEBDL, core.SourceBluRay, core.SourceWEBRip, core.SourceHDTV},
				PreferredCodecs:  []string{"h264", "hevc"},
				PreferredAudio:   []string{"eac3", "ac3", "aac"},
				MinSizeMBPerMin:  5,
				MaxSizeMBPerMin:  150,
			},
			{
				Name:             "720p",
				MaxHeight:        720,
				MinHeight:        480,
				PreferredSources: []core.Source{core.SourceWEBDL, core.SourceWEBRip, core.SourceBluRay, core.SourceHDTV},
				PreferredCodecs:  []string{"h264"},
				PreferredAudio:   []string{"aac", "ac3", "eac3"},
				MinSizeMBPerMin:  2,
				MaxSizeMBPerMin:  60,
			},
		},
		Rules: []QualityRule{
			{Product: "Apple TV", Profile: "4K"},
		},
	}
}
