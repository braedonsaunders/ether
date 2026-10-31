package migrate

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"io"
	"strconv"
	"strings"
	"time"
)

// PreviewCSV accepts Letterboxd diary/ratings/watchlist and Trakt CSV columns.
// Letterboxd's export lacks external IDs, so exact title AND year are required.
func (s *Service) PreviewCSV(ctx context.Context, viewer int64, format, kind string, r io.Reader) (Plan, error) {
	p := Plan{Source: "csv:" + format + ":" + kind, Items: []Item{}, Lists: []List{}, Warnings: []string{}}
	if viewer <= 0 {
		return p, errors.New("viewer required")
	}
	if _, err := s.viewer(ctx, viewer); err != nil {
		return p, errors.New("unknown viewer")
	}
	if format != "letterboxd" && format != "trakt" {
		return p, errors.New("format must be letterboxd or trakt")
	}
	if kind != "history" && kind != "ratings" && kind != "watchlist" {
		return p, errors.New("kind must be history, ratings or watchlist")
	}
	// Read one byte beyond the limit so an oversized file cannot masquerade
	// as EOF and leave the owner with a silently truncated import plan.
	input, e := io.ReadAll(io.LimitReader(r, (16<<20)+1))
	if e != nil {
		return p, e
	}
	if len(input) > 16<<20 {
		return p, errors.New("CSV size limit exceeded")
	}
	rd := csv.NewReader(bytes.NewReader(input))
	header, e := rd.Read()
	if e != nil {
		return p, e
	}
	cols := map[string]int{}
	for i, v := range header {
		cols[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(v, "\ufeff")))] = i
	}
	get := func(row []string, names ...string) string {
		for _, name := range names {
			if i, ok := cols[name]; ok && i < len(row) {
				return strings.TrimSpace(row[i])
			}
		}
		return ""
	}
	for line := 2; line <= 100002; line++ {
		row, e := rd.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return p, fmt.Errorf("CSV row %d: %w", line, e)
		}
		if line == 100002 {
			return p, errors.New("CSV row limit exceeded")
		}
		title := get(row, "name", "title")
		year, _ := strconv.Atoi(get(row, "year"))
		typ := strings.ToLower(get(row, "type"))
		switch typ {
		case "":
			typ = "movie"
		case "series", "tv":
			typ = "show"
		}
		season, _ := strconv.Atoi(get(row, "season", "season_number", "season number"))
		episode, _ := strconv.Atoi(get(row, "episode", "episode_number", "episode number"))
		epTitle := get(row, "episode_title", "episode title")
		if typ == "movie" && (season > 0 || episode > 0) {
			typ = "episode"
		}
		key := s.resolveCSVRow(ctx, row, get, typ, title, year, season, episode)
		if key == 0 {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Row %d unmatched: %s (%d)", line, title, year))
			continue
		}
		rating, _ := strconv.ParseFloat(get(row, "rating"), 64)
		if format == "letterboxd" {
			rating *= 2
		}
		if rating < 0 || rating > 10 {
			return p, fmt.Errorf("CSV row %d rating out of range", line)
		}
		date := get(row, "watched date", "watched_at", "rated_at", "date")
		var at time.Time
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02"} {
			at, e = time.Parse(layout, date)
			if e == nil {
				break
			}
		}
		if at.IsZero() && kind == "history" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("Row %d missing valid watched date", line))
			continue
		}
		itemTitle := title
		if typ == "episode" && epTitle != "" && title != "" && !strings.EqualFold(epTitle, title) {
			itemTitle = epTitle
		}
		it := Item{External: fingerprint([]byte(strings.Join(row, "\x00"))), Key: key, Viewer: viewer, Title: itemTitle, Rating: rating, ViewedAt: at.Unix()}
		if at.IsZero() {
			it.ViewedAt = 0
		}
		if kind == "history" {
			it.Views = 1
		}
		it.Watchlist = kind == "watchlist"
		p.Items = append(p.Items, it)
	}
	return s.savePlan(ctx, p)
}

type guid = struct {
	ID string `xml:"id,attr"`
}

func (s *Service) resolveCSVRow(ctx context.Context, row []string, get func([]string, ...string) string, typ, title string, year, season, episode int) core.RatingKey {
	tmdbID := get(row, "tmdb id", "tmdb_id", "tmdb")
	imdbID := get(row, "imdb id", "imdb_id", "imdb")
	tvdbID := get(row, "tvdb id", "tvdb_id", "tvdb")
	showTMDB := get(row, "show_tmdb_id", "show tmdb id")
	showIMDB := get(row, "show_imdb_id", "show imdb id")
	showTVDB := get(row, "show_tvdb_id", "show tvdb id")
	showTitle := get(row, "show_title", "show title")

	if typ == "episode" {
		if episodeKeyFits(season, episode) {
			for _, id := range []string{showTMDB, tmdbID} {
				if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > 0 && n < 100_000_000 {
					k := core.EpisodeKey(n, season, episode)
					if s.catalog == nil {
						return k
					}
					if _, err := s.catalogGet(ctx, k); err == nil {
						return k
					}
					if _, err := s.catalogGet(ctx, core.ShowKey(n)); err == nil {
						return k
					}
				}
			}
			var showGUIDs []guid
			for _, id := range []string{showIMDB, imdbID} {
				if id != "" {
					showGUIDs = append(showGUIDs, guid{ID: "imdb://" + id})
				}
			}
			for _, id := range []string{showTVDB, tvdbID} {
				if id != "" {
					showGUIDs = append(showGUIDs, guid{ID: "tvdb://" + id})
				}
			}
			if len(showGUIDs) > 0 {
				if showKey, err := s.mapMediaGUIDs(ctx, "", "", media{Type: "show", GUIDs: showGUIDs}); err == nil && showKey.Kind() == core.KindShow && showKey.TMDB() > 0 && showKey.TMDB() < 100_000_000 {
					return core.EpisodeKey(showKey.TMDB(), season, episode)
				}
			}
		}
		var epGUIDs []guid
		if imdbID != "" {
			epGUIDs = append(epGUIDs, guid{ID: "imdb://" + imdbID})
		}
		if tvdbID != "" {
			epGUIDs = append(epGUIDs, guid{ID: "tvdb://" + tvdbID})
		}
		if len(epGUIDs) > 0 {
			if k, err := s.mapMediaGUIDs(ctx, "", "", media{Type: "episode", GUIDs: epGUIDs, Season: season, Episode: episode}); err == nil && k != 0 {
				return k
			}
		}
		if episodeKeyFits(season, episode) && s.catalog != nil {
			for _, st := range []string{showTitle, title} {
				if st == "" {
					continue
				}
				if t, err := s.searchShow(ctx, st, year, season); err == nil && t != nil && t.Key.Kind() == core.KindShow && t.Key.TMDB() > 0 && t.Key.TMDB() < 100_000_000 {
					return core.EpisodeKey(t.Key.TMDB(), season, episode)
				}
			}
		}
		return 0
	}

	m := media{Title: title, Year: year, Type: typ}
	if tmdbID != "" {
		m.GUIDs = append(m.GUIDs, guid{ID: "tmdb://" + tmdbID})
	}
	if imdbID != "" {
		m.GUIDs = append(m.GUIDs, guid{ID: "imdb://" + imdbID})
	}
	if tvdbID != "" {
		m.GUIDs = append(m.GUIDs, guid{ID: "tvdb://" + tvdbID})
	}
	key, _ := s.mapMedia(ctx, "", "", m)
	if key == 0 && s.catalog != nil && title != "" && year > 0 {
		ts, se := s.catalogSearch(ctx, title, []core.MediaType{core.MediaType(typ)}, 20)
		if se == nil {
			for _, t := range ts {
				if strings.EqualFold(t.Title, title) && t.Year == year {
					if key != 0 {
						key = 0
						break
					}
					key = t.Key
				}
			}
		}
	}
	return key
}

func (s *Service) ExportCSV(ctx context.Context, viewer int64, format, kind string, w io.Writer) error {
	if format != "letterboxd" && format != "trakt" {
		return errors.New("invalid format")
	}
	if kind != "history" && kind != "ratings" && kind != "watchlist" {
		return errors.New("invalid kind")
	}
	type entry struct {
		k      core.RatingKey
		rating float64
		at     int64
	}
	entries := []entry{}
	var e error
	if kind == "watchlist" {
		keys, err := watchlist.ExportKeys(ctx, s.db, viewer)
		if err != nil {
			return err
		}
		for _, key := range keys {
			entries = append(entries, entry{k: key})
		}
	} else {
		exported, err := s.state.ExportEntries(ctx, viewer, kind == "ratings")
		if err != nil {
			return err
		}
		for _, v := range exported {
			at := int64(0)
			if !v.At.IsZero() {
				at = v.At.UnixNano()
			}
			entries = append(entries, entry{k: v.Key, rating: v.Rating, at: at})
		}
	}
	wr := csv.NewWriter(w)
	if format == "letterboxd" {
		e = wr.Write([]string{"Date", "Name", "Year", "Rating", "Watched Date", "TMDB ID"})
	} else {
		e = wr.Write([]string{"title", "year", "type", "tmdb_id", "imdb_id", "rating", "watched_at", "season", "episode", "episode_title"})
	}
	if e != nil {
		return e
	}
	for _, v := range entries {
		if s.catalog == nil {
			return errors.New("catalog unavailable")
		}
		t, e := s.catalogGet(ctx, v.k)
		if e != nil {
			return e
		}
		date := ""
		if v.at > 0 {
			date = time.Unix(0, v.at).UTC().Format(time.RFC3339)
		}
		rating := v.rating
		if format == "letterboxd" {
			rating /= 2
			if date != "" {
				date = date[:10]
			}
			e = wr.Write([]string{date, t.Title, strconv.Itoa(t.Year), strconv.FormatFloat(rating, 'f', -1, 64), date, strconv.FormatInt(t.IDs.TMDB, 10)})
		} else {
			rowTitle := t.Title
			rowYear := t.Year
			rowTMDB := t.IDs.TMDB
			rowIMDB := t.IDs.IMDB
			seasonStr := ""
			episodeStr := ""
			epTitle := ""
			if t.Type == core.TypeEpisode || v.k.Kind() == core.KindEpisode {
				epTitle = t.Title
				if t.ShowTitle != "" {
					rowTitle = t.ShowTitle
				}
				if t.ShowYear > 0 {
					rowYear = t.ShowYear
				}
				switch {
				case t.ShowIDs.TMDB > 0:
					rowTMDB = t.ShowIDs.TMDB
				case t.ShowKey.TMDB() > 0:
					rowTMDB = t.ShowKey.TMDB()
				case v.k.TMDB() > 0:
					rowTMDB = v.k.TMDB()
				}
				if rowIMDB == "" && t.ShowIDs.IMDB != "" {
					rowIMDB = t.ShowIDs.IMDB
				}
				season := t.Season
				if season <= 0 {
					season = v.k.Season()
				}
				ep := t.Episode
				if ep <= 0 {
					ep = v.k.Episode()
				}
				if season > 0 {
					seasonStr = strconv.Itoa(season)
				}
				if ep > 0 {
					episodeStr = strconv.Itoa(ep)
				}
			}
			e = wr.Write([]string{rowTitle, strconv.Itoa(rowYear), string(t.Type), strconv.FormatInt(rowTMDB, 10), rowIMDB, strconv.FormatFloat(rating, 'f', -1, 64), date, seasonStr, episodeStr, epTitle})
		}
		if e != nil {
			return e
		}
	}
	wr.Flush()
	return wr.Error()
}
