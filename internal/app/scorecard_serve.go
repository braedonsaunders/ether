//go:build scorecard && !plexparity

package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/transcode"
)

type ScorecardItem struct {
	Id, SeriesId, Name, Type, Path, Overview, OriginalTitle, OriginalLanguage, OfficialRating string
	ProviderIds                                                                               map[string]string
	IndexNumber, ParentIndexNumber, ProductionYear                                            int
	RunTimeTicks                                                                              int64
	CommunityRating                                                                           float64
	Genres, ProductionLocations, Taglines                                                     []string
	Studios                                                                                   []struct{ Name string }
	People                                                                                    []struct{ Name, Role, Type string }
	UserData                                                                                  struct {
		Played                bool
		PlaybackPositionTicks int64
	}
}

// StartScorecard creates a disposable app and exposes the actual protocol handlers.
// The caller owns the returned cleanup. No Plex identity is registered remotely.
func StartScorecard(ctx context.Context, dir, username, password string, items []ScorecardItem) (map[string]http.Handler, func(), error) {
	return startScorecard(ctx, dir, username, password, items, func(ctx context.Context, src core.MediaSource) (*core.MediaInfo, error) {
		info, err := transcode.ProbeSource(ctx, src, src.Size())
		if !errors.Is(err, transcode.ErrProbeIncomplete) {
			return info, err
		}
		local, ok := src.(*scorecardMedia)
		if !ok {
			return nil, fmt.Errorf("scorecard source is not a local file")
		}
		out, err := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", "-show_chapters", "-show_data", "-i", local.File.Name()).Output()
		if err != nil {
			return nil, fmt.Errorf("scorecard ffprobe: %w", err)
		}
		return transcode.ParseProbe(out)
	})
}

type scorecardProbe func(context.Context, core.MediaSource) (*core.MediaInfo, error)

func prepareScorecardFiles(ctx context.Context, files []ScorecardFile, probe scorecardProbe) error {
	for n := range files {
		entry := &files[n]
		f, e := os.Open(entry.Path)
		if e != nil {
			return e
		}
		st, e := f.Stat()
		if e != nil {
			f.Close()
			return e
		}
		src := &scorecardMedia{File: f, size: st.Size(), id: scorecardReleaseID(*entry)}
		entry.Media, e = probe(ctx, src)
		src.Close()
		if e != nil {
			return e
		}
		if entry.Media == nil {
			return fmt.Errorf("empty probe for scorecard source")
		}
	}
	return nil
}
func startScorecard(ctx context.Context, dir, username, password string, items []ScorecardItem, probe scorecardProbe) (map[string]http.Handler, func(), error) {
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		return nil, nil, fmt.Errorf("scorecard requires a fresh data directory")
	} else if !os.IsNotExist(err) {
		return nil, nil, err
	}
	data, e := os.ReadFile(filepath.Join(dir, "scorecard-files.json"))
	if e != nil {
		return nil, nil, e
	}
	var files []ScorecardFile
	if e = json.Unmarshal(data, &files); e != nil {
		return nil, nil, e
	}
	if e = VerifyScorecardFiles(files); e != nil {
		return nil, nil, e
	}
	if e = prepareScorecardFiles(ctx, files, probe); e != nil {
		return nil, nil, e
	}
	prepared, e := json.Marshal(files)
	if e != nil {
		return nil, nil, e
	}
	if e = os.WriteFile(filepath.Join(dir, "scorecard-files.json"), prepared, 0600); e != nil {
		return nil, nil, e
	}
	cfg, e := config.Open(dir)
	if e != nil {
		return nil, nil, e
	}
	e = cfg.Update(func(c *config.Config) error {
		c.SetupComplete = true
		c.Catalog.TMDBKey = "fixture"
		c.Plex.Token = "fixture-owner-token"
		c.Server.MaxStreams = 100
		c.Frontends = map[string]config.Frontend{"plex": {Enabled: false}}
		for i := range c.Quality.Profiles {
			c.Quality.Profiles[i].MinSizeMBPerMin = 0
			c.Quality.Profiles[i].MaxSizeMBPerMin = 0
		}
		return nil
	})
	if e != nil {
		return nil, nil, e
	}
	a, e := New(ctx, Options{DataDir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if e != nil {
		return nil, nil, e
	}
	fail := func(e error) (map[string]http.Handler, func(), error) { a.Close(); return nil, nil, e }
	if e = a.users.SetOwner(ctx, plextv.Account{ID: 42, Username: "scorecard-owner"}, "fixture-owner-token"); e != nil {
		return fail(e)
	}
	local, e := identity.NewLocal(a.db)
	if e != nil {
		return fail(e)
	}
	viewerID, e := local.CreateViewer(ctx, username, password)
	if e != nil {
		return fail(e)
	}
	if e = a.users.RefreshViewers(ctx); e != nil {
		return fail(e)
	}
	viewer, e := a.users.User(ctx, viewerID)
	if e != nil {
		return fail(e)
	}
	mapped := map[string]bool{}
	preferred := map[int64]ScorecardFile{}
	for _, f := range files {
		mapped[f.Path] = true
		if f.Season == 0 && f.Episode == 0 {
			old, ok := preferred[f.TMDB]
			if !ok || (f.Info.Height > 0 && (old.Info.Height == 0 || f.Info.Height < old.Info.Height)) {
				preferred[f.TMDB] = f
			}
		}
	}
	seriesIDs := map[string]int64{}
	for _, x := range items {
		if x.Type == "Series" {
			id, _ := strconv.ParseInt(x.ProviderIds["Tmdb"], 10, 64)
			if id > 0 {
				seriesIDs[x.Id] = id
			}
		}
	}
	for _, x := range items {
		if (x.Type == "Movie" || x.Type == "Episode") && !mapped[x.Path] {
			continue
		}
		tmdbID, _ := strconv.ParseInt(x.ProviderIds["Tmdb"], 10, 64)
		tvdb, _ := strconv.ParseInt(x.ProviderIds["Tvdb"], 10, 64)
		if x.Type == "Movie" && preferred[tmdbID].Path != x.Path {
			continue
		}
		title := core.Title{Title: x.Name, Summary: x.Overview, IDs: core.IDs{TMDB: tmdbID, IMDB: x.ProviderIds["Imdb"], TVDB: tvdb}, Year: x.ProductionYear, Runtime: time.Duration(x.RunTimeTicks) * 100 * time.Nanosecond, Released: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), PosterPath: "/fixture.jpg", BackdropPath: "/fixture.jpg", StillPath: "/fixture.jpg"}
		title.OriginalTitle, title.Language, title.ContentRating, title.Rating = x.OriginalTitle, x.OriginalLanguage, x.OfficialRating, x.CommunityRating
		title.Genres, title.Countries = x.Genres, x.ProductionLocations
		if len(x.Taglines) > 0 {
			title.Tagline = x.Taglines[0]
		}
		for _, s := range x.Studios {
			title.Studios = append(title.Studios, s.Name)
		}
		for _, p := range x.People {
			person := core.Person{Name: p.Name, Role: p.Role}
			switch p.Type {
			case "Actor":
				title.Cast = append(title.Cast, person)
			case "Director":
				title.Directors = append(title.Directors, person)
			case "Writer":
				title.Writers = append(title.Writers, person)
			}
		}
		switch x.Type {
		case "Movie":
			if tmdbID == 0 {
				continue
			}
			title.Key = core.MovieKey(tmdbID)
			title.Type = core.TypeMovie
		case "Series":
			if tmdbID == 0 {
				continue
			}
			title.Key = core.ShowKey(tmdbID)
			title.Type = core.TypeShow
		case "Season":
			showID := seriesIDs[x.SeriesId]
			if showID == 0 {
				continue
			}
			title.Key = core.SeasonKey(showID, x.IndexNumber)
			title.Type = core.TypeSeason
			title.ShowKey = core.ShowKey(showID)
			title.Season = x.IndexNumber
		case "Episode":
			showID := seriesIDs[x.SeriesId]
			if showID == 0 {
				continue
			}
			title.Key = core.EpisodeKey(showID, x.ParentIndexNumber, x.IndexNumber)
			title.Type = core.TypeEpisode
			title.ShowKey = core.ShowKey(showID)
			title.ShowIDs = core.IDs{TMDB: showID}
			title.Season = x.ParentIndexNumber
			title.Episode = x.IndexNumber
		default:
			continue
		}
		if e = a.catalog.UpsertTitle(ctx, title, true); e != nil {
			return fail(e)
		}
		if x.UserData.Played {
			if e = a.state.Scrobble(ctx, viewer.ID, title.Key); e != nil {
				return fail(e)
			}
		} else if x.UserData.PlaybackPositionTicks > 0 {
			if e = a.state.Progress(ctx, viewer.ID, title.Key, time.Duration(x.UserData.PlaybackPositionTicks)*100*time.Nanosecond, title.Runtime, "stopped"); e != nil {
				return fail(e)
			}
		}
	}
	handlers := map[string]http.Handler{}
	for _, p := range []core.Protocol{"jellyfin", "emby"} {
		inst, err := a.frontends.registry.Mount(ctx, p)
		if err != nil {
			return fail(fmt.Errorf("mount %s: %w", p, err))
		}
		actual := inst.Handler()
		handlers[string(p)] = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/__scorecard/") {
				host, _, err := net.SplitHostPort(r.RemoteAddr)
				ip := net.ParseIP(host)
				if err != nil || ip == nil || !ip.IsLoopback() {
					http.NotFound(w, r)
					return
				}
			}
			if r.URL.Path == "/__scorecard/broker" && r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(a.frontends.broker.ScorecardSessions(r.Context()))
				return
			}
			if r.URL.Path == "/__scorecard/sessions" && r.Method == http.MethodGet {
				type row struct {
					ID             string `json:"id"`
					ReleaseID      string `json:"release_id"`
					ExpectedSHA256 string `json:"expected_sha256"`
					SourceID       string `json:"source_id"`
					ClientID       string `json:"client_id"`
					ItemID         string `json:"item_id"`
				}
				rows := []row{}
				for _, session := range a.playback.Sessions() {
					if session.Resolution == nil || session.Source == nil {
						continue
					}
					id := session.Resolution.Release.ID
					entry, err := (scorecardSource{dir}).lookup(id)
					if err != nil {
						continue
					}
					rows = append(rows, row{ID: session.ID, ReleaseID: id, ExpectedSHA256: entry.SHA256, SourceID: session.Source.ReleaseID(), ClientID: session.ClientID, ItemID: ids.RatingKeyGUID(session.Key)})
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(rows)
				return
			}
			if r.URL.Path == "/__scorecard/verify" && r.Method == http.MethodGet {
				session, ok := a.playback.Session(r.URL.Query().Get("id"))
				if !ok || session.Resolution == nil || session.Source == nil {
					http.NotFound(w, r)
					return
				}
				id := session.Resolution.Release.ID
				entry, err := (scorecardSource{dir}).lookup(id)
				if err != nil {
					http.Error(w, "unknown source", http.StatusConflict)
					return
				}
				h := sha256.New()
				_, err = io.Copy(h, io.NewSectionReader(session.Source, 0, session.Source.Size()))
				if err != nil {
					http.Error(w, "source read failed", http.StatusConflict)
					return
				}
				actualHash := hex.EncodeToString(h.Sum(nil))
				result := struct {
					ReleaseID string `json:"release_id"`
					SourceID  string `json:"source_id"`
					Expected  string `json:"expected_sha256"`
					Actual    string `json:"actual_sha256"`
					Match     bool   `json:"match"`
				}{id, session.Source.ReleaseID(), entry.SHA256, actualHash, actualHash == entry.SHA256 && session.Source.ReleaseID() == id}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(result)
				return
			}
			actual.ServeHTTP(w, r)
		})
	}
	return handlers, a.Close, nil
}
