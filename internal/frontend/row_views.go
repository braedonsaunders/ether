package frontend

import "context"

// RowView identifies one server-owned, viewer/device/surface-scoped visit.
// ID binds one immutable snapshot and changes on refresh.
type RowView struct {
	ID          string
	Title, Kind string
	Purpose     RowPurpose
	Layout      RowLayout
	Rank        int
}

type RowViewRequest struct {
	Viewer    *Viewer
	Device    Device
	Protocol  Protocol
	Placement Placement
	ViewID    string
	Item      RatingKey
	// Refresh starts new snapshots and invalidates earlier page generations.
	Refresh bool
}

type RowViewPageRequest struct {
	Viewer        *Viewer
	Device        Device
	Protocol      Protocol
	ID            string
	Offset, Limit int
	Kinds         []RowEntryKind
}

type RowViewPage struct {
	Entries          []RowEntry
	ImpressionHandle string
	Total            int
	TotalExact       bool
	More             bool
	// Truncated means the snapshot is a bounded prefix of the source row.
	// More only indicates entries available in this snapshot.
	Truncated bool
}

// RowViews is optional; IDs are not credentials. Implementations
// must reauthorize each call and issue page-scoped interaction handles.
type RowViews interface {
	ListRowViews(context.Context, RowViewRequest) ([]RowView, error)
	ResolveRowView(context.Context, RowViewPageRequest) (RowViewPage, error)
}
