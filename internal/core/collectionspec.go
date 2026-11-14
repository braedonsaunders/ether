package core

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// CollectionSpec is an owner-designed collection: candidates gathered from
// Sources, filtered by Rules, sorted, limited and published to Plex.
type CollectionSpec struct {
	ID          string             `json:"id,omitempty"` // server-assigned
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	MediaType   string             `json:"mediaType"` // "movie" | "show" | "mixed"
	Sources     []CollectionSource `json:"sources"`
	Match       string             `json:"match"` // "all" | "any"
	Rules       []CollectionRule   `json:"rules"`
	Exclude     []CollectionRule   `json:"exclude"`
	// Sort: popularity | rating | released | title | added | random | personal | source.
	Sort          string            `json:"sort"`
	SortDesc      bool              `json:"sortDesc"`
	Limit         int               `json:"limit"`   // 0 = no limit
	Refresh       string            `json:"refresh"` // live | hourly | daily | weekly
	Rotate        *CollectionRotate `json:"rotate,omitempty"`
	Artwork       CollectionArtwork `json:"artwork"`
	Targets       CollectionTargets `json:"targets"`
	RankedTop10   bool              `json:"rankedTop10,omitempty"`
	Enabled       bool              `json:"enabled"`
	LocalOnly     bool              `json:"localOnly,omitempty"`     // evaluate rules against the local catalogue only; no remote discovery
	ReleaseMode   string            `json:"releaseMode,omitempty"`   // released (default), upcoming, or mixed
	LookaheadDays int               `json:"lookaheadDays,omitempty"` // 1..366 when upcoming is enabled
	TemplateID    string            `json:"templateId,omitempty"`
	// Server-maintained.
	UpdatedAt   *time.Time `json:"updatedAt,omitempty"`
	LastBuiltAt *time.Time `json:"lastBuiltAt,omitempty"`
	ItemCount   int        `json:"itemCount"`

	// Additive fields beyond the contract (read-only for clients).
	BuiltIn   bool     `json:"builtIn,omitempty"`   // shipped with Ether; only targets/artwork/rotate/enabled are editable
	RatingKey string   `json:"ratingKey,omitempty"` // the Plex ratingKey of the published collection
	Warnings  []string `json:"warnings,omitempty"`  // from the last build
	// OwnerUserID is set for private collections created by a Plex user
	// (core.Collections.CreateSmart); 0 = server-wide.
	OwnerUserID int64 `json:"ownerUserId,omitempty"`
}

// CollectionSource is one place candidates come from. Kind selects which
// other fields apply:
//
//	catalog                                   the whole browsable catalogue
//	tmdbList        List, MediaType           trending_day|trending_week|popular|top_rated|now_playing|upcoming|airing_today|on_the_air
//	tmdbCollection  ID, Label                 a TMDB franchise
//	tmdbDiscover    Params                    raw /discover parameters (names allowed for ids)
//	tmdbKeyword | tmdbCompany | tmdbNetwork | tmdbPerson   ID, Label (Role for people)
//	watchProvider   ID, Region, Label         what's streaming on a service
//	traktList | letterboxdList | mdblist      URL
//	anilistList                               URL (an AniList anime list; optionally one named list)
//	stevenlu                                  List: movies (default) or a score-filtered variant
//	plugin:<id>/<key>  Params, MaxItems       a plugin's or Stremio add-on's list (catalog)
//	titles          Keys                      hand-picked rating keys
//
// When ID is 0 the Label is resolved to a TMDB id by name.
type CollectionSource struct {
	Kind      string            `json:"kind"`
	List      string            `json:"list,omitempty"`
	MediaType string            `json:"mediaType,omitempty"`
	ID        int64             `json:"id,omitempty"`
	Label     string            `json:"label,omitempty"`
	Params    map[string]string `json:"params,omitempty"`
	Region    string            `json:"region,omitempty"`
	URL       string            `json:"url,omitempty"`
	Keys      []string          `json:"keys,omitempty"`
	// Role narrows tmdbPerson credits: "cast", "director", "writer",
	// "crew" or "" (every credit). Additive to the contract.
	Role string `json:"role,omitempty"`
	// MaxItems caps how many items a plugin or add-on list source pulls per
	// refresh (1..2000). Additive to the contract.
	MaxItems int `json:"maxItems,omitempty"`
}

// CollectionRule is one condition. Value is typed by Field; Label is display
// text for ids.
type CollectionRule struct {
	Field string          `json:"field"`
	Op    string          `json:"op"`
	Value json.RawMessage `json:"value"`
	Label string          `json:"label,omitempty"`
}

// CollectionRotate shows a rotating subset of the members.
type CollectionRotate struct {
	Size  int    `json:"size"`
	Every string `json:"every"` // "daily" | "weekly"
}

// CollectionArtwork selects the poster Plex shows for the collection.
type CollectionArtwork struct {
	Mode   string `json:"mode"` // auto | collage | backdrop | tmdb | custom
	URL    string `json:"url,omitempty"`
	Accent string `json:"accent,omitempty"` // "#rrggbb"
}

// CollectionTargets says where and for whom the collection is published.
type CollectionTargets struct {
	CollectionsTab bool                `json:"collectionsTab"`
	HomeRow        bool                `json:"homeRow"`
	SectionRow     bool                `json:"sectionRow"`
	Users          UserTargets         `json:"users"`
	Schedule       *CollectionSchedule `json:"schedule,omitempty"`
}

// CollectionSchedule limits when a collection's rows appear. It is the one
// schedule of a collection row: the Collections editor and the Home
// designer both edit it. Days use 0 = Sunday … 6 = Saturday; hours are
// local, FromHour inclusive and ToHour exclusive, wrapping past midnight
// when FromHour > ToHour. Dayparts are DaypartMorning…DaypartLate, Months
// 1 = January … 12, and StartDate/EndDate inclusive YYYY-MM-DD local dates.
type CollectionSchedule struct {
	Days      []int    `json:"days,omitempty"`
	FromHour  *int     `json:"fromHour,omitempty"`
	ToHour    *int     `json:"toHour,omitempty"`
	Devices   []string `json:"devices,omitempty"`
	Dayparts  []string `json:"dayparts,omitempty"`
	Months    []int    `json:"months,omitempty"`
	StartDate string   `json:"startDate,omitempty"`
	EndDate   string   `json:"endDate,omitempty"`
}

// IsZero reports whether the schedule sets nothing (always on).
func (sc *CollectionSchedule) IsZero() bool {
	return sc == nil || len(sc.Days) == 0 && sc.FromHour == nil && sc.ToHour == nil &&
		len(sc.Devices) == 0 && len(sc.Dayparts) == 0 && len(sc.Months) == 0 &&
		sc.StartDate == "" && sc.EndDate == ""
}

// Allows reports whether the schedule lets the row appear at now (local
// time) on a device class. A nil schedule always allows; an unknown device
// ("") passes a device filter; equal From and To hours mean all day.
func (sc *CollectionSchedule) Allows(device string, now time.Time) bool {
	if sc == nil {
		return true
	}
	if len(sc.Months) > 0 && !containsInt(sc.Months, int(now.Month())) {
		return false
	}
	if len(sc.Days) > 0 && !containsInt(sc.Days, int(now.Weekday())) {
		return false
	}
	if len(sc.Dayparts) > 0 {
		dp, ok := DaypartAt(now), false
		for _, d := range sc.Dayparts {
			ok = ok || d == dp
		}
		if !ok {
			return false
		}
	}
	date := now.Format("2006-01-02")
	if sc.StartDate != "" && date < sc.StartDate || sc.EndDate != "" && date > sc.EndDate {
		return false
	}
	if sc.FromHour != nil || sc.ToHour != nil {
		from, to := 0, 24
		if sc.FromHour != nil {
			from = *sc.FromHour
		}
		if sc.ToHour != nil {
			to = *sc.ToHour
		}
		h := now.Hour()
		switch {
		case from == to:
			// All day.
		case from < to:
			if h < from || h >= to {
				return false
			}
		default: // wraps midnight, e.g. 22 → 2
			if h < from && h >= to {
				return false
			}
		}
	}
	if len(sc.Devices) > 0 && device != "" {
		ok := false
		for _, d := range sc.Devices {
			ok = ok || d == device
		}
		if !ok {
			return false
		}
	}
	return true
}

func containsInt(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// UserTargets is either every user ("all" in JSON) or a list of local user
// ids.
type UserTargets struct {
	All bool
	IDs []int64
}

// AllUsers targets everyone.
func AllUsers() UserTargets { return UserTargets{All: true} }

// Includes reports whether a user (nil = anonymous) is targeted.
func (u UserTargets) Includes(user *User) bool {
	if u.All {
		return true
	}
	if user == nil {
		return false
	}
	for _, id := range u.IDs {
		if id == user.ID {
			return true
		}
	}
	return false
}

func (u UserTargets) MarshalJSON() ([]byte, error) {
	if u.All {
		return []byte(`"all"`), nil
	}
	ids := u.IDs
	if ids == nil {
		ids = []int64{}
	}
	return json.Marshal(ids)
}

func (u *UserTargets) UnmarshalJSON(b []byte) error {
	*u = UserTargets{}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		if s == "all" || s == "" {
			u.All = true
			return nil
		}
		return fmt.Errorf(`users: want "all" or a list of user ids, got %q`, s)
	}
	if string(b) == "null" {
		u.All = true
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf(`users: want "all" or a list of user ids`)
	}
	for _, r := range raw {
		var n int64
		if err := json.Unmarshal(r, &n); err == nil {
			u.IDs = append(u.IDs, n)
			continue
		}
		var str string
		if err := json.Unmarshal(r, &str); err == nil {
			if n, err := strconv.ParseInt(str, 10, 64); err == nil {
				u.IDs = append(u.IDs, n)
				continue
			}
		}
		return fmt.Errorf("users: %s is not a user id", string(r))
	}
	if u.IDs == nil {
		u.IDs = []int64{}
	}
	return nil
}

// CollectionTemplate is a curated starting point in the Collection Studio.
type CollectionTemplate struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Category    string         `json:"category"`
	Spec        CollectionSpec `json:"spec"`
}

// CollectionPreview is the result of evaluating an unsaved spec.
type CollectionPreview struct {
	Total    int      `json:"total"`
	Items    []Title  `json:"items"`
	Warnings []string `json:"warnings"`
	TookMs   int64    `json:"tookMs"`
}

// LookupResult is one TMDB entity (person, keyword, company, network,
// collection, watch provider) found by name for the collection builder.
type LookupResult struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Subtitle  string `json:"subtitle,omitempty"`
	ImagePath string `json:"imagePath,omitempty"` // TMDB image path
}
