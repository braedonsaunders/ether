package livetv

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/contentrating"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("livetv", 1, `CREATE TABLE livetv_channels(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE livetv_sources(id TEXT PRIMARY KEY, data TEXT NOT NULL);
CREATE TABLE livetv_programmes(source_id TEXT NOT NULL, channel_id TEXT NOT NULL, start_ms INTEGER NOT NULL, end_ms INTEGER NOT NULL, data TEXT NOT NULL, PRIMARY KEY(source_id,channel_id,start_ms));
CREATE INDEX livetv_programmes_window ON livetv_programmes(channel_id,start_ms,end_ms);
CREATE TABLE livetv_policies(user_id INTEGER PRIMARY KEY, data TEXT NOT NULL);`)
}

type Service struct {
	authCache        authCache  // provider credentials for the stream path (auth.go)
	inputRelay       inputRelay // loopback ffmpeg input relay (input_relay.go)
	probeMu          sync.Mutex
	probeCache       map[string]liveProbeEntry
	probeWait        map[string]*liveProbeFlight
	sourceMu         sync.Mutex
	sourceHealth     map[string]streamSourceHealth
	transMu          sync.RWMutex
	transcodes       map[string]core.HLSSession
	o                Options
	policyReads      atomic.Uint64
	mu               sync.Mutex
	lineupUnix       int64
	onChange         func()
	sessions         map[string]*session
	nextSubscription int64 // guarded by mu
	ctx              context.Context
	cancel           context.CancelFunc
	wg               sync.WaitGroup
}

func New(o Options) (*Service, error) {
	if o.DB == nil {
		return nil, fmt.Errorf("livetv requires a database")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTPClient == nil {
		o.HTTPClient = egress.Client(egress.CapDirect, 20*time.Second)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Service{o: o, sessions: map[string]*session{}, transcodes: map[string]core.HLSSession{}, ctx: ctx, cancel: cancel, lineupUnix: o.Now().Unix()}
	s.wg.Add(2)
	go s.maintain()
	go s.refreshLoop()
	return s, nil
}
func (s *Service) Close() error {
	s.cancel()
	s.wg.Wait()
	s.mu.Lock()
	all := s.sessions
	s.sessions = map[string]*session{}
	s.mu.Unlock()
	for _, ss := range all {
		ss.close(s)
	}
	return nil
}
func (s *Service) stopTrack(t *track) {
	if t.cancel != nil {
		t.cancel()
	}
	s.transMu.Lock()
	delete(s.transcodes, t.playbackID)
	s.transMu.Unlock()
	if t.hls != nil {
		t.hls.Close()
	}
	if s.o.Playback != nil {
		s.o.Playback.Stop(t.playbackID)
	}
}
func (s *Service) invalidate(channelID, sourceID string) {
	s.mu.Lock()
	var stale []*session
	for id, ss := range s.sessions {
		if ss.info.Channel.ID == channelID || sourceID != "" && ss.info.Channel.SourceID == sourceID {
			stale = append(stale, ss)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, ss := range stale {
		ss.close(s)
	}
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Native clients parse Media.uuid as a canonical UUID.
func liveSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func (s *Service) maintain() {
	defer s.wg.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	warmed := map[core.RatingKey]time.Time{}
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
			s.expireSessions()
			if s.o.Warm != nil {
				cs, err := s.Channels(s.ctx)
				if err != nil {
					continue
				}
				for _, c := range cs {
					if !c.Enabled || c.Kind != "virtual" {
						continue
					}
					ps := Schedule(c, s.o.Now(), s.o.Now().Add(3*time.Minute))
					for _, p := range ps {
						if p.Item.Key == 0 || s.o.Now().Sub(warmed[p.Item.Key]) < 10*time.Minute {
							continue
						}
						ctx, cancel := context.WithTimeout(s.ctx, 15*time.Second)
						err := s.o.Warm(ctx, p.Item.Key)
						cancel()
						if err == nil {
							warmed[p.Item.Key] = s.o.Now()
						}
					}
				}
				for k, t := range warmed {
					if s.o.Now().Sub(t) > time.Hour {
						delete(warmed, k)
					}
				}
			}
		}
	}
}

func (s *Service) expireSessions() {
	s.mu.Lock()
	var stale []*session
	for id, ss := range s.sessions {
		// A bounded startup or programme preparation may hold the session
		// lock across I/O. Never let it block monitoring or other viewers.
		if !ss.mu.TryLock() {
			continue
		}
		ttl := 2 * time.Minute
		if !ss.mediaStarted {
			ttl = 30 * time.Second
		}
		expired := s.o.Now().Sub(ss.last) > ttl
		ss.mu.Unlock()
		if expired {
			stale = append(stale, ss)
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
	for _, ss := range stale {
		ss.close(s)
	}
}
func (s *Service) Channels(ctx context.Context) ([]Channel, error) {
	return channels(ctx, s.o.DB)
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func channels(ctx context.Context, db queryer) ([]Channel, error) {
	rows, err := db.QueryContext(ctx, `SELECT data FROM livetv_channels ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Channel{}
	for rows.Next() {
		var b []byte
		var c Channel
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sort.SliceStable(out, func(i, j int) bool { return naturalNumber(out[i].Number) < naturalNumber(out[j].Number) })
	return out, rows.Err()
}
func naturalNumber(n string) string {
	var a, b int
	fmt.Sscanf(n, "%d.%d", &a, &b)
	return fmt.Sprintf("%09d.%09d/%s", a, b, n)
}
func (s *Service) Channel(ctx context.Context, id string) (Channel, error) {
	var b []byte
	err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_channels WHERE id=?`, id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return Channel{}, core.ErrNotFound
	}
	var c Channel
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	return c, err
}
func itemFromTitle(t core.Title) Item {
	var released int64
	if !t.Released.IsZero() {
		released = t.Released.UnixMilli()
	}
	return Item{Key: t.Key, Title: t.Title, DurationMS: t.Runtime.Milliseconds(), Summary: t.Summary, Rating: t.ContentRating, Thumb: t.PosterPath, ShowKey: t.ShowKey, Season: t.Season, Episode: t.Episode, ReleasedMS: released}
}
func (s *Service) expand(ctx context.Context, c *Channel) error {
	items, err := s.resolveProgramming(ctx, c.Items, c.Collection, c.Filter, c.Sources, c.Watched)
	if err != nil {
		return err
	}
	if c.Collection != 0 || c.Filter != nil || len(c.Sources) > 0 || c.Watched != "" {
		c.Items = items
	}
	for i := range c.Blocks {
		b := &c.Blocks[i]
		if b.Collection == 0 && b.Filter == nil && len(b.Sources) == 0 && b.Watched == "" {
			continue
		}
		blockItems, err := s.resolveProgramming(ctx, b.Items, b.Collection, b.Filter, b.Sources, b.Watched)
		if err != nil {
			return err
		}
		b.Items = blockItems
	}
	return nil
}

// resolveProgramming turns a collection, smart filter or weighted mix into
// a stable item list. With none of those set, items pass through (watched
// still applies when asked). Shows become their episodes, in catalogue order.
func (s *Service) resolveProgramming(ctx context.Context, existing []Item, collection core.RatingKey, filter *core.Filter, sources []ProgramSource, watched string) ([]Item, error) {
	var items []Item
	var err error
	switch {
	case len(sources) > 0:
		parts := make([][]Item, len(sources))
		weights := make([]int, len(sources))
		for i, src := range sources {
			parts[i], err = s.resolveProgramming(ctx, src.Items, src.Collection, src.Filter, nil, src.Watched)
			if err != nil {
				return nil, err
			}
			weights[i] = src.Weight
			if weights[i] <= 0 {
				weights[i] = 1
			}
			if weights[i] > 20 {
				weights[i] = 20
			}
		}
		items = interleave(parts, weights)
	case collection != 0 || filter != nil:
		items, err = s.expandQuery(ctx, collection, filter)
		if err != nil {
			return nil, err
		}
	default:
		items = append([]Item(nil), existing...)
	}
	if watched == "" {
		return items, nil
	}
	return s.filterWatched(ctx, items, watched)
}

func interleave(parts [][]Item, weights []int) []Item {
	idx := make([]int, len(parts))
	var out []Item
	for len(out) < 10000 {
		progressed := false
		for i, p := range parts {
			for n := 0; n < weights[i] && idx[i] < len(p); n++ {
				out = append(out, p[idx[i]])
				idx[i]++
				progressed = true
				if len(out) >= 10000 {
					return out
				}
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

func (s *Service) expandQuery(ctx context.Context, collection core.RatingKey, filter *core.Filter) ([]Item, error) {
	var page core.Page
	var err error
	if collection != 0 {
		if s.o.Collections == nil {
			return nil, fmt.Errorf("collections unavailable")
		}
		page, err = s.o.Collections.Items(ctx, &core.User{IsOwner: true}, collection, 0, 2000)
	} else {
		if s.o.Catalog == nil || filter == nil {
			return nil, fmt.Errorf("catalogue unavailable")
		}
		for len(page.Items) < 2000 {
			var p core.Page
			if p, err = s.o.Catalog.Browse(ctx, core.BrowseQuery{Filter: *filter, Sort: core.SortTitle, Offset: len(page.Items), Limit: 2000 - len(page.Items)}); err != nil {
				break
			}
			page.Total = p.Total
			page.Items = append(page.Items, p.Items...)
			if len(p.Items) == 0 || len(page.Items) >= p.Total {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	var items []Item
	for _, t := range page.Items {
		ts := []core.Title{t}
		if t.Type == core.TypeShow {
			if s.o.Catalog == nil {
				return nil, fmt.Errorf("catalogue unavailable")
			}
			ts, err = s.o.Catalog.Grandchildren(ctx, t.Key)
			if err != nil {
				return nil, err
			}
		}
		for _, t := range ts {
			if !t.Adult && t.Runtime > 0 && (t.Type == core.TypeMovie || t.Type == core.TypeEpisode) {
				items = append(items, itemFromTitle(t))
			}
			if len(items) >= 10000 {
				return nil, fmt.Errorf("channel exceeds 10000 programmes")
			}
		}
	}
	return items, nil
}

func (s *Service) filterWatched(ctx context.Context, items []Item, watched string) ([]Item, error) {
	if watched != "watched" && watched != "unwatched" {
		return nil, fmt.Errorf("watched must be watched or unwatched")
	}
	if s.o.DB == nil {
		return nil, fmt.Errorf("watch history unavailable")
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT DISTINCT rating_key FROM state_history WHERE completed=1`)
	if err != nil {
		return nil, fmt.Errorf("watch history unavailable")
	}
	defer rows.Close()
	seen := map[core.RatingKey]struct{}{}
	for rows.Next() {
		var key int64
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		seen[core.RatingKey(key)] = struct{}{}
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var out []Item
	for _, it := range items {
		_, hit := seen[it.Key]
		if watched == "watched" && hit || watched == "unwatched" && !hit {
			out = append(out, it)
		}
	}
	return out, nil
}
func checkPlay(mode, align, fit string, repeat int, watched string) error {
	switch mode {
	case "", "order", "shuffle", "random", "marathon", "newest":
	default:
		return fmt.Errorf("unknown playout")
	}
	switch align {
	case "", "hour", "half":
	default:
		return fmt.Errorf("alignment must be hour or half hour")
	}
	switch fit {
	case "", "cut", "pad":
	default:
		return fmt.Errorf("fit must be cut or pad")
	}
	if repeat < 0 || repeat > 30 {
		return fmt.Errorf("repeat window must be 0–30 days")
	}
	switch watched {
	case "", "watched", "unwatched":
	default:
		return fmt.Errorf("watched must be watched or unwatched")
	}
	return nil
}

func blocksOverlap(a, b Block) bool {
	if a.End <= b.Start || b.End <= a.Start {
		return false
	}
	if len(a.Days) == 0 || len(b.Days) == 0 {
		return true
	}
	for _, d := range a.Days {
		for _, e := range b.Days {
			if d == e {
				return true
			}
		}
	}
	return false
}

func validateChannel(c *Channel) error {
	c.Name = strings.TrimSpace(c.Name)
	c.Number = strings.TrimSpace(c.Number)
	if c.Name == "" || c.Number == "" || len(c.Name) > 200 || len(c.Number) > 32 {
		return fmt.Errorf("channel needs a name and number")
	}
	if c.Kind != "virtual" && c.Kind != "iptv" {
		return fmt.Errorf("kind must be virtual or iptv")
	}
	if c.Kind == "iptv" {
		if err := normalizeStreamSources(c); err != nil {
			return err
		}
	}
	if c.Kind == "virtual" && len(c.Items) == 0 && !c.OffAir && len(c.Fallback) == 0 {
		return fmt.Errorf("virtual channel needs base programming")
	}
	if err := checkPlay(c.Mode, c.Align, c.Fit, c.RepeatDays, c.Watched); err != nil {
		return err
	}
	if c.Timezone != "" {
		if _, err := time.LoadLocation(c.Timezone); err != nil {
			return fmt.Errorf("unknown timezone")
		}
	}
	check := func(items []Item) error {
		if len(items) > 10000 {
			return fmt.Errorf("too many items")
		}
		for _, it := range items {
			if it.Key <= 0 || it.DurationMS < 1000 || it.DurationMS > 24*60*60*1000 {
				return fmt.Errorf("programme needs a catalogue key and duration of 1 second to 24 hours")
			}
		}
		return nil
	}
	if err := check(c.Items); err != nil {
		return err
	}
	for i, b := range c.Blocks {
		if b.Start < 0 || b.End <= b.Start || b.End > 1440 {
			return fmt.Errorf("time blocks must be within 00:00–24:00")
		}
		if len(b.Items) == 0 && len(c.Fallback) == 0 && !c.OffAir {
			return fmt.Errorf("UTC time blocks must be nonoverlapping, within 00:00–24:00, and contain programming")
		}
		if err := check(b.Items); err != nil {
			return err
		}
		if err := check(b.Filler); err != nil {
			return err
		}
		if err := checkPlay(b.Mode, b.Align, b.Fit, b.RepeatDays, b.Watched); err != nil {
			return err
		}
		for _, d := range b.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("time block day must be 0–6")
			}
		}
		for j := i + 1; j < len(c.Blocks); j++ {
			if blocksOverlap(b, c.Blocks[j]) {
				return fmt.Errorf("UTC time blocks must be nonoverlapping, within 00:00–24:00, and contain programming")
			}
		}
	}
	if err := check(c.Filler); err != nil {
		return err
	}
	if err := check(c.Fallback); err != nil {
		return err
	}
	if c.Epoch.IsZero() {
		c.Epoch = time.Unix(0, 0).UTC()
	}
	return nil
}
func (s *Service) SaveChannel(ctx context.Context, c Channel) (Channel, error) {
	if c.ID == "" {
		c.ID = randomID()
	}
	if err := s.expand(ctx, &c); err != nil {
		return c, err
	}
	if err := validateChannel(&c); err != nil {
		return c, err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	_, err = s.o.DB.ExecContext(ctx, `INSERT INTO livetv_channels(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, c.ID, b)
	if err == nil {
		s.invalidate(c.ID, "")
		s.bumpLineup()
	}
	return c, err
}
func (s *Service) DeleteChannel(ctx context.Context, id string) error {
	_, err := s.o.DB.ExecContext(ctx, `DELETE FROM livetv_channels WHERE id=?`, id)
	if err == nil {
		s.invalidate(id, "")
		s.bumpLineup()
	}
	return err
}

// SetOnChange registers the Plex lineup refresh. Save, delete and source
// import call it after the database commit.
func (s *Service) SetOnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// LineupChanged is the DVR refreshedAt clients poll. It moves whenever the
// channel set changes, including twice in the same wall-clock second.
func (s *Service) LineupChanged() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lineupUnix == 0 {
		return time.Time{}
	}
	return time.Unix(s.lineupUnix, 0).UTC()
}

func (s *Service) bumpLineup() {
	s.mu.Lock()
	now := s.o.Now().Unix()
	if now <= s.lineupUnix {
		now = s.lineupUnix + 1
	}
	s.lineupUnix = now
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}
func (s *Service) Policy(ctx context.Context, userID int64) (Policy, error) {
	s.policyReads.Add(1)
	p := Policy{UserID: userID}
	var b []byte
	err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_policies WHERE user_id=?`, userID).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	return p, err
}
func (s *Service) SavePolicy(ctx context.Context, p Policy) error {
	if p.UserID <= 0 {
		return fmt.Errorf("userId must be positive")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = s.o.DB.ExecContext(ctx, `INSERT INTO livetv_policies(user_id,data) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET data=excluded.data`, p.UserID, b)
	return err
}
func containsUser(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
func containsString(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
func (s *Service) allowed(ctx context.Context, u *core.User, c Channel, p *Programme) bool {
	var lookup func(context.Context, core.RatingKey) (*core.Title, error)
	if s.o.Catalog != nil {
		lookup = s.o.Catalog.Get
	}
	return s.allowedWithLookup(ctx, u, c, p, lookup, false)
}

type loadedPolicyKey struct{}

type loadedPolicy struct {
	id  int64
	pol Policy
}

// withPolicy reads the viewer's Live TV policy once and reuses it for every
// channel check on this request.
func (s *Service) withPolicy(ctx context.Context, u *core.User) (context.Context, Policy, error) {
	if u == nil {
		return ctx, Policy{}, nil
	}
	if loaded, ok := ctx.Value(loadedPolicyKey{}).(loadedPolicy); ok && loaded.id == u.ID {
		return ctx, loaded.pol, nil
	}
	pol, err := s.Policy(ctx, u.ID)
	if err != nil {
		return ctx, Policy{}, err
	}
	return context.WithValue(ctx, loadedPolicyKey{}, loadedPolicy{id: u.ID, pol: pol}), pol, nil
}

func (s *Service) allowedWithLookup(ctx context.Context, u *core.User, c Channel, p *Programme, lookup func(context.Context, core.RatingKey) (*core.Title, error), strict bool) bool {
	if u == nil || !c.Enabled {
		return false
	}
	_, pol, err := s.withPolicy(ctx, u)
	if err != nil {
		return false
	}
	if !u.IsOwner && (pol.Disabled || len(pol.Channels) > 0 && !containsString(pol.Channels, c.ID) || len(c.AllowedUsers) > 0 && !containsUser(c.AllowedUsers, u.ID)) {
		return false
	}
	rating := c.Rating
	if p != nil {
		rating = p.Item.Rating
		if rating == "" {
			rating = c.Rating
		}
	}
	if contentrating.Exceeds(rating, u.MaxContentRating) || contentrating.Exceeds(rating, pol.MaxRating) {
		return false
	}
	if p != nil && p.Item.Key != 0 && (lookup != nil || strict) {
		if lookup == nil {
			return false
		}
		checkTitle := func(t *core.Title) bool {
			if t == nil || t.Adult || contentrating.Exceeds(t.ContentRating, u.MaxContentRating) || contentrating.Exceeds(t.ContentRating, pol.MaxRating) {
				return false
			}
			if len(u.AllowedSections) > 0 {
				section := 1
				if t.Type != core.TypeMovie {
					section = 2
				}
				ok := false
				for _, id := range u.AllowedSections {
					if id == section {
						ok = true
					}
				}
				if !ok {
					return false
				}
			}
			if s.o.Allowed != nil && !s.o.Allowed(u, t) {
				return false
			}
			return true
		}
		t, err := lookup(ctx, p.Item.Key)
		if err != nil || !checkTitle(t) || (strict && t.Key != p.Item.Key) {
			return false
		}
		if strict && t.ShowKey != 0 {
			show, err := lookup(ctx, t.ShowKey)
			if err != nil || show == nil || show.Key != t.ShowKey || !checkTitle(show) {
				return false
			}
		}
	}
	return true
}

// AllowedChannelCached checks one channel and its current programme using
// local title facts only. Missing programme title or inherited show facts fail
// closed. The supplied lookup must not perform network or release resolution.
func (s *Service) AllowedChannelCached(ctx context.Context, u *core.User, id string, cached func(context.Context, core.RatingKey) (*core.Title, error)) error {
	c, err := s.Channel(ctx, id)
	if err != nil {
		return err
	}
	now := s.o.Now()
	programmes, err := s.programmes(ctx, c, now, now.Add(time.Second))
	if err != nil {
		return err
	}
	var current *Programme
	if len(programmes) > 0 {
		current = &programmes[0]
	}
	if !s.allowedWithLookup(ctx, u, c, current, cached, true) {
		return core.ErrNotFound
	}
	return nil
}
func (s *Service) AllowedChannels(ctx context.Context, u *core.User) ([]Channel, error) {
	ctx, _, err := s.withPolicy(ctx, u)
	if err != nil {
		return nil, err
	}
	cs, err := s.Channels(ctx)
	if err != nil {
		return nil, err
	}
	out := []Channel{}
	for _, c := range cs {
		p, ok, err := s.programmeAt(ctx, c, s.o.Now())
		if err != nil {
			return nil, err
		}
		var current *Programme
		if ok {
			current = &p
		}
		if s.allowed(ctx, u, c, current) {
			c.StreamURL = ""
			c.StreamSources = nil
			out = append(out, c)
		}
	}
	return out, nil
}

// ChannelPermitted decides one channel. A memo on ctx, when present, answers
// repeat checks for the same channel without another guide read. Callers that
// are not looking at a live channel must not call this.
func (s *Service) ChannelPermitted(ctx context.Context, u *core.User, id string) (bool, error) {
	if u == nil || id == "" {
		return false, nil
	}
	if memo, ok := channelMemoFrom(ctx); ok {
		if decided, hit := memo.get(id); hit {
			return decided, nil
		}
	}
	c, err := s.Channel(ctx, id)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			rememberChannel(ctx, id, false)
			return false, nil
		}
		return false, err
	}
	p, ok, err := s.programmeAt(ctx, c, s.o.Now())
	if err != nil {
		return false, err
	}
	var current *Programme
	if ok {
		current = &p
	}
	allowed := s.allowed(ctx, u, c, current)
	rememberChannel(ctx, id, allowed)
	return allowed, nil
}

// programmeAt reads the single programme airing at a time. The primary key
// is (source, channel, start), so the latest start at or before that time is
// one indexed row rather than a walk of the channel's guide.
func (s *Service) programmeAt(ctx context.Context, c Channel, at time.Time) (Programme, bool, error) {
	if c.Kind == "virtual" {
		ps := Schedule(c, at, at.Add(time.Second))
		if len(ps) == 0 {
			return Programme{}, false, nil
		}
		return ps[0], true, nil
	}
	var b []byte
	err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_programmes WHERE source_id=? AND channel_id=? AND start_ms<=? ORDER BY start_ms DESC LIMIT 1`, guideSource(c), c.GuideID, at.UnixMilli()).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return Programme{}, false, nil
	}
	if err != nil {
		return Programme{}, false, err
	}
	var p Programme
	if err = json.Unmarshal(b, &p); err != nil {
		return Programme{}, false, err
	}
	if !p.End.After(at) {
		return Programme{}, false, nil
	}
	p.ChannelID = c.ID
	p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
	return p, true, nil
}

// guideChannel drops titles the resolver already knows cannot play. Unknown
// titles keep their saved order, so a channel with no availability cache
// schedules exactly as it did before.
func (s *Service) guideChannel(ctx context.Context, c Channel) Channel {
	src := s.o.Availability
	if src == nil {
		return c
	}
	items, itemsChanged := omitUnavailable(ctx, src, c.Items)
	fallback, fallbackChanged := omitUnavailable(ctx, src, c.Fallback)
	filler, fillerChanged := omitUnavailable(ctx, src, c.Filler)
	blocks := c.Blocks
	blocksChanged := false
	if len(c.Blocks) > 0 {
		next := make([]Block, len(c.Blocks))
		copy(next, c.Blocks)
		for i := range next {
			bi, ch := omitUnavailable(ctx, src, next[i].Items)
			bf, ch2 := omitUnavailable(ctx, src, next[i].Filler)
			if ch || ch2 {
				blocksChanged = true
				next[i].Items = bi
				next[i].Filler = bf
			}
		}
		if blocksChanged {
			blocks = next
		}
	}
	if !itemsChanged && !fallbackChanged && !fillerChanged && !blocksChanged {
		return c
	}
	c.Items, c.Fallback, c.Filler, c.Blocks = items, fallback, filler, blocks
	return c
}

func omitUnavailable(ctx context.Context, src core.AvailabilitySource, items []Item) ([]Item, bool) {
	if len(items) == 0 {
		return items, false
	}
	out := make([]Item, 0, len(items))
	dropped := false
	for _, it := range items {
		if it.Key != 0 && it.Slate == "" {
			if known, ok := src.Available(ctx, it.Key); known && !ok {
				dropped = true
				continue
			}
		}
		out = append(out, it)
	}
	if !dropped {
		return items, false
	}
	return out, true
}

func (s *Service) programmes(ctx context.Context, c Channel, from, until time.Time) ([]Programme, error) {
	if c.Kind == "virtual" {
		return Schedule(s.guideChannel(ctx, c), from, until), nil
	}
	if until.Sub(from) <= time.Second {
		p, ok, err := s.programmeAt(ctx, c, from)
		if err != nil || !ok {
			return nil, err
		}
		return []Programme{p}, nil
	}
	// end_ms leads so the guide index can skip programmes that have already ended.
	rows, err := s.o.DB.QueryContext(ctx, `SELECT data FROM livetv_programmes WHERE source_id=? AND channel_id=? AND end_ms>? AND start_ms<? ORDER BY start_ms`, guideSource(c), c.GuideID, from.UnixMilli(), until.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Programme{}
	for rows.Next() {
		var b []byte
		var p Programme
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		p.ChannelID = c.ID
		p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Service) Guide(ctx context.Context, u *core.User, from, until time.Time) ([]Programme, error) {
	if !until.After(from) || until.Sub(from) > 7*24*time.Hour {
		return nil, fmt.Errorf("guide window must be positive and at most 7 days")
	}
	var err error
	ctx, _, err = s.withPolicy(ctx, u)
	if err != nil {
		return nil, err
	}
	cs, err := s.Channels(ctx)
	if err != nil {
		return nil, err
	}
	out := []Programme{}
	for _, c := range cs {
		if !c.Enabled {
			continue
		}
		ps, err := s.programmes(ctx, c, from, until)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			if s.allowed(ctx, u, c, &p) {
				out = append(out, p)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// OnNow is the bounded guide used to build a live row: one policy read, then
// at most the current and next programme for each enabled channel.
func (s *Service) OnNow(ctx context.Context, u *core.User, now, until time.Time) ([]Channel, []Programme, error) {
	return s.onNow(ctx, u, now, until, 0)
}

// OnNowN stops after limit allowed channels. Home uses it so a large guide
// does not query a programme window for every channel on the request.
func (s *Service) OnNowN(ctx context.Context, u *core.User, now, until time.Time, limit int) ([]Channel, []Programme, error) {
	return s.onNow(ctx, u, now, until, limit)
}

func (s *Service) onNow(ctx context.Context, u *core.User, now, until time.Time, limit int) ([]Channel, []Programme, error) {
	if u == nil || !until.After(now) {
		return nil, nil, fmt.Errorf("invalid on-now window")
	}
	ctx, _, err := s.withPolicy(ctx, u)
	if err != nil {
		return nil, nil, err
	}
	cs, err := s.Channels(ctx)
	if err != nil {
		return nil, nil, err
	}
	channels := make([]Channel, 0, len(cs))
	programmes := make([]Programme, 0, len(cs)*2)
	for _, c := range cs {
		if !c.Enabled {
			continue
		}
		ps, err := s.programmesLimit(ctx, c, now, until, 2)
		if err != nil {
			return nil, nil, err
		}
		var current *Programme
		if len(ps) > 0 {
			current = &ps[0]
		}
		if !s.allowed(ctx, u, c, current) {
			continue
		}
		c.StreamURL = ""
		c.StreamSources = nil
		channels = append(channels, c)
		programmes = append(programmes, ps...)
		if limit > 0 && len(channels) >= limit {
			break
		}
	}
	return channels, programmes, nil
}

func (s *Service) programmesLimit(ctx context.Context, c Channel, from, until time.Time, limit int) ([]Programme, error) {
	if limit <= 0 {
		return nil, nil
	}
	if c.Kind == "virtual" {
		ps := Schedule(s.guideChannel(ctx, c), from, until)
		if len(ps) > limit {
			ps = ps[:limit]
		}
		return ps, nil
	}
	rows, err := s.o.DB.QueryContext(ctx, `SELECT data FROM livetv_programmes WHERE source_id=? AND channel_id=? AND end_ms>? AND start_ms<? ORDER BY start_ms LIMIT ?`, guideSource(c), c.GuideID, from.UnixMilli(), until.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Programme, 0, limit)
	for rows.Next() {
		var b []byte
		var p Programme
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &p); err != nil {
			return nil, err
		}
		p.ChannelID = c.ID
		p.ID = fmt.Sprintf("%s-%d", c.ID, p.Start.UnixMilli())
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) Sources(ctx context.Context) ([]Source, error) {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT data FROM livetv_sources ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Source{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		v, _, err := s.decodeSource(b)
		if err != nil {
			return nil, err
		}
		v.HasPlaylist = v.Playlist != "" || v.PlaylistURL != ""
		v.HasGuide = v.Guide != "" || v.GuideURL != ""
		v.Playlist = ""
		v.Guide = ""
		redactSourceAuth(&v)
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return out, s.fillSourceStats(ctx, out)
}
func (s *Service) readURL(ctx context.Context, raw string, max int64) (string, error) {
	return s.readURLWith(ctx, raw, max, nil, "source")
}

// ImportSource atomically replaces source programming. Owner changes to mapping,
// enabled state, channel numbering, favourites and restrictions survive refresh.
func (s *Service) ImportSource(ctx context.Context, src Source) (Source, error) {
	if src.ID == "" {
		src.ID = randomID()
	}
	if strings.TrimSpace(src.Name) == "" {
		return src, fmt.Errorf("source needs a name")
	}
	if err := s.keepStoredUploads(ctx, &src); err != nil {
		return src, err
	}
	if err := s.prepareSourceAuth(ctx, &src); err != nil {
		return src, err
	}
	var err error
	if src.PlaylistURL != "" {
		src.Playlist, err = s.readURLWith(ctx, src.PlaylistURL, maxPlaylistBytes, src.Auth.header(), "playlist")
		if err != nil {
			return src, err
		}
	}
	if strings.TrimSpace(src.Playlist) == "" && strings.TrimSpace(src.Guide) == "" && src.GuideURL == "" {
		return src, fmt.Errorf("source needs a playlist or an XMLTV guide")
	}
	var cs []Channel
	if strings.TrimSpace(src.Playlist) != "" { // guide-only (EPG) sources add no channels
		cs, err = ParseM3U(strings.NewReader(src.Playlist), src.ID)
		if err != nil {
			return src, err
		}
	}
	allow, err := s.guideAllow(ctx, src, cs)
	if err != nil {
		return src, err
	}
	filter := guideFilter{From: s.o.Now().Add(-guideLookback), Until: s.o.Now().Add(guideLookahead), IDs: allow}
	g := XMLGuide{}
	if src.GuideURL != "" {
		g, err = s.readGuide(ctx, src.GuideURL, src.Auth.header(), filter)
		if err != nil {
			return src, err
		}
	} else if src.Guide != "" {
		var dropped bool
		g, dropped, err = parseXMLTV(strings.NewReader(src.Guide), filter)
		if err != nil {
			return src, err
		}
		if dropped {
			trimmed, err := formatXMLTV(g)
			if err != nil {
				return src, err
			}
			src.Guide = trimmed
		}
	}
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return src, err
	}
	defer tx.Rollback()
	// Writing the source row first takes its row lock, so a concurrent save
	// of the same source on another node waits here; the owner's channel
	// edits are then read inside the transaction and cannot be lost.
	src.UpdatedAt = s.o.Now()
	sb, err := s.encodeSource(src)
	if err != nil {
		return src, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_sources(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, src.ID, sb); err != nil {
		return src, err
	}
	old, err := channels(ctx, tx)
	if err != nil {
		return src, err
	}
	oldByID := map[string]Channel{}
	for _, c := range old {
		if c.SourceID == src.ID {
			oldByID[c.ID] = c
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM livetv_programmes WHERE source_id=?`, src.ID); err != nil {
		return src, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM livetv_channels WHERE `+store.JSONText(store.DialectOf(s.o.DB), "data", "sourceId")+`=?`, src.ID); err != nil {
		return src, err
	}
	stored := make([]Channel, 0, len(cs))
	for _, c := range cs {
		if previous, ok := oldByID[c.ID]; ok {
			c.Number = previous.Number
			c.Enabled = previous.Enabled
			c.Favorite = previous.Favorite
			c.AllowedUsers = previous.AllowedUsers
			c.Rating = previous.Rating
			c.GuideID = previous.GuideID
			c.GuideSourceID = previous.GuideSourceID
			c.Transcode = previous.Transcode
			c.StreamSources = mergeImportedStreams(c, previous)
			if err := normalizeStreamSources(&c); err != nil {
				return src, err
			}
		}
		if guide, ok := g.Channels[c.GuideID]; ok && c.Logo == "" {
			c.Logo = guide.Logo
		}
		b, e := json.Marshal(c)
		if e != nil {
			return src, e
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_channels(id,data) VALUES(?,?)`, c.ID, b); err != nil {
			return src, err
		}
		stored = append(stored, c)
	}
	for _, p := range g.Programmes {
		b, e := json.Marshal(p)
		if e != nil {
			return src, e
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO livetv_programmes(source_id,channel_id,start_ms,end_ms,data) VALUES(?,?,?,?,?)
			ON CONFLICT(source_id,channel_id,start_ms) DO UPDATE SET end_ms=excluded.end_ms, data=excluded.data`,
			src.ID, p.ChannelID, p.Start.UnixMilli(), p.End.UnixMilli(), b); err != nil {
			return src, err
		}
	}
	if err = replaceGuideChannels(ctx, tx, src.ID, g); err != nil {
		return src, err
	}
	err = tx.Commit()
	if err == nil {
		s.bumpLineup()
		// Only tunes whose stream changed or disappeared are interrupted, so
		// a scheduled refresh does not cut off viewers.
		for _, id := range changedStreams(oldByID, stored) {
			s.invalidate(id, "")
		}
	}
	s.forgetSourceAuth(src.ID)
	src = storedSource(src)
	src.HasPlaylist = src.Playlist != "" || src.PlaylistURL != ""
	src.HasGuide = src.Guide != "" || src.GuideURL != ""
	src.Playlist = ""
	src.Guide = ""
	redactSourceAuth(&src)
	if err == nil {
		one := []Source{src}
		if e := s.fillSourceStats(ctx, one); e == nil {
			src = one[0]
		}
	}
	return src, err
}

// guideAllow is the set of XMLTV channel ids a playlist source keeps.
// Nil means every channel (a guide-only source). Stored guide ids are
// included so an owner mapping survives the next refresh.
func (s *Service) guideAllow(ctx context.Context, src Source, playlist []Channel) (map[string]struct{}, error) {
	if strings.TrimSpace(src.Playlist) == "" && src.PlaylistURL == "" {
		return nil, nil
	}
	ids := map[string]struct{}{}
	add := func(id string) {
		id = strings.ToLower(strings.TrimSpace(id))
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	for _, c := range playlist {
		add(c.GuideID)
	}
	stored, err := channels(ctx, s.o.DB)
	if err != nil {
		return nil, err
	}
	for _, c := range stored {
		if c.SourceID == src.ID || guideSource(c) == src.ID {
			add(c.GuideID)
		}
	}
	return ids, nil
}

// SavePendingSource stores a named source that will receive a file upload
// next. A body that already has a playlist or guide is imported instead.
func (s *Service) SavePendingSource(ctx context.Context, src Source) (Source, error) {
	if src.ID == "" {
		src.ID = randomID()
	}
	if strings.TrimSpace(src.Name) == "" {
		return src, fmt.Errorf("source needs a name")
	}
	if err := s.prepareSourceAuth(ctx, &src); err != nil {
		return src, err
	}
	if src.Playlist != "" || src.PlaylistURL != "" || src.Guide != "" || src.GuideURL != "" {
		return s.ImportSource(ctx, src)
	}
	old, err := s.storedSourceByID(ctx, src.ID)
	if err == nil {
		src.Playlist, src.PlaylistURL = old.Playlist, old.PlaylistURL
		src.Guide, src.GuideURL = old.Guide, old.GuideURL
		if src.Auth == nil {
			src.Auth = old.Auth
		}
		if src.Playlist != "" || src.PlaylistURL != "" || src.Guide != "" || src.GuideURL != "" {
			return s.ImportSource(ctx, src)
		}
	} else if !errors.Is(err, core.ErrNotFound) {
		return src, err
	}
	src.UpdatedAt = s.o.Now()
	b, err := s.encodeSource(src)
	if err != nil {
		return src, err
	}
	if _, err = s.o.DB.ExecContext(ctx, `INSERT INTO livetv_sources(id,data) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`, src.ID, b); err != nil {
		return src, err
	}
	src.HasPlaylist = false
	src.HasGuide = false
	src.Playlist, src.Guide = "", ""
	redactSourceAuth(&src)
	one := []Source{src}
	if e := s.fillSourceStats(ctx, one); e == nil {
		src = one[0]
	}
	return src, nil
}

// ImportUpload streams a raw playlist or guide body (gzip is decompressed
// while reading) into an existing source.
func (s *Service) ImportUpload(ctx context.Context, id, part string, body io.Reader) (Source, error) {
	src, err := s.storedSourceByID(ctx, id)
	if err != nil {
		return src, err
	}
	var max int64 = maxPlaylistBytes
	kind := "playlist"
	if part == "guide" {
		max = maxGuideBytes
		kind = "guide"
	}
	rd, err := maybeGunzip(body, max, kind)
	if err != nil {
		return src, err
	}
	defer closeReader(rd)
	if part == "playlist" {
		b, err := io.ReadAll(&hardLimit{r: rd, max: max, kind: kind})
		if err != nil {
			if lim := limitCause(err); lim != nil {
				return src, lim
			}
			return src, err
		}
		src.Playlist = string(b)
		src.PlaylistURL = ""
		return s.ImportSource(ctx, src)
	}
	var cs []Channel
	if strings.TrimSpace(src.Playlist) != "" {
		cs, err = ParseM3U(strings.NewReader(src.Playlist), src.ID)
		if err != nil {
			return src, err
		}
	}
	allow, err := s.guideAllow(ctx, src, cs)
	if err != nil {
		return src, err
	}
	g, _, err := parseXMLTV(rd, guideFilter{From: s.o.Now().Add(-guideLookback), Until: s.o.Now().Add(guideLookahead), IDs: allow})
	if err != nil {
		return src, err
	}
	trimmed, err := formatXMLTV(g)
	if err != nil {
		return src, err
	}
	src.Guide = trimmed
	src.GuideURL = ""
	return s.ImportSource(ctx, src)
}

func (s *Service) DeleteSource(ctx context.Context, id string) error {
	defer s.forgetSourceAuth(id)
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	d := store.DialectOf(s.o.DB)
	for _, q := range []string{`DELETE FROM livetv_channels WHERE ` + store.JSONText(d, "data", "sourceId") + `=?`, `DELETE FROM livetv_programmes WHERE source_id=?`, `DELETE FROM livetv_sources WHERE id=?`, `DELETE FROM livetv_guide_channels WHERE source_id=?`,
		`UPDATE livetv_channels SET data=` + store.JSONRemove(d, "data", "guideSourceId") + ` WHERE ` + store.JSONText(d, "data", "guideSourceId") + `=?`} {
		if _, err = tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	err = tx.Commit()
	if err == nil {
		s.invalidate("", id)
		s.bumpLineup()
	}
	return err
}
