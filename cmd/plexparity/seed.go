package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/braedonsaunders/ether/internal/app" // register the same migrations as Ether
	"github.com/braedonsaunders/ether/internal/auth"
	"github.com/braedonsaunders/ether/internal/catalog"
	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/livetv"
	"github.com/braedonsaunders/ether/internal/overlays"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/recommend"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/trackprefs"
	"github.com/braedonsaunders/ether/internal/watchlist"
)

var fixtureNow = time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

// seed creates one fixture directory, which must be copied before either server starts.
// It uses the baseline's migrations and public component APIs; it refuses existing data.
func seed(ctx context.Context, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "ether.db")); err == nil {
		return fmt.Errorf("fixture database already exists")
	} else if !os.IsNotExist(err) {
		return err
	}
	cfg, err := config.Open(dir)
	if err != nil {
		return err
	}
	if err = cfg.Update(func(c *config.Config) error {
		c.Catalog.TMDBKey = "offline-fixture-key"
		c.Server.MachineIdentifier = "0123456789abcdef0123456789abcdef01234567"
		c.Server.FriendlyName = "Parity fixture"
		c.Plex.ClientIdentifier = "0123456789abcdef0123456789abcdef"
		c.Plex.Claimed = true
		c.Plex.OwnerUsername = "owner"
		c.Plex.UseUPnP = false
		c.Markers.TheIntroDB = false
		return nil
	}); err != nil {
		return err
	}
	db, err := store.Open(ctx, dir)
	if err != nil {
		return err
	}
	defer db.Close()
	now := func() time.Time { return fixtureNow }
	cat, err := catalog.New(db, nil, cfg.Get().Catalog, nil, catalog.WithNow(now))
	if err != nil {
		return err
	}
	released := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 60; i++ {
		title := core.Title{Key: core.MovieKey(10000 + i), Type: core.TypeMovie, IDs: core.IDs{TMDB: 10000 + i}, Title: fmt.Sprintf("Fixture Movie %03d", i), Year: 2020, Released: released, Runtime: 90 * time.Minute, Language: "en", ContentRating: "PG", Genres: []string{"Drama"}, Rating: 7, VoteCount: 1000, Popularity: float64(100 - i)}
		title.PosterPath = "/fixture.jpg"
		title.BackdropPath = "/fixture.jpg"
		title.Cast = []core.Person{{TMDB: 123, Name: "Fixture Actor", ThumbPath: "/fixture.jpg"}}
		if i <= 12 {
			title.CollectionID = 40001
			title.CollectionName = "Fixture Collection"
		}
		if i%3 == 0 {
			title.ContentRating = "R"
		}
		if err = cat.UpsertTitle(ctx, title, true); err != nil {
			return err
		}
	}
	for i := int64(1); i <= 20; i++ {
		id := 20000 + i
		show := core.Title{Key: core.ShowKey(id), Type: core.TypeShow, IDs: core.IDs{TMDB: id}, Title: fmt.Sprintf("Fixture Show %03d", i), Year: 2020, Released: released, Language: "en", ContentRating: "TV-PG", Genres: []string{"Drama"}, Rating: 7, VoteCount: 1000, Popularity: float64(100 - i), ChildCount: 1, LeafCount: 4}
		if i == 1 {
			// Episode history predates the child's later restriction; the
			// episode itself has no rating and inherits this show rating.
			show.ContentRating = "TV-MA"
		}
		show.PosterPath = "/fixture.jpg"
		show.BackdropPath = "/fixture.jpg"
		if err = cat.UpsertTitle(ctx, show, true); err != nil {
			return err
		}
		season := core.Title{Key: core.SeasonKey(id, 1), Type: core.TypeSeason, ShowKey: show.Key, ShowIDs: show.IDs, ShowTitle: show.Title, Season: 1, Title: "Season 1", Released: released, ChildCount: 4, LeafCount: 4}
		season.PosterPath = "/fixture.jpg"
		season.BackdropPath = "/fixture.jpg"
		if err = cat.UpsertTitle(ctx, season, false); err != nil {
			return err
		}
		for ep := 1; ep <= 4; ep++ {
			t := core.Title{Key: core.EpisodeKey(id, 1, ep), Type: core.TypeEpisode, ShowKey: show.Key, ShowIDs: show.IDs, ShowTitle: show.Title, SeasonTitle: season.Title, Season: 1, Episode: ep, Title: fmt.Sprintf("Episode %d", ep), Released: released, Runtime: 45 * time.Minute}
			t.StillPath = "/fixture.jpg"
			t.PosterPath = "/fixture.jpg"
			t.BackdropPath = "/fixture.jpg"
			if err = cat.UpsertTitle(ctx, t, false); err != nil {
				return err
			}
		}
	}
	users, err := auth.New(ctx, db, auth.Options{Now: now})
	if err != nil {
		return err
	}
	if err = users.SetOwner(ctx, plextv.Account{ID: 1, Username: "owner", Title: "Owner"}, "parity-owner"); err != nil {
		return err
	}
	fs := []plextv.Friend{{PlexID: 2, Username: "adult", Title: "Adult", Token: "parity-adult", Home: true, AllLibs: true}, {PlexID: 3, Username: "teen", Title: "Teen", Token: "parity-teen", Home: true, AllLibs: true}, {PlexID: 4, Username: "child", Title: "Child", Token: "parity-child", Home: true, Restricted: true, AllLibs: true}, {PlexID: 5, Username: "friend", Title: "Friend", Token: "parity-friend", AllLibs: true}}
	if err = users.SyncFriends(ctx, fs); err != nil {
		return err
	}
	list, err := users.Users(ctx)
	if err != nil {
		return err
	}
	if len(list) != 5 {
		return fmt.Errorf("expected five viewers, got %d", len(list))
	}
	st := state.New(db, state.Options{Now: now})
	defer st.Close(ctx)
	prefs := trackprefs.New(db)
	watch := watchlist.New(db, watchlist.Options{Catalog: cat, Now: now})
	if err = watch.SaveSettings(ctx, watchlist.Settings{Enabled: true, CheckHours: 6}); err != nil {
		return err
	}
	for _, u := range list {
		u.AudioLanguages = []string{"en", "fr"}
		u.SubtitleLanguages = []string{"en"}
		if u.Username == "teen" {
			u.MaxContentRating = "PG-13"
			u.AllowedSections = []int{1}
		}
		if u.Username == "child" {
			u.MaxContentRating = "PG"
		}
		if err = prefs.PutRules(ctx, u.ID, trackprefs.Rules{AudioLanguages: []string{"en", "fr"}, SubtitleLanguages: []string{"en"}, SubtitleMode: "always", SubtitleStyle: "normal"}); err != nil {
			return err
		}
		if err = watch.Remind(ctx, &u, core.MovieKey(10022)); err != nil {
			return err
		}
		// Restricted watchlists deliberately exceed a shelf and interleave
		// disallowed titles, as can happen when restrictions are added later.
		if u.Username == "teen" || u.Username == "child" {
			beforeRestriction := u
			beforeRestriction.MaxContentRating = ""
			beforeRestriction.AllowedSections = nil
			for i := int64(23); i <= 60; i++ {
				if err = watch.Remind(ctx, &beforeRestriction, core.MovieKey(10000+i)); err != nil {
					return err
				}
			}
		}
		if err = users.UpdateUser(ctx, u); err != nil {
			return err
		}
		for i := int64(1); i <= 12; i++ {
			if err = st.Scrobble(ctx, u.ID, core.MovieKey(10000+i)); err != nil {
				return err
			}
			if err = st.Rate(ctx, u.ID, core.MovieKey(10000+i), float64(5+i%5)); err != nil {
				return err
			}
		}
		if err = st.Progress(ctx, u.ID, core.MovieKey(10020), 20*time.Minute, 90*time.Minute, "paused"); err != nil {
			return err
		}
		if err = st.Scrobble(ctx, u.ID, core.EpisodeKey(20001, 1, 1)); err != nil {
			return err
		}
		if err = st.Progress(ctx, u.ID, core.EpisodeKey(20001, 1, 2), 10*time.Minute, 45*time.Minute, "paused"); err != nil {
			return err
		}
		if _, err = st.Create(ctx, u.ID, "Fixture playlist", []core.RatingKey{core.MovieKey(10001), core.MovieKey(10002)}); err != nil {
			return err
		}
	}
	overlay := overlays.New(db, overlays.Options{Now: now})
	if err = overlay.SaveSettings(ctx, overlays.Settings{Enabled: true, Ratings: true}); err != nil {
		return err
	}
	tv, err := livetv.New(livetv.Options{DB: db, Catalog: cat, Now: now})
	if err != nil {
		return err
	}
	_, err = tv.SaveChannel(ctx, livetv.Channel{ID: "fixture-channel", Number: "101", Name: "Fixture TV", Kind: "virtual", Enabled: true, Epoch: fixtureNow, Seed: 42, Items: []livetv.Item{{Key: core.MovieKey(10001), Title: "Fixture Movie 001", DurationMS: 5400000}, {Key: core.MovieKey(10002), Title: "Fixture Movie 002", DurationMS: 5400000}}})
	if err == nil {
		_, err = tv.SaveChannel(ctx, livetv.Channel{ID: "fixture-iptv", Number: "102", Name: "Fixture IPTV", Kind: "iptv", Enabled: true, StreamURL: "http://127.0.0.1:32410/index.m3u8"})
	}
	if err == nil {
		_, err = tv.SaveChannel(ctx, livetv.Channel{ID: "fixture-iptv-relay", Number: "103", Name: "Fixture IPTV relay", Kind: "iptv", Enabled: true, Transcode: true, StreamURL: "http://127.0.0.1:32410/index.m3u8"})
	}
	tv.Close()
	if err != nil {
		return err
	}
	cols, err := collections.New(db, cat, nil)
	if err != nil {
		return err
	}
	if _, err = cols.CreateSmart(ctx, core.Collection{Title: "Fixture drama", Filter: &core.Filter{Type: core.TypeMovie, Genres: []string{"Drama"}}}); err != nil {
		return err
	}

	engine, err := personalize.New(personalize.Options{DB: db, Catalog: cat, State: st, Events: st, Users: users, Playlists: st.Playlists(), Now: now, Location: time.UTC, RefreshEvery: -1, RebuildDebounce: time.Hour, Dependencies: func(sc core.PersonalScorer) (core.Recommender, core.CollectionRows, error) {
		c, err := collections.New(db, cat, nil, collections.WithState(st), collections.WithAuth(users), collections.WithScorer(sc))
		if err != nil {
			return nil, nil, err
		}
		return recommend.New(recommend.Options{Catalog: cat, State: st, Scorer: sc}), c, nil
	}})
	if err != nil {
		return err
	}
	defer engine.Close()
	if _, err = engine.SetHomeLayout(ctx, core.HomeLayout{Global: []core.LayoutPage{{Section: "home", MaxRows: 16}}}); err != nil {
		return err
	}
	for _, u := range list {
		if _, err = engine.SetTasteOverrides(ctx, &u, []core.TasteOverride{{Kind: "genre", Value: "Drama", Weight: 0.5}}); err != nil {
			return err
		}
		if err = engine.Refresh(ctx, &u); err != nil {
			return err
		}
		hc := core.HubContext{Device: core.DeviceWeb, Daypart: core.DaypartAfternoon, Now: fixtureNow, ClientID: "fixture"}
		hubs, err := engine.HomeHubsFor(ctx, &u, hc, 20)
		if err != nil {
			return err
		}
		if len(hubs) == 0 {
			return fmt.Errorf("no personalization rows for %s", u.Username)
		}
		engine.RecordImpressions(&u, hc, hubs)
		engine.Flush(ctx)
		var played core.RatingKey
		for _, hub := range hubs {
			for _, item := range hub.Items {
				if (item.Type == core.TypeMovie || item.Type == core.TypeEpisode) && (played == 0 || item.Key < played) {
					played = item.Key
				}
			}
		}
		if played == 0 {
			return fmt.Errorf("no playable impressed item for %s", u.Username)
		}
		// Real progress events flow through state and the personalization
		// subscriber, which attributes the play to the impressed row.
		if err = st.Progress(ctx, u.ID, played, 5*time.Minute, 90*time.Minute, "playing"); err != nil {
			return err
		}
		if err = st.Progress(ctx, u.ID, played, 30*time.Minute, 90*time.Minute, "stopped"); err != nil {
			return err
		}
		deadline := time.NewTimer(10 * time.Second)
		for {
			var count int
			err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM personalize_plays WHERE user_id=? AND item_key=? AND hub_id<>'' AND kind<>'' AND max_fraction>=0.3`, u.ID, played).Scan(&count)
			if err != nil {
				deadline.Stop()
				return err
			}
			if count > 0 {
				deadline.Stop()
				break
			}
			select {
			case <-ctx.Done():
				deadline.Stop()
				return ctx.Err()
			case <-deadline.C:
				return fmt.Errorf("attributed personalization play missing for %s", u.Username)
			case <-time.After(10 * time.Millisecond):
			}
		}
		if err = engine.Refresh(ctx, &u); err != nil {
			return err
		}
	}
	engine.Flush(ctx)
	return nil
}
