// Personalized home rows are extra CollectionFolders on this engine.
// Jellyfin Web and jellyfin-androidtv turn each user view into a
// "Recently Added in {name}" / "Latest in {name}" shelf from /Items/Latest,
// in view order, with no client sort. Swiftfin, Findroid, Streamyfin and
// JellyCon open a view with SortBy=SortName; these folders still return rank
// order. Infuse on Jellyfin calls /Items/Latest per view after /UserViews.
// The Emby SDK sample browses /Items?ParentId. Continue Watching and Up Next
// stay on their own routes. See the owner Apps pages for the on/count/kind
// controls. Plex does not use this path.
package mediabrowser

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/frontend/kit/paging"
	"github.com/braedonsaunders/ether/internal/frontend/kit/route"
)

const (
	personalRowFetch    = 50
	personalRowMax      = 12
	personalRowDefault  = 4
	playCreditCap       = 4096
	personalIdentityTTL = 30 * time.Second
)

// personalHomeConfig is the owner control for Jellyfin and Emby home rows.
// Missing keys mean the feature is on, with about four rows of every kind.
type personalHomeConfig struct {
	enabled bool
	count   int
	kinds   map[string]bool
}

func personalHomeFrom(settings frontend.Settings) personalHomeConfig {
	cfg := personalHomeConfig{enabled: true, count: personalRowDefault}
	if settings == nil {
		return cfg
	}
	if value, ok := settings["homeRows"].(bool); ok {
		cfg.enabled = value
	}
	if value, ok := settings["homeRowCount"].(float64); ok && value >= 0 && value <= personalRowMax && value == math.Trunc(value) {
		cfg.count = int(value)
	}
	raw, ok := settings["homeRowKinds"].([]any)
	if !ok {
		return cfg
	}
	cfg.kinds = map[string]bool{}
	for _, item := range raw {
		kind, ok := item.(string)
		if ok && kind != "" {
			cfg.kinds[kind] = true
		}
	}
	return cfg
}

func (i *instance) personalConfig() personalHomeConfig {
	if i.host.Config == nil {
		return personalHomeFrom(nil)
	}
	return personalHomeFrom(i.host.Config())
}

func validatePersonalSettings(s frontend.Settings) error {
	for key, value := range s {
		switch key {
		case "port", "serverName", "discovery", "quickConnect", "publicViewerPicker":
		case "homeRows":
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("invalid %s", key)
			}
		case "homeRowCount":
			n, ok := value.(float64)
			if !ok || n < 0 || n > personalRowMax || n != math.Trunc(n) {
				return fmt.Errorf("invalid %s", key)
			}
		case "homeRowKinds":
			list, ok := value.([]any)
			if !ok {
				return fmt.Errorf("invalid %s", key)
			}
			for _, item := range list {
				kind, ok := item.(string)
				if !ok || kind == "" || len(kind) > 64 {
					return fmt.Errorf("invalid %s", key)
				}
			}
		default:
			return fmt.Errorf("unknown setting %s", key)
		}
	}
	return nil
}

// personalViewNumber is stable for one viewer and one row identity.
func personalViewNumber(viewerID int64, rowID string) int64 {
	sum := sha256.Sum256([]byte(strconv.FormatInt(viewerID, 10) + "\n" + rowID))
	n := int64(binary.BigEndian.Uint64(sum[:8]) & 0x3FFFFFFFFFFFFFFF)
	if n == 0 {
		n = 1
	}
	return n | ids.PersonalViewBit
}

type personalShelf struct {
	number int64
	wireID string
	name   string
	kind   string
	count  int
	row    frontend.Row
	titles []frontend.Title
}

type personalIdentitySlot struct {
	at      time.Time
	gen     int64
	shelves []personalShelf
}

type personalIdentityCache struct {
	mu       sync.Mutex
	items    map[string]personalIdentitySlot
	saved    map[string][]personalShelf
	inflight map[string]bool
}

const personalWarmDevice = "ether-personal-home"

type policyGeneration interface {
	PolicyGeneration(context.Context) (int64, error)
}

func (i *instance) policyGeneration(ctx context.Context) int64 {
	src, ok := i.host.Experience.(policyGeneration)
	if !ok || src == nil {
		return 0
	}
	gen, err := src.PolicyGeneration(ctx)
	if err != nil || gen < 0 {
		return 0
	}
	return gen
}

func personalCacheKey(protocol string, v *frontend.Viewer, dev frontend.Device, cfg personalHomeConfig) string {
	kinds := make([]string, 0, len(cfg.kinds))
	for kind := range cfg.kinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return fmt.Sprintf("%s\n%d\n%s\n%t\n%d\n%s", protocol, v.ID, dev.ID, cfg.enabled, cfg.count, strings.Join(kinds, ","))
}

// personalShelves is the cached identity list. It never builds row items and
// never waits on a model. A request serves the last list for this device, or
// the last list saved for this viewer, and refreshes in the background.
func (i *instance) personalShelves(r *http.Request, v *frontend.Viewer) []personalShelf {
	cfg := i.personalConfig()
	if !cfg.enabled || cfg.count == 0 || v == nil || i.host.Experience == nil {
		return nil
	}
	dev := device(r)
	if dev.ID == "" {
		return nil
	}
	key := personalCacheKey(string(i.protocol), v, dev, cfg)
	gen := i.policyGeneration(r.Context())
	i.personal.mu.Lock()
	slot, ok := i.personal.items[key]
	saved := i.personal.saved[personalSavedKey(string(i.protocol), v.ID, cfg)]
	i.personal.mu.Unlock()
	fresh := ok && time.Since(slot.at) < personalIdentityTTL && slot.gen == gen
	if !fresh {
		i.warmPersonalIdentities(key, gen, v, dev)
	}
	if ok && len(slot.shelves) > 0 {
		return cloneShelves(slot.shelves)
	}
	if len(saved) > 0 {
		return cloneShelves(saved)
	}
	return nil
}

func (i *instance) warmPersonalIdentities(key string, gen int64, v *frontend.Viewer, dev frontend.Device) {
	viewer := *v
	i.personal.mu.Lock()
	if i.personal.inflight == nil {
		i.personal.inflight = map[string]bool{}
	}
	if i.personal.inflight[key] {
		i.personal.mu.Unlock()
		return
	}
	i.personal.inflight[key] = true
	i.personal.mu.Unlock()
	go func() {
		defer func() {
			i.personal.mu.Lock()
			delete(i.personal.inflight, key)
			i.personal.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(i.life, 15*time.Second)
		defer cancel()
		loaded := i.loadPersonalShelves(ctx, &viewer, dev)
		if loaded == nil {
			return
		}
		identities := make([]personalShelf, len(loaded))
		for n, shelf := range loaded {
			shelf.titles = nil
			shelf.row.Entries = nil
			shelf.row.Items = nil
			shelf.row.Handle = ""
			identities[n] = shelf
		}
		if len(identities) == 0 {
			return
		}
		i.storePersonalIdentities(key, gen, &viewer, identities)
	}()
}

func cloneShelves(in []personalShelf) []personalShelf {
	out := make([]personalShelf, len(in))
	copy(out, in)
	return out
}

func personalSavedKey(protocol string, viewerID int64, cfg personalHomeConfig) string {
	return fmt.Sprintf("%s\n%d\n%s", protocol, viewerID, personalConfigKey(cfg))
}

func personalConfigKey(cfg personalHomeConfig) string {
	kinds := make([]string, 0, len(cfg.kinds))
	for kind := range cfg.kinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return fmt.Sprintf("%t\n%d\n%s", cfg.enabled, cfg.count, strings.Join(kinds, ","))
}

type storedPersonalShelf struct {
	Number int64  `json:"number"`
	WireID string `json:"wireID"`
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Count  int    `json:"count"`
}

func encodeShelves(in []personalShelf) ([]byte, error) {
	out := make([]storedPersonalShelf, len(in))
	for i, shelf := range in {
		out[i] = storedPersonalShelf{Number: shelf.number, WireID: shelf.wireID, Name: shelf.name, Kind: shelf.kind, Count: shelf.count}
	}
	return json.Marshal(out)
}

func decodeShelves(body []byte) ([]personalShelf, error) {
	var stored []storedPersonalShelf
	if err := json.Unmarshal(body, &stored); err != nil {
		return nil, err
	}
	out := make([]personalShelf, 0, len(stored))
	for _, shelf := range stored {
		if shelf.WireID == "" || shelf.Name == "" {
			continue
		}
		out = append(out, personalShelf{number: shelf.Number, wireID: shelf.WireID, name: shelf.Name, kind: shelf.Kind, count: shelf.Count})
	}
	return out, nil
}

func (i *instance) storePersonalIdentities(key string, gen int64, v *frontend.Viewer, identities []personalShelf) {
	cfg := i.personalConfig()
	i.personal.mu.Lock()
	if i.personal.items == nil {
		i.personal.items = map[string]personalIdentitySlot{}
	}
	if i.personal.saved == nil {
		i.personal.saved = map[string][]personalShelf{}
	}
	i.personal.items[key] = personalIdentitySlot{at: time.Now(), gen: gen, shelves: identities}
	savedKey := personalSavedKey(string(i.protocol), v.ID, cfg)
	i.personal.saved[savedKey] = cloneShelves(identities)
	prefix := fmt.Sprintf("%s\n%d\n", i.protocol, v.ID)
	for existing := range i.personal.items {
		if existing != key && strings.HasPrefix(existing, prefix) {
			delete(i.personal.items, existing)
		}
	}
	i.personal.mu.Unlock()
	if i.host.PersonalHomes == nil {
		return
	}
	body, err := encodeShelves(identities)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(i.life, 2*time.Second)
	defer cancel()
	if err := i.host.PersonalHomes.SavePersonalHome(ctx, string(i.protocol), v.ID, personalConfigKey(cfg), body); err != nil && i.host.Log != nil {
		i.host.Log.Warn("personal home save", "err", err)
	}
}

// PreparePersonalHomes loads the last saved library list before the listener
// accepts clients, then refreshes each viewer without waiting for a model.
func (i *instance) PreparePersonalHomes(ctx context.Context) {
	if i.host.PersonalHomes == nil {
		return
	}
	cfg := i.personalConfig()
	if !cfg.enabled || cfg.count == 0 {
		return
	}
	users, err := i.host.PersonalHomes.PersonalHomeViewers(ctx)
	if err != nil {
		return
	}
	for _, user := range users {
		body, err := i.host.PersonalHomes.LoadPersonalHome(ctx, string(i.protocol), user.ID, personalConfigKey(cfg))
		if err != nil || len(body) == 0 {
			continue
		}
		shelves, err := decodeShelves(body)
		if err != nil || len(shelves) == 0 {
			continue
		}
		i.personal.mu.Lock()
		if i.personal.saved == nil {
			i.personal.saved = map[string][]personalShelf{}
		}
		i.personal.saved[personalSavedKey(string(i.protocol), user.ID, cfg)] = shelves
		i.personal.mu.Unlock()
	}
	dev := frontend.Device{ID: personalWarmDevice, Client: "Ether"}
	gen := i.policyGeneration(ctx)
	for _, user := range users {
		viewer := user
		key := personalCacheKey(string(i.protocol), &viewer, dev, cfg)
		i.warmPersonalIdentities(key, gen, &viewer, dev)
	}
}

// RefreshPersonalHome rebuilds one viewer's libraries after a model lands.
// It does not wait for the build that just finished.
func (i *instance) RefreshPersonalHome(userID int64) {
	if i.host.PersonalHomes == nil || userID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(i.life, 2*time.Second)
	defer cancel()
	users, err := i.host.PersonalHomes.PersonalHomeViewers(ctx)
	if err != nil {
		return
	}
	cfg := i.personalConfig()
	dev := frontend.Device{ID: personalWarmDevice, Client: "Ether"}
	for _, user := range users {
		if user.ID != userID {
			continue
		}
		viewer := user
		i.warmPersonalIdentities(personalCacheKey(string(i.protocol), &viewer, dev, cfg), i.policyGeneration(ctx), &viewer, dev)
		return
	}
}

func (i *instance) loadPersonalShelves(ctx context.Context, v *frontend.Viewer, dev frontend.Device) []personalShelf {
	cfg := i.personalConfig()
	if !cfg.enabled || cfg.count == 0 || v == nil || i.host.Experience == nil || dev.ID == "" {
		return nil
	}
	rows, err := i.host.Experience.Rows(ctx, frontend.RowRequest{Viewer: v, Device: dev, Placement: frontend.PlacementHome, Limit: personalRowFetch})
	if err != nil {
		return nil
	}
	if len(rows) == 0 {
		return []personalShelf{}
	}
	out := make([]personalShelf, 0, cfg.count)
	seen := map[string]bool{}
	var pinned *personalShelf
	for _, row := range rows {
		if row.ID == "" || seen[row.ID] {
			continue
		}
		available := nowAvailableExperienceRow(row)
		if !available && (!personalRowAllowed(cfg, row) || len(out) >= cfg.count) {
			continue
		}
		titles := personalTitles(row)
		if len(titles) == 0 {
			continue
		}
		seen[row.ID] = true
		number := personalViewNumber(v.ID, row.ID)
		wire, err := ids.GUID(ids.View, number)
		if err != nil {
			continue
		}
		shelf := personalShelf{number: number, wireID: wire, name: personalLibraryName(row), kind: personalCollectionType(titles), count: len(titles), row: row, titles: titles}
		if available {
			copy := shelf
			pinned = &copy
			continue
		}
		out = append(out, shelf)
	}
	if pinned != nil {
		out = append(out, *pinned)
	}
	return out
}

// nowAvailableExperienceRow is the watchlist hub the library-features work
// fills. Its id stays watchlist.available so Jellyfin, Emby and Silo publish
// the same shelf. It does not consume the owner's personalized-row count.
func nowAvailableExperienceRow(row frontend.Row) bool {
	return row.ID == "watchlist.available" || row.Title == "Now available"
}

func personalRowAllowed(cfg personalHomeConfig, row frontend.Row) bool {
	switch row.Purpose {
	case frontend.RowContinue, frontend.RowNextUp, frontend.RowLive:
		return false
	}
	switch row.Kind {
	case "continue-watching", "up-next":
		return false
	}
	if cfg.kinds == nil {
		return true
	}
	if cfg.kinds[row.Kind] {
		return true
	}
	return strings.HasPrefix(row.Kind, "collection:") && cfg.kinds["collection"]
}

func personalTitles(row frontend.Row) []frontend.Title {
	seen := map[frontend.RatingKey]bool{}
	var out []frontend.Title
	add := func(title frontend.Title) {
		if title.Key == 0 || seen[title.Key] {
			return
		}
		seen[title.Key] = true
		out = append(out, title)
	}
	if len(row.Entries) > 0 {
		for _, entry := range row.Entries {
			if entry.Kind == frontend.RowEntryTitle && entry.Title != nil {
				add(*entry.Title)
			}
		}
		return out
	}
	for _, title := range row.Items {
		add(title)
	}
	return out
}

func personalLibraryName(row frontend.Row) string {
	title := strings.TrimSpace(row.Title)
	if seed, ok := personalSeed(title, "because you watched ", "because you've watched "); ok {
		return seed + " and Similar"
	}
	if seed, ok := personalSeed(title, "because you liked ", "because you like "); ok {
		return seed + " and Similar"
	}
	if title != "" {
		return title
	}
	switch row.Kind {
	case "top-10", "top-10-today":
		return "For You"
	case "trending", "trending-in-your-taste", "trending-with-fans":
		return "Trending"
	case "because-you-watched":
		return "Similar Picks"
	case "fans-also-loved":
		return "Fans Also Loved"
	case "new-releases", "new-this-week":
		return "New Releases"
	default:
		return "For You"
	}
}

func personalSeed(title string, prefixes ...string) (string, bool) {
	lower := strings.ToLower(title)
	for _, prefix := range prefixes {
		if strings.HasPrefix(lower, prefix) {
			seed := strings.Trim(strings.TrimSpace(title[len(prefix):]), " .…")
			if seed != "" {
				return seed, true
			}
		}
	}
	return "", false
}

func personalChildCount(shelf personalShelf) int {
	if len(shelf.titles) > 0 {
		return len(shelf.titles)
	}
	return shelf.count
}

func personalCollectionType(titles []frontend.Title) string {
	shows := 0
	for _, title := range titles {
		if title.Type == "show" || title.Type == "season" || title.Type == "episode" {
			shows++
		}
	}
	if len(titles) > 0 && shows == len(titles) {
		return "tvshows"
	}
	return "movies"
}

func (i *instance) personalByNumber(r *http.Request, v *frontend.Viewer, number int64) (personalShelf, bool) {
	for _, shelf := range i.loadPersonalShelves(r.Context(), v, device(r)) {
		if shelf.number == number {
			return shelf, true
		}
	}
	return personalShelf{}, false
}

func (i *instance) personalFolder(r *http.Request, v *frontend.Viewer, shelf personalShelf) map[string]any {
	item := map[string]any{
		"Id": shelf.wireID, "Name": shelf.name, "Type": "CollectionFolder", "CollectionType": shelf.kind,
		"IsFolder": true, "ServerId": i.info.ID, "SortName": shelf.name, "DisplayPreferencesId": shelf.wireID,
		"CanDelete": false, "CanDownload": false, "ProviderIds": map[string]string{}, "ChildCount": personalChildCount(shelf),
	}
	if i.protocol == "emby" {
		i.embyViewDTO(item)
	}
	if i.protocol == "jellyfin" {
		i.jellyfinCollectionFolder(item, shelf.wireID)
		item["ParentId"] = jellyfinRootID()
		item["ChildCount"] = personalChildCount(shelf)
	}
	return item
}

func (i *instance) prependPersonalViews(r *http.Request, v *frontend.Viewer, items []any) []any {
	shelves := i.personalShelves(r, v)
	if len(shelves) == 0 {
		return items
	}
	head := make([]any, 0, len(shelves)+len(items))
	for _, shelf := range shelves {
		head = append(head, i.personalFolder(r, v, shelf))
	}
	return append(head, items...)
}

func (i *instance) writePersonalFolder(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, number int64) {
	for _, shelf := range i.personalShelves(r, v) {
		if shelf.number == number {
			i.replyItems(w, r, i.personalFolder(r, v, shelf))
			return
		}
	}
	http.NotFound(w, r)
}

func (i *instance) personalTitlePage(r *http.Request, v *frontend.Viewer, shelf personalShelf, q frontend.ItemQuery, latest bool) ([]frontend.Title, int, error) {
	titles := append([]frontend.Title(nil), shelf.titles...)
	if len(q.Types) > 0 || q.Watched != nil || q.Favorite != nil || latest {
		keys := make([]frontend.RatingKey, 0, len(titles))
		for _, title := range titles {
			keys = append(keys, title.Key)
		}
		states, err := i.host.Experience.UserData(r.Context(), v, keys)
		if err != nil {
			return nil, 0, err
		}
		played, filterPlayed := frontend.LatestPlayedFilter(r.Context())
		kept := titles[:0]
		for _, title := range titles {
			state := states[title.Key]
			watched := state.ViewCount > 0
			if latest && filterPlayed && watched != played {
				continue
			}
			if q.Watched != nil && watched != *q.Watched {
				continue
			}
			if q.Favorite != nil && state.IsFavorite != *q.Favorite {
				continue
			}
			if len(q.Types) > 0 && !titleTypeMatches(title.Type, q.Types) {
				continue
			}
			kept = append(kept, title)
		}
		titles = kept
	}
	total := len(titles)
	offset, limit := q.Offset, q.Limit
	if limit <= 0 {
		limit = total
	}
	return paging.Slice(titles, offset, limit), total, nil
}

func titleTypeMatches(typ frontend.MediaType, types []frontend.MediaType) bool {
	for _, want := range types {
		if typ == want {
			return true
		}
	}
	return false
}

func (i *instance) notePersonalDelivery(r *http.Request, v *frontend.Viewer, shelf personalShelf, titles []frontend.Title) {
	if len(titles) == 0 {
		return
	}
	if reporter, ok := i.host.Experience.(frontend.RowInteractionReporter); ok && shelf.row.Handle != "" {
		err := reporter.ReportRowInteraction(r.Context(), frontend.RowInteraction{Viewer: v, Device: device(r), Protocol: i.protocol, Handle: shelf.row.Handle, Kind: frontend.RowImpression})
		if err != nil && i.host.Log != nil {
			i.host.Log.Warn("row impression failed", "error", err)
		}
	}
	if shelf.row.Handle == "" {
		return
	}
	dev := device(r).ID
	for _, title := range titles {
		i.credits.remember(v.ID, dev, ids.RatingKeyGUID(title.Key), shelf.row.Handle)
	}
}

func (i *instance) writePersonalItems(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, number int64) {
	shelf, ok := i.personalByNumber(r, v, number)
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := itemQuery(r)
	if err := navigationQuery(r, &q, false); err != nil {
		http.Error(w, "Invalid navigation anchor", http.StatusBadRequest)
		return
	}
	page, total, err := i.personalTitlePage(r, v, shelf, q, false)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	i.notePersonalDelivery(r, v, shelf, page)
	i.page(w, r, v, frontend.Page{Items: page, Total: total}, q.Offset)
}

func (i *instance) writePersonalActivity(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, number int64, latest bool, offset, limit int) {
	shelf, ok := i.personalByNumber(r, v, number)
	if !ok {
		http.NotFound(w, r)
		return
	}
	q := frontend.ItemQuery{Offset: offset, Limit: limit}
	if !latest {
		for _, value := range route.Query(r.URL.Query(), "MediaTypes") {
			for _, typ := range strings.Split(value, ",") {
				if !strings.EqualFold(strings.TrimSpace(typ), "Video") {
					http.Error(w, "Unsupported media type", http.StatusBadRequest)
					return
				}
				q.Types = []frontend.MediaType{"movie", "episode"}
			}
		}
	}
	page, total, err := i.personalTitlePage(r, v, shelf, q, latest)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	i.notePersonalDelivery(r, v, shelf, page)
	i.writeListedTitles(w, r, v, page, nil, total, true, offset, latest)
}

func (i *instance) personalViewOrder(r *http.Request, v *frontend.Viewer) []string {
	shelves := i.personalShelves(r, v)
	if len(shelves) == 0 || i.host.Experience == nil {
		return nil
	}
	order := make([]string, 0, len(shelves))
	for _, shelf := range shelves {
		order = append(order, shelf.wireID)
	}
	views, err := i.host.Experience.Views(r.Context(), v)
	if err != nil {
		return order
	}
	for _, view := range views {
		n, err := ids.ParseDecimal(view.ID)
		if err != nil || ids.PersonalView(n) {
			continue
		}
		wire, err := ids.GUID(ids.View, n)
		if err == nil {
			order = append(order, wire)
		}
	}
	return order
}

const (
	userConfigClient = "mediabrowser"
	userConfigKey    = "user-configuration"
)

func (i *instance) storedUserConfig(r *http.Request, v *frontend.Viewer) (map[string]any, bool) {
	if i.host.Preferences == nil || v == nil {
		return nil, false
	}
	body, err := i.host.Preferences.Get(r.Context(), v, userConfigClient, userConfigKey)
	if err != nil || len(body) == 0 {
		return nil, false
	}
	var object map[string]any
	if json.Unmarshal(body, &object) != nil || object == nil {
		return nil, false
	}
	return object, true
}

func (i *instance) applyOrderedViews(r *http.Request, v *frontend.Viewer, configuration map[string]any) {
	if stored, ok := i.storedUserConfig(r, v); ok {
		for _, key := range []string{"OrderedViews", "LatestItemsExcludes", "MyMediaExcludes", "HidePlayedInLatest"} {
			if value, exists := stored[key]; exists {
				configuration[key] = value
			}
		}
		return
	}
	if order := i.personalViewOrder(r, v); len(order) > 0 {
		configuration["OrderedViews"] = order
	}
}

func (i *instance) saveUserConfiguration(w http.ResponseWriter, r *http.Request) {
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		if errors.Is(err, errViewerScope) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var object map[string]any
	if decode(r, &object) != nil || object == nil {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	_, hasMode := object["SubtitleMode"]
	_, hasLanguage := object["SubtitleLanguagePreference"]
	home := false
	for _, key := range []string{"OrderedViews", "LatestItemsExcludes", "MyMediaExcludes", "HidePlayedInLatest"} {
		if _, ok := object[key]; ok {
			home = true
			break
		}
	}
	if home && i.host.Preferences == nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	if hasMode || hasLanguage {
		if err = i.saveSubtitleConfiguration(r, v, object); err != nil {
			if errors.Is(err, errBadSubtitle) {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	kept := map[string]any{}
	if stored, ok := i.storedUserConfig(r, v); ok {
		for key, value := range stored {
			kept[key] = value
		}
	}
	for _, key := range []string{"OrderedViews", "LatestItemsExcludes", "MyMediaExcludes"} {
		value, exists := object[key]
		if !exists {
			continue
		}
		if !stringList(value) {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		kept[key] = value
	}
	if value, exists := object["HidePlayedInLatest"]; exists {
		if _, ok := value.(bool); !ok {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		kept["HidePlayedInLatest"] = value
	}
	if !home {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	body, err := json.Marshal(kept)
	if err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	if err = i.host.Preferences.Put(r.Context(), v, userConfigClient, userConfigKey, body); err != nil {
		http.Error(w, "Unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func stringList(value any) bool {
	list, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range list {
		text, ok := item.(string)
		if !ok || len(text) > 128 {
			return false
		}
	}
	return true
}

func (i *instance) applyHomeSectionDefaults(r *http.Request, v *frontend.Viewer, id string, object map[string]any) {
	if !strings.EqualFold(id, "usersettings") || v == nil {
		return
	}
	prefs, _ := object["CustomPrefs"].(map[string]any)
	if prefs == nil {
		prefs = map[string]any{}
		object["CustomPrefs"] = prefs
	}
	for key := range prefs {
		if strings.HasPrefix(strings.ToLower(key), "homesection") {
			return
		}
	}
	if len(i.personalShelves(r, v)) == 0 {
		return
	}
	defaults := []string{"latestmedia", "resume", "nextup", "smalllibrarytiles", "none", "none", "none", "none", "none", "none"}
	for index, value := range defaults {
		prefs["homesection"+strconv.Itoa(index)] = value
	}
}

func (i *instance) recommendations(w http.ResponseWriter, r *http.Request, want frontend.MediaType) {
	r = withJellyfinListFiles(r)
	v, _, err := i.viewer(r)
	if err != nil || v == nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	shelves := i.loadPersonalShelves(r.Context(), v, device(r))
	categories := make([]any, 0, len(shelves))
	for _, shelf := range shelves {
		titles := make([]frontend.Title, 0, len(shelf.titles))
		for _, title := range shelf.titles {
			if recommendationTypeMatches(want, title.Type) {
				titles = append(titles, title)
			}
		}
		if len(titles) == 0 {
			continue
		}
		limit := queryInt(r, "Limit", len(titles))
		if limit > 0 && len(titles) > limit {
			titles = titles[:limit]
		}
		i.notePersonalDelivery(r, v, shelf, titles)
		keys := make([]frontend.RatingKey, len(titles))
		for n, title := range titles {
			keys[n] = title.Key
		}
		states, err := i.host.Experience.UserData(r.Context(), v, keys)
		if err != nil {
			http.Error(w, "Unavailable", http.StatusServiceUnavailable)
			return
		}
		items := make([]any, 0, len(titles))
		images := i.gridImagesFor(r, titles)
		ratingFacts := i.pageRatingFacts(r, v, titles)
		for _, title := range titles {
			item := i.itemDTO(title, states[title.Key])
			for key, value := range images[title.Key] {
				item[key] = value
			}
			i.shapeJellyfinItem(r, v, item, title)
			i.applyListedRatings(item, ratingFacts, title.Key)
			items = append(items, item)
		}
		kind, baseline := recommendationWire(shelf.row)
		categories = append(categories, map[string]any{
			"BaselineItemName": baseline, "CategoryId": shelf.wireID, "Items": items, "RecommendationType": kind,
		})
	}
	jsonReply(w, categories)
}

func recommendationTypeMatches(want, got frontend.MediaType) bool {
	if want == "movie" {
		return got == "movie"
	}
	return got == "show" || got == "season" || got == "episode"
}

func recommendationWire(row frontend.Row) (string, string) {
	title := strings.TrimSpace(row.Title)
	switch row.Kind {
	case "because-you-watched":
		return "SimilarToRecentlyPlayed", recommendationBaseline(title, "because you watched ", "because you've watched ")
	case "favorites":
		return "SimilarToLikedItem", recommendationBaseline(title, "because you liked ", "because you like ")
	case "more-from-person":
		if strings.Contains(strings.ToLower(title), "director") {
			return "HasDirectorFromRecentlyPlayed", recommendationBaseline(title, "more from ")
		}
		return "HasActorFromRecentlyPlayed", recommendationBaseline(title, "more from ")
	default:
		if strings.Contains(strings.ToLower(title), "director") {
			return "HasDirectorFromRecentlyPlayed", title
		}
		if title == "" {
			title = personalLibraryName(row)
		}
		return "SimilarToLikedItem", title
	}
}

func recommendationBaseline(title string, prefixes ...string) string {
	if seed, ok := personalSeed(title, prefixes...); ok {
		seed = strings.TrimSuffix(seed, " and Similar")
		if seed != "" {
			return seed
		}
	}
	if title == "" {
		return "For You"
	}
	return title
}

type playCredits struct {
	mu    sync.Mutex
	items map[string]string
}

func playCreditKey(viewer int64, device, item string) string {
	return strconv.FormatInt(viewer, 10) + "\n" + device + "\n" + item
}

func (c *playCredits) remember(viewer int64, device, item, handle string) {
	if handle == "" || item == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = map[string]string{}
	}
	if len(c.items) >= playCreditCap {
		c.items = map[string]string{}
	}
	c.items[playCreditKey(viewer, device, item)] = handle
}

func (c *playCredits) take(viewer int64, device, item string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	handle, ok := c.items[playCreditKey(viewer, device, item)]
	if ok {
		delete(c.items, playCreditKey(viewer, device, item))
	}
	return handle, ok
}

func (i *instance) creditPlay(r *http.Request, v *frontend.Viewer, itemID string) {
	if v == nil || itemID == "" {
		return
	}
	handle, ok := i.credits.take(v.ID, device(r).ID, itemID)
	if !ok {
		return
	}
	reporter, ok := i.host.Experience.(frontend.RowInteractionReporter)
	if !ok {
		return
	}
	err := reporter.ReportRowInteraction(r.Context(), frontend.RowInteraction{Viewer: v, Device: device(r), Protocol: i.protocol, Handle: handle, Kind: frontend.RowPlay})
	if err != nil && i.host.Log != nil {
		i.host.Log.Warn("row play failed", "error", err)
	}
}
