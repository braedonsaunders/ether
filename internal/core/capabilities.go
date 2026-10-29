package core

// These named extension contracts describe optional cross-component capabilities.
// Consumers may select a capability without inflating the base interfaces or
// hiding method signatures in call sites. Implementations retain their base
// contracts; capability absence has an explicit fallback at each consumer.

import (
	"context"
	"time"
)

// ActiveUserSource checks current server membership for delegated stream access.
type ActiveUserSource interface {
	ActiveUser(context.Context, int64) (*User, error)
}

// ArtworkAccessPolicy exposes the optional Restricted capability.
type ArtworkAccessPolicy interface{ Restricted(*User) bool }

// AvailabilitySource exposes the optional Available capability.
type AvailabilitySource interface {
	Available(context.Context, RatingKey) (bool, bool)
}

// BitrateAccessPolicy exposes the optional BitrateLimit capability.
type BitrateAccessPolicy interface{ BitrateLimit(*User) int }

// CachePlayRecorder exposes the optional RecordPlay capability.
type CachePlayRecorder interface{ RecordPlay(string, string, int64) }

// CachedTitleSource exposes the optional CachedTitles capability.
type CachedTitleSource interface {
	CachedTitles(context.Context, []RatingKey) ([]Title, error)
}

// ChannelPreparer exposes the optional PrepareChannel capability.
type ChannelPreparer interface {
	PrepareChannel(context.Context, *User, RatingKey, ClientCaps, string) (*PlaySession, error)
}

// CollectionArtworkSource exposes the optional CollectionArtwork capability.
type CollectionArtworkSource interface {
	CollectionArtwork(context.Context, *User, RatingKey, string, int) ([]byte, string, bool, error)
}

// CollectionLayoutSource exposes the optional LayoutRow capability.
type CollectionLayoutSource interface {
	LayoutRow(context.Context, *User, HubContext, string) (*CollectionRow, error)
}

// CollectionSpecSource exposes the optional ListSpecs capability.
type CollectionSpecSource interface {
	ListSpecs(context.Context) ([]CollectionSpec, error)
}

// ContextReaderAt reads source bytes with request cancellation.
type ContextReaderAt interface {
	ReadAtContext(context.Context, []byte, int64) (int, error)
}

// DamageReporter exposes the optional SetDamageHandler capability.
type DamageReporter interface{ SetDamageHandler(func(int64, int64) bool) }

// DirectPlayRetrier exposes the optional RetryDirectPlay capability.
type DirectPlayRetrier interface{ RetryDirectPlay() bool }

// DownloadAccessPolicy exposes the optional AllowDownloads capability.
type DownloadAccessPolicy interface{ AllowDownloads(*User) bool }

// DurablePrefetcher exposes the optional DurablePrefetch capability.
type DurablePrefetcher interface {
	DurablePrefetch(context.Context, string, []byte, int64, int64) error
}

// HomeCandidateSource exposes the optional HomeCandidatesFor capability.
type HomeCandidateSource interface {
	HomeCandidatesFor(context.Context, *User, HubContext, int) ([]Hub, error)
}

// PolicyHomeCandidateSource filters a viewer's candidate pool before ranking
// and row limits. The caller supplies a fail-closed visibility predicate.
type PolicyHomeCandidateSource interface {
	HomeCandidatesWithPolicy(context.Context, *User, HubContext, int, func(context.Context, *User, *Title) bool) ([]Hub, error)
	SectionCandidatesWithPolicy(context.Context, *User, MediaType, HubContext, int, func(context.Context, *User, *Title) bool) ([]Hub, error)
}

// HomeLayoutSource exposes the optional FinalizeHome capability.
type HomeLayoutSource interface {
	FinalizeHome(context.Context, *User, HubContext, string, []Hub) ([]Hub, error)
}

// MetadataReaderAt reads container metadata without advancing playback or read-ahead.
type MetadataReaderAt interface {
	ReadMetadataAtContext(context.Context, []byte, int64) (int, error)
}

// PlaybackAccessPolicy exposes the optional CanPlay capability.
type PlaybackAccessPolicy interface {
	CanPlay(*User, bool, time.Time) bool
}

// PlaybackPosition exposes the optional RequestedPosition capability.
type PlaybackPosition interface{ RequestedPosition() time.Duration }

// PlaybackPreparer warms a title without reserving playback or changing history.
type PlaybackPreparer interface {
	PreResolve(context.Context, *User, RatingKey, ClientCaps) bool
}

// PrefetchReaderAt reads speculative bytes at background priority.
type PrefetchReaderAt interface {
	ReadAtPrefetchContext(context.Context, []byte, int64) (int, error)
}

// ProcessCPUTime exposes the optional CPUTime capability.
type ProcessCPUTime interface {
	CPUTime() (time.Duration, time.Duration)
}

// ProcessIdentity exposes the optional PID capability.
type ProcessIdentity interface{ PID() int }

// ProcessPeakMemory exposes the optional PeakRSSBytes capability.
type ProcessPeakMemory interface{ PeakRSSBytes() (uint64, bool) }

// ReleaseSizeSource exposes the optional ReleaseSizes capability.
type ReleaseSizeSource interface{ ReleaseSizes() map[string]int64 }

// RescueIndexer exposes the optional SearchRescue capability.
type RescueIndexer interface {
	SearchRescue(context.Context, SearchRequest) ([]Release, error)
}

// SectionCandidateSource exposes the optional SectionCandidatesFor capability.
type SectionCandidateSource interface {
	SectionCandidatesFor(context.Context, *User, MediaType, HubContext, int) ([]Hub, error)
}

// SegmentPresence exposes the optional Has capability.
type SegmentPresence interface{ Has(string, int) bool }

// StreamingIndexer exposes the optional SearchWith capability.
type StreamingIndexer interface {
	SearchWith(context.Context, SearchRequest, func([]Release)) ([]Release, error)
}

// StreamingRescueIndexer exposes the optional SearchRescueWith capability.
type StreamingRescueIndexer interface {
	SearchRescueWith(context.Context, SearchRequest, func([]Release)) ([]Release, error)
}

// TrickplayCanceler exposes the optional CancelTrickplay capability.
type TrickplayCanceler interface{ CancelTrickplay(string) }

// UserPolicySource exposes the optional UserPolicy capability.
type UserPolicySource interface{ UserPolicy(*User) *User }

// ViewerInvalidator exposes the optional Invalidate capability.
type ViewerInvalidator interface{ Invalidate(userID int64) }

// SourceDeliveryObserver receives uncached source throughput evidence. It is
// neither client bandwidth nor a claim about an entire provider network.
type SourceDeliveryObserver interface {
	ObserveSourceDelivery(Release, float64)
}
