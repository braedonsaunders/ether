package app

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/braedonsaunders/ether/internal/collections"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/favorites"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/mdblist"
	"github.com/braedonsaunders/ether/internal/notinterested"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/recommend"
	"github.com/braedonsaunders/ether/internal/simkl"
	"github.com/braedonsaunders/ether/internal/state"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/subtitles"
	"github.com/braedonsaunders/ether/internal/trakt"
)

func (a *App) buildPersonalization(ctx context.Context, cfg config.Config) error {
	log := a.log
	dataDir := a.opts.DataDir
	var err error
	// Watch state and recommendations. Default the account-sync journal gate
	// closed until buildPlexWatch installs per-viewer consent checks, so a
	// server without Plex never accumulates undeliverable journal rows.
	a.state = state.New(a.db, state.Options{Now: parityClock(), Logger: log.With("component", "state"), Availability: a.dbAvail, PendingSQLite: store.DialectOf(a.db) == store.SQLite})
	a.state.SetJournalGate(func(context.Context, *sql.Tx, int64) (int64, bool, error) { return 0, false, nil })
	basePlaylists := a.state.Playlists()
	marked, ok := basePlaylists.(core.TransactionalMarkedPlaylists)
	if !ok {
		return fmt.Errorf("favorites: marked playlists unavailable")
	}
	libs := []experience.Library{{ID: 1, Name: "Movies", Type: core.TypeMovie}, {ID: 2, Name: "TV Shows", Type: core.TypeShow}}
	visibility := experienceVisibility(a.safety, a.catalog.Get, func(ctx context.Context, titles []core.Title) []core.Title {
		if a.availability == nil {
			return titles
		}
		return a.availability.FilterDiscovery(ctx, titles)
	}, func() []*core.PlaySession {
		if a.playback == nil {
			return nil
		}
		return a.playback.Sessions()
	}, libs)
	a.favorites, err = favorites.New(favorites.Options{DB: a.db, Marked: marked, Active: a.users.ActiveUser,
		Allowed: func(ctx context.Context, u *core.User, subject favorites.Subject) bool {
			return a.favoriteAllowed(ctx, u, subject, visibility)
		},
		AllowedTitles: func(ctx context.Context, u *core.User, keys []core.RatingKey) (bool, error) {
			return a.favoriteTitlesCached(ctx, u, keys, visibility)
		},
		PreloadTitles: a.catalog.PreloadStored,
		Changed:       func(ctx context.Context, id int64, subject favorites.Subject) { a.favoriteChanged(ctx, id, subject) },
	})
	if err != nil {
		return fmt.Errorf("favorites: %w", err)
	}
	a.notInterested, err = notinterested.New(notinterested.Options{DB: a.db, Active: a.users.ActiveUser,
		Allowed: func(ctx context.Context, u *core.User, key core.RatingKey) bool {
			return a.favoriteAllowed(ctx, u, favorites.Subject{Kind: favorites.KindTitle, ID: key.String()}, visibility)
		},
		Changed: a.notInterestedChanged,
	})
	if err != nil {
		return fmt.Errorf("not interested: %w", err)
	}
	a.playlists, err = favorites.NewPlaylists(basePlaylists, a.favorites)
	if err != nil {
		return fmt.Errorf("favorites playlists: %w", err)
	}
	a.closers = append(a.closers, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.state.Close(ctx); err != nil {
			a.log.Warn("state: watch progress lost at shutdown", "err", err)
		}
	})
	a.trakt = trakt.New(trakt.Options{Config: cfg.Trakt, DB: a.db, Sealer: a.cfg, Catalog: a.catalog, NodeID: a.clusterNodeID(), Logger: log.With("component", "trakt")})
	a.trakt.SubscribeTo(a.state)
	a.simklDevice = &simkl.DeviceManager{AuthConfig: a.simklAuth, Tokens: simkl.TokenStore{DB: a.db, Sealer: a.cfg}}
	unsubSimkl := a.state.Subscribe(func(ev state.Event) {
		if !a.cfg.Get().Simkl.Enabled {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := simkl.Enqueue(ctx, a.db, ev); err != nil {
			a.log.Warn("simkl: enqueue failed", "err", err)
		}
	})
	a.closers = append(a.closers, unsubSimkl)
	a.mdblistTokens = &mdblist.TokenStore{DB: a.db, Sealer: a.cfg}
	unsubMDBList := a.state.Subscribe(func(ev state.Event) {
		if !a.cfg.Get().MDBList.Enabled {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := mdblist.Enqueue(ctx, a.db, ev); err != nil {
			a.log.Warn("mdblist: enqueue failed", "err", err)
		}
	})
	a.closers = append(a.closers, unsubMDBList)
	a.closers = append(a.closers, a.trakt.Close)
	a.llm = llm.New(llm.ServiceOptions{
		Client:  llm.NewClient(llm.ClientOptions{Config: cfg.LLM, Logger: log.With("component", "llm")}),
		Catalog: a.catalog,
		Logger:  log.With("component", "llm"),
	})
	// Per-viewer Homes, built in the background and served to Plex.
	if a.personalize, err = personalize.New(personalize.Options{
		PersonalWatchlist: func(ctx context.Context, u *core.User) ([]core.Title, error) {
			if a.watchlist == nil {
				return nil, nil
			}
			items, err := a.watchlist.Items(ctx, u)
			titles := make([]core.Title, 0, len(items))
			for _, item := range items {
				titles = append(titles, item.Title)
			}
			return titles, err
		},
		PersonalCollections: func(ctx context.Context, u *core.User, key core.RatingKey) ([]core.Title, error) {
			if a.collections == nil {
				return nil, nil
			}
			page, err := a.collections.Items(ctx, u, key, 0, 500)
			return page.Items, err
		},
		Now:          parityClock(),
		RefreshEvery: parityRefreshEvery(),
		Ranker:       parityRankerOptions(),
		DB:           a.db, Catalog: a.catalog, State: a.state, Events: a.state, Users: a.users,
		FilterAvailable: func(ctx context.Context, ts []core.Title) []core.Title {
			ts = a.catalog.FilterVisibility(ctx, ts)
			if a.availability == nil {
				return ts
			}
			return a.availability.FilterDiscovery(ctx, ts)
		},
		CatalogRulesRevision: a.catalog.ViewerRulesRevision,
		UserPolicy:           a.safety.UserPolicy, Allowed: a.safety.Allowed,
		LLM: a.llm, Trakt: a.trakt, Playlists: a.playlists, Favorites: a.favoriteSignals, NotInterested: a.notInterestedSignals, NotInterestedSnapshot: a.notInterestedSnapshot,
		ExtraHomeRows: a.extraHomeRows,
		Dependencies: func(scorer core.PersonalScorer) (core.Recommender, core.CollectionRows, error) {
			opts := append(a.collectionOptions(cfg), collections.WithState(a.state), collections.WithAuth(a.users), collections.WithScorer(scorer))
			var err error
			a.collections, err = collections.New(a.db, a.catalog, log.With("component", "collections"), opts...)
			if err != nil {
				return nil, nil, fmt.Errorf("collections: %w", err)
			}
			a.wireCatalogueSources()
			a.recommend = recommend.New(recommend.Options{Catalog: a.catalog, State: a.state, LLM: a.llm, Scorer: scorer, Logger: log.With("component", "recommend")})
			return a.recommend, a.collections, nil
		},
		ServerName: func() string { return a.cfg.Read().Server.FriendlyName },
		Logger:     log.With("component", "personalize"),
		OnBuilt:    a.refreshPersonalHomes,
		// Background training waits while anyone is streaming.
		Busy: func() bool {
			if a.playback != nil && len(a.playback.Sessions()) > 0 {
				return true
			}
			return a.livetv != nil && len(a.livetv.ViewingSessions()) > 0
		},
		// Netflix-style hooks (#31/#32) follow the owner's live switches.
		IntentEnabled: func() bool { return a.cfg.Read().Personalize.SessionIntent },
		ClockFormat: func() string {
			c := a.cfg.Get()
			if c.Personalize.ClockFormat != "" {
				return c.Personalize.ClockFormat
			}
			return c.Catalog.Language
		},
	}); err != nil {
		return fmt.Errorf("personalization: %w", err)
	}
	// One-time: Home layout schedules for collection rows move onto their
	// collections, which own them now.
	if a.collections != nil {
		if err := a.collections.AdoptLayoutSchedules(ctx, a.personalize); err != nil {
			a.log.Warn("collections: could not move Home layout schedules onto collections; retrying next start", "err", err)
		}
	}
	if err := parityWarmPersonalization(ctx, a); err != nil {
		a.personalize.Close()
		return err
	}
	a.closers = append(a.closers, a.personalize.Close)
	a.subtitles = subtitles.New(subtitles.Options{
		Config:    cfg.OpenSubtitles,
		SubDL:     cfg.SubDL,
		Providers: cfg.Subtitles.Providers,
		CacheDir:  filepath.Join(dataDir, "subtitles"),
		Logger:    log.With("component", "subtitles"),
		UserAgent: "Ether v" + a.opts.Version,
	})

	return nil
}
