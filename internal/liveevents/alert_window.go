package liveevents

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

const (
	// AlertWindowLeadTime opens an alert window 15 minutes before scheduled start (A7).
	AlertWindowLeadTime = 15 * time.Minute
	// DefaultScheduleFetchInterval enforces at most 1 schedule discovery fetch per feed per hour (A7).
	DefaultScheduleFetchInterval = time.Hour
	// DefaultMinAlertPollInterval enforces the >= 10s live poll floor (A1/A7).
	DefaultMinAlertPollInterval = 15 * time.Second
	// DefaultMaxAlertWindowDuration bounds an alert window even if a feed stops updating.
	DefaultMaxAlertWindowDuration = 8 * time.Hour

	scheduleLookbackWindow  = 4 * time.Hour
	scheduleLookaheadWindow = 24 * time.Hour
)

// FeedPollResult is the response from a plugin live feed fetch (A1).
type FeedPollResult struct {
	Events      []core.LiveEvent
	NextPollSec int
}

// AlertFeedPoller fetches schedule and active-window event updates from a
// plugin feed. Production wires W255's hub/plugin client; tests supply fakes.
type AlertFeedPoller interface {
	FetchSchedule(ctx context.Context, pluginID, feedID string, from, to time.Time) (FeedPollResult, error)
	PollEvents(ctx context.Context, pluginID, feedID string, eventIDs []string) (FeedPollResult, error)
}

type feedKey struct {
	pluginID string
	feedID   string
}

type trackedAlertWindow struct {
	eventID     string
	feed        feedKey
	start       time.Time
	windowStart time.Time
	windowEnd   time.Time
	terminal    bool
}

type feedScheduleState struct {
	lastScheduleFetch time.Time
	lastPoll          time.Time
	nextPollAt        time.Time
	consecutiveErrors int
}

// AlertSchedulerOptions configures the A7 bounded alert-window scheduler.
type AlertSchedulerOptions struct {
	Favorites        FavoriteEntitiesReader
	Poller           AlertFeedPoller
	Engine           *AlertEngine
	Now              func() time.Time
	ScheduleInterval time.Duration
	MinPollInterval  time.Duration
	MaxWindowAge     time.Duration
}

// AlertScheduler performs bounded background alert polling ONLY inside open
// alert windows ([event.Start - 15m, final whistle]), discovered via at most
// one schedule fetch per watched feed per hour while any viewer has entity
// favourites. When no viewer has favourites, it performs zero fetches and
// sleeps idle until NotifyFavoritesChanged is signaled.
type AlertScheduler struct {
	favorites        FavoriteEntitiesReader
	poller           AlertFeedPoller
	engine           *AlertEngine
	now              func() time.Time
	scheduleInterval time.Duration
	minPollInterval  time.Duration
	maxWindowAge     time.Duration

	wakeCh chan struct{}

	mu      sync.Mutex
	feeds   map[feedKey]*feedScheduleState
	windows map[string]*trackedAlertWindow
}

// NewAlertScheduler creates a bounded A7 alert window scheduler.
func NewAlertScheduler(opts AlertSchedulerOptions) *AlertScheduler {
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	schedInt := opts.ScheduleInterval
	if schedInt <= 0 {
		schedInt = DefaultScheduleFetchInterval
	}
	minPoll := opts.MinPollInterval
	if minPoll < 10*time.Second {
		minPoll = DefaultMinAlertPollInterval
	}
	maxWin := opts.MaxWindowAge
	if maxWin <= 0 {
		maxWin = DefaultMaxAlertWindowDuration
	}
	return &AlertScheduler{
		favorites:        opts.Favorites,
		poller:           opts.Poller,
		engine:           opts.Engine,
		now:              nowFn,
		scheduleInterval: schedInt,
		minPollInterval:  minPoll,
		maxWindowAge:     maxWin,
		wakeCh:           make(chan struct{}, 1),
		feeds:            make(map[feedKey]*feedScheduleState),
		windows:          make(map[string]*trackedAlertWindow),
	}
}

// NotifyFavoritesChanged wakes Run immediately when a viewer adds or removes
// an entity favourite so a newly added favourite discovers its schedule and a
// removed last favourite drops to zero idle polling immediately.
func (s *AlertScheduler) NotifyFavoritesChanged() {
	if s == nil {
		return
	}
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

// Run executes the scheduler loop until ctx is canceled. When no viewer has
// favourites, no timer is armed and the loop blocks purely on wakeCh/ctx.Done.
func (s *AlertScheduler) Run(ctx context.Context) {
	if s == nil {
		return
	}
	for {
		nextWake, hasWake, _ := s.Step(ctx)
		if !hasWake {
			select {
			case <-ctx.Done():
				return
			case <-s.wakeCh:
				continue
			}
		}
		delay := time.Until(nextWake)
		if s.now != nil {
			delay = nextWake.Sub(s.now())
		}
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wakeCh:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// Step performs one synchronous scheduling pass at s.now() and returns the
// next wake time (hasWake == false when idle with zero favourites).
func (s *AlertScheduler) Step(ctx context.Context) (nextWake time.Time, hasWake bool, err error) {
	if s == nil || s.favorites == nil || s.poller == nil {
		return time.Time{}, false, nil
	}
	now := s.now()

	favs, err := s.favorites.ListEntityFavorites(ctx)
	if err != nil {
		return now.Add(s.minPollInterval), true, err
	}

	// Group valid favourites by feed. If no viewer has entity favourites, clear
	// tracked windows and return idle (zero background work).
	favsByFeed := make(map[feedKey][]ParsedEntityKey)
	for _, f := range favs {
		if f.ViewerID <= 0 {
			continue
		}
		pk, ok := ParseEntityKey(f.Key)
		if !ok {
			continue
		}
		fk := feedKey{pluginID: pk.PluginID, feedID: pk.Feed}
		favsByFeed[fk] = append(favsByFeed[fk], pk)
	}
	if len(favsByFeed) == 0 {
		s.mu.Lock()
		clear(s.windows)
		clear(s.feeds)
		s.mu.Unlock()
		return time.Time{}, false, nil
	}

	// Prune feeds/windows that no longer have any viewer favourite.
	s.mu.Lock()
	for fk := range s.feeds {
		if _, active := favsByFeed[fk]; !active {
			delete(s.feeds, fk)
		}
	}
	for id, w := range s.windows {
		if _, active := favsByFeed[w.feed]; !active || w.terminal || !now.Before(w.windowEnd) {
			delete(s.windows, id)
		}
	}
	s.mu.Unlock()

	// Sort feed keys for deterministic execution order.
	feedOrder := make([]feedKey, 0, len(favsByFeed))
	for fk := range favsByFeed {
		feedOrder = append(feedOrder, fk)
	}
	sort.Slice(feedOrder, func(i, j int) bool {
		if feedOrder[i].pluginID != feedOrder[j].pluginID {
			return feedOrder[i].pluginID < feedOrder[j].pluginID
		}
		return feedOrder[i].feedID < feedOrder[j].feedID
	})

	considerWake := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if !hasWake || t.Before(nextWake) {
			nextWake = t
			hasWake = true
		}
	}

	for _, fk := range feedOrder {
		feedFavs := favsByFeed[fk]
		st := s.feedState(fk)

		// 1. Schedule discovery: at most 1 fetch per feed per scheduleInterval (1h).
		if st.lastScheduleFetch.IsZero() || !now.Before(st.lastScheduleFetch.Add(s.scheduleInterval)) {
			res, fetchErr := s.poller.FetchSchedule(ctx, fk.pluginID, fk.feedID, now.Add(-scheduleLookbackWindow), now.Add(scheduleLookaheadWindow))
			s.mu.Lock()
			st.lastScheduleFetch = now
			s.mu.Unlock()
			if fetchErr == nil {
				s.ingestSchedule(ctx, fk, feedFavs, favs, res.Events, now)
			}
		}
		considerWake(st.lastScheduleFetch.Add(s.scheduleInterval))

		// 2. Active window check: poll ONLY events inside [start - 15m, final].
		openIDs, earliestUpcoming := s.openAndUpcomingForFeed(fk, now)
		if !earliestUpcoming.IsZero() {
			considerWake(earliestUpcoming)
		}
		if len(openIDs) == 0 {
			continue
		}

		// Respect poll backoff for this feed.
		s.mu.Lock()
		due := st.nextPollAt.IsZero() || !now.Before(st.nextPollAt)
		scheduledNext := st.nextPollAt
		s.mu.Unlock()
		if !due {
			considerWake(scheduledNext)
			continue
		}

		res, pollErr := s.poller.PollEvents(ctx, fk.pluginID, fk.feedID, openIDs)
		pollInterval := s.effectivePollInterval(res.NextPollSec)
		s.mu.Lock()
		if pollErr != nil {
			st.consecutiveErrors++
			pollInterval = max(s.minPollInterval, errorBackoff(s.minPollInterval, st.consecutiveErrors, 0))
		} else {
			st.consecutiveErrors = 0
		}
		st.lastPoll = now
		st.nextPollAt = s.now().Add(pollInterval)
		scheduledNext = st.nextPollAt
		s.mu.Unlock()

		if pollErr == nil {
			s.ingestPollEvents(ctx, favs, res.Events, now)
		}

		// Re-check whether any windows remain open after processing poll updates
		// (for example, if the final whistle just blew, stop polling immediately).
		remainingOpen, nextUpcoming := s.openAndUpcomingForFeed(fk, now)
		if len(remainingOpen) > 0 {
			considerWake(scheduledNext)
		} else if !nextUpcoming.IsZero() {
			considerWake(nextUpcoming)
		}
	}

	return nextWake, hasWake, nil
}

func (s *AlertScheduler) feedState(fk feedKey) *feedScheduleState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.feeds[fk]
	if !ok {
		st = &feedScheduleState{}
		s.feeds[fk] = st
	}
	return st
}

func (s *AlertScheduler) ingestSchedule(
	ctx context.Context,
	fk feedKey,
	feedFavs []ParsedEntityKey,
	allFavs []EntityFavorite,
	scheduleEvents []core.LiveEvent,
	now time.Time,
) {
	var matchingLive []core.LiveEvent
	s.mu.Lock()
	for _, ev := range scheduleEvents {
		if ev.ID == "" {
			continue
		}
		matched := false
		for _, pk := range feedFavs {
			if EventMatchesEntityKey(ev, pk) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if isTerminalEventState(ev.Status.State) {
			if s.engine != nil {
				s.engine.SeedScheduleEvent(ev)
			}
			delete(s.windows, ev.ID)
			continue
		}
		start := parseEventTime(ev.Start)
		if start.IsZero() {
			start = now
		}
		wStart := start.Add(-AlertWindowLeadTime)
		wEnd := start.Add(s.maxWindowAge)
		if !now.Before(wEnd) {
			continue
		}
		s.windows[ev.ID] = &trackedAlertWindow{
			eventID:     ev.ID,
			feed:        fk,
			start:       start,
			windowStart: wStart,
			windowEnd:   wEnd,
		}
		if ev.Status.State == core.LiveEventStateIn {
			matchingLive = append(matchingLive, ev)
		} else if s.engine != nil {
			s.engine.SeedScheduleEvent(ev)
		}
	}
	s.mu.Unlock()

	if len(matchingLive) > 0 && s.engine != nil {
		_ = s.engine.ProcessEventsWithFavorites(ctx, matchingLive, allFavs)
	}
}

func (s *AlertScheduler) ingestPollEvents(ctx context.Context, allFavs []EntityFavorite, polled []core.LiveEvent, now time.Time) {
	if s.engine != nil && len(polled) > 0 {
		_ = s.engine.ProcessEventsWithFavorites(ctx, polled, allFavs)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range polled {
		if w, ok := s.windows[ev.ID]; ok {
			if isTerminalEventState(ev.Status.State) || !now.Before(w.windowEnd) {
				w.terminal = true
				delete(s.windows, ev.ID)
			}
		}
	}
}

func (s *AlertScheduler) openAndUpcomingForFeed(fk feedKey, now time.Time) (openIDs []string, earliestUpcoming time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, w := range s.windows {
		if w.feed != fk || w.terminal {
			continue
		}
		if !now.Before(w.windowEnd) {
			delete(s.windows, id)
			continue
		}
		if !now.Before(w.windowStart) {
			openIDs = append(openIDs, w.eventID)
		} else if earliestUpcoming.IsZero() || w.windowStart.Before(earliestUpcoming) {
			earliestUpcoming = w.windowStart
		}
	}
	sort.Strings(openIDs)
	return openIDs, earliestUpcoming
}

func (s *AlertScheduler) effectivePollInterval(nextPollSec int) time.Duration {
	d := time.Duration(nextPollSec) * time.Second
	if d < s.minPollInterval {
		return s.minPollInterval
	}
	return d
}

// ActiveWindowCount reports the number of currently tracked upcoming or open
// alert windows (for diagnostics and tests).
func (s *AlertScheduler) ActiveWindowCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.windows)
}
