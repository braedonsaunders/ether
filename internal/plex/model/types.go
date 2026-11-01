package model

// Constants PMS uses across responses.
const (
	LibraryIdentifier = "com.plexapp.plugins.library"
	MediaTagPrefix    = "/system/bundle/media/flags/"
	// MediaTagVersion is the flag bundle version real servers report; any
	// stable value works.
	MediaTagVersion int64 = 1720000000
)

// Ptr returns a pointer to v, for optional attributes that must be emitted
// even when zero.
func Ptr[T any](v T) *T { return &v }

// MediaContainer is the root of almost every PMS response.
type MediaContainer struct {
	decision  bool
	Size      int  `plex:"size"`
	TotalSize *int `plex:"totalSize"`
	Offset    *int `plex:"offset"`

	AllowSync           *bool  `plex:"allowSync"`
	Art                 string `plex:"art,omitempty"`
	Content             string `plex:"content,omitempty"`
	Identifier          string `plex:"identifier,omitempty"`
	Key                 string `plex:"key,omitempty"`
	LibrarySectionID    *int   `plex:"librarySectionID"`
	LibrarySectionTitle string `plex:"librarySectionTitle,omitempty"`
	LibrarySectionUUID  string `plex:"librarySectionUUID,omitempty"`
	MediaTagPrefix      string `plex:"mediaTagPrefix,omitempty"`
	MediaTagVersion     int64  `plex:"mediaTagVersion,omitempty"`
	Mixed               *bool  `plex:"mixedParents"`
	Nocache             *bool  `plex:"nocache"`
	SortAsc             *bool  `plex:"sortAsc"`
	Thumb               string `plex:"thumb,omitempty"`
	Title1              string `plex:"title1,omitempty"`
	Title2              string `plex:"title2,omitempty"`
	ViewGroup           string `plex:"viewGroup,omitempty"`
	ViewMode            int    `plex:"viewMode,omitempty"`

	// Parent attributes on /children and /allLeaves containers.
	ParentIndex          *int   `plex:"parentIndex"`
	ParentTitle          string `plex:"parentTitle,omitempty"`
	ParentYear           int    `plex:"parentYear,omitempty"`
	ParentRatingKey      string `plex:"parentRatingKey,omitempty"`
	GrandparentRatingKey int64  `plex:"grandparentRatingKey,omitempty"` // an int in PMS JSON here
	GrandparentTitle     string `plex:"grandparentTitle,omitempty"`
	GrandparentThumb     string `plex:"grandparentThumb,omitempty"`
	Summary              string `plex:"summary,omitempty"`
	Theme                string `plex:"theme,omitempty"`

	// Playlist containers.
	PlaylistType string `plex:"playlistType,omitempty"`
	Smart        *bool  `plex:"smart"`
	Duration     int64  `plex:"duration,omitempty"`
	LeafCount    *int   `plex:"leafCount"`
	RatingKey    string `plex:"ratingKey,omitempty"`
	Composite    string `plex:"composite,omitempty"`

	// Extra carries endpoint-specific attributes (server info, decisions,
	// play queues) in order, after the common ones above.
	Extra []Attr `plex:",attrs"`

	Meta                *Meta                 `plex:"Meta,object"`
	Directory           []Directory           `plex:"Directory"`
	Metadata            []Metadata            `plex:"Metadata"`
	Stream              []Stream              `plex:"Stream"` // subtitle search results
	Hub                 []Hub                 `plex:"Hub"`
	SearchResult        []SearchResult        `plex:"SearchResult"`
	MediaProvider       []MediaProvider       `plex:"MediaProvider"`
	Setting             []Setting             `plex:"Setting"`
	Server              []Server              `plex:"Server"`
	Agent               []Agent               `plex:"Agent"`
	StatisticsBandwidth []StatisticsBandwidth `plex:"StatisticsBandwidth"`
	StatisticsResources []StatisticsResources `plex:"StatisticsResources"`
	StatisticsMedia     []StatisticsMedia     `plex:"StatisticsMedia"`
	Device              []Device              `plex:"Device"`
	Activity            []Activity            `plex:"Activity"`
	ButlerTask          []ButlerTask          `plex:"ButlerTask"`
	Account             []Account             `plex:"Account"`
	Release             []UpdateRelease       `plex:"Release"`
	TranscodeSess       []TranscodeSess       `plex:"TranscodeSession"`
	FieldType           []FieldType           `plex:"FieldType"`
	Type                []MetaType            `plex:"Type"`
	Sort                []Sort                `plex:"Sort"`
	Filter              []Filter              `plex:"Filter"`
	Field               []Field               `plex:"Field"`
	PlaySessionCtl      []SessionControl      `plex:"SessionControl"`
}

// SetPage fills size/offset/totalSize.
func (m *MediaContainer) SetPage(offset, total int) {
	m.Offset = Ptr(offset)
	m.TotalSize = Ptr(total)
}

// Directory is a navigational entry: a library section, a filter value, a
// root directory or a section sub-listing.
type Directory struct {
	Count            int    `plex:"count,omitempty"`
	Size             *int   `plex:"size"`
	Secondary        *bool  `plex:"secondary"`
	Prompt           string `plex:"prompt,omitempty"`
	Search           *bool  `plex:"search"`
	LeafCount        *int   `plex:"leafCount"`
	AllowSync        *bool  `plex:"allowSync"`
	Art              string `plex:"art,omitempty"`
	Composite        string `plex:"composite,omitempty"`
	Filters          *bool  `plex:"filters"`
	Refreshing       *bool  `plex:"refreshing"`
	Thumb            string `plex:"thumb,omitempty"`
	ViewedLeafCount  *int   `plex:"viewedLeafCount"`
	ID               string `plex:"id,omitempty"`
	FastKey          string `plex:"fastKey,omitempty"`
	Key              string `plex:"key,omitempty"`
	HubKey           string `plex:"hubKey,omitempty"`
	Type             string `plex:"type,omitempty"`
	Title            string `plex:"title,omitempty"`
	Agent            string `plex:"agent,omitempty"`
	Scanner          string `plex:"scanner,omitempty"`
	Language         string `plex:"language,omitempty"`
	UUID             string `plex:"uuid,omitempty"`
	UpdatedAt        int64  `plex:"updatedAt,omitempty"`
	CreatedAt        int64  `plex:"createdAt,omitempty"`
	ScannedAt        int64  `plex:"scannedAt,omitempty"`
	Content          *bool  `plex:"content"`
	IsDirectory      *bool  `plex:"directory"`
	ContentChangedAt int64  `plex:"contentChangedAt,omitempty"`
	Hidden           *int   `plex:"hidden"`
	Icon             string `plex:"icon,omitempty"`

	Extra []Attr `plex:",attrs"`

	Location []Location `plex:"Location"`
	Pivot    []Pivot    `plex:"Pivot"`
	Filter   []Filter   `plex:"Filter"`
	Sort     []Sort     `plex:"Sort"`
}

// ProviderDirectory is a content directory inside a /media/providers
// feature; PMS orders its attributes differently from Directory.
type ProviderDirectory struct {
	Agent      string `plex:"agent,omitempty"`
	Language   string `plex:"language,omitempty"`
	Refreshing *bool  `plex:"refreshing"`
	Scanner    string `plex:"scanner,omitempty"`
	UUID       string `plex:"uuid,omitempty"`
	ID         string `plex:"id,omitempty"`
	Key        string `plex:"key,omitempty"`
	HubKey     string `plex:"hubKey,omitempty"`
	Type       string `plex:"type,omitempty"`
	Title      string `plex:"title"`
	UpdatedAt  int64  `plex:"updatedAt,omitempty"`
	ScannedAt  int64  `plex:"scannedAt,omitempty"`
	Icon       string `plex:"icon,omitempty"`

	Pivot []Pivot `plex:"Pivot"`
}

// XMLElementName implements ElementNamer.
func (ProviderDirectory) XMLElementName() string { return "Directory" }

type Location struct {
	ID   int    `plex:"id"`
	Path string `plex:"path"`
}

type Pivot struct {
	ID      string `plex:"id"`
	Key     string `plex:"key"`
	Type    string `plex:"type"`
	Title   string `plex:"title"`
	Context string `plex:"context"`
	Symbol  string `plex:"symbol"`
}

// Metadata is a library item. In XML it renders as <Video> for playable
// items, <Directory> for containers and <Playlist> for playlists; in JSON
// all are under "Metadata".
type Metadata struct {
	element string
	alpha   bool

	// On Deck items lead with their sync flag and section (PMS).
	AllowSync             *bool   `plex:"allowSync"`
	RatingKey             string  `plex:"ratingKey,omitempty"`
	Key                   string  `plex:"key,omitempty"`
	ParentRatingKey       string  `plex:"parentRatingKey,omitempty"`
	GrandparentRatingKey  string  `plex:"grandparentRatingKey,omitempty"`
	GUID                  string  `plex:"guid,omitempty"`
	ParentGUID            string  `plex:"parentGuid,omitempty"`
	GrandparentGUID       string  `plex:"grandparentGuid,omitempty"`
	Slug                  string  `plex:"slug,omitempty"`
	GrandparentSlug       string  `plex:"grandparentSlug,omitempty"`
	ParentSlug            string  `plex:"parentSlug,omitempty"`
	ParentStudio          string  `plex:"parentStudio,omitempty"`
	Studio                string  `plex:"studio,omitempty"`
	Type                  string  `plex:"type,omitempty"`
	Subtype               string  `plex:"subtype,omitempty"`
	Title                 string  `plex:"title"`
	GrandparentKey        string  `plex:"grandparentKey,omitempty"`
	ParentKey             string  `plex:"parentKey,omitempty"`
	LibrarySectionTitle   string  `plex:"librarySectionTitle,omitempty"`
	LibrarySectionID      int     `plex:"librarySectionID,omitempty"`
	LibrarySectionKey     string  `plex:"librarySectionKey,omitempty"`
	LibrarySectionUUID    string  `plex:"librarySectionUUID,omitempty"`
	GrandparentTitle      string  `plex:"grandparentTitle,omitempty"`
	ParentTitle           string  `plex:"parentTitle,omitempty"`
	OriginalTitle         string  `plex:"originalTitle,omitempty"`
	TitleSort             string  `plex:"titleSort,omitempty"`
	ContentRating         string  `plex:"contentRating,omitempty"`
	Summary               string  `plex:"summary"`
	Smart                 *bool   `plex:"smart"`
	PlaylistType          string  `plex:"playlistType,omitempty"`
	Index                 *int    `plex:"index"`
	ParentIndex           *int    `plex:"parentIndex"`
	Rating                float64 `plex:"rating,omitempty"`
	AudienceRating        float64 `plex:"audienceRating,omitempty"`
	UserRating            float64 `plex:"userRating,omitempty"`
	ViewOffset            int64   `plex:"viewOffset,omitempty"`
	ViewCount             int     `plex:"viewCount,omitempty"`
	SkipCount             int     `plex:"skipCount,omitempty"`
	LastViewedAt          int64   `plex:"lastViewedAt,omitempty"`
	IncludedAt            int64   `plex:"includedAt,omitempty"` // joined Continue Watching
	LastRatedAt           int64   `plex:"lastRatedAt,omitempty"`
	Year                  int     `plex:"year,omitempty"`
	ParentYear            int     `plex:"parentYear,omitempty"`
	Tagline               string  `plex:"tagline,omitempty"`
	Thumb                 string  `plex:"thumb,omitempty"`
	Art                   string  `plex:"art,omitempty"`
	Banner                string  `plex:"banner,omitempty"`
	Composite             string  `plex:"composite,omitempty"`
	Icon                  string  `plex:"icon,omitempty"`
	ParentThumb           string  `plex:"parentThumb,omitempty"`
	GrandparentThumb      string  `plex:"grandparentThumb,omitempty"`
	GrandparentArt        string  `plex:"grandparentArt,omitempty"`
	Theme                 string  `plex:"theme,omitempty"`
	GrandparentTheme      string  `plex:"grandparentTheme,omitempty"`
	Duration              int64   `plex:"duration,omitempty"`
	OriginallyAvailableAt string  `plex:"originallyAvailableAt,omitempty"`
	LeafCount             *int    `plex:"leafCount"`
	ViewedLeafCount       *int    `plex:"viewedLeafCount"`
	ChildCount            *int    `plex:"childCount"`
	MinYear               string  `plex:"minYear,omitempty"`
	MaxYear               string  `plex:"maxYear,omitempty"`
	CollectionSort        string  `plex:"collectionSort,omitempty"`
	AddedAt               int64   `plex:"addedAt,omitempty"`
	UpdatedAt             int64   `plex:"updatedAt,omitempty"`
	AudienceRatingImage   string  `plex:"audienceRatingImage,omitempty"`
	ChapterSource         string  `plex:"chapterSource,omitempty"`
	PrimaryExtraKey       string  `plex:"primaryExtraKey,omitempty"`
	RatingImage           string  `plex:"ratingImage,omitempty"`
	SkipChildren          *bool   `plex:"skipChildren"`
	PlaylistItemID        int64   `plex:"playlistItemID,omitempty"`
	PlayQueueItemID       int64   `plex:"playQueueItemID,omitempty"`
	SessionKey            string  `plex:"sessionKey,omitempty"`
	Reason                string  `plex:"reason,omitempty"`
	ReasonTitle           string  `plex:"reasonTitle,omitempty"`
	ReasonID              string  `plex:"reasonID,omitempty"`
	Score                 string  `plex:"score,omitempty"`

	Extra []Attr `plex:",attrs"`

	// Children follow PMS's order: Media, Image, UltraBlurColors, then the
	// tags (Genre, Country, Guid, Rating, Collection, Director, Writer,
	// Role, Producer, Label, Similar).
	Marker          []Marker         `plex:"Marker"`
	Chapter         []Chapter        `plex:"Chapter"`
	Media           []Media          `plex:"Media"`
	Image           []Image          `plex:"Image"`
	UltraBlurColors *UltraBlurColors `plex:"UltraBlurColors,object"`
	// UltraBlurList is UltraBlurColors as decision responses send it: an
	// array in JSON. Set one or the other, never both.
	UltraBlurList []UltraBlurColors `plex:"UltraBlurColors"`
	// GuidLead is Guid as list items (related hubs) carry it: before the
	// tags, where detail pages put it after Country.
	GuidLead   []GUID      `plex:"Guid"`
	Genre      []Tag       `plex:"Genre"`
	Country    []Tag       `plex:"Country"`
	Guid       []GUID      `plex:"Guid"`
	Rating_    []RatingTag `plex:"Rating"`
	Collection []Tag       `plex:"Collection"`
	Director   []Tag       `plex:"Director"`
	Writer     []Tag       `plex:"Writer"`
	Role       []Tag       `plex:"Role"`
	Producer   []Tag       `plex:"Producer"`
	Label      []Tag       `plex:"Label"`
	Similar    []Tag       `plex:"Similar"`
	Studio_    []Tag       `plex:"Studio"`

	User             *SessionUser   `plex:"User,object"`
	Player           *Player        `plex:"Player,object"`
	Session          *Session       `plex:"Session,object"`
	TranscodeSession *TranscodeSess `plex:"TranscodeSession,object"`

	// Extras (includeExtras=1): always empty for Ether. PMS sends them
	// before Related.
	Extras *Extras `plex:"Extras,object"`
	// Related hubs inside a detail response (includeRelated=1).
	Related *Related `plex:"Related,object"`
	// OnDeck episode inside a show detail (includeOnDeck=1).
	OnDeck *OnDeck `plex:"OnDeck,object"`
	// Children inside a detail response (includeChildren=1).
	Children *Children `plex:"Children,object"`
}

// SetElement overrides the XML element name.
func (m *Metadata) SetElement(name string) { m.element = name }

// XMLElementName implements ElementNamer.
func (m Metadata) XMLElementName() string {
	if m.element != "" {
		return m.element
	}
	switch m.Type {
	case "movie", "episode", "clip", "trailer":
		return "Video"
	case "playlist":
		return "Playlist"
	case "track":
		return "Track"
	case "photo":
		return "Photo"
	}
	return "Directory"
}

type Related struct {
	Hub []Hub `plex:"Hub"`
}

type OnDeck struct {
	// PMS uses a single object here in JSON, unlike the Metadata arrays
	// in Children and MediaContainer. XML still renders one Video child.
	Metadata Metadata `plex:"Metadata,object"`
}

type Children struct {
	Size     int        `plex:"size"`
	Metadata []Metadata `plex:"Metadata"`
}

type Extras struct {
	Size     int        `plex:"size"`
	Metadata []Metadata `plex:"Metadata"`
}

// Tag is Genre, Director, Writer, Role, Country, Collection... entries.
type Tag struct {
	alpha  bool
	ID     int64  `plex:"id,omitempty"`
	Filter string `plex:"filter,omitempty"`
	Tag    string `plex:"tag"`
	TagKey string `plex:"tagKey,omitempty"`
	Role   string `plex:"role,omitempty"`
	Thumb  string `plex:"thumb,omitempty"`
	Count  int    `plex:"count,omitempty"`
}

// GUID is an external identifier, e.g. "imdb://tt0111161".
type GUID struct {
	ID string `plex:"id"`
}

type RatingTag struct {
	alpha bool
	Image string  `plex:"image"`
	Value float64 `plex:"value"`
	Type  string  `plex:"type"`
}

type Image struct {
	Alt  string `plex:"alt"`
	Type string `plex:"type"`
	URL  string `plex:"url"`
}

// UltraBlurColors are the four corner colours (6-digit hex, no "#") the
// newer Plex apps blend into a blurred background behind an item. One
// element in XML, an object (not an array) in JSON.
type UltraBlurColors struct {
	alpha       bool
	TopLeft     string `plex:"topLeft"`
	TopRight    string `plex:"topRight"`
	BottomRight string `plex:"bottomRight"`
	BottomLeft  string `plex:"bottomLeft"`
}

// Media is one version of an item. Ether always exposes exactly one.
type Media struct {
	Extra                 []Attr `plex:",attrs"`
	alpha                 bool
	lead                  []string // decision: attributes before the alphabetical rest
	ID                    int64    `plex:"id"`
	Duration              int64    `plex:"duration,omitempty"`
	Bitrate               int      `plex:"bitrate,omitempty"`
	Width                 int      `plex:"width,omitempty"`
	Height                int      `plex:"height,omitempty"`
	AspectRatio           float64  `plex:"aspectRatio,omitempty"`
	AudioChannels         int      `plex:"audioChannels,omitempty"`
	AudioCodec            string   `plex:"audioCodec,omitempty"`
	VideoCodec            string   `plex:"videoCodec,omitempty"`
	VideoResolution       string   `plex:"videoResolution,omitempty"`
	Container             string   `plex:"container,omitempty"`
	VideoFrameRate        string   `plex:"videoFrameRate,omitempty"`
	OptimizedForStreaming *int     `plex:"optimizedForStreaming"` // an int in PMS JSON, unlike Part's bool
	AudioProfile          string   `plex:"audioProfile,omitempty"`
	Has64bitOffsets       *bool    `plex:"has64bitOffsets"`
	VideoProfile          string   `plex:"videoProfile,omitempty"`
	Protocol              string   `plex:"protocol,omitempty"`
	Selected              *bool    `plex:"selected"`
	HasVoiceActivity      *bool    `plex:"hasVoiceActivity"`

	Part []Part `plex:"Part"`
}

type Part struct {
	alpha                 bool
	lead                  []string // decision: attributes before the alphabetical rest
	ID                    int64    `plex:"id"`
	Key                   string   `plex:"key,omitempty"`
	Duration              int64    `plex:"duration,omitempty"`
	File                  string   `plex:"file,omitempty"`
	Size                  int64    `plex:"size,omitempty"`
	AudioProfile          string   `plex:"audioProfile,omitempty"`
	Container             string   `plex:"container,omitempty"`
	Has64bitOffsets       *bool    `plex:"has64bitOffsets"`
	OptimizedForStreaming *bool    `plex:"optimizedForStreaming"`
	VideoProfile          string   `plex:"videoProfile,omitempty"`
	Indexes               string   `plex:"indexes,omitempty"`
	Bitrate               int      `plex:"bitrate,omitempty"`
	Width                 int      `plex:"width,omitempty"`
	Height                int      `plex:"height,omitempty"`
	Decision              string   `plex:"decision,omitempty"`
	Protocol              string   `plex:"protocol,omitempty"`
	Selected              *bool    `plex:"selected"`
	Exists                *bool    `plex:"exists"`
	Accessible            *bool    `plex:"accessible"`

	Stream []Stream `plex:"Stream"`
}

// Stream is a video (1), audio (2) or subtitle (3) track.
type Stream struct {
	alpha                bool
	ID                   int64  `plex:"id"`
	StreamType           int    `plex:"streamType"`
	CanAutoSync          *bool  `plex:"canAutoSync"`          // subtitles
	CanNormalizeLoudness *bool  `plex:"canNormalizeLoudness"` // audio
	Selected             *bool  `plex:"selected"`
	Default              *bool  `plex:"default"`
	Forced               *bool  `plex:"forced"`
	Codec                string `plex:"codec,omitempty"`
	Index                *int   `plex:"index"`
	Key                  string `plex:"key,omitempty"`
	Channels             int    `plex:"channels,omitempty"`
	Bitrate              int    `plex:"bitrate,omitempty"`
	Language             string `plex:"language,omitempty"`
	LanguageTag          string `plex:"languageTag,omitempty"`
	LanguageCode         string `plex:"languageCode,omitempty"`
	// From here PMS lists the technical attributes alphabetically.
	AudioChannelLayout   string  `plex:"audioChannelLayout,omitempty"`
	BitDepth             int     `plex:"bitDepth,omitempty"`
	ChromaSubsampling    string  `plex:"chromaSubsampling,omitempty"`
	ColorPrimaries       string  `plex:"colorPrimaries,omitempty"`
	ColorRange           string  `plex:"colorRange,omitempty"`
	ColorSpace           string  `plex:"colorSpace,omitempty"`
	ColorTrc             string  `plex:"colorTrc,omitempty"`
	DOVIBLCompatID       *int    `plex:"DOVIBLCompatID"`
	DOVIBLPresent        *bool   `plex:"DOVIBLPresent"`
	DOVIELPresent        *bool   `plex:"DOVIELPresent"`
	DOVILevel            int     `plex:"DOVILevel,omitempty"`
	DOVIPresent          *bool   `plex:"DOVIPresent"`
	DOVIProfile          int     `plex:"DOVIProfile,omitempty"`
	DOVIRPUPresent       *bool   `plex:"DOVIRPUPresent"`
	DOVIVersion          string  `plex:"DOVIVersion,omitempty"`
	Format               string  `plex:"format,omitempty"`
	FrameRate            float64 `plex:"frameRate,omitempty"`
	Height               int     `plex:"height,omitempty"`
	Level                int     `plex:"level,omitempty"`
	Profile              string  `plex:"profile,omitempty"`
	RefFrames            int     `plex:"refFrames,omitempty"`
	SamplingRate         int     `plex:"samplingRate,omitempty"`
	ScanType             string  `plex:"scanType,omitempty"`
	Width                int     `plex:"width,omitempty"`
	Title                string  `plex:"title,omitempty"`
	DisplayTitle         string  `plex:"displayTitle,omitempty"`
	ExtendedDisplayTitle string  `plex:"extendedDisplayTitle,omitempty"`
	Decision             string  `plex:"decision,omitempty"`
	Location             string  `plex:"location,omitempty"`
	External             *bool   `plex:"external"`
	// Extra carries endpoint-specific attributes (subtitle search results).
	Extra []Attr `plex:",attrs"`
}

// Hub is a row of items.
type Hub struct {
	HubKey        string `plex:"hubKey,omitempty"`
	Key           string `plex:"key,omitempty"`
	Title         string `plex:"title"`
	Type          string `plex:"type"`
	HubIdentifier string `plex:"hubIdentifier"`
	Context       string `plex:"context"`
	Size          int    `plex:"size"`
	TotalSize     *int   `plex:"totalSize"`
	More          bool   `plex:"more"`
	Style         string `plex:"style,omitempty"`
	Promoted      *bool  `plex:"promoted"`
	Random        *bool  `plex:"random"`

	Metadata  []Metadata  `plex:"Metadata"`
	Directory []Directory `plex:"Directory"`
}

// SearchResult wraps one /library/search hit.
type SearchResult struct {
	Score     float64    `plex:"score"`
	Metadata  *Metadata  `plex:"Metadata,object"`
	Directory *Directory `plex:"Directory,object"`
}

// Meta describes a section's types, filters, sorts and fields
// (includeMeta=1).
type Meta struct {
	Type      []MetaType  `plex:"Type"`
	FieldType []FieldType `plex:"FieldType"`
}

type MetaType struct {
	Key    string   `plex:"key"`
	Type   string   `plex:"type"`
	Title  string   `plex:"title"`
	Active bool     `plex:"active"`
	Filter []Filter `plex:"Filter"`
	Sort   []Sort   `plex:"Sort"`
	Field  []Field  `plex:"Field"`
}

type Filter struct {
	Filter     string `plex:"filter"`
	FilterType string `plex:"filterType"`
	Key        string `plex:"key"`
	Title      string `plex:"title"`
	Type       string `plex:"type"`
}

type Sort struct {
	Active            *bool  `plex:"active"`
	ActiveDirection   string `plex:"activeDirection,omitempty"`
	Default           string `plex:"default,omitempty"`
	DefaultDirection  string `plex:"defaultDirection"`
	DescKey           string `plex:"descKey"`
	FirstCharacterKey string `plex:"firstCharacterKey,omitempty"`
	Key               string `plex:"key"`
	Title             string `plex:"title"`
}

type Field struct {
	Key     string `plex:"key"`
	Title   string `plex:"title"`
	Type    string `plex:"type"`
	SubType string `plex:"subType,omitempty"`
}

type FieldType struct {
	Type     string     `plex:"type"`
	Operator []Operator `plex:"Operator"`
}

type Operator struct {
	Key   string `plex:"key"`
	Title string `plex:"title"`
}

// MediaProvider is one entry of /media/providers.
type MediaProvider struct {
	Extra      []Attr    `plex:",attrs"`
	Identifier string    `plex:"identifier"`
	Title      string    `plex:"title"`
	Types      string    `plex:"types"`
	Protocols  string    `plex:"protocols"`
	Feature    []Feature `plex:"Feature"`
}

type Feature struct {
	Flavor        string              `plex:"flavor,omitempty"`
	ScrobbleKey   string              `plex:"scrobbleKey,omitempty"`
	UnscrobbleKey string              `plex:"unscrobbleKey,omitempty"`
	Key           string              `plex:"key,omitempty"`
	Type          string              `plex:"type"`
	Directory     []ProviderDirectory `plex:"Directory"`
	Action        []Action            `plex:"Action"`
}

type Action struct {
	ID  string `plex:"id"`
	Key string `plex:"key"`
}

// Setting is one server preference.
type Setting struct {
	ID         string `plex:"id"`
	Label      string `plex:"label"`
	Summary    string `plex:"summary"`
	Type       string `plex:"type"`
	Default    any    `plex:"default"` // typed: string, int or bool
	Value      any    `plex:"value"`
	Hidden     bool   `plex:"hidden"`
	Advanced   bool   `plex:"advanced"`
	Group      string `plex:"group"`
	EnumValues string `plex:"enumValues,omitempty"`
}

type Server struct {
	Name              string `plex:"name"`
	Host              string `plex:"host"`
	Address           string `plex:"address"`
	Port              int    `plex:"port"`
	MachineIdentifier string `plex:"machineIdentifier"`
	Version           string `plex:"version"`
}

type Agent struct {
	HasAttributionURL bool        `plex:"hasAttribution"`
	HasPrefs          bool        `plex:"hasPrefs"`
	Identifier        string      `plex:"identifier"`
	Primary           bool        `plex:"primary"`
	Name              string      `plex:"name"`
	MediaType         []MediaType `plex:"MediaType"`
}

type MediaType struct {
	MediaType int    `plex:"mediaType"`
	Name      string `plex:"name"`
}

type Device struct {
	ID               int64  `plex:"id"`
	Name             string `plex:"name"`
	Platform         string `plex:"platform"`
	ClientIdentifier string `plex:"clientIdentifier"`
	CreatedAt        int64  `plex:"createdAt"`
}

// StatisticsBandwidth is one Dashboard bandwidth series row.
type StatisticsBandwidth struct {
	AccountID int64 `plex:"accountID"`
	DeviceID  int64 `plex:"deviceID"`
	Timespan  int   `plex:"timespan"`
	At        int64 `plex:"at"`
	Lan       bool  `plex:"lan"`
	Bytes     int64 `plex:"bytes"`
}

// StatisticsResources is one Dashboard host-sample row.
type StatisticsResources struct {
	Timespan   int     `plex:"timespan"`
	At         int64   `plex:"at"`
	HostCPU    float64 `plex:"hostCpuUtilization"`
	ProcessCPU float64 `plex:"processCpuUtilization"`
	HostMemory float64 `plex:"hostMemoryUtilization"`
	ProcessMem float64 `plex:"processMemoryUtilization"`
}

// StatisticsMedia is one Dashboard completed-play row.
type StatisticsMedia struct {
	AccountID    int64 `plex:"accountID"`
	DeviceID     int64 `plex:"deviceID"`
	Timespan     int   `plex:"timespan"`
	At           int64 `plex:"at"`
	MetadataType int   `plex:"metadataType"`
	Count        int64 `plex:"count"`
	Duration     int64 `plex:"duration"`
}

type Activity struct {
	UUID        string `plex:"uuid"`
	Type        string `plex:"type"`
	Cancellable bool   `plex:"cancellable"`
	UserID      int64  `plex:"userID"`
	Title       string `plex:"title"`
	Subtitle    string `plex:"subtitle"`
	Progress    int    `plex:"progress"`
}

type ButlerTask struct {
	Name              string `plex:"name"`
	Interval          int    `plex:"interval"`
	ScheduleRandomize bool   `plex:"scheduleRandomized"`
	Enabled           bool   `plex:"enabled"`
	Title             string `plex:"title"`
	Description       string `plex:"description"`
}

type Account struct {
	ID                   int64  `plex:"id"`
	Key                  string `plex:"key"`
	Name                 string `plex:"name"`
	DefaultAudioLanguage string `plex:"defaultAudioLanguage,omitempty"`
	AutoSelectAudio      bool   `plex:"autoSelectAudio"`
	DefaultSubtitleLang  string `plex:"defaultSubtitleLanguage,omitempty"`
	SubtitleMode         int    `plex:"subtitleMode"`
	Thumb                string `plex:"thumb,omitempty"`
}

type UpdateRelease struct {
	Key     string `plex:"key"`
	Version string `plex:"version"`
	State   string `plex:"state"`
}

// SessionUser, Player, Session and TranscodeSess appear in /status/sessions.
type SessionUser struct {
	ID    string `plex:"id"`
	Thumb string `plex:"thumb,omitempty"`
	Title string `plex:"title"`
}

type Player struct {
	Address             string `plex:"address"`
	Device              string `plex:"device,omitempty"`
	MachineIdentifier   string `plex:"machineIdentifier"`
	Model               string `plex:"model,omitempty"`
	Platform            string `plex:"platform,omitempty"`
	PlatformVersion     string `plex:"platformVersion,omitempty"`
	Product             string `plex:"product,omitempty"`
	Profile             string `plex:"profile,omitempty"`
	RemotePublicAddress string `plex:"remotePublicAddress,omitempty"`
	State               string `plex:"state"`
	Title               string `plex:"title,omitempty"`
	Vendor              string `plex:"vendor,omitempty"`
	Version             string `plex:"version,omitempty"`
	Local               bool   `plex:"local"`
	Relayed             bool   `plex:"relayed"`
	Secure              bool   `plex:"secure"`
	UserID              int64  `plex:"userID"`
}

type Session struct {
	ID        string `plex:"id"`
	Bandwidth int    `plex:"bandwidth"`
	Location  string `plex:"location"`
}

type TranscodeSess struct {
	Key                  string  `plex:"key"`
	Throttled            bool    `plex:"throttled"`
	Complete             bool    `plex:"complete"`
	Progress             float64 `plex:"progress"`
	Speed                float64 `plex:"speed,omitempty"`
	Duration             int64   `plex:"duration,omitempty"`
	Context              string  `plex:"context"`
	SourceVideoCodec     string  `plex:"sourceVideoCodec,omitempty"`
	SourceAudioCodec     string  `plex:"sourceAudioCodec,omitempty"`
	VideoDecision        string  `plex:"videoDecision,omitempty"`
	AudioDecision        string  `plex:"audioDecision,omitempty"`
	SubtitleDecision     string  `plex:"subtitleDecision,omitempty"`
	Protocol             string  `plex:"protocol"`
	Container            string  `plex:"container"`
	VideoCodec           string  `plex:"videoCodec,omitempty"`
	AudioCodec           string  `plex:"audioCodec,omitempty"`
	AudioChannels        int     `plex:"audioChannels,omitempty"`
	TranscodeHwRequested bool    `plex:"transcodeHwRequested"`
	TimeStamp            float64 `plex:"timeStamp,omitempty"`
}

type SessionControl struct {
	Key string `plex:"key"`
}

// MyPlex is the root of /myplex/account.
type MyPlex struct {
	AuthToken            string `plex:"authToken,omitempty"`
	Username             string `plex:"username"`
	MappingState         string `plex:"mappingState"`
	MappingError         string `plex:"mappingError"`
	SignInState          string `plex:"signInState"`
	PublicAddress        string `plex:"publicAddress,omitempty"`
	PublicPort           int    `plex:"publicPort,omitempty"`
	PrivateAddress       string `plex:"privateAddress,omitempty"`
	PrivatePort          int    `plex:"privatePort,omitempty"`
	SubscriptionFeatures string `plex:"subscriptionFeatures,omitempty"`
	SubscriptionActive   bool   `plex:"subscriptionActive"`
	SubscriptionState    string `plex:"subscriptionState"`
}

// XMLElementName implements ElementNamer.
func (MyPlex) XMLElementName() string { return "MyPlex" }

// Notification containers for websockets/eventsource.
type NotificationContainer struct {
	Type                         string                         `plex:"type"`
	Size                         int                            `plex:"size"`
	PlaySessionStateNotification []PlaySessionStateNotification `plex:"PlaySessionStateNotification"`
	TimelineEntry                []TimelineEntry                `plex:"TimelineEntry"`
	ActivityNotification         []ActivityNotification         `plex:"ActivityNotification"`
	StatusNotification           []StatusNotification           `plex:"StatusNotification"`
	TranscodeSession             []TranscodeSess                `plex:"TranscodeSession"`
}

// XMLElementName implements ElementNamer.
func (NotificationContainer) XMLElementName() string { return "NotificationContainer" }

type PlaySessionStateNotification struct {
	SessionKey       string `plex:"sessionKey"`
	ClientIdentifier string `plex:"clientIdentifier"`
	GUID             string `plex:"guid"`
	RatingKey        string `plex:"ratingKey"`
	URL              string `plex:"url"`
	Key              string `plex:"key"`
	ViewOffset       int64  `plex:"viewOffset"`
	PlayQueueItemID  int64  `plex:"playQueueItemID,omitempty"`
	State            string `plex:"state"`
	TranscodeSession string `plex:"transcodeSession,omitempty"`
}

type TimelineEntry struct {
	Identifier    string `plex:"identifier"`
	SectionID     string `plex:"sectionID"`
	ItemID        string `plex:"itemID"`
	ParentItemID  string `plex:"parentItemID,omitempty"`
	RootItemID    string `plex:"rootItemID,omitempty"`
	Type          int    `plex:"type"`
	Title         string `plex:"title"`
	State         int    `plex:"state"`
	MetadataState string `plex:"metadataState,omitempty"`
	UpdatedAt     int64  `plex:"updatedAt"`
}

type ActivityNotification struct {
	Event    string    `plex:"event"`
	UUID     string    `plex:"uuid"`
	Activity *Activity `plex:"Activity,object"`
}

type StatusNotification struct {
	Title            string `plex:"title"`
	Description      string `plex:"description"`
	NotificationName string `plex:"notificationName"`
}

// A builds an Attr.
func A(name string, value any) Attr { return Attr{Name: name, Value: value} }
