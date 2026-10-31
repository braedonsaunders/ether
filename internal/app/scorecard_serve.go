//go:build scorecard && !plexparity

package app

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

	"github.com/braedonsaunders/ether/internal/admin"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend/kit/ids"
	"github.com/braedonsaunders/ether/internal/identity"
	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/resolver"
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

// applyScorecardLabBinaries points a disposable fixture at the same ffmpeg
// the reference lab uses. Empty values keep the normal bundled/PATH lookup.
func applyScorecardLabBinaries(c *config.Config) {
	if p := os.Getenv("SCORECARD_FFMPEG"); p != "" {
		c.Transcode.FFmpegPath = p
	}
	if p := os.Getenv("SCORECARD_FFPROBE"); p != "" {
		c.Transcode.FFprobePath = p
	}
}

func scorecardExtraViewers() ([][2]string, error) {
	raw := os.Getenv("SCORECARD_EXTRA_USERS")
	if raw == "" {
		return nil, nil
	}
	var rows []struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal([]byte(raw), &rows); err != nil {
		return nil, fmt.Errorf("scorecard extra viewers")
	}
	out := make([][2]string, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Username == "" || row.Password == "" || seen[row.Username] {
			return nil, fmt.Errorf("scorecard extra viewers")
		}
		seen[row.Username] = true
		out = append(out, [2]string{row.Username, row.Password})
	}
	return out, nil
}

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
		applyScorecardLabBinaries(c)
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
	extras, e := scorecardExtraViewers()
	if e != nil {
		return fail(e)
	}
	for _, extra := range extras {
		if _, e = local.CreateViewer(ctx, extra[0], extra[1]); e != nil {
			return fail(e)
		}
	}
	if e = a.users.RefreshViewers(ctx); e != nil {
		return fail(e)
	}
	// The fixture selects its disposable viewer explicitly; production DLNA
	// keeps its normal opt-in, viewer and LAN authorization requirements.
	if e = a.cfg.Update(func(c *config.Config) error {
		c.Frontends["dlna"] = config.Frontend{Enabled: false, Settings: map[string]any{"viewerId": float64(viewerID), "discovery": false}}
		return nil
	}); e != nil {
		return fail(e)
	}
	// Fixture-only credentials are written to the private data directory for
	// the operator; never place them in logs or the diagnostic HTTP endpoints.
	appGrants, e := identity.NewGrants(a.db, a.users, 30*24*time.Hour, nil)
	if e != nil {
		return fail(e)
	}
	credentials := make(map[string]map[string]string)
	for _, surface := range []string{"webdav", "stremio", "m3u"} {
		secret, err := appGrants.Issue(ctx, viewerID, nil, surface, core.Device{ID: "scorecard-" + surface, Client: surface, Product: surface})
		if err != nil {
			return fail(err)
		}
		credentials[surface] = map[string]string{"username": strconv.FormatInt(viewerID, 10), "password": secret}
	}
	credentialData, e := json.Marshal(credentials)
	if e != nil {
		return fail(e)
	}
	credentialFile, e := os.OpenFile(filepath.Join(dir, "scorecard-app-credentials.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return fail(e)
	}
	_, writeErr := credentialFile.Write(credentialData)
	closeErr := credentialFile.Close()
	if writeErr != nil {
		return fail(writeErr)
	}
	if closeErr != nil {
		return fail(closeErr)
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
	for _, p := range []core.Protocol{"jellyfin", "emby", "silo", "webdav", "stremio", "dlna", "m3u"} {
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
			if r.URL.Path == "/__scorecard/events" && r.Method == http.MethodGet {
				events := scorecardEvents(a.logs.Entries(200, "", ""))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(events)
				return
			}
			if r.URL.Path == "/__scorecard/traces" && r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(scorecardResolverTraces(a.resolver.Traces()))
				return
			}
			if r.URL.Path == "/__scorecard/broker" && r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(a.frontends.broker.ScorecardSessions(r.Context()))
				return
			}
			if r.URL.Path == "/__scorecard/detail-offer" && r.Method == http.MethodGet {
				row, err := scorecardDetailOffer(r.Context(), a.db, dir, p, r.URL.Query().Get("handle"), time.Now())
				if err != nil {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(row)
				return
			}
			if r.URL.Path == "/__scorecard/offer" && r.Method == http.MethodGet {
				// A mapping is evidence only when the handle was first obtained
				// from this viewer's authenticated item-detail response. This
				// fixture-only endpoint does not authorize playback.
				handle := r.URL.Query().Get("handle")
				if len(handle) != 36 {
					http.NotFound(w, r)
					return
				}
				var item int64
				var release string
				err := a.db.QueryRowContext(r.Context(), `SELECT item_id,release_id FROM sessions_version_handles WHERE handle=? AND surface=?`, handle, string(p)).Scan(&item, &release)
				if err != nil {
					http.NotFound(w, r)
					return
				}
				entry, err := (scorecardSource{dir}).lookup(release)
				if err != nil {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(struct {
					Handle         string `json:"handle"`
					ItemID         string `json:"item_id"`
					ReleaseID      string `json:"release_id"`
					ExpectedSHA256 string `json:"expected_sha256"`
				}{handle, ids.RatingKeyGUID(core.RatingKey(item)), release, entry.SHA256})
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

type scorecardEvent struct {
	Time     time.Time `json:"time"`
	Message  string    `json:"message"`
	Category string    `json:"category"`
	Stage    string    `json:"stage,omitempty"`
}

func scorecardEvents(entries []admin.LogEntry) []scorecardEvent {
	allowed := map[string]bool{"prepare failed": true, "release failed to open": true, "probe failed; using estimated media info": true, "ffmpeg failed": true, "start ffmpeg": true, "segment unavailable after restarts": true, "scorecard session stage failure": true, "scorecard profile decision": true}
	out := []scorecardEvent{}
	for _, entry := range entries {
		if !allowed[entry.Message] {
			continue
		}
		category := "other"
		stage := ""
		if entry.Message == "scorecard session stage failure" {
			switch entry.Attrs["stage"] {
			case "plan", "open", "start_stream":
				stage = entry.Attrs["stage"]
			}
			switch entry.Attrs["category"] {
			case "not_available", "not_found", "unauthorized", "stream_limit", "transcode_capacity", "deadline", "canceled":
				category = entry.Attrs["category"]
			}
		}
		if entry.Message == "scorecard profile decision" {
			switch entry.Attrs["category"] {
			case "required_condition_unknown", "direct_rejected", "delivery_rejected", "fallback_transcode", "fallback_remux", "no_delivery":
				category = entry.Attrs["category"]
			}
		}
		if entry.Message != "scorecard profile decision" {
			v := strings.ToLower(entry.Attrs["err"])
			for _, known := range []string{"not available yet", "stream limit reached", "transcode capacity exhausted", "context deadline exceeded", "context canceled", "media source temporarily unavailable"} {
				if strings.Contains(v, known) {
					category = known
					break
				}
			}
		}
		out = append(out, scorecardEvent{Time: entry.Time, Message: entry.Message, Category: category, Stage: stage})
	}
	return out
}

// scorecardResolverTraces exposes fixed rejection categories only; names,
// paths, errors, client data, indexers and arbitrary trace text are excluded.
func scorecardResolverTraces(traces []resolver.Trace) []map[string]any {
	out := make([]map[string]any, 0, len(traces))
	for _, trace := range traces {
		candidates := make([]map[string]any, 0, len(trace.Candidates))
		for _, c := range trace.Candidates {
			reason := "other"
			switch c.Reason {
			case "video codec unsupported by client", "above profile max height", "below profile min height", "dolby vision not allowed for client", "season/episode mismatch", "quality excluded by imported profile":
				reason = c.Reason
			case "":
				reason = "none"
			}
			row := map[string]any{"rejected": c.Rejected, "reason": reason, "attempted": c.Attempted, "health_checked": c.HealthChecked, "healthy": c.Healthy}
			if strings.HasPrefix(c.ReleaseID, "scorecard-") {
				_, digest, found := strings.Cut(c.ReleaseID, "-")
				if found {
					parts := strings.Split(digest, "-")
					last := parts[len(parts)-1]
					decoded, err := hex.DecodeString(last)
					if err == nil && len(decoded) == sha256.Size {
						row["fixture_sha256"] = strings.ToLower(last)
					}
				}
			}
			candidates = append(candidates, row)
		}
		out = append(out, map[string]any{"item_id": ids.RatingKeyGUID(trace.TitleKey), "at": trace.At, "winner_present": trace.Winner != "", "error_present": trace.Err != "", "candidates": candidates})
	}
	return out
}

// This fixture-only join is evidence, not authorization: the caller must first
// obtain the handle from an authenticated detail response and verify the actual
// opened broker session and source bytes independently.
type scorecardDetailOfferRow struct {
	Handle         string `json:"handle"`
	ItemID         string `json:"item_id"`
	ReleaseID      string `json:"release_id"`
	Revision       string `json:"revision"`
	ExpectedSHA256 string `json:"expected_sha256"`
}

func scorecardDetailOffer(ctx context.Context, db *sql.DB, dir string, surface core.Protocol, handle string, now time.Time) (scorecardDetailOfferRow, error) {
	var row scorecardDetailOfferRow
	if len(handle) != 36 {
		return row, core.ErrNotFound
	}
	var key int64
	var release, revision string
	err := db.QueryRowContext(ctx, `SELECT item_id,release_id,revision FROM sessions_detail_sources WHERE handle=? AND surface=? AND expires>?`, handle, string(surface), now.UnixMilli()).Scan(&key, &release, &revision)
	if err != nil {
		return row, core.ErrNotFound
	}
	if !strings.HasPrefix(revision, "src-") || len(revision) != 36 {
		return row, core.ErrNotFound
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(revision, "src-"))
	if err != nil || len(raw) != 16 {
		return row, core.ErrNotFound
	}
	file, err := (scorecardSource{dir}).lookup(release)
	if err != nil {
		return row, core.ErrNotFound
	}
	return scorecardDetailOfferRow{Handle: handle, ItemID: ids.RatingKeyGUID(core.RatingKey(key)), ReleaseID: release, Revision: revision, ExpectedSHA256: file.SHA256}, nil
}
