package plex

import (
	"context"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/episodeorder"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/store"
	"reflect"
	"sort"
	"testing"
	"time"
)

// Test-only snapshot of the pre-extraction algorithm, retained for parity.
func (s *Server) controlLegacyInProgressTitles(ctx context.Context, pr *Request, t core.MediaType, limit int) ([]core.Title, map[core.RatingKey]core.WatchStatus) {
	if s.d.State == nil || pr.User == nil {
		return nil, nil
	}
	sts, err := s.d.State.InProgress(ctx, pr.User.ID, limit*2+10)
	if err != nil {
		s.log.Warn("load in-progress", "err", err)
		return nil, nil
	}
	keys := make([]core.RatingKey, 0, len(sts))
	state := make(map[core.RatingKey]core.WatchStatus, len(sts))
	for _, st := range sts {
		keys = append(keys, st.Key)
		state[st.Key] = st
	}
	titles := s.getTitles(ctx, keys)
	// Resume unfinished media and advance recently completed series. A
	// completed episode has no offset, so InProgress alone loses the show.
	if t != core.TypeMovie {
		titles = s.controlLegacyAppendNextContinue(ctx, pr, titles, state, limit)
	}
	sort.SliceStable(titles, func(i, j int) bool {
		a, b := state[titles[i].Key].LastViewedAt, state[titles[j].Key].LastViewedAt
		if !a.Equal(b) {
			return a.After(b)
		}
		return titles[i].Key < titles[j].Key
	})
	if s.d.FavoriteShows != nil {
		if favorites, err := s.d.FavoriteShows(ctx, pr.User); err == nil {
			experience.PrioritizeFavoriteShows(titles, favorites)
		}
	}
	seenShows := map[int64]bool{}
	out := titles[:0:0]
	for _, ti := range titles {
		if t != "" {
			want := t
			if t == core.TypeShow {
				want = core.TypeEpisode
			}
			if ti.Type != want {
				continue
			}
		}
		sec, ok := s.sectionFor(ti.Type)
		if ok && !s.canSeeSection(pr.User, sec.ID) {
			continue
		}
		if ti.Type == core.TypeEpisode {
			if seenShows[ti.Key.TMDB()] {
				continue
			}
			seenShows[ti.Key.TMDB()] = true
		}
		out = append(out, ti)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, state
}

func (s *Server) controlLegacyAppendNextContinue(ctx context.Context, pr *Request, titles []core.Title, state map[core.RatingKey]core.WatchStatus, limit int) []core.Title {
	history, err := s.d.State.History(ctx, pr.User.ID, s.now().Add(-90*24*time.Hour), 200)
	if err != nil {
		return titles
	}
	seen := map[int64]bool{}
	for _, t := range titles {
		if t.Type == core.TypeEpisode {
			seen[t.Key.TMDB()] = true
		}
	}
	var recent []core.HistoryEntry
	var keys, shows []core.RatingKey
	for _, h := range history {
		if !h.Completed || h.Key.Kind() != core.KindEpisode || seen[h.Key.TMDB()] {
			continue
		}
		seen[h.Key.TMDB()] = true
		recent = append(recent, h)
		keys = append(keys, h.Key)
		shows = append(shows, core.ShowKey(h.Key.TMDB()))
		if len(recent) >= min(max(limit, 1), 50) {
			break
		}
	}
	statuses, err := s.d.State.GetMany(ctx, pr.User.ID, keys)
	if err != nil {
		return titles
	}
	grouped := map[core.RatingKey]bool{}
	if s.d.EpisodeOrder != nil {
		grouped, err = s.d.EpisodeOrder.Grouped(ctx, pr.User, shows)
		if err != nil {
			return titles
		}
	}
	for _, h := range recent {
		if ctx.Err() != nil {
			break
		}
		if statuses[h.Key].ViewCount == 0 {
			continue
		} // subsequently marked unwatched
		current := h.Key
		var ordered []core.Title
		if grouped[core.ShowKey(current.TMDB())] {
			eps, e := s.metadataGrandchildren(ctx, pr, core.ShowKey(current.TMDB()))
			if e != nil {
				continue
			}
			for i := range eps {
				if eps[i].Key == current {
					ordered = eps[i+1:]
					break
				}
			}
		}
		for n := 0; n < 32; n++ {
			var next *core.Title
			if grouped[core.ShowKey(current.TMDB())] {
				if n >= len(ordered) {
					break
				}
				next = &ordered[n]
			} else {
				next, err = s.d.Catalog.NextEpisode(ctx, current)
				if err != nil || next == nil || next.Key == current {
					break
				}
			}
			current = next.Key
			if next.Season == 0 {
				continue
			}
			if !next.Released.IsZero() && next.Released.After(s.now()) {
				break
			}
			if !s.visibleTitle(ctx, pr.User, next) || s.sameDayUnavailable(ctx, pr, next) {
				break
			}
			st, e := s.d.State.Get(ctx, pr.User.ID, next.Key)
			if e != nil {
				break
			}
			if st.ViewCount > 0 && st.ViewOffset <= 0 {
				continue
			}
			st.Key = next.Key
			if st.LastViewedAt.Before(h.ViewedAt) {
				st.LastViewedAt = h.ViewedAt
			}
			state[next.Key] = st
			titles = append(titles, *next)
			break
		}
	}
	return titles
}

type continueControlState struct {
	*fakeState
	history []core.HistoryEntry
}

func (s *continueControlState) History(context.Context, int64, time.Time, int) ([]core.HistoryEntry, error) {
	return s.history, nil
}

type continueDenyPolicy struct{ denied map[core.RatingKey]bool }

func (p continueDenyPolicy) Allowed(_ *core.User, t *core.Title) bool { return !p.denied[t.Key] }

func TestContinueLegacyControl(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		setup      func(*fixture, *continueControlState)
		kind       core.MediaType
		limit      int
		want       []core.RatingKey
		stateCount int
	}{
		{name: "title and parent denial, ties, missing title and state map", kind: core.TypeShow, limit: 1, stateCount: 3, want: []core.RatingKey{bbE1.Key}, setup: func(f *fixture, s *continueControlState) {
			f.srv.d.AccessPolicy = continueDenyPolicy{map[core.RatingKey]bool{breakingBad.Key: true, bbE1.Key: true}}
			for _, k := range []core.RatingKey{bbE1.Key, bbE2.Key, core.MovieKey(999999)} {
				s.set(ownerUser.ID, core.WatchStatus{Key: k, ViewOffset: time.Second, LastViewedAt: fixedNow})
			}
		}},
		{name: "absent movie section retains legacy result", kind: core.TypeMovie, limit: 2, stateCount: 2, want: []core.RatingKey{fightClub.Key, alien.Key}, setup: func(f *fixture, s *continueControlState) {
			f.srv.sections = nil
			s.set(ownerUser.ID, core.WatchStatus{Key: fightClub.Key, ViewOffset: time.Second, LastViewedAt: fixedNow})
			s.set(ownerUser.ID, core.WatchStatus{Key: alien.Key, ViewOffset: time.Second, LastViewedAt: fixedNow.Add(-time.Minute)})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			defer f.srv.Close()
			state := &continueControlState{fakeState: f.state}
			f.srv.d.State = state
			tc.setup(f, state)
			pr := &Request{User: &ownerUser}
			want, wantState := f.srv.controlLegacyInProgressTitles(ctx, pr, tc.kind, tc.limit)
			got, gotState := f.srv.inProgressTitles(ctx, pr, tc.kind, tc.limit)
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotState, wantState) {
				t.Fatalf("legacy=%+v state=%+v; shared=%+v state=%+v", want, wantState, got, gotState)
			}
			keys := make([]core.RatingKey, len(got))
			for i := range got {
				keys[i] = got[i].Key
			}
			if !reflect.DeepEqual(keys, tc.want) || len(gotState) != tc.stateCount {
				t.Fatalf("keys=%v state=%+v", keys, gotState)
			}
		})
	}
}

func TestContinueLegacyControlGroupedNextAndPolicy(t *testing.T) {
	for _, denied := range []core.RatingKey{0, breakingBad.Key, bbE1.Key} {
		name := map[core.RatingKey]string{0: "allowed", breakingBad.Key: "denied parent", bbE1.Key: "denied title"}[denied]
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture()
			defer f.srv.Close()
			db, err := store.OpenMemory(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			order := episodeorder.New(db, orderedSource{}, f.catalog)
			f.srv.d.EpisodeOrder = order
			if err = order.Save(ctx, 0, breakingBad.Key, episodeorder.Preference{GroupID: "dvd", Specials: "last"}); err != nil {
				t.Fatal(err)
			}
			state := &continueControlState{fakeState: f.state, history: []core.HistoryEntry{{Key: bbE2.Key, Completed: true, ViewedAt: fixedNow}}}
			f.srv.d.State = state
			state.set(ownerUser.ID, core.WatchStatus{Key: bbE2.Key, ViewCount: 1})
			if denied != 0 {
				f.srv.d.AccessPolicy = continueDenyPolicy{map[core.RatingKey]bool{denied: true}}
			}
			pr := &Request{User: &ownerUser}
			want, ws := f.srv.controlLegacyInProgressTitles(ctx, pr, core.TypeShow, 5)
			got, gs := f.srv.inProgressTitles(ctx, pr, core.TypeShow, 5)
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gs, ws) {
				t.Fatalf("legacy=%+v state=%+v; shared=%+v state=%+v", want, ws, got, gs)
			}
			if denied != 0 && len(got) != 0 || denied == 0 && (len(got) != 1 || got[0].Key != bbE1.Key) {
				t.Fatalf("unexpected grouped next: %+v", got)
			}
		})
	}
}
