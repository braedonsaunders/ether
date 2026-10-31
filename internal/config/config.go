// Package config is Ether's owner-editable settings, stored as JSON in the
// data directory. The web UI edits it through internal/admin; components get
// a snapshot at construction and subscribe for changes.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

type Config struct {
	// Frontends selects protocol listeners by their registered ID.
	Frontends map[string]Frontend `json:"frontends,omitempty"`
	// FrontendPorts are node-local listener port overrides.
	FrontendPorts map[string]int `json:"frontendPorts,omitempty"`
	Posters       Posters        `json:"posters"`
	Availability  Availability   `json:"availability"`
	// SharedCache is the cluster-wide NAS policy. Mount paths stay in Cache.
	SharedCache   *Cache        `json:"sharedCache,omitempty"`
	Server        Server        `json:"server"`
	Plex          Plex          `json:"plex"`
	Usenet        Usenet        `json:"usenet"`
	Indexers      []Indexer     `json:"indexers"`
	Cache         Cache         `json:"cache"`
	Catalog       Catalog       `json:"catalog"`
	Resolver      Resolver      `json:"resolver"`
	Quality       Quality       `json:"quality"`
	Transcode     Transcode     `json:"transcode"`
	Trakt         Trakt         `json:"trakt"`
	Simkl         Simkl         `json:"simkl"`
	OpenSubtitles OpenSubtitles `json:"openSubtitles"`
	LLM           LLM           `json:"llm"`
	Updates       Updates       `json:"updates"`
	Spike         Spike         `json:"spike"`
	Downloads     Downloads     `json:"downloads"`
	Markers       Markers       `json:"markers"`
	Personalize   Personalize   `json:"personalize"`
	Extras        Extras        `json:"extras"`
	Debrid        Debrid        `json:"debrid"`
	// Database is node-local storage selection (SQLite or PostgreSQL).
	Database      Database     `json:"database"`
	IndexerSites  IndexerSites `json:"indexerSites"`
	SetupComplete bool         `json:"setupComplete"`
}

// Frontend holds the enabled state of a protocol front end.
type Frontend struct {
	Enabled  bool           `json:"enabled"`
	Settings map[string]any `json:"settings,omitempty"`
}

type Server struct {
	// TrustedProxyCIDRs are proxy peer networks whose forwarding headers are trusted.
	TrustedProxyCIDRs []string `json:"trustedProxyCIDRs,omitempty"`
	// Port for the Plex API and web UI. PMS uses 32400.
	Port         int    `json:"port"`
	BindAddress  string `json:"bindAddress"`
	FriendlyName string `json:"friendlyName"`
	// MoviesLibraryName is the Movies section title shown in Plex apps.
	// Empty falls back to "Movies".
	MoviesLibraryName string `json:"moviesLibraryName"`
	// TVShowsLibraryName is the TV Shows section title shown in Plex apps.
	// Empty falls back to "TV Shows".
	TVShowsLibraryName string `json:"tvShowsLibraryName"`
	// MachineIdentifier is generated on first run and never changes.
	MachineIdentifier string `json:"machineIdentifier"`
	LogLevel          string `json:"logLevel"`
	// DefaultStreamLimit applies to users without their own limit.
	DefaultStreamLimit int `json:"defaultStreamLimit"`
	MaxStreams         int `json:"maxStreams"` // server-wide cap, 0 = derived from Usenet connections
	// ClusterWeight is this node's weight when a cluster divides
	// account-wide budgets (Usenet provider connections) among live nodes;
	// a node with weight 2 gets twice the share of a node with weight 1.
	// 0 means 1. Node-local; ignored on a standalone server.
	ClusterWeight int `json:"clusterWeight,omitempty"`
	// WebFrameOrigins permits embedding the owner UI in these exact HTTP(S)
	// origins (for example a dashboard). Empty denies framing everywhere.
	WebFrameOrigins []string `json:"webFrameOrigins,omitempty"`
}

type Plex struct {
	CompatibilityVersion       string `json:"compatibilityVersion,omitempty"`
	LatestCompatibilityVersion string `json:"latestCompatibilityVersion,omitempty"`
	// TrackLatestVersion advertises the newest public PMS release instead of
	// the version Ether's responses were verified against. Plex apps enable
	// features by server version, so tracking can reach endpoints Ether lacks.
	TrackLatestVersion bool `json:"trackLatestVersion,omitempty"`
	// Deprecated: retained for config compatibility; latest tracking is now opt-in.
	DisableVersionTracking bool `json:"disableVersionTracking,omitempty"`
	// Token is the owner's plex.tv token from sign-in; the server token.
	Token            string `json:"token"`
	ClientIdentifier string `json:"clientIdentifier"` // X-Plex-Client-Identifier Ether uses towards plex.tv
	OwnerPlexID      int64  `json:"ownerPlexId"`
	OwnerUsername    string `json:"ownerUsername"`
	Claimed          bool   `json:"claimed"`
	// Remote access
	RemoteAccess bool     `json:"remoteAccess"`
	UseUPnP      bool     `json:"useUpnp"`
	PublicPort   int      `json:"publicPort"` // manual port forward; 0 = same as Server.Port
	PublicHost   string   `json:"publicHost"` // override detected WAN IP
	CustomURLs   []string `json:"customUrls"`
	// TLS certificate for *.plex.direct (or a self-signed fallback).
	CertFile          string `json:"certFile"`
	KeyFile           string `json:"keyFile"`
	CertHash          string `json:"certHash"`          // plex.direct subdomain hash
	SecureConnections string `json:"secureConnections"` // "required", "preferred", "disabled"
}

type Usenet struct {
	Repair    Repair           `json:"repair"`
	Providers []UsenetProvider `json:"providers"`
	// MinIdle is how many connections per primary (non-backfill) provider
	// are kept warm (dialled, authenticated, DATE-pinged) at all times so
	// Play never pays a cold handshake. 0 = default (2); negative = off
	// (connections are dialled lazily and expire when idle). A provider's
	// own MinIdle overrides this.
	MinIdle int `json:"minIdle"`
}

// Repair bounds opportunistic PAR2 reconstruction. Disabled until the owner
// opts in; the live benchmark enables it explicitly without editing config.
type Repair struct {
	AdaptiveSummaries bool     `json:"adaptiveSummaries"`
	Enabled           bool     `json:"enabled"`
	WholeSlices       bool     `json:"wholeSlices,omitempty"`
	SummaryRows       int      `json:"summaryRows"`
	MaxDownloadBytes  int64    `json:"maxDownloadBytes"`
	LiveWait          Duration `json:"liveWait"`
	AheadWait         Duration `json:"aheadWait"`
}

type UsenetProvider struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	TLS         bool   `json:"tls"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Connections int    `json:"connections"`
	// Priority: lower is tried first. Block accounts set Backfill.
	Priority int  `json:"priority"`
	Backfill bool `json:"backfill"` // only used for articles missing elsewhere
	Enabled  bool `json:"enabled"`
	// MinIdle overrides Usenet.MinIdle for this provider: 0 = inherit the
	// global setting, positive = that many warm connections, negative =
	// no warm floor for this provider. Backfill providers are never kept
	// warm regardless.
	MinIdle int `json:"minIdle,omitempty"`
}

type Indexer struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"` // base URL; /api is appended
	APIKey     string `json:"apiKey"`
	DailyLimit int    `json:"dailyLimit"` // optional personal API cap per UTC day; 0 = automatic service limits
	DailyGrabs int    `json:"dailyGrabs"` // optional personal NZB cap per UTC day; 0 = automatic service limits
	Priority   int    `json:"priority"`
	Enabled    bool   `json:"enabled"`
	// Categories override; empty uses Newznab defaults (2000 movies, 5000 TV).
	MovieCategories []int `json:"movieCategories"`
	TVCategories    []int `json:"tvCategories"`
	// Protocol is "usenet" (Newznab, the default when empty) or "torrent"
	// (Torznab; results stream through a debrid provider).
	Protocol string `json:"protocol,omitempty"`
	// Kind selects how the indexer is searched: "" or IndexerKindNewznab
	// for a Newznab/Torznab API at URL, IndexerKindCardigann for a built-in
	// site run from an owner-installed definition, IndexerKindStremio for a
	// Stremio stream addon. For a built-in site URL is the site address and
	// Protocol follows the definition; for an addon URL is its install URL
	// and results are torrents.
	Kind string `json:"kind,omitempty"`
	// Definition is the installed definition id of a built-in site.
	Definition string `json:"definition,omitempty"`
	// Settings are the definition's setting values by name.
	Settings map[string]string `json:"settings,omitempty"`
	// SecretSettings are the definition's password-type settings. They are
	// sealed at rest and redacted in API responses like API keys.
	SecretSettings map[string]string `json:"secretSettings,omitempty"`
}

// Indexer kinds (Indexer.Kind).
const (
	IndexerKindNewznab   = "newznab"
	IndexerKindCardigann = "cardigann"
	IndexerKindStremio   = "stremio"
)

// IsTorrent reports whether the indexer's results are torrents (Torznab or
// a Stremio addon).
func (ix Indexer) IsTorrent() bool { return ix.Protocol == "torrent" || ix.IsStremio() }

// IsBuiltin reports whether the indexer is a built-in site.
func (ix Indexer) IsBuiltin() bool { return ix.Kind == IndexerKindCardigann }

// IsStremio reports whether the indexer is a Stremio stream addon.
func (ix Indexer) IsStremio() bool { return ix.Kind == IndexerKindStremio }

// UsesBackend reports whether the indexer is searched through a backend
// rather than the Newznab API: a built-in site or a Stremio addon.
func (ix Indexer) UsesBackend() bool { return ix.IsBuiltin() || ix.IsStremio() }

type Cache struct {
	Storage string `json:"storage,omitempty"` // local (default) or shared; restart required

	PreloadEnabled         bool   `json:"preloadEnabled,omitempty"`
	PreloadBudgetBytes     int64  `json:"preloadBudgetBytes,omitempty"`
	PreloadMaxTitles       int    `json:"preloadMaxTitles,omitempty"`
	PreloadRateMiB         int    `json:"preloadRateMiB,omitempty"`
	PreloadDuringPlayback  bool   `json:"preloadDuringPlayback,omitempty"`
	PreloadMinFreeBytes    int64  `json:"preloadMinFreeBytes,omitempty"`
	PreloadSource          string `json:"preloadSource,omitempty"`
	PreloadContent         string `json:"preloadContent,omitempty"`         // mixed (default), movies, tv
	PreloadHouseholdWeight *int   `json:"preloadHouseholdWeight,omitempty"` // 0–100; nil = 65
	PreloadHouseholdScope  string `json:"preloadHouseholdScope,omitempty"`  // home (default), all
	PreloadEpisodesAhead   int    `json:"preloadEpisodesAhead,omitempty"`   // per-show shelf window; 0 = 3
	PreloadHistoryDays     int    `json:"preloadHistoryDays,omitempty"`     // 0 = 90

	// Mode: empty/demand retains fetched bytes; adaptive favours replayed titles;
	// off bypasses persistent media writes. Zero budget remains unlimited.
	Mode               string `json:"mode,omitempty"`
	RetentionDays      int    `json:"retentionDays,omitempty"`
	ReplayBoostHours   int    `json:"replayBoostHours,omitempty"`
	DisableSpeculation bool   `json:"disableSpeculation,omitempty"`
	DetailWarmup       string `json:"detailWarmup,omitempty"` // empty/on or off
	DetailBytes        int64  `json:"detailBytes,omitempty"`  // zero warms 40s of source media, capped at 256 MiB

	Dir         string `json:"dir"`
	BudgetBytes int64  `json:"budgetBytes"`
	// Prefetch
	PrefetchNextEpisode bool    `json:"prefetchNextEpisode"`
	PrefetchBytes       int64   `json:"prefetchBytes"` // opening bytes pulled ahead
	KeepInProgressWarm  bool    `json:"keepInProgressWarm"`
	NextEpisodeAt       float64 `json:"nextEpisodeAt"` // fraction of current episode, e.g. 0.8
	// InProgressPinMaxAge: an in-progress title nobody has watched for this
	// long stops pinning its release in the cache (0 = default 30 days).
	InProgressPinMaxAge Duration `json:"inProgressPinMaxAge,omitempty"`
	// InProgressPinPercent caps the share of BudgetBytes that in-progress
	// pins may hold; the least recently watched titles lose their pin first
	// (0 = default 50, max 100).
	InProgressPinPercent int `json:"inProgressPinPercent,omitempty"`
}

type Catalog struct {
	// ShowUpcomingEpisodes adds metadata-only scheduled episodes to airing seasons.
	ShowUpcomingEpisodes bool `json:"showUpcomingEpisodes"`
	// GateMoviesByHomeRelease waits for a worldwide digital or physical release.
	GateMoviesByHomeRelease bool `json:"gateMoviesByHomeRelease"`
	// HideFranchiseCollections hides automatic TMDB groups from Plex's
	// Collections tab. The zero value preserves existing libraries.
	HideFranchiseCollections bool                    `json:"hideFranchiseCollections"`
	Exclusions               []core.LibraryExclusion `json:"exclusions,omitempty"`
	TMDBKey                  string                  `json:"tmdbKey"`  // owner override; empty uses the bundled key
	Language                 string                  `json:"language"` // TMDB language, e.g. "en-US"
	Region                   string                  `json:"region"`
	MinPopularity            float64                 `json:"minPopularity"`
	MinVotes                 int                     `json:"minVotes"`
	IncludeAdult             bool                    `json:"includeAdult"`
	RefreshInterval          Duration                `json:"refreshInterval"`
}

type Resolver struct {
	HealthRace           int                 `json:"healthRace"`      // 0 = ranked checks; >0 = first healthy of bounded concurrent candidates
	OptimisticStart      bool                `json:"optimisticStart"` // return top NZB before background health check
	CustomFormats        []core.CustomFormat `json:"customFormats,omitempty"`
	CustomFormatScores   map[string]int      `json:"customFormatScores,omitempty"`
	MinCustomFormatScore *int                `json:"minCustomFormatScore,omitempty"`
	AllowCam             bool                `json:"allowCam"` // owner setting, off by default
	HealthSamples        int                 `json:"healthSamples"`
	SearchCacheTTL       Duration            `json:"searchCacheTtl"`
	UnavailableCooloff   Duration            `json:"unavailableCooloff"` // hide titles with no release this long
	MaxCandidates        int                 `json:"maxCandidates"`
	BlockedGroups        []string            `json:"blockedGroups"`
	BlockedTerms         []string            `json:"blockedTerms"`
	// SourcePreference orders releases of equivalent quality by source:
	// "auto" (or empty) puts the source that measurably starts faster
	// first, and cached debrid first for popular titles until both
	// sources have measurements; "usenet" or "debrid" always prefer one.
	// Quality is never traded for speed (see resolver/routing.go).
	SourcePreference string `json:"sourcePreference,omitempty"`
	// SourceScoreMargin is how far below the leader's score a release of
	// the same quality tier may be and still be moved first for its
	// source (0 = 25).
	SourceScoreMargin float64 `json:"sourceScoreMargin,omitempty"`
}

type Quality struct {
	// Profiles by name. Rules pick a profile per client.
	Profiles []core.QualityProfile `json:"profiles"`
	Rules    []QualityRule         `json:"rules"`
	Default  string                `json:"default"`
	Remote   string                `json:"remote"` // profile for remote clients, "" = same as default
}

// QualityRule selects a profile when all non-empty matchers match.
type QualityRule struct {
	Protocol core.Protocol `json:"protocol,omitempty"`
	Product  string        `json:"product,omitempty"`  // substring of X-Plex-Product
	Platform string        `json:"platform,omitempty"` // substring of X-Plex-Platform
	UserID   int64         `json:"userId,omitempty"`
	Profile  string        `json:"profile"`
}

type Transcode struct {
	RTXVideo RTXVideo `json:"rtxVideo"`

	FFmpegPath           string `json:"ffmpegPath"` // empty = bundled, then PATH
	FFprobePath          string `json:"ffprobePath"`
	HWAccel              string `json:"hwAccel"` // "auto", "none", "videotoolbox", "nvenc", "qsv", "vaapi"
	VAAPIDevice          string `json:"vaapiDevice"`
	TempDir              string `json:"tempDir"`
	MaxTranscodes        int    `json:"maxTranscodes"`
	RemoteMaxBitrateKbps int    `json:"remoteMaxBitrateKbps"`

	// Adaptive HLS engine (additive). Numeric zero values select the
	// transcode package defaults; see Defaults for the shipped values.
	FastStart          bool     `json:"fastStart"`          // short, quick opening segments
	FastStartSegments  int      `json:"fastStartSegments"`  // opening segments in the ramp (default 2)
	FastStartSegment   Duration `json:"fastStartSegment"`   // ramp segment length (default 2s)
	FastStartMaxHeight int      `json:"fastStartMaxHeight"` // ramp encode height cap (default 720)
	ABRLadder          bool     `json:"abrLadder"`          // multi-rung master playlist / MPD
	ABRLadderUnderCopy bool     `json:"abrLadderUnderCopy"` // soft encode rungs under Direct Stream / video-copy (default on; set false to disable)
	VBVReservoir       bool     `json:"vbvReservoir"`       // constant-quality + generous VBV for capped transcodes
	VBVBufferSeconds   int      `json:"vbvBufferSeconds"`   // VBV buffer depth in seconds of maxrate (default 8)
	SpikeGovernor      bool     `json:"spikeGovernor"`      // clamp VBR spikes in copy streams to measured goodput
	SpikeMarginPercent int      `json:"spikeMarginPercent"` // spike threshold over goodput, percent (default 125)
}

// Downloads controls Plex offline downloads (allowSync + the Download
// Queue API). A download resolves a compact mobile-native release and
// remuxes it (stream copy) to a temporary MP4; nothing is stored
// permanently. Enabled by default for the owner: it is bounded by the
// disk budget, the concurrency caps and the normal stream limits, and
// the owner can turn it off entirely here.
type Downloads struct {
	Enabled bool `json:"enabled"`
	// AllowFriends lets shared (non-owner) users download. Off by
	// default, matching PMS sharing where sync is a separate grant; an
	// AccessPolicy AllowDownloads decision overrides this per user.
	AllowFriends bool `json:"allowFriends"`
	// MaxConcurrent bounds simultaneous remuxes server-wide (default 2),
	// MaxConcurrentPerUser per viewer (default 1).
	MaxConcurrent        int `json:"maxConcurrent"`
	MaxConcurrentPerUser int `json:"maxConcurrentPerUser"`
	// DiskBudgetBytes bounds the temporary remuxed files on disk at any
	// time (default 20 GiB). 0 uses the default.
	DiskBudgetBytes int64 `json:"diskBudgetBytes"`
	// Expiry is how long a finished download stays fetchable before its
	// temp file is deleted (default 24h).
	Expiry Duration `json:"expiry"`
	// MaxHeight caps the download profile's resolution (default 1080).
	MaxHeight int `json:"maxHeight"`
}

type Simkl struct {
	Enabled      bool   `json:"enabled"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type Trakt struct {
	Enabled      bool   `json:"enabled"`
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

type OpenSubtitles struct {
	Enabled  bool   `json:"enabled"`
	APIKey   string `json:"apiKey"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type LLM struct {
	Enabled bool `json:"enabled"` // off by default
	// Provider is an LLMProviders ID; empty means LLMProviderCustom.
	Provider string `json:"provider"`
	// BaseURL overrides the provider's endpoint for local servers and is
	// required for a custom OpenAI-compatible endpoint, e.g.
	// http://localhost:11434/v1. Hosted providers ignore it.
	BaseURL string `json:"baseUrl"`
	APIKey  string `json:"apiKey"`
	Model   string `json:"model"`
}

type Updates struct {
	Channel    string `json:"channel"` // "stable", "beta"
	AutoUpdate bool   `json:"autoUpdate"`
	FeedURL    string `json:"feedUrl"`
	// MaintenanceStart/End bound the local-time window ("HH:MM") in which
	// a downloaded update may be installed automatically. The window may
	// wrap midnight; equal values mean "any time".
	MaintenanceStart string `json:"maintenanceStart"`
	MaintenanceEnd   string `json:"maintenanceEnd"`
	// MaxDrain bounds how long a clustered node about to install an update
	// waits for its own playback sessions to end (it admits no new ones
	// meanwhile) before installing anyway inside the window. 0 means 30
	// minutes. Ignored on a standalone server, which waits for idle.
	MaxDrain Duration `json:"maxDrain,omitempty"`
}

// Spike serves local files as a library so the Plex front end can be
// verified against official apps without Usenet.
type Spike struct {
	Enabled    bool     `json:"enabled"`
	LocalFiles []string `json:"localFiles"`
}

// Duration marshals as a Go duration string ("6h").
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		var n int64
		if err2 := json.Unmarshal(b, &n); err2 != nil {
			return err
		}
		*d = Duration(n)
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}
func (d Duration) D() time.Duration { return time.Duration(d) }

// DefaultDataDir is where Ether keeps config, database and cache. On
// Windows that is %ProgramData%\Ether: the server runs as a LocalSystem
// service, so a per-user directory would depend on who installed it.
func DefaultDataDir() string {
	return defaultDataDir(runtime.GOOS, os.Getenv, os.UserHomeDir, os.Geteuid)
}

func defaultDataDir(goos string, getenv func(string) string, home func() (string, error), euid func() int) string {
	if v := getenv("ETHER_DATA_DIR"); v != "" {
		return v
	}
	switch goos {
	case "windows":
		base := getenv("PROGRAMDATA")
		if base == "" {
			base = `C:\ProgramData`
		}
		// Join with a backslash explicitly so the result is right even
		// when computed (in tests) on a non-Windows host.
		return strings.TrimRight(base, `\/`) + `\Ether`
	case "darwin":
		if h, err := home(); err == nil {
			return filepath.Join(h, "Library", "Application Support", "Ether")
		}
	case "linux":
		if euid() == 0 {
			return "/var/lib/ether"
		}
		if v := getenv("XDG_DATA_HOME"); v != "" {
			return filepath.Join(v, "ether")
		}
	}
	h, _ := home()
	return filepath.Join(h, ".local", "share", "ether")
}

// Store loads, saves and broadcasts the config.
type Store struct {
	policyMu      sync.RWMutex
	policyBump    func() error
	policyErr     error
	path          string
	credentialKey []byte
	mu            sync.RWMutex
	cfg           Config
	// generation changes whenever the live config is replaced, so readers
	// can reuse a derived snapshot until the next successful update.
	generation uint64
	subs       []func(Config)
	// syncDir is the post-rename directory-sync step used by save. Open
	// sets it to syncDirAfterRename (production: skipped on Windows);
	// tests may replace it per Store to inject a post-rename failure.
	syncDir func(string) error

	// keyCreated is set when Open generated the credential key, so data
	// sealed elsewhere (the database) cannot have been sealed with it.
	keyCreated bool
	// bound seals database fields and writes the shared settings in v2
	// (credentials.go, fieldseal.go): set by UpgradeCredentials, at Open
	// for a configuration already upgraded, and on loading shared
	// settings another node already upgraded.
	bound atomic.Bool
	// localBound writes config.json in v2. Only UpgradeCredentials (this
	// node's own rollback gate) or an upgraded config.json sets it: the
	// node-local file must stay readable if this node rolls back.
	localBound atomic.Bool

	// Cluster mode (see shared.go). shared is nil on a standalone server,
	// where config.json is the only source. writeMu sequences cluster-mode
	// writers (Update, refresh) so mu is never held across database calls.
	writeMu    sync.Mutex
	shared     SharedBackend
	sharedVer  int64
	sharedOpts SharedOptions
	notifier   Notifier
	unsubs     []func()
	kick       chan struct{}
	stopWatch  func()
}

// EnsureDataDir creates dataDir owner-only (0700) when it does not exist:
// it holds config.json, the credential key and ether.db. Parents are made
// as usual, and a directory that already exists keeps the mode its owner
// or a package postinstall gave it.
func EnsureDataDir(dataDir string) error {
	if err := os.MkdirAll(filepath.Dir(dataDir), 0o755); err != nil {
		return err
	}
	err := os.Mkdir(dataDir, 0o700)
	if errors.Is(err, fs.ErrExist) {
		return os.MkdirAll(dataDir, 0o700) // nil for a directory, an error for a file
	}
	return err
}

// DataDirWarning returns a warning when the data directory or the
// credential key can be read by other local accounts, or "" (always on
// Windows, where the installer and service install manage ACLs). It never
// changes permissions: an existing directory keeps the mode its owner or a
// package postinstall chose. The message names modes, not paths.
func DataDirWarning(dataDir string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	var exposed []string
	if fi, err := os.Stat(dataDir); err == nil && fi.Mode().Perm()&0o077 != 0 {
		exposed = append(exposed, fmt.Sprintf("the data directory (mode %04o)", fi.Mode().Perm()))
	}
	if fi, err := os.Stat(filepath.Join(dataDir, CredentialKeyFile)); err == nil && fi.Mode().Perm()&0o077 != 0 {
		exposed = append(exposed, fmt.Sprintf("the credential key (mode %04o)", fi.Mode().Perm()))
	}
	if len(exposed) == 0 {
		return ""
	}
	return "data directory: " + strings.Join(exposed, " and ") + " can be read by other local accounts; it holds the credential key and database. Restrict it to the server account (chmod 700 on the directory, 600 on .credentials-key) unless shared access is deliberate"
}

func Open(dataDir string) (*Store, error) {
	if err := EnsureDataDir(dataDir); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dataDir, "config.json"), cfg: Defaults(dataDir), kick: make(chan struct{}, 1), syncDir: syncDirAfterRename}
	b, err := os.ReadFile(s.path)
	readErr := err
	_, statErr := os.Lstat(filepath.Join(dataDir, CredentialKeyFile))
	key, keyErr := credentialKey(dataDir, b)
	if keyErr != nil {
		return nil, keyErr
	}
	s.credentialKey = key
	s.keyCreated = errors.Is(statErr, fs.ErrNotExist)
	err = readErr
	switch {
	case err == nil:
		plain, stats, err := openDocument(b, s.credentialKey)
		if err != nil {
			return nil, err
		}
		if s.cfg, err = decodeOverDefaults(s.cfg, plain); err != nil {
			return nil, fmt.Errorf("parse %s: %w", s.path, err)
		}
		// A configuration already upgraded to v2 credentials stays v2.
		s.bound.Store(stats.strict)
		s.localBound.Store(stats.strict)
		// Seal secrets written in plaintext (by hand, or by a release that
		// predates sealing).
		if stats.plaintext > 0 || !ContainsSealed(b) {
			if err := s.save(); err != nil {
				return nil, err
			}
		}
	case os.IsNotExist(err):
		if err := s.save(); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	return s, nil
}

// Read loads dataDir's settings without creating or rewriting anything:
// no directory, no credential key and no re-sealed config.json, unlike
// Open. It is for other processes (the tray) that only look at the
// server's settings and must not race its writes. Sealed credentials need
// the existing credential key.
func Read(dataDir string) (Config, error) {
	path := filepath.Join(dataDir, "config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	plain := b
	if ContainsSealed(b) {
		key, err := ReadCredentialKey(dataDir)
		if err != nil {
			return Config{}, fmt.Errorf("config: credential key: %w", err)
		}
		if plain, _, err = openDocument(b, key); err != nil {
			return Config{}, err
		}
	}
	cfg, err := decodeOverDefaults(Defaults(dataDir), plain)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return cfg, nil
}

// decodeOverDefaults decodes the config document b over defaults. The
// merge happens on the JSON documents (objects recursively, see mergeDoc)
// and the result is decoded into a fresh Config: json.Unmarshal
// straight into defaults would decode a file's array elements into the
// default slice's existing elements, so a saved element would inherit every
// default field the file omits (omitempty zero values) — a saved quality
// rule {"platform":"iOS"} would come back still matching the default rule's
// "Apple TV" product.
func decodeOverDefaults(defaults Config, b []byte) (Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var file map[string]any
	if err := dec.Decode(&file); err != nil {
		return Config{}, err
	}
	// Like json.Unmarshal, reject anything after the document.
	if _, err := dec.Token(); err != io.EOF {
		return Config{}, fmt.Errorf("invalid data after the top-level value")
	}
	doc := toDoc(defaults)
	mergeDoc(doc, file)
	return fromDoc(doc)
}

// mergeDoc overlays src onto dst recursively: objects merge key by key at
// every depth (a key the file lacks keeps its default, however deeply
// nested), while arrays and scalars replace dst's value whole.
func mergeDoc(dst, src map[string]any) {
	for k, v := range src {
		if obj, ok := v.(map[string]any); ok {
			if target, ok := dst[k].(map[string]any); ok {
				mergeDoc(target, obj)
				continue
			}
		}
		dst[k] = v
	}
}

func (s *Store) Path() string { return s.path }

func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return clone(s.cfg)
}

// TrustedProxyCIDRs is the live trusted-proxy list. The returned slice is a
// copy, so a caller cannot change the stored configuration.
func (s *Store) TrustedProxyCIDRs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.cfg.Server.TrustedProxyCIDRs...)
}

// Read returns the live config without cloning it. Callers must not mutate
// the result or anything it points at. Browse paths use it so a page miss
// does not marshal the whole config.
func (s *Store) Read() Config {
	if s == nil {
		return Config{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Generation changes when the live config is replaced. It does not change
// when a save is rolled back, so a snapshot taken at this value stays valid.
func (s *Store) Generation() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.generation
}

// Update applies fn to a copy of the config, saves it and notifies subscribers.
//
// On a cluster node the shared part is written to the database with an
// optimistic version check; when another node changed it first, fn is
// applied again to the fresh settings, so fn must only mutate its argument.
func (s *Store) Update(fn func(*Config) error) error {
	return s.policyUpdate(func() error { return s.update(fn) })
}

func (s *Store) update(fn func(*Config) error) error {
	if s.Clustered() {
		return s.updateClustered(fn)
	}
	s.mu.Lock()
	next := clone(s.cfg)
	if err := fn(&next); err != nil {
		s.mu.Unlock()
		return err
	}
	prev := s.cfg
	s.cfg = next
	if err := s.save(); err != nil {
		s.cfg = prev
		s.mu.Unlock()
		return err
	}
	s.generation++
	subs := append([]func(Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(clone(next))
	}
	return nil
}

func (s *Store) Subscribe(fn func(Config)) {
	s.mu.Lock()
	s.subs = append(s.subs, fn)
	s.mu.Unlock()
}

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	b, err = sealDocument(b, s.credentialKey, s.localBound.Load())
	if err != nil {
		return err
	}
	syncDir := s.syncDir
	if syncDir == nil { // Store not built by Open
		syncDir = syncDirAfterRename
	}
	return writeFileAtomicWithDirSync(s.path, b, 0o600, syncDir)
}

// EffectiveCache combines the shared pool policy with this node's mount path.
func (c Config) EffectiveCache() Cache {
	if c.Cache.Storage == "shared" && c.SharedCache != nil {
		p := *c.SharedCache
		p.Dir, p.Storage = c.Cache.Dir, "shared"
		return p
	}
	return c.Cache
}

// SetCachePolicy updates the shared policy only when this node joins the NAS pool.
func (c *Config) SetCachePolicy(p Cache) {
	c.Cache = p
	if p.Storage == "shared" {
		p.Dir, p.Storage = "", ""
		c.SharedCache = &p
	}
}
