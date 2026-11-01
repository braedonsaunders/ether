package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/plextv"
	"github.com/braedonsaunders/ether/internal/sharefilter"
	"github.com/braedonsaunders/ether/internal/store"
	"github.com/braedonsaunders/ether/internal/viewerpolicy"
)

func init() {
	store.Register("auth", 8, `
CREATE TABLE auth_plex_defaults (
	user_id INTEGER PRIMARY KEY,
	plex_id INTEGER NOT NULL,
	payload TEXT NOT NULL,
	updated_at INTEGER NOT NULL
);`)
}

// PlexViewerDefaults is the cached plex.tv grant for one Ether viewer.
// It is a default layer only: owner Ether policy and the viewer's own
// picks always win.
type PlexViewerDefaults struct {
	AudioLanguage    string `json:"audioLanguage,omitempty"`
	AutoSelectAudio  bool   `json:"autoSelectAudio,omitempty"`
	SubtitleLanguage string `json:"subtitleLanguage,omitempty"`
	SubtitleMode     int    `json:"subtitleMode,omitempty"`
	ForcedOnly       bool   `json:"forcedOnly,omitempty"`
	HasProfile       bool   `json:"hasProfile,omitempty"`
	FilterMovies     string `json:"filterMovies,omitempty"`
	FilterTelevision string `json:"filterTelevision,omitempty"`
	AllowSync        *bool  `json:"allowSync,omitempty"`
	HasSettings      bool   `json:"hasSettings,omitempty"`
}

// EtherMode is the Ether subtitle mode for this cached plex.tv profile.
func (d PlexViewerDefaults) EtherMode() string {
	return plextv.TrackDefaults{SubtitleMode: d.SubtitleMode, ForcedOnly: d.ForcedOnly}.EtherMode()
}

func defaultsFromFriend(f plextv.Friend) PlexViewerDefaults {
	return PlexViewerDefaults{
		AudioLanguage:    f.Track.AudioLanguage,
		AutoSelectAudio:  f.Track.AutoSelectAudio,
		SubtitleLanguage: f.Track.SubtitleLanguage,
		SubtitleMode:     f.Track.SubtitleMode,
		ForcedOnly:       f.Track.ForcedOnly,
		HasProfile:       f.Track.HasProfile,
		FilterMovies:     f.Share.FilterMovies,
		FilterTelevision: f.Share.FilterTelevision,
		AllowSync:        f.Share.AllowSync,
		HasSettings:      f.Share.HasSettings,
	}
}

func defaultsFromAccount(a plextv.Account) PlexViewerDefaults {
	return PlexViewerDefaults{
		AudioLanguage:    a.Track.AudioLanguage,
		AutoSelectAudio:  a.Track.AutoSelectAudio,
		SubtitleLanguage: a.Track.SubtitleLanguage,
		SubtitleMode:     a.Track.SubtitleMode,
		ForcedOnly:       a.Track.ForcedOnly,
		HasProfile:       a.Track.HasProfile,
	}
}

func (d PlexViewerDefaults) empty() bool {
	return !d.HasProfile && !d.HasSettings && d.AudioLanguage == "" && d.SubtitleLanguage == "" &&
		d.FilterMovies == "" && d.FilterTelevision == "" && d.AllowSync == nil
}

func mergePlexDefaults(have, incoming PlexViewerDefaults) PlexViewerDefaults {
	if incoming.HasProfile {
		have.AudioLanguage = incoming.AudioLanguage
		have.AutoSelectAudio = incoming.AutoSelectAudio
		have.SubtitleLanguage = incoming.SubtitleLanguage
		have.SubtitleMode = incoming.SubtitleMode
		have.ForcedOnly = incoming.ForcedOnly
		have.HasProfile = true
	}
	if incoming.HasSettings {
		have.FilterMovies = incoming.FilterMovies
		have.FilterTelevision = incoming.FilterTelevision
		have.AllowSync = incoming.AllowSync
		have.HasSettings = true
	}
	return have
}

func upsertPlexDefaultsTx(ctx context.Context, tx *sql.Tx, userID, plexID, now int64, d PlexViewerDefaults) error {
	if userID <= 0 || d.empty() {
		return nil
	}
	var haveID int64
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT plex_id,payload FROM auth_plex_defaults WHERE user_id=?`, userID).Scan(&haveID, &raw)
	if err == nil {
		var have PlexViewerDefaults
		if json.Unmarshal([]byte(raw), &have) == nil {
			d = mergePlexDefaults(have, d)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if haveID == plexID && raw == string(body) {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO auth_plex_defaults(user_id,plex_id,payload,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(user_id) DO UPDATE SET plex_id=excluded.plex_id, payload=excluded.payload, updated_at=excluded.updated_at`,
		userID, plexID, string(body), now)
	return err
}

func (s *Service) loadPlexDefaults(ctx context.Context) (map[int64]PlexViewerDefaults, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id,payload FROM auth_plex_defaults`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]PlexViewerDefaults{}
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var d PlexViewerDefaults
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return nil, err
		}
		out[id] = d
	}
	return out, rows.Err()
}

// PlexDefaults returns the cached plex.tv layer for a local viewer id.
func (s *Service) PlexDefaults(userID int64) (PlexViewerDefaults, bool) {
	if s == nil || userID <= 0 {
		return PlexViewerDefaults{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, ok := s.plexDefaults[userID]
	return d, ok
}

// PlexDefaultsHandler serves GET /{id} as the read-only plex.tv layer
// shown in the owner console.
func (s *Service) PlexDefaultsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(strings.Trim(r.URL.Path, "/"), 10, 64)
		if err != nil || id <= 0 || r.Method != http.MethodGet {
			http.Error(w, "invalid viewer", http.StatusBadRequest)
			return
		}
		d, ok := s.PlexDefaults(id)
		if !ok {
			d = PlexViewerDefaults{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"audioLanguage":         d.AudioLanguage,
			"autoSelectAudio":       d.AutoSelectAudio,
			"subtitleLanguage":      d.SubtitleLanguage,
			"subtitleMode":          d.EtherMode(),
			"plexSubtitleMode":      d.SubtitleMode,
			"forcedOnly":            d.ForcedOnly,
			"hasProfile":            d.HasProfile,
			"filterMovies":          d.FilterMovies,
			"filterTelevision":      d.FilterTelevision,
			"filterMoviesParts":     sharefilter.Format(d.FilterMovies),
			"filterTelevisionParts": sharefilter.Format(d.FilterTelevision),
			"allowSync":             d.AllowSync,
			"hasSettings":           d.HasSettings,
			"labelNote":             "TMDB collection name only. Titles without a collection name are not hidden.",
		})
	})
}

func plexDefaultsEqual(a, b map[int64]PlexViewerDefaults) bool {
	if len(a) != len(b) {
		return false
	}
	for id, left := range a {
		right, ok := b[id]
		if !ok {
			return false
		}
		la, _ := json.Marshal(left)
		lb, _ := json.Marshal(right)
		if string(la) != string(lb) {
			return false
		}
	}
	return true
}

func (s *Service) bumpViewerPolicy(ctx context.Context) {
	_ = viewerpolicy.Bump(ctx, s.db)
}
