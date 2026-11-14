package etherplugin

import (
	"encoding/json"
	"fmt"
)

// PluginKind identifies the top-level plugin classification in manifest.ether.kind.
type PluginKind string

const (
	KindSource              PluginKind = "source"
	KindIntegration         PluginKind = "integration"
	KindNotificationChannel PluginKind = "notification_channel"
	KindLiveTVSource        PluginKind = "livetv_source"
	KindExtension           PluginKind = "extension"
	KindContent             PluginKind = "content"
)

// Capability names a capability declared in manifest.ether.capabilities.
type Capability string

const (
	CapStreamSource    Capability = "stream_source"
	CapDebridProvider  Capability = "debrid_provider"
	CapListSource      Capability = "list_source"
	CapLiveTV          Capability = "livetv_channels"
	CapSubtitles       Capability = "subtitles"
	CapMarkers         Capability = "markers"
	CapNotify          Capability = "notify"
	CapEvents          Capability = "events"
	CapRankingStep     Capability = "ranking_step"
	CapRuleCondition   Capability = "rule_condition"
	CapTools           Capability = "tools"
	CapUI              Capability = "ui"
	CapCatalogProvider Capability = "catalog_provider"
	CapKindResolver    Capability = "kind_resolver"
	CapFrontendAdapter Capability = "frontend_adapter"
	CapMatcher         Capability = "matcher"
)

// Manifest is the EPP/1 manifest document served at GET /manifest.json.
type Manifest struct {
	ID            string               `json:"id"`
	Version       string               `json:"version"`
	Name          string               `json:"name"`
	Description   string               `json:"description,omitempty"`
	Logo          string               `json:"logo,omitempty"`
	Background    string               `json:"background,omitempty"`
	Types         []string             `json:"types,omitempty"`
	Catalogs      []StremioCatalogDecl `json:"catalogs,omitempty"`
	Resources     []any                `json:"resources,omitempty"`
	IDPrefixes    []string             `json:"idPrefixes,omitempty"`
	BehaviorHints *BehaviorHints       `json:"behaviorHints,omitempty"`
	Ether         *EtherManifestBlock  `json:"ether,omitempty"`
}

// BehaviorHints carries Stremio-compatible behavior hints.
type BehaviorHints struct {
	Adult                 bool `json:"adult,omitempty"`
	P2P                   bool `json:"p2p,omitempty"`
	Configurable          bool `json:"configurable,omitempty"`
	ConfigurationRequired bool `json:"configurationRequired,omitempty"`
}

// StremioCatalogDecl declares a Stremio catalog in the manifest.
type StremioCatalogDecl struct {
	Type  string              `json:"type"`
	ID    string              `json:"id"`
	Name  string              `json:"name,omitempty"`
	Extra []StremioExtraField `json:"extra,omitempty"`
}

// StremioExtraField declares a supported extra parameter on a catalog.
type StremioExtraField struct {
	Name         string   `json:"name"`
	IsRequired   bool     `json:"isRequired,omitempty"`
	Options      []string `json:"options,omitempty"`
	OptionsLimit int      `json:"optionsLimit,omitempty"`
}

// EtherManifestBlock holds the EPP/1 `ether` manifest block.
type EtherManifestBlock struct {
	Protocol     int             `json:"protocol"`
	Kind         PluginKind      `json:"kind"`
	Capabilities []Capability    `json:"capabilities,omitempty"`
	Summary      string          `json:"summary"`
	Author       *PluginAuthor   `json:"author,omitempty"`
	License      string          `json:"license,omitempty"`
	Repo         string          `json:"repo,omitempty"`
	Icon         string          `json:"icon,omitempty"`
	Runtime      RuntimeDecl     `json:"runtime"`
	Cluster      string          `json:"cluster,omitempty"`
	Health       *HealthDecl     `json:"health,omitempty"`
	Permissions  *Permissions    `json:"permissions,omitempty"`
	Settings     *SettingsSchema `json:"settings,omitempty"`
	Strings      map[string]any  `json:"strings,omitempty"`
	Schedule     []ScheduleEntry `json:"schedule,omitempty"`
	Contributes  *Contributes    `json:"contributes,omitempty"`
}

// PluginAuthor identifies the plugin author.
type PluginAuthor struct {
	Name string `json:"name,omitempty"`
	URL  string `json:"url,omitempty"`
}

// RuntimeDecl declares how the plugin is executed.
type RuntimeDecl struct {
	Kind   string         `json:"kind"`
	Entry  string         `json:"entry,omitempty"`
	Module string         `json:"module,omitempty"`
	Limits *RuntimeLimits `json:"limits,omitempty"`
}

// RuntimeLimits requests hosted/data resources within owner-controlled ceilings.
// Defaults: 32 MiB heap, 12h, effectively unlimited background work units.
type RuntimeLimits struct {
	FetchBytes           int64  `json:"fetchBytes,omitempty"`
	ArchiveMemberBytes   int64  `json:"archiveMemberBytes,omitempty"`
	ArchiveExpandedBytes int64  `json:"archiveExpandedBytes,omitempty"`
	ArchiveMembers       int    `json:"archiveMembers,omitempty"`
	MemoryBytes          uint64 `json:"memoryBytes,omitempty"`
	Deadline             string `json:"deadline,omitempty"`
	UnitBudget           int64  `json:"unitBudget,omitempty"`
}

// HealthDecl configures periodic health checks.
type HealthDecl struct {
	Interval string `json:"interval,omitempty"`
}

// ScheduleEntry declares a scheduled background job.
type ScheduleEntry struct {
	Every string `json:"every"`
	Call  string `json:"call"`
}

// Permissions declares the host access a plugin requests.
type Permissions struct {
	Network     []string           `json:"network,omitempty"`
	Catalog     string             `json:"catalog,omitempty"`
	Viewers     *ViewerPermissions `json:"viewers,omitempty"`
	Collections string             `json:"collections,omitempty"`
	Folders     string             `json:"folders,omitempty"`
	Secrets     []string           `json:"secrets,omitempty"`
	// Sources lets a kind resolver ask Ether to find bytes: ""|none|search|acquire.
	Sources string `json:"sources,omitempty"`
}

// ViewerPermissions scopes viewer identity and watch-history access.
type ViewerPermissions struct {
	Identity        string `json:"identity,omitempty"`
	WatchHistory    string `json:"watch_history,omitempty"`
	WriteWatchState string `json:"write_watch_state,omitempty"`
}

// SettingsSchema describes a plugin's configuration fields.
type SettingsSchema struct {
	Ref    string          `json:"$ref,omitempty"`
	Fields []SettingField  `json:"fields,omitempty"`
	Groups []SettingsGroup `json:"groups,omitempty"`
}

// SettingsGroup groups related settings fields.
type SettingsGroup struct {
	Key      string         `json:"key,omitempty"`
	Label    string         `json:"label,omitempty"`
	Hint     string         `json:"hint,omitempty"`
	Advanced bool           `json:"advanced,omitempty"`
	Fields   []SettingField `json:"fields"`
}

// SettingField describes one leaf or group setting field.
type SettingField struct {
	Key         string         `json:"key"`
	Kind        string         `json:"kind"`
	Label       string         `json:"label,omitempty"`
	Hint        string         `json:"hint,omitempty"`
	Required    bool           `json:"required,omitempty"`
	Default     any            `json:"default,omitempty"`
	Options     []FieldOption  `json:"options,omitempty"`
	Fields      []SettingField `json:"fields,omitempty"`
	Min         *float64       `json:"min,omitempty"`
	Max         *float64       `json:"max,omitempty"`
	Placeholder string         `json:"placeholder,omitempty"`
}

// FieldOption is one selectable option for a select or multiSelect field.
type FieldOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	Hint  string `json:"hint,omitempty"`
}

// Contributes declares the descriptors a plugin contributes to Ether.
type Contributes struct {
	Source           *SourceMeta           `json:"source,omitempty"`
	Channels         *ChannelsMeta         `json:"channels,omitempty"`
	Subtitles        *SubtitlesMeta        `json:"subtitles,omitempty"`
	Markers          *MarkersMeta          `json:"markers,omitempty"`
	Notification     *NotificationMeta     `json:"notification,omitempty"`
	Events           *EventsMeta           `json:"events,omitempty"`
	Integration      *IntegrationMeta      `json:"integration,omitempty"`
	ListSources      []ListSourceMeta      `json:"listSources,omitempty"`
	RankingSteps     []RankingStepMeta     `json:"rankingSteps,omitempty"`
	RuleConditions   []RuleConditionMeta   `json:"ruleConditions,omitempty"`
	Tools            []ToolMeta            `json:"tools,omitempty"`
	Pages            []PageMeta            `json:"pages,omitempty"`
	Slots            []SlotMeta            `json:"slots,omitempty"`
	Commands         []CommandMeta         `json:"commands,omitempty"`
	MediaKinds       []MediaKindMeta       `json:"mediaKinds,omitempty"`
	FrontendAdapters []FrontendAdapterMeta `json:"frontendAdapters,omitempty"`
	Setup            *SetupMeta            `json:"setup,omitempty"`
}

// Presentations declares how a library is published per protocol.
type Presentations struct {
	AdapterOnly bool `json:"adapterOnly,omitempty"`
	Plex        *struct {
		Base string `json:"base"` // movie|show|artist|photo
	} `json:"plex,omitempty"`
	MediaBrowser *struct {
		CollectionType string            `json:"collectionType"`
		ItemTypes      map[string]string `json:"itemTypes,omitempty"`
	} `json:"mediaBrowser,omitempty"`
	Stremio *struct {
		Type string `json:"type"`
	} `json:"stremio,omitempty"`
	Xtream *struct {
		Category string `json:"category"`
	} `json:"xtream,omitempty"`
	Silo *struct {
		Type string `json:"type"`
	} `json:"silo,omitempty"`
}

// MediaKindMeta declares one content kind; each kind becomes its own library.
type MediaKindMeta struct {
	Key     string          `json:"key"`
	Label   string          `json:"label"`
	Library KindLibraryDecl `json:"library"`
	Levels  []KindLevel     `json:"levels"`
	Fields  []KindField     `json:"fields,omitempty"`
	Art     []KindArt       `json:"art,omitempty"`
	Present Presentations   `json:"present"`
	Browse  KindBrowseDecl  `json:"browse"`
	Sync    KindSyncDecl    `json:"sync"`
	Cache   *KindCacheDecl  `json:"cache,omitempty"`
	Match   *KindMatchDecl  `json:"match,omitempty"`
}

// KindLibraryDecl is the library created for a kind on install.
type KindLibraryDecl struct {
	Name string `json:"name"`
	Icon string `json:"icon,omitempty"`
}

// KindLevel is one hierarchy level of a kind.
type KindLevel struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Playable  bool   `json:"playable,omitempty"`
	Container bool   `json:"container,omitempty"`
}

// KindField is one declared item field.
type KindField struct {
	Key     string   `json:"key"`
	Type    string   `json:"type"` // string|int|float|bool|date|duration|enum|strings|people
	Label   string   `json:"label"`
	Indexed bool     `json:"indexed,omitempty"`
	Enum    []string `json:"enum,omitempty"`
}

// KindArt declares one artwork role beyond poster/backdrop/thumb/logo.
type KindArt struct {
	Role  string `json:"role"`
	Label string `json:"label"`
}

// KindBrowseDecl is how a kind library is browsed.
type KindBrowseDecl struct {
	Sorts        []string `json:"sorts,omitempty"`
	Facets       []string `json:"facets,omitempty"`
	Search       bool     `json:"search"`
	SearchFields []string `json:"searchFields,omitempty"`
	// LiveSearch: client searches also call the plugin's /search route
	// with a short budget; results are upserted and merged (§15.4).
	LiveSearch bool `json:"liveSearch,omitempty"`
}

// Kind sync modes: pull (Ether walks browse/changes) or push (KindUpsert).
const (
	KindSyncPull = "pull"
	KindSyncPush = "push"
)

// KindSyncDecl is how a kind's items reach Ether's store.
type KindSyncDecl struct {
	Mode    string `json:"mode"` // pull|push
	Refresh string `json:"refresh,omitempty"`
}

// KindCacheDecl is the per-library blob cache budget; 0 means server default.
type KindCacheDecl struct {
	MaxBytes int64  `json:"maxBytes,omitempty"`
	TTL      string `json:"ttl,omitempty"`
}

// KindMatchDecl holds matcher defaults for a kind.
type KindMatchDecl struct {
	ThresholdDefault float64 `json:"thresholdDefault,omitempty"`
}

// FrontendAdapterMeta declares one plugin-provided protocol surface.
type FrontendAdapterMeta struct {
	Key          string   `json:"key"`
	Label        string   `json:"label"`
	ProtocolHint string   `json:"protocolHint"` // opds|subsonic|custom
	PathPrefix   string   `json:"pathPrefix"`
	DefaultPort  int      `json:"defaultPort,omitempty"`
	Clients      []string `json:"clients,omitempty"`
	Auth         string   `json:"auth,omitempty"` // ether_token (default)|basic|none_lan
}

// SetupMeta declares guided setup for any plugin kind.
type SetupMeta struct {
	Steps []SetupStep `json:"steps"`
}

// SetupStep is one setup wizard step; an empty Test means saving is the step.
type SetupStep struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Hint     string   `json:"hint,omitempty"`
	Fields   []string `json:"fields"`
	Test     string   `json:"test"`
	Required bool     `json:"required"`
}

// SourceMeta describes a stream source contribution.
type SourceMeta struct {
	Class     string         `json:"class"`
	Abilities []string       `json:"abilities,omitempty"`
	Limits    map[string]any `json:"limits,omitempty"`
}

// ChannelsMeta describes a Live TV channel contribution.
type ChannelsMeta struct {
	Refresh string `json:"refresh,omitempty"`
	Test    bool   `json:"test"`
}

// SubtitlesMeta describes a subtitle provider contribution.
type SubtitlesMeta struct {
	Languages any      `json:"languages,omitempty"`
	Formats   []string `json:"formats,omitempty"`
}

// MarkersMeta describes a playback markers contribution.
type MarkersMeta struct {
	Kinds []string `json:"kinds"`
}

// NotificationMeta describes a notification channel contribution.
type NotificationMeta struct {
	Fields    any      `json:"fields,omitempty"`
	SendTest  bool     `json:"sendTest"`
	Formats   []string `json:"formats,omitempty"`
	MaxLength int      `json:"maxLength,omitempty"`
}

// EventsMeta lists subscribed event names.
type EventsMeta struct {
	Subscribe []string `json:"subscribe"`
}

// IntegrationMeta describes an external service integration.
type IntegrationMeta struct {
	Category string   `json:"category"`
	Logo     string   `json:"logo,omitempty"`
	Adds     []string `json:"adds"`
	Connect  string   `json:"connect"`
	Test     bool     `json:"test,omitempty"`
}

// ListSourceMeta describes a catalog list source.
type ListSourceMeta struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Hint     string `json:"hint,omitempty"`
	Catalog  string `json:"catalog"`
	Params   any    `json:"params,omitempty"`
	MaxItems int    `json:"maxItems,omitempty"`
	Refresh  string `json:"refresh,omitempty"`
}

// RankingStepMeta describes a custom ranking step.
type RankingStepMeta struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Hint   string `json:"hint,omitempty"`
	Editor any    `json:"editor,omitempty"`
	Budget any    `json:"budget,omitempty"`
}

// RuleConditionMeta describes a custom rule condition.
type RuleConditionMeta struct {
	Key       string   `json:"key"`
	Label     string   `json:"label"`
	Operators []string `json:"operators,omitempty"`
	Editor    any      `json:"editor,omitempty"`
}

// ToolMeta describes an agent tool contributed by a plugin.
type ToolMeta struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	ReadOnly    bool           `json:"readOnly,omitempty"`
	Destructive bool           `json:"destructive,omitempty"`
	OpenWorld   bool           `json:"openWorld,omitempty"`
}

// PageMeta describes a ViewSpec page contributed by a plugin.
type PageMeta struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Route string `json:"route"`
	Group string `json:"group,omitempty"`
	Icon  string `json:"icon,omitempty"`
}

// SlotMeta describes a UI slot contributed by a plugin.
type SlotMeta struct {
	Slot  string `json:"slot"`
	Key   string `json:"key"`
	Label string `json:"label"`
	Route string `json:"route"`
}

// CommandMeta describes a command contributed by a plugin.
type CommandMeta struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Action string `json:"action"`
}

// --- Route request and response types ---

// CatalogItem is one item in a catalog response.
type CatalogItem struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Name        string   `json:"name"`
	Poster      string   `json:"poster,omitempty"`
	PosterShape string   `json:"posterShape,omitempty"`
	Background  string   `json:"background,omitempty"`
	Logo        string   `json:"logo,omitempty"`
	Description string   `json:"description,omitempty"`
	ReleaseInfo string   `json:"releaseInfo,omitempty"`
	IMDBRating  string   `json:"imdbRating,omitempty"`
	Genres      []string `json:"genres,omitempty"`
}

// CatalogRequest is passed to a Catalog route handler.
type CatalogRequest struct {
	Type   string            `json:"type"`
	ID     string            `json:"id"`
	Extra  map[string]string `json:"extra"`
	Skip   int               `json:"skip"`
	Search string            `json:"search,omitempty"`
	Genre  string            `json:"genre,omitempty"`
}

// CatalogResponse is returned by a Catalog route handler.
type CatalogResponse struct {
	Metas []CatalogItem `json:"metas"`
}

// MetaVideo describes one episode/video inside a MetaItem.
type MetaVideo struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Released  string `json:"released,omitempty"`
	Season    int    `json:"season,omitempty"`
	Episode   int    `json:"episode,omitempty"`
	Overview  string `json:"overview,omitempty"`
	Thumbnail string `json:"thumbnail,omitempty"`
}

// MetaItem is a full metadata record returned by a Meta route handler.
type MetaItem struct {
	CatalogItem
	Runtime  string      `json:"runtime,omitempty"`
	Cast     []string    `json:"cast,omitempty"`
	Director []string    `json:"director,omitempty"`
	Videos   []MetaVideo `json:"videos,omitempty"`
}

// MetaRequest is passed to a Meta route handler.
type MetaRequest struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// MetaResponse is returned by a Meta route handler.
type MetaResponse struct {
	Meta MetaItem `json:"meta"`
}

// StreamEtherExt holds Ether-specific extensions on a StreamItem.
type StreamEtherExt struct {
	ID          string   `json:"id,omitempty"`
	ReleaseName string   `json:"releaseName,omitempty"`
	Size        int64    `json:"size,omitempty"`
	Languages   []string `json:"languages,omitempty"`
	Lazy        bool     `json:"lazy,omitempty"`
	File        string   `json:"file,omitempty"`
}

// StreamItem is one playable candidate returned by a Stream route handler.
type StreamItem struct {
	Name        string          `json:"name,omitempty"`
	Title       string          `json:"title,omitempty"`
	Description string          `json:"description,omitempty"`
	URL         string          `json:"url,omitempty"`
	InfoHash    string          `json:"infoHash,omitempty"`
	FileIdx     *int            `json:"fileIdx,omitempty"`
	YtID        string          `json:"ytId,omitempty"`
	ExternalURL string          `json:"externalUrl,omitempty"`
	Ether       *StreamEtherExt `json:"ether,omitempty"`
}

// StreamRequest is passed to a Stream route handler.
type StreamRequest struct {
	Type  string            `json:"type"`
	ID    string            `json:"id"`
	Extra map[string]string `json:"extra"`
}

// StreamResponse is returned by a Stream route handler.
type StreamResponse struct {
	Streams []StreamItem `json:"streams"`
}

// OpenStreamRequest is passed to an OpenStream route handler.
type OpenStreamRequest struct {
	StreamID string `json:"streamId"`
}

// OpenStreamResponse is returned by an OpenStream route handler.
type OpenStreamResponse struct {
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	ExpiresAt string            `json:"expiresAt,omitempty"`
	File      string            `json:"file,omitempty"`
}

// SubtitleItem is one subtitle track returned by a Subtitles route handler.
type SubtitleItem struct {
	ID              string `json:"id"`
	URL             string `json:"url,omitempty"`
	Name            string `json:"name,omitempty"`
	Lang            string `json:"lang"`
	Label           string `json:"label,omitempty"`
	Format          string `json:"format,omitempty"`
	HearingImpaired bool   `json:"hearingImpaired,omitempty"`
}

// SubtitlesRequest is passed to a Subtitles route handler.
type SubtitlesRequest struct {
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Extra     map[string]string `json:"extra"`
	PickToken string            `json:"pickToken"`
	VideoHash string            `json:"videoHash,omitempty"`
	VideoSize int64             `json:"videoSize,omitempty"`
	Filename  string            `json:"filename,omitempty"`
}

// SubtitlesResponse is returned by a Subtitles route handler.
type SubtitlesResponse struct {
	Subtitles []SubtitleItem `json:"subtitles"`
}

// SubtitleDownloadRequest identifies a viewer-picked subtitle.
type SubtitleDownloadRequest struct {
	ID     string `json:"id"`
	Lang   string `json:"lang"`
	Format string `json:"format"`
}

// SubtitleDownloadResponse carries subtitle text or base64-encoded bytes.
type SubtitleDownloadResponse struct {
	Format   string `json:"format"`
	Encoding string `json:"encoding"`
	Data     string `json:"data"`
}

// MarkerItem is one intro/credits/recap/preview marker.
type MarkerItem struct {
	Kind  string  `json:"kind"`
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Label string  `json:"label,omitempty"`
}

// MarkersRequest is passed to a Markers route handler.
type MarkersRequest struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// MarkersResponse is returned by a Markers route handler.
type MarkersResponse struct {
	Markers []MarkerItem `json:"markers"`
}

// ChannelInput is one stream input for a Live TV channel.
type ChannelInput struct {
	URL      string            `json:"url"`
	Name     string            `json:"name,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Priority int               `json:"priority,omitempty"`
}

// ChannelItem is one Live TV channel.
type ChannelItem struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Number  int            `json:"number,omitempty"`
	Group   string         `json:"group,omitempty"`
	Logo    string         `json:"logo,omitempty"`
	GuideID string         `json:"guideId,omitempty"`
	Inputs  []ChannelInput `json:"inputs"`
}

// ChannelsResponse is returned by a Channels route handler.
type ChannelsResponse struct {
	Channels []ChannelItem `json:"channels"`
}

// GuideRequest is passed to a Guide route handler.
type GuideRequest struct {
	ChannelID string `json:"channelId"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
}

// ProgrammeItem is one EPG programme entry.
type ProgrammeItem struct {
	ID          string   `json:"id,omitempty"`
	ChannelID   string   `json:"channelId,omitempty"`
	Title       string   `json:"title"`
	Subtitle    string   `json:"subtitle,omitempty"`
	Description string   `json:"description,omitempty"`
	Start       string   `json:"start"`
	Stop        string   `json:"stop"`
	Icon        string   `json:"icon,omitempty"`
	Categories  []string `json:"categories,omitempty"`
}

// GuideResponse is returned by a Guide route handler.
type GuideResponse struct {
	Programmes []ProgrammeItem `json:"programmes"`
}

// ConfigureResponse is returned by a Configure route handler.
type ConfigureResponse struct {
	Errors map[string]string `json:"errors"`
}

// TestResponse is returned by a Test route handler.
type TestResponse struct {
	OK      bool     `json:"ok"`
	Message string   `json:"message,omitempty"`
	Details []string `json:"details,omitempty"`
}

// HealthResponse is returned by a Health route handler.
type HealthResponse struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// ViewSpecDocument is a declarative ViewSpec v1 page specification.
type ViewSpecDocument struct {
	SpecVersion int              `json:"specVersion"`
	Layout      string           `json:"layout"`
	Route       string           `json:"route,omitempty"`
	Header      []map[string]any `json:"header"`
	Body        []map[string]any `json:"body"`
}

// ViewRequest is passed to a View route handler.
type ViewRequest struct {
	Page  string            `json:"page"`
	Query map[string]string `json:"query"`
}

// ViewResponse is returned by a View route handler.
type ViewResponse struct {
	Spec ViewSpecDocument `json:"spec"`
	Data map[string]any   `json:"data"`
}

// ActionProposal describes a confirmation proposal returned by an Action handler.
type ActionProposal struct {
	Category    string         `json:"category"`
	Description string         `json:"description"`
	Input       map[string]any `json:"input,omitempty"`
}

// ActionRequest is passed to an Action route handler.
type ActionRequest struct {
	Action string         `json:"action"`
	Input  map[string]any `json:"input"`
}

// ActionResponse is returned by an Action route handler.
type ActionResponse struct {
	Toast    string          `json:"toast,omitempty"`
	Refresh  bool            `json:"refresh,omitempty"`
	Open     string          `json:"open,omitempty"`
	Proposal *ActionProposal `json:"proposal,omitempty"`
}

// EtherEvent is one event delivered in a batch to POST /ether/event.
type EtherEvent struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	At       string         `json:"at"`
	ViewerID int64          `json:"viewerId,omitempty"`
	Payload  map[string]any `json:"payload,omitempty"`
}

// EventResponse is returned by an Event route handler.
type EventResponse struct {
	Ack []string `json:"ack"`
}

// ToolRequest is passed to a Tool route handler.
type ToolRequest struct {
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolResponse is returned by a Tool route handler.
type ToolResponse map[string]any

// JobRequest is passed to a Job route handler.
type JobRequest struct {
	Name         string          `json:"name"`
	Continuation json.RawMessage `json:"continuation,omitempty"`
}

// JobResponse returns continueInMs, continuation and progress to continue;
// done:true completes, failed:string stops with an error. Ether caps jobs at
// the owner time ceiling (12h by default) and 1000000 calls; store checkpoints in KV for restart recovery.
type JobResponse map[string]any

// NotifyRequest is passed to a Notify route handler.
type NotifyRequest struct {
	Title    string         `json:"title,omitempty"`
	Message  string         `json:"message"`
	Event    string         `json:"event,omitempty"`
	Settings map[string]any `json:"settings,omitempty"`
}

// NotifyResponse is returned by a Notify route handler.
type NotifyResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// FileRequest is passed to a File route handler.
type FileRequest struct {
	Path     string `json:"path"`
	Range    string `json:"range,omitempty"`
	HeadOnly bool   `json:"headOnly,omitempty"`
}

// FileRangeResponse is returned by a File route handler.
type FileRangeResponse struct {
	Status       int               `json:"status,omitempty"`
	ContentType  string            `json:"contentType,omitempty"`
	ContentRange string            `json:"contentRange,omitempty"`
	TotalSize    *int64            `json:"totalSize,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Body         []byte            `json:"-"`
}

// ClientCaps describes playback client capabilities passed to hot-path functions.
type ClientCaps struct {
	MaxHeight int      `json:"maxHeight,omitempty"`
	HDR       []string `json:"hdr,omitempty"`
	Codecs    []string `json:"codecs,omitempty"`
}

// CandidateFacts is the release candidate structure passed to RankKey and CondMatch.
type CandidateFacts struct {
	ReleaseID     string      `json:"releaseId"`
	Name          string      `json:"name"`
	Size          int64       `json:"size,omitempty"`
	Protocol      string      `json:"protocol,omitempty"`
	SourceClass   string      `json:"sourceClass,omitempty"`
	Indexer       string      `json:"indexer,omitempty"`
	Resolution    any         `json:"resolution,omitempty"`
	HDR           []string    `json:"hdr,omitempty"`
	VideoCodec    string      `json:"videoCodec,omitempty"`
	AudioCodecs   []string    `json:"audioCodecs,omitempty"`
	AudioChannels string      `json:"audioChannels,omitempty"`
	Languages     []string    `json:"languages,omitempty"`
	Subtitles     []string    `json:"subtitles,omitempty"`
	ReleaseType   string      `json:"releaseType,omitempty"`
	Group         string      `json:"group,omitempty"`
	Cached        bool        `json:"cached,omitempty"`
	Verified      bool        `json:"verified,omitempty"`
	ClientCaps    *ClientCaps `json:"clientCaps,omitempty"`
}

// RawRequest is the low-level transport-agnostic request passed to Plugin.Dispatch.
type RawRequest struct {
	PluginID string            `json:"pluginId,omitempty"`
	Method   string            `json:"method,omitempty"`
	Path     string            `json:"path"`
	Query    map[string]string `json:"query,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     json.RawMessage   `json:"body,omitempty"`
	Locale   string            `json:"locale,omitempty"`
}

// RawResponse is the low-level transport-agnostic response returned by Plugin.Dispatch.
type RawResponse struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
	BinaryBody []byte            `json:"-"`
}

// RouteError represents a structured HTTP error returned by a plugin handler.
type RouteError struct {
	Status  int
	Code    string
	Message string
}

func (e *RouteError) Error() string {
	return fmt.Sprintf("%s (%d): %s", e.Code, e.Status, e.Message)
}

// NewRouteError constructs a structured RouteError.
func NewRouteError(status int, code, message string) *RouteError {
	return &RouteError{Status: status, Code: code, Message: message}
}
