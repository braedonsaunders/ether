package core

import (
	"context"
	"time"
)

// HomeSituation describes the viewer's device and local time for row ranking.
type HomeSituation struct {
	Device, Daypart                        string
	Weekend                                bool
	Now                                    time.Time
	LibraryID                              int
	Product, Platform, SessionID, ClientID string
}

// NeutralHomeRow carries a stable producer identity and semantic row type.
// Kind is the owner-layout identity (for example a collection rule), not a wire context.
type NeutralHomeRow struct {
	ID, Title, Kind string
	Purpose         RowPurpose
	Type            MediaType
	Items           []Title
	ItemCount       int
}

// NeutralHomeLayoutSource arranges rows without a protocol-specific context.
type NeutralHomeLayoutSource interface {
	FinalizeNeutralHome(context.Context, *User, HomeSituation, string, []NeutralHomeRow) ([]NeutralHomeRow, error)
}

// NeutralImpression is a server-issued snapshot of a delivered row.
type NeutralImpression struct {
	Situation          HomeSituation
	RowID, Kind, Title string
	Purpose            RowPurpose
	Rank               int
	Items              []RatingKey
	Collection         RatingKey
	Item               RatingKey // Parent item for detail rows.
	Placement, ViewID  string
	Entries            []NeutralEntryRef
}

// NeutralEntryRef identifies one delivered entry in a stable namespace.
type NeutralEntryRef struct{ Kind, ID string }

// NeutralImpressionSink records a validated server-issued row snapshot.
type NeutralImpressionSink interface {
	RecordNeutralImpression(*User, NeutralImpression)
}
