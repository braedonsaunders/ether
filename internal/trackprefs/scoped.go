package trackprefs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/braedonsaunders/ether/internal/store"
)

func init() {
	store.Register("trackprefs", 2, `CREATE TABLE trackprefs_scoped(user_id INTEGER NOT NULL, scope TEXT NOT NULL, scope_id TEXT NOT NULL, field TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(user_id,scope,scope_id,field));`)
}

// ScopedRules only changes fields explicitly supplied; nil means inherit.
type ScopedRules struct {
	AudioLanguages    *[]string `json:"audioLanguages,omitempty"`
	SubtitleLanguages *[]string `json:"subtitleLanguages,omitempty"`
	SubtitleMode      *string   `json:"subtitleMode,omitempty"`
	MaxBitrateKbps    *int      `json:"maxBitrateKbps,omitempty"`
	MaxHeight         *int      `json:"maxHeight,omitempty"`
	PreferredQuality  *string   `json:"preferredQuality,omitempty"`
}

// Scope identifies the authenticated device and the title's series and library.
type Scope struct{ Device, Series, Library string }

// DeleteScopedField clears one override without disturbing concurrent writes
// to the other fields in the same scope.
func (s *Service) DeleteScopedField(ctx context.Context, user int64, scope, id, field string) error {
	if user <= 0 || (scope != "device" && scope != "series" && scope != "library") || id == "" || len(id) > 128 || strings.ContainsAny(id, "\x00\r\n") || (field != "audio" && field != "subtitle" && field != "subtitle_mode" && field != "bitrate" && field != "height") {
		return errors.New("invalid scoped track preference")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM trackprefs_scoped WHERE user_id=? AND scope=? AND scope_id=? AND field=?`, user, scope, id, field)
	return err
}

func (s *Service) PutScoped(ctx context.Context, user int64, scope, id string, value ScopedRules) error {
	if user <= 0 || (scope != "device" && scope != "series" && scope != "library") || id == "" || len(id) > 128 || strings.ContainsAny(id, "\x00\r\n") {
		return errors.New("invalid scoped track preference")
	}
	check := Default()
	if value.AudioLanguages != nil {
		check.AudioLanguages = *value.AudioLanguages
	}
	if value.SubtitleLanguages != nil {
		check.SubtitleLanguages = *value.SubtitleLanguages
	}
	if value.SubtitleMode != nil {
		check.SubtitleMode = *value.SubtitleMode
	}
	if value.MaxBitrateKbps != nil {
		check.MaxBitrateKbps = *value.MaxBitrateKbps
	}
	if value.MaxHeight != nil {
		check.MaxHeight = *value.MaxHeight
	}
	if value.PreferredQuality != nil {
		check.PreferredQuality = *value.PreferredQuality
	}
	if (value.MaxHeight == nil) != (value.PreferredQuality == nil) {
		return errors.New("incomplete quality preference")
	}
	if err := check.Validate(); err != nil {
		return err
	}
	if value.AudioLanguages == nil && value.SubtitleLanguages == nil && value.SubtitleMode == nil && value.MaxBitrateKbps == nil && value.MaxHeight == nil {
		_, err := s.db.ExecContext(ctx, `DELETE FROM trackprefs_scoped WHERE user_id=? AND scope=? AND scope_id=?`, user, scope, id)
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	fields := map[string]any{}
	if value.AudioLanguages != nil {
		fields["audio"] = *value.AudioLanguages
	}
	if value.SubtitleLanguages != nil {
		fields["subtitle"] = *value.SubtitleLanguages
	}
	if value.SubtitleMode != nil {
		fields["subtitle_mode"] = *value.SubtitleMode
	}
	if value.MaxBitrateKbps != nil {
		fields["bitrate"] = *value.MaxBitrateKbps
	}
	if value.MaxHeight != nil {
		quality := ""
		if value.PreferredQuality != nil {
			quality = *value.PreferredQuality
		}
		fields["height"] = struct {
			Height  int    `json:"height"`
			Quality string `json:"quality"`
		}{*value.MaxHeight, quality}
	}
	for _, field := range []string{"audio", "subtitle", "subtitle_mode", "bitrate", "height"} {
		v, ok := fields[field]
		if !ok {
			continue
		}
		raw, e := json.Marshal(v)
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO trackprefs_scoped(user_id,scope,scope_id,field,value) VALUES(?,?,?,?,?) ON CONFLICT(user_id,scope,scope_id,field) DO UPDATE SET value=excluded.value`, user, scope, id, field, string(raw)); e != nil {
			return e
		}
	}
	return tx.Commit()
}

// ScopedRules returns the effective rules and whether any override applied.
// Each field inherits independently, with device taking highest precedence.
func (s *Service) ScopedRules(ctx context.Context, user int64, scope Scope) (Rules, bool, error) {
	r, sources, err := s.ScopedRulesWithSources(ctx, user, scope)
	for _, source := range sources {
		if source != "profile" {
			return r, true, err
		}
	}
	return r, false, err
}

// ScopedRulesWithSources also identifies the winning scoped override for each field.
func (s *Service) ScopedRulesWithSources(ctx context.Context, user int64, scope Scope) (Rules, map[string]string, error) {
	r, err := s.GetRules(ctx, user)
	if err != nil {
		return r, nil, err
	}
	sources := map[string]string{}
	var hasProfile int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM trackprefs_rules WHERE user_id=?`, user).Scan(&hasProfile); err != nil {
		return r, nil, err
	}
	if hasProfile != 0 {
		for _, field := range []string{"audio", "subtitle", "subtitle_mode", "bitrate", "height"} {
			sources[field] = "profile"
		}
	}
	for _, entry := range []struct{ kind, id string }{{"library", scope.Library}, {"series", scope.Series}, {"device", scope.Device}} {
		if entry.id == "" {
			continue
		}
		rows, e := s.db.QueryContext(ctx, `SELECT field,value FROM trackprefs_scoped WHERE user_id=? AND scope=? AND scope_id=?`, user, entry.kind, entry.id)
		if e != nil {
			return r, nil, e
		}
		for rows.Next() {
			var field, raw string
			if e = rows.Scan(&field, &raw); e != nil {
				rows.Close()
				return r, nil, e
			}
			switch field {
			case "audio":
				e = json.Unmarshal([]byte(raw), &r.AudioLanguages)
				r.PreferOriginal = false
			case "subtitle":
				e = json.Unmarshal([]byte(raw), &r.SubtitleLanguages)
			case "subtitle_mode":
				e = json.Unmarshal([]byte(raw), &r.SubtitleMode)
			case "bitrate":
				e = json.Unmarshal([]byte(raw), &r.MaxBitrateKbps)
			case "height":
				var quality struct {
					Height  int    `json:"height"`
					Quality string `json:"quality"`
				}
				e = json.Unmarshal([]byte(raw), &quality)
				r.MaxHeight, r.PreferredQuality = quality.Height, quality.Quality
			default:
				e = errors.New("unknown scoped track preference")
			}
			if e != nil {
				rows.Close()
				return r, nil, e
			}
			sources[field] = entry.kind
		}
		if e = rows.Err(); e != nil {
			rows.Close()
			return r, nil, e
		}
		rows.Close()
	}
	return r, sources, nil
}

// LimitBitrate can only tighten an existing client cap; the transcoder still
// applies the independent owner, safety, and remote limits.
func LimitBitrate(existing, preference int) int {
	if preference > 0 && (existing <= 0 || preference < existing) {
		return preference
	}
	return existing
}
