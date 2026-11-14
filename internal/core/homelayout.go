package core

import "time"

// HomeLayout is the owner's global template, with complete row policies in
// viewer layers replacing matching global policies. Slots are one-based.
type HomeLayout struct {
	Global    []LayoutPage   `json:"global"`
	Viewers   []ViewerLayout `json:"viewers"`
	UpdatedAt time.Time      `json:"updatedAt,omitempty"`
	// CollectionSchedules carries collection schedules (by collection id)
	// for the Home designer: the owner API fills it on read and saves
	// entries to their collections on write (null clears), and a draft
	// preview applies it. It is never stored with the layout; the
	// collection owns its schedule, so Everyone policies for a collection
	// row carry no When.
	CollectionSchedules map[string]*CollectionSchedule `json:"collectionSchedules,omitempty"`
}
type ViewerLayout struct {
	UserID int64        `json:"userId"`
	Pages  []LayoutPage `json:"pages"`
}
type LayoutPage struct {
	Section       string         `json:"section"`
	MaxRows       int            `json:"maxRows,omitempty"`
	DeviceMaxRows map[string]int `json:"deviceMaxRows,omitempty"`
	Rows          []RowPolicy    `json:"rows"`
	// DedupeHome keeps titles the viewer's Home leads with off this page
	// (Movies/TV Shows only). Nil means the default: on.
	DedupeHome *bool `json:"dedupeHome,omitempty"`
	// Tuning shapes how the page's rows and titles are chosen. Nil fields
	// inherit (viewer page → global page → engine default).
	Tuning *LayoutTuning `json:"tuning,omitempty"`
}

// LayoutTuning is the owner's three dials for a page, each 0..1 with 0.5 as
// the engine default.
type LayoutTuning struct {
	// Discovery: 0 sticks to proven favourites, 1 explores new territory.
	Discovery *float64 `json:"discovery,omitempty"`
	// Freshness: 0 favours timeless picks, 1 favours new and trending.
	Freshness *float64 `json:"freshness,omitempty"`
	// Variety: 0 keeps rows focused on the strongest tastes, 1 spreads
	// across genres, eras and moods.
	Variety *float64 `json:"variety,omitempty"`
}
type RowPolicy struct {
	// Nil inherits the collection setting; false explicitly disables previews.
	PreviewOnFocus *bool       `json:"previewOnFocus,omitempty"`
	Row            string      `json:"row"`
	Status         string      `json:"status"`
	Slot           int         `json:"slot,omitempty"`
	MinSlot        int         `json:"minSlot,omitempty"`
	MaxSlot        int         `json:"maxSlot,omitempty"`
	Priority       float64     `json:"priority,omitempty"`
	MaxPerPage     int         `json:"maxPerPage,omitempty"`
	MaxDaysPerWeek int         `json:"maxDaysPerWeek,omitempty"`
	When           *LayoutWhen `json:"when,omitempty"`
}
