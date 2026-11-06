// Package plugins is Ether's plugin host core (SPEC §5.1, §5.4, §5.5, §5.7,
// §5.13, WS7): manifest parsing and validation, install and lifecycle,
// permissions and grants, settings and sealed secrets, KV, health, the pull
// scheduler, the event outbox and log, the journal (shadow and undo), update
// and rollback, removal, cluster placement, and the owner API.
//
// Runtimes are separate workstreams and plug in through Runtime and
// RuntimeFactory: WS8a registers the hosted (wazero) runtime, WS8b the
// remote (URL and tunnel) runtimes. Capability adapters (WS9) call plugins
// through Manager.Call and read state through Manager's accessors; they
// never touch the tables directly. The conformance kit (WS11) reuses
// ParseManifest, Manifest.Validate and LintCopy.
package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/stremio"
	"net/http"
	"time"
)

// Protocol is the Ether Plugin Protocol major this host speaks (EPP/1).
const Protocol = 1

// Kind decides where the owner meets a plugin (SPEC §5.1.1).
type Kind string

const (
	KindSource              Kind = "source"
	KindIntegration         Kind = "integration"
	KindNotificationChannel Kind = "notification_channel"
	KindLiveTVSource        Kind = "livetv_source"
	KindExtension           Kind = "extension"
)

// Capability is one thing a plugin can do (SPEC §5.1.3).
type Capability string

const (
	CapStreamSource    Capability = "stream_source"
	CapDebridProvider  Capability = "debrid_provider"
	CapListSource      Capability = "list_source"
	CapMetadata        Capability = "metadata"
	CapPluginDirectory Capability = "plugin_directory"
	CapLiveTV          Capability = "livetv_channels"
	CapSubtitles       Capability = "subtitles"
	CapMarkers         Capability = "markers"
	CapNotify          Capability = "notify"
	CapEvents          Capability = "events"
	CapRankingStep     Capability = "ranking_step"
	CapRuleCondition   Capability = "rule_condition"
	CapTools           Capability = "tools"
	CapUI              Capability = "ui"
	CapOnAirContent    Capability = "onair_content"
	CapLiveTVPack      Capability = "livetv_pack"
)

// Reserved capabilities are known names a manifest may not claim in v1.
var ReservedCapabilities = map[Capability]bool{
	"artwork": true, "ratings": true,
	"stats_export": true, "sign_in": true,
}

// Manifest is a plugin's manifest.json: the Stremio manifest fields, typed
// by the one Stremio model (internal/stremio), plus Ether's fields under
// Ether (SPEC §5.1.1). A manifest with a nil Ether is a plain Stremio
// add-on. Every Stremio rule (resources, ids, catalogs, extras) is the
// library's, reached through Stremio().
type Manifest struct {
	ID            string                `json:"id"`
	Version       string                `json:"version"`
	Name          string                `json:"name"`
	Description   string                `json:"description,omitempty"`
	Logo          string                `json:"logo,omitempty"`
	Background    string                `json:"background,omitempty"`
	Types         []string              `json:"types,omitempty"`
	AddonCatalogs []StremioCat          `json:"addonCatalogs,omitempty"`
	Catalogs      []StremioCat          `json:"catalogs,omitempty"`
	Resources     stremio.Resources     `json:"resources,omitempty"`
	IDPrefixes    []string              `json:"idPrefixes,omitempty"`
	BehaviorHints stremio.BehaviorHints `json:"behaviorHints,omitzero"`
	Ether         *EtherBlock           `json:"ether,omitempty"`
	// rolesOff are capabilities the owner switched off on the installed
	// record (Record.RolesOff); EffectiveEther leaves them out.
	rolesOff []Capability
}

// Stremio is the manifest's Stremio part.
func (m Manifest) Stremio() stremio.Manifest {
	return stremio.Manifest{
		ID: m.ID, Version: m.Version, Name: m.Name, Description: m.Description, Logo: m.Logo,
		Background: m.Background, Types: m.Types, IDPrefixes: m.IDPrefixes, Resources: m.Resources,
		Catalogs: m.Catalogs, AddonCatalogs: m.AddonCatalogs, BehaviorHints: m.BehaviorHints,
	}
}

// HasResource reports whether the manifest declares the named resource.
func (m Manifest) HasResource(name string) bool { return m.Stremio().HasResource(name) }

// Serves reports whether the named resource answers content type typ.
func (m Manifest) Serves(name, typ string) bool { return m.Stremio().Serves(name, typ) }

// CatalogsOfType returns the catalogs of one content type.
func (m Manifest) CatalogsOfType(typ string) []StremioCat { return m.Stremio().CatalogsOfType(typ) }

// StremioCat is one Stremio catalog declaration.
type StremioCat = stremio.Catalog

// StremioExtra is one catalog extra declaration.
type StremioExtra = stremio.Extra

// EtherBlock is the `ether` object of a manifest.
type EtherBlock struct {
	Protocol int `json:"protocol"`
	// Stremio marks a native plugin that answers Stremio add-on routes,
	// such as an add-on opened and edited in Studio: Ether talks to it
	// exactly as to the add-on it came from.
	Stremio      bool         `json:"stremio,omitempty"`
	Kind         Kind         `json:"kind"`
	Capabilities []Capability `json:"capabilities,omitempty"`
	// Summary is plain text or a "$t:key" reference into Strings.
	Summary string      `json:"summary,omitempty"`
	Author  Author      `json:"author,omitempty"`
	License string      `json:"license,omitempty"`
	Repo    string      `json:"repo,omitempty"`
	Icon    string      `json:"icon,omitempty"`
	Runtime RuntimeDecl `json:"runtime"`
	// Cluster is "leader" (default) or "every_node".
	Cluster     string      `json:"cluster,omitempty"`
	Health      HealthDecl  `json:"health,omitempty"`
	Permissions Permissions `json:"permissions,omitempty"`
	// Settings is a settings schema (SPEC §5.5), {} or {"$ref":"file"}.
	Settings json.RawMessage `json:"settings,omitempty"`
	// Strings maps a locale to a file path (string) or an inline
	// key-to-text object. "en" is required when any $t: key is used.
	Strings     map[string]json.RawMessage `json:"strings,omitempty"`
	Schedule    []ScheduleEntry            `json:"schedule,omitempty"`
	Contributes Contributes                `json:"contributes,omitempty"`
}

// Author names a plugin's author.
type Author struct {
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}

// RuntimeKind is the runtime that serves the plugin. A plain Stremio
// add-on (no ether block) is always reached over HTTP: "remote".
func (m Manifest) RuntimeKind() string {
	if m.Ether == nil || m.Ether.Runtime.Kind == "" {
		return "remote"
	}
	return m.Ether.Runtime.Kind
}

// RuntimeDecl declares how a plugin runs: hosted (Entry JS or Module WASM)
// or remote (the manifest URL or a tunnel is the endpoint).
type RuntimeDecl struct {
	Kind   string `json:"kind"` // "hosted" or "remote"
	Entry  string `json:"entry,omitempty"`
	Module string `json:"module,omitempty"`
}

// HealthDecl declares the health-check interval (minimum 15s).
type HealthDecl struct {
	Interval string `json:"interval,omitempty"`
}

// ScheduleEntry is one pull job Ether runs (minimum interval 15m).
type ScheduleEntry struct {
	Every string `json:"every"`
	Call  string `json:"call"`
}

// Permissions is what a plugin asks for; anything not declared is denied
// (SPEC §5.4). Empty means none.
type Permissions struct {
	Network     []string    `json:"network,omitempty" yaml:"network,omitempty"`
	Catalog     string      `json:"catalog,omitempty" yaml:"catalog,omitempty"` // ""|none|read
	Viewers     ViewerPerms `json:"viewers,omitempty" yaml:"viewers,omitempty"`
	Collections string      `json:"collections,omitempty" yaml:"collections,omitempty"` // ""|none|write
	Folders     string      `json:"folders,omitempty" yaml:"folders,omitempty"`         // ""|none|read
	Secrets     []string    `json:"secrets,omitempty" yaml:"secrets,omitempty"`
}

// ViewerPerms holds the three viewer permission levels.
type ViewerPerms struct {
	Identity        string `json:"identity,omitempty" yaml:"identity,omitempty"`                   // none|granted_viewers|all
	WatchHistory    string `json:"watch_history,omitempty" yaml:"watch_history,omitempty"`         // none|granted_viewers|all
	WriteWatchState string `json:"write_watch_state,omitempty" yaml:"write_watch_state,omitempty"` // none|granted_viewers
}

// Contributes carries per-capability metadata and UI contributions.
type PackStyle struct {
	ID   string `json:"id"`
	File string `json:"file"`
}

type Contributes struct {
	Looks          []PackStyle         `json:"looks,omitempty"`
	OnAirFeeds     []OnAirFeed         `json:"onairFeeds,omitempty"`
	OnAirSegments  []OnAirSegment      `json:"onairSegments,omitempty"`
	Directories    []DirectoryMeta     `json:"directories,omitempty"`
	Metadata       *MetadataMeta       `json:"metadata,omitempty"`
	Source         *SourceMeta         `json:"source,omitempty"`
	Channels       *ChannelsMeta       `json:"channels,omitempty"`
	Subtitles      *SubtitlesMeta      `json:"subtitles,omitempty"`
	Markers        *MarkersMeta        `json:"markers,omitempty"`
	Notification   *NotificationMeta   `json:"notification,omitempty"`
	Events         *EventsMeta         `json:"events,omitempty"`
	Integration    *IntegrationMeta    `json:"integration,omitempty"`
	ListSources    []ListSourceMeta    `json:"listSources,omitempty"`
	RankingSteps   []RankingStepMeta   `json:"rankingSteps,omitempty"`
	RuleConditions []RuleConditionMeta `json:"ruleConditions,omitempty"`
	Tools          []ToolMeta          `json:"tools,omitempty"`
	Pages          []PageMeta          `json:"pages,omitempty"`
	Slots          []SlotMeta          `json:"slots,omitempty"`
	Commands       []CommandMeta       `json:"commands,omitempty"`
}

// SourceMeta is mandatory for stream_source.
type SourceMeta struct {
	Class     string          `json:"class"` // debrid|usenet|indexer|direct
	Abilities []string        `json:"abilities,omitempty"`
	Limits    json.RawMessage `json:"limits,omitempty"`
}

// ChannelsMeta is mandatory for livetv_channels.
type ChannelsMeta struct {
	Refresh string `json:"refresh,omitempty"`
	Test    bool   `json:"test"`
}

// SubtitlesMeta is mandatory for subtitles. Languages is "any" or a list.
type SubtitlesMeta struct {
	Languages json.RawMessage `json:"languages,omitempty"`
	Formats   []string        `json:"formats,omitempty"`
}

// MarkersMeta is mandatory for markers.
type MarkersMeta struct {
	Kinds []string `json:"kinds"` // intro|credits|recap|preview
}

// NotificationMeta is mandatory for notify.
type NotificationMeta struct {
	Fields    json.RawMessage `json:"fields,omitempty"`
	SendTest  bool            `json:"sendTest"`
	Formats   []string        `json:"formats,omitempty"`
	MaxLength int             `json:"maxLength,omitempty"`
}

// EventsMeta is mandatory for events.
type EventsMeta struct {
	Subscribe []string `json:"subscribe"`
}

// IntegrationMeta is mandatory for the integration kind.
type IntegrationMeta struct {
	Category string   `json:"category"`
	Logo     string   `json:"logo,omitempty"`
	Adds     []string `json:"adds"`    // 1..4 plain lines or $t: keys
	Connect  string   `json:"connect"` // fields|device_code|none
	Test     bool     `json:"test,omitempty"`
}

// ListSourceMeta is one contributed list source.
type ListSourceMeta struct {
	Key      string          `json:"key"`
	Label    string          `json:"label"`
	Hint     string          `json:"hint,omitempty"`
	Catalog  string          `json:"catalog"`
	Params   json.RawMessage `json:"params,omitempty"`
	MaxItems int             `json:"maxItems,omitempty"` // ≤ 2000
	Refresh  string          `json:"refresh,omitempty"`  // ≥ 1h
}

// RankingStepMeta is one contributed ranking step (hosted only).
type RankingStepMeta struct {
	Key             string          `json:"key"`
	Label           string          `json:"label"`
	Hint            string          `json:"hint,omitempty"`
	Editor          json.RawMessage `json:"editor,omitempty"`
	DefaultPosition int             `json:"defaultPosition,omitempty"`
	Budget          json.RawMessage `json:"budget,omitempty"`
}

// RuleConditionMeta is one contributed rule condition (hosted only).
type RuleConditionMeta struct {
	Key       string          `json:"key"`
	Label     string          `json:"label"`
	Operators []string        `json:"operators,omitempty"`
	Editor    json.RawMessage `json:"editor,omitempty"`
}

// ToolMeta is one contributed agent tool.
type ToolMeta struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	ReadOnly    bool            `json:"readOnly,omitempty"`
	Destructive bool            `json:"destructive,omitempty"`
	OpenWorld   bool            `json:"openWorld,omitempty"`
}

// PageMeta is one contributed console page (ViewSpec, SPEC §5.6).
type PageMeta struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Route string `json:"route"` // plugin route returning {spec,data}
	Group string `json:"group,omitempty"`
	Icon  string `json:"icon,omitempty"`
}

// SlotMeta is one contributed console slot.
type SlotMeta struct {
	Slot   string `json:"slot"` // title.panel|viewer.tab|dashboard.panel|actionMenu.*
	Key    string `json:"key"`
	Label  string `json:"label"`
	Route  string `json:"route,omitempty"`
	Action string `json:"action,omitempty"`
}

// CommandMeta is one contributed ⌘K command.
type CommandMeta struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Action string `json:"action"` // plugin action name
}

// Request is one HTTP-shaped call from Ether to a plugin (SPEC §5.1.2).
// Hosted plugins receive the same object in-process.
type Request struct {
	Op       string            `json:"op,omitempty"`
	ID       string            `json:"id,omitempty"`
	PluginID string            `json:"pluginId"`
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Query    map[string]string `json:"query,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     json.RawMessage   `json:"body,omitempty"`
	// Locale is the console locale for calls that return copy.
	Locale string `json:"locale,omitempty"`
}

// Response is a plugin's answer to a Request.
type Response struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
}

// Runtime is how the host reaches one installed plugin. WS8a (hosted) and
// WS8b (remote) implement it; tests use a fake. Call must honour ctx
// deadlines. Close releases the instance; the host may open a fresh one
// after a failure (restart with backoff).
type Runtime interface {
	Call(ctx context.Context, req Request) (Response, error)
	Close() error
}

// Installed is the host's record of one installed plugin, handed to a
// RuntimeFactory when an instance is opened.
type Installed struct {
	ID       string
	Manifest Manifest
	// SourceURL is where the manifest came from (remote endpoint base).
	SourceURL string
	// Files returns an artifact file (entry, module, strings, schema) for
	// the installed version, or false. Hosted runtimes load code through it.
	Files func(path string) ([]byte, bool)
	// Host is the per-plugin host-services handle (SPEC §5.1.5).
	Host Host
}

// RuntimeFactory opens a Runtime for one installed plugin. The factory is
// chosen by manifest ether.runtime.kind ("hosted" or "remote").
type RuntimeFactory func(inst Installed) (Runtime, error)

// Host is the set of Ether services a plugin may call (SPEC §5.1.5).
// Every method re-checks the plugin's live grants, so a revoke takes
// effect mid-call. Writes are journalled; in shadow mode they are
// recorded without applying. The hosted runtime exposes Host as imports;
// remote plugins reach it over /ether/plugin-host/* (WS8b).
type Host interface {
	KVGet(ctx context.Context, key string) ([]byte, bool, error)
	KVPut(ctx context.Context, key string, value []byte) error
	KVDelete(ctx context.Context, key string) error
	KVList(ctx context.Context, prefix string) ([]string, error)

	// SecretGet returns a sealed setting the manifest declared in
	// permissions.secrets. Hosted plugins only.
	SecretGet(ctx context.Context, name string) (string, error)

	// HTTPFetch fetches through egress and safefetch; only manifest
	// network hosts are allowed. Observed hosts feed the privacy label.
	HTTPFetch(ctx context.Context, req FetchRequest) (FetchResponse, error)

	// CatalogSearch and CatalogGet need catalog: read. Results are JSON
	// title summaries produced by the injected catalog backend.
	CatalogSearch(ctx context.Context, query string, limit, offset int) (json.RawMessage, error)
	CatalogGet(ctx context.Context, id string) (json.RawMessage, error)

	// ViewerHistory needs viewers.watch_history for that viewer.
	ViewerGranted(ctx context.Context) (json.RawMessage, error)
	ViewerHistory(ctx context.Context, viewerID int64, limit, offset int) (json.RawMessage, error)
	// SetWatchState needs viewers.write_watch_state; journalled.
	SetWatchState(ctx context.Context, viewerID int64, titleID string, watched bool) error
	// CollectionsUpsert needs collections: write; journalled.
	CollectionsUpsert(ctx context.Context, collection json.RawMessage) error

	// FoldersList and FoldersStat need folders: read and an owner-picked
	// root. They never return file contents.
	FoldersList(ctx context.Context, root, path string) ([]FolderEntry, error)
	FoldersStat(ctx context.Context, root, path string) (FolderEntry, error)

	// Settings returns the plugin's current non-secret settings. Plugins
	// read them per request instead of keeping them in instance memory,
	// because hosted plugins run in a pool of instances that restart.
	// Secrets stay behind SecretGet.
	Settings(ctx context.Context) (map[string]any, error)

	// NotifyOwner posts a console notice (rate-limited).
	NotifyOwner(ctx context.Context, text string) error
	// Log appends to the plugin's log ring (redacted, rate-limited).
	Log(ctx context.Context, level, msg string, attrs map[string]any) error
}

// FetchRequest is one plugin outbound HTTP call.
type FetchRequest struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// FetchResponse is the answer to a FetchRequest (body capped at 10 MiB).
type FetchResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// FolderEntry is one entry from FoldersList or FoldersStat.
type FolderEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size,omitempty"`
	Mtime int64  `json:"mtime,omitempty"` // unix seconds
}

// Event is one normalized Ether event (SPEC §5.7). Payload is JSON with no
// secrets; viewer identity appears only for plugins granted it (the host
// strips it per plugin at delivery).
type Event struct {
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	At       time.Time       `json:"at"`
	ViewerID int64           `json:"viewerId,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// Normalized event names (SPEC §5.7).
const (
	EventPlaybackStarted   = "playback.started"
	EventPlaybackPaused    = "playback.paused"
	EventPlaybackResumed   = "playback.resumed"
	EventPlaybackStopped   = "playback.stopped"
	EventPlaybackCompleted = "playback.completed"
	EventPlaybackError     = "playback.error"
	EventPlaybackFailover  = "playback.failover"
	EventTitleAvailable    = "title.available"
	EventViewerAdded       = "viewer.added"
	EventViewerAccess      = "viewer.access_changed"
	EventCollectionRebuilt = "collection.rebuilt"
	EventServerHealth      = "server.health_changed"
)

// KnownEvents is the closed v1 event set.
var KnownEvents = map[string]bool{
	EventPlaybackStarted: true, EventPlaybackPaused: true, EventPlaybackResumed: true,
	EventPlaybackStopped: true, EventPlaybackCompleted: true, EventPlaybackError: true,
	EventPlaybackFailover: true, EventTitleAvailable: true, EventViewerAdded: true,
	EventViewerAccess: true, EventCollectionRebuilt: true, EventServerHealth: true,
}

// CandidateFacts is the hot-path input for ranking steps and rule
// conditions (SPEC §5.8), JSON-encoded. It never carries viewer data.
type CandidateFacts struct {
	ReleaseID     string     `json:"releaseId"`
	Name          string     `json:"name"`
	Size          int64      `json:"size,omitempty"`
	Protocol      string     `json:"protocol,omitempty"` // usenet|torrent|direct
	SourceClass   string     `json:"sourceClass,omitempty"`
	Indexer       string     `json:"indexer,omitempty"`
	Resolution    string     `json:"resolution,omitempty"`
	HDR           []string   `json:"hdr,omitempty"`
	VideoCodec    string     `json:"videoCodec,omitempty"`
	AudioCodecs   []string   `json:"audioCodecs,omitempty"`
	AudioChannels string     `json:"audioChannels,omitempty"`
	Languages     []string   `json:"languages,omitempty"`
	Subtitles     []string   `json:"subtitles,omitempty"`
	ReleaseType   string     `json:"releaseType,omitempty"`
	Group         string     `json:"group,omitempty"`
	Cached        bool       `json:"cached,omitempty"`
	Verified      bool       `json:"verified,omitempty"`
	ClientCaps    ClientCaps `json:"clientCaps,omitempty"`
}

// ClientCaps is what the playing client can show.
type ClientCaps struct {
	MaxHeight int      `json:"maxHeight,omitempty"`
	HDR       []string `json:"hdr,omitempty"`
	Codecs    []string `json:"codecs,omitempty"`
}

// CatalogueArtifact is a resolved version from the signed plugin catalogue (WS12).
type CatalogueArtifact struct {
	ID           string
	Version      string
	URL          string
	SHA256       string
	Kind         Kind
	Permissions  Permissions
	Capabilities []Capability
}

// Catalogue is the signed catalogue hook used by the owner API, install
// and update flows (WS12).
type Catalogue interface {
	http.Handler
	// Resolve looks up a plugin in the verified catalogue by plugin ID (or
	// artifact URL) and optional version (empty means latest). It fails when
	// the plugin is not in the catalogue or is revoked.
	Resolve(ctx context.Context, idOrURL, version string) (CatalogueArtifact, error)
}

// Sentinel errors. Adapters and the API map them to plain reasons.
var (
	ErrNotFound = errors.New("plugins: not found")

	ErrPermissionDenied = errors.New("plugins: permission denied")
	ErrQuotaExceeded    = errors.New("plugins: storage quota exceeded")
	ErrDisabled         = errors.New("plugins: plugin is disabled")
	ErrNoRuntime        = errors.New("plugins: no runtime registered for this plugin")
	ErrUnavailable      = errors.New("plugins: host service unavailable")

	ErrDigestMismatch = errors.New("plugins: artifact digest does not match")
	ErrRevoked        = errors.New("plugins: plugin was revoked")
)

// FieldError is one validation problem, keyed by a dotted field path.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError carries every field problem found at once.
type ValidationError struct{ Fields []FieldError }

func (e *ValidationError) Error() string {
	if len(e.Fields) == 0 {
		return "plugins: invalid"
	}
	return fmt.Sprintf("plugins: invalid: %s: %s", e.Fields[0].Field, e.Fields[0].Message)
}

// MetadataMeta declares a provider of canonical catalogue records. Resolution
// happens during explicit materialization, never as part of ordinary browsing.
// Ether-native providers implement POST /ether/metadata/resolve using
// core.MetadataRequest and a {"title": core.Title} response.
type MetadataMeta struct {
	Types []string `json:"types"`
}

// DirectoryMeta declares a protocol-neutral plugin discovery source.
// Entries are suggestions, never installation authorization or verification.
type DirectoryMeta struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// OnAirFeed declares a bounded household feed. MaxAge is a Go duration.
type OnAirFeed struct {
	ID     string            `json:"id"`
	Fields map[string]string `json:"fields"`
	Sample map[string]any    `json:"sample,omitempty"`
	MaxAge string            `json:"maxAge"`
}

// OnAirSegment is a fixed-duration declarative scene, never arbitrary video.
type OnAirSegment struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Duration int      `json:"duration"`
	File     string   `json:"file"`
	Feeds    []string `json:"feeds"`
	Rating   string   `json:"rating,omitempty"`
}
