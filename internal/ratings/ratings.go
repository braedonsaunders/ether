// Package ratings caches licensed, owner-keyed rating data. Values preserve
// their source scale; Plex's main rating fields alone use the 0–10 scale.
package ratings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/store"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const Redacted = "[configured]"

func init() {
	store.Register("ratings", 1, `CREATE TABLE ratings_settings(id INTEGER PRIMARY KEY CHECK(id=1),value TEXT NOT NULL);CREATE TABLE ratings_cache(item_key INTEGER NOT NULL,provider TEXT NOT NULL,value TEXT NOT NULL,updated INTEGER NOT NULL,PRIMARY KEY(item_key,provider));`)
}

type Settings struct {
	Enabled    bool   `json:"enabled"`
	Provider   string `json:"provider"`
	APIKey     string `json:"apiKey"`
	Critic     string `json:"critic"`
	Audience   string `json:"audience"`
	CacheHours int    `json:"cacheHours"`
}
type Badge struct {
	Source string  `json:"source"`
	Value  float64 `json:"value"`
	Scale  float64 `json:"scale"`
	Votes  int64   `json:"votes,omitempty"`
}

func (b Badge) PlexValue() float64 {
	if b.Scale <= 0 {
		return 0
	}
	return math.Round(b.Value/b.Scale*100) / 10
}

// PlexImage returns only icons understood by official Plex clients. Other
// providers remain explicitly named poster badges; they never impersonate IMDb.
func (b Badge) PlexImage() string {
	switch b.Source {
	case "imdb":
		return "imdb://image.rating"
	case "tomatoes":
		if b.Value/b.Scale >= .6 {
			return "rottentomatoes://image.rating.ripe"
		}
		return "rottentomatoes://image.rating.rotten"
	case "popcorn":
		if b.Value/b.Scale >= .6 {
			return "rottentomatoes://image.rating.upright"
		}
		return "rottentomatoes://image.rating.spilled"
	}
	return ""
}

type Options struct {
	Client         *http.Client
	OMDbBaseURL    string
	MDBListBaseURL string
	Now            func() time.Time
	// Sealer protects the stored API key, bound to its provider; nil
	// stores it as given.
	Sealer config.FieldSealer
}

// refreshCall is one in-flight Refresh fetch for a title (singleflight,
// same pattern as resolver.inflight). done is closed after badges/err are
// set, so waiters reading them after <-done are race-free.
type refreshKey struct {
	key       core.RatingKey
	settings  Settings
	imdb      string
	tmdb      int64
	titleType core.MediaType
}

type refreshCall struct {
	done      chan struct{}
	badges    []Badge
	err       error
	abandoned bool // the leader's own context ended the fetch
}

type Service struct {
	db       *sql.DB
	o        Options
	flightMu sync.Mutex
	flights  map[refreshKey]*refreshCall
	queue    chan core.Title
	qmu      sync.Mutex
	queued   map[core.RatingKey]bool
	retryAt  map[core.RatingKey]time.Time
}

func New(db *sql.DB, o Options) *Service {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Client == nil {
		o.Client = egress.Client(egress.CapMetadata, 15*time.Second)
		o.Client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	}
	if o.OMDbBaseURL == "" {
		o.OMDbBaseURL = "https://www.omdbapi.com/"
	}
	if o.MDBListBaseURL == "" {
		o.MDBListBaseURL = "https://api.mdblist.com"
	}
	return &Service{db: db, o: o, flights: map[refreshKey]*refreshCall{}, queue: make(chan core.Title, 128), queued: map[core.RatingKey]bool{}, retryAt: map[core.RatingKey]time.Time{}}
}
func (s *Service) settings(ctx context.Context) (Settings, error) {
	if snap, ok := ctx.Value(settingsSnapshotKey{s}).(settingsSnapshot); ok {
		return snap.value, snap.err
	}
	v := Settings{Provider: "omdb", Critic: "tomatoes", Audience: "imdb", CacheHours: 168}
	var raw string
	e := s.db.QueryRowContext(ctx, `SELECT value FROM ratings_settings WHERE id=1`).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return v, nil
	}
	if e != nil {
		return v, e
	}
	if e = json.Unmarshal([]byte(raw), &v); e != nil {
		return v, e
	}
	if v.APIKey, _, e = config.OpenWith(s.o.Sealer, v.APIKey, apiKeyContext(v.Provider)...); e != nil {
		return v, fmt.Errorf("ratings: stored API key: %w", e)
	}
	return v, nil
}

// apiKeyContext binds the stored key to the provider it is sent to.
func apiKeyContext(provider string) []string {
	return []string{"ratings_settings", "apiKey", "1", provider}
}

// encodeSettings is the stored form of v, with the API key sealed.
func (s *Service) encodeSettings(v Settings) (string, error) {
	sealed, err := config.SealWith(s.o.Sealer, v.APIKey, apiKeyContext(v.Provider)...)
	if err != nil {
		return "", fmt.Errorf("ratings: seal API key: %w", err)
	}
	v.APIKey = sealed
	raw, err := json.Marshal(v)
	return string(raw), err
}

// MigrateSecrets seals an API key stored in plaintext; it returns 1 when
// it rewrote the settings. The row is replaced only if unchanged since
// read, and a key that does not open is left untouched.
func (s *Service) MigrateSecrets(ctx context.Context) (int, error) {
	if s.db == nil || s.o.Sealer == nil {
		return 0, nil
	}
	var raw string
	e := s.db.QueryRowContext(ctx, `SELECT value FROM ratings_settings WHERE id=1`).Scan(&raw)
	if errors.Is(e, sql.ErrNoRows) {
		return 0, nil
	}
	if e != nil {
		return 0, e
	}
	var v Settings
	if e = json.Unmarshal([]byte(raw), &v); e != nil {
		return 0, e
	}
	plain, legacy, e := config.OpenWith(s.o.Sealer, v.APIKey, apiKeyContext(v.Provider)...)
	if e != nil {
		return 0, fmt.Errorf("ratings: stored API key left unchanged: %w", e)
	}
	if !legacy {
		return 0, nil
	}
	v.APIKey = plain
	next, e := s.encodeSettings(v)
	if e != nil || next == raw {
		return 0, e
	}
	res, e := s.db.ExecContext(ctx, `UPDATE ratings_settings SET value=? WHERE id=1 AND value=?`, next, raw)
	if e != nil {
		return 0, e
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	v, e := s.settings(ctx)
	if v.APIKey != "" {
		v.APIKey = Redacted
	}
	return v, e
}
func (s *Service) SaveSettings(ctx context.Context, v Settings) error {
	if v.Provider != "omdb" && v.Provider != "mdblist" {
		return errors.New("provider must be omdb or mdblist")
	}
	if v.CacheHours < 1 || v.CacheHours > 2160 {
		return errors.New("cache interval must be 1–2160 hours")
	}
	if v.APIKey == Redacted {
		old, e := s.settings(ctx)
		if e != nil {
			return e
		}
		// A key belongs to its provider: never send it to the other one.
		if old.APIKey != "" && old.Provider != v.Provider {
			return errors.New("enter the API key for the new ratings provider")
		}
		v.APIKey = old.APIKey
	}
	for _, src := range []string{v.Critic, v.Audience} {
		if src != "" && src != "imdb" && src != "tomatoes" && src != "popcorn" {
			return errors.New("Plex rating slots support imdb, tomatoes or popcorn; other ratings are poster badges")
		}
	}
	if v.Enabled && strings.TrimSpace(v.APIKey) == "" {
		return errors.New("an owner API key is required")
	}
	raw, e := s.encodeSettings(v)
	if e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO ratings_settings(id,value) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET value=excluded.value`, raw)
	return e
}

// Cached never performs network I/O; safe for metadata and hub rendering.
func (s *Service) Cached(ctx context.Context, t *core.Title) ([]Badge, error) {
	cfg, e := s.settings(ctx)
	if e != nil || !cfg.Enabled || t == nil {
		return nil, e
	}
	var raw string
	var at int64
	e = s.db.QueryRowContext(ctx, `SELECT value,updated FROM ratings_cache WHERE item_key=? AND provider=?`, t.Key, cfg.Provider).Scan(&raw, &at)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if s.o.Now().Sub(time.Unix(at, 0)) > time.Duration(cfg.CacheHours)*time.Hour {
		return nil, nil
	}
	var out []Badge
	e = json.Unmarshal([]byte(raw), &out)
	return out, e
}

// CachedMany is Cached for a page. Missing and expired titles are omitted.
// The provider being disabled returns an empty map and a nil error.
func (s *Service) CachedMany(ctx context.Context, titles []core.Title) (map[core.RatingKey][]Badge, error) {
	out := map[core.RatingKey][]Badge{}
	if s == nil || s.db == nil {
		return out, nil
	}
	cfg, err := s.settings(ctx)
	if err != nil || !cfg.Enabled {
		return out, err
	}
	keys := make([]core.RatingKey, 0, len(titles))
	seen := map[core.RatingKey]bool{}
	for _, title := range titles {
		if title.Key == 0 || seen[title.Key] {
			continue
		}
		seen[title.Key] = true
		keys = append(keys, title.Key)
	}
	freshAfter := s.o.Now().Add(-time.Duration(cfg.CacheHours) * time.Hour).Unix()
	for start := 0; start < len(keys); start += 40 {
		end := min(start+40, len(keys))
		args := make([]any, 0, 1+end-start)
		args = append(args, cfg.Provider)
		query := `SELECT item_key,value,updated FROM ratings_cache WHERE provider=? AND item_key IN (?`
		args = append(args, int64(keys[start]))
		for _, key := range keys[start+1 : end] {
			query += ",?"
			args = append(args, int64(key))
		}
		query += `)`
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, updated int64
			var raw string
			if err = rows.Scan(&key, &raw, &updated); err != nil {
				rows.Close()
				return nil, err
			}
			if updated < freshAfter {
				continue
			}
			var badges []Badge
			if json.Unmarshal([]byte(raw), &badges) != nil {
				continue
			}
			out[core.RatingKey(key)] = badges
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Refresh returns fresh-or-cached badges for t.
//
// Semântica de concorrência: nenhum lock é segurado durante o HTTP —
// flightMu só protege o map flights (leitura/escrita curta), e cache/DB
// são acessados sem lock global. Chamadas concorrentes para a MESMA key
// compartilham um único fetch em voo (singleflight): quem espera recebe
// o resultado (badges/err) do líder, sem disparar outro request. Se o
// contexto do líder encerrou o fetch (abandoned) e o contexto de quem
// espera ainda está vivo, quem espera NÃO herda esse erro de cancelamento:
// ele refaz — vira o próximo líder ou entra no fetch seguinte. Se o
// contexto de quem espera é cancelado enquanto aguarda, ele retorna
// ctx.Err() sem cancelar o fetch do líder. Keys diferentes fazem fetch
// em paralelo.
func (s *Service) Refresh(ctx context.Context, t *core.Title) ([]Badge, error) {
	if t == nil {
		return s.refresh(ctx, t)
	}
	ctx = s.WithSettingsSnapshot(ctx)
	cfg, err := s.settings(ctx)
	if err != nil {
		return nil, err
	}
	key := refreshKey{key: t.Key, settings: cfg, imdb: t.IDs.IMDB, tmdb: t.IDs.TMDB, titleType: t.Type}
	for {
		s.flightMu.Lock()
		if f, ok := s.flights[key]; ok {
			s.flightMu.Unlock()
			select {
			case <-f.done:
				if f.abandoned && ctx.Err() == nil {
					continue
				}
				return f.badges, f.err
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if s.flights == nil {
			s.flights = map[refreshKey]*refreshCall{}
		}
		f := &refreshCall{done: make(chan struct{})}
		s.flights[key] = f
		s.flightMu.Unlock()

		badges, err := s.refresh(ctx, t)
		f.badges, f.err, f.abandoned = badges, err, err != nil && ctx.Err() != nil
		s.flightMu.Lock()
		delete(s.flights, key)
		s.flightMu.Unlock()
		close(f.done)
		return badges, err
	}
}

// refresh faz cache-check + HTTP + grava cache, sem segurar lock algum.
func (s *Service) refresh(ctx context.Context, t *core.Title) ([]Badge, error) {
	if cached, e := s.Cached(ctx, t); e != nil || cached != nil {
		return cached, e
	}
	cfg, e := s.settings(ctx)
	if e != nil || !cfg.Enabled || t == nil {
		return nil, e
	}
	base := s.o.OMDbBaseURL
	if cfg.Provider == "mdblist" {
		if t.IDs.TMDB <= 0 {
			return nil, nil
		}
		typ := "movie"
		if t.Type == core.TypeShow {
			typ = "show"
		} else if t.Type != core.TypeMovie {
			return nil, nil
		}
		base = strings.TrimRight(s.o.MDBListBaseURL, "/") + "/tmdb/" + typ + "/" + strconv.FormatInt(t.IDs.TMDB, 10) + "/"
	} else if t.IDs.IMDB == "" {
		return nil, nil
	}
	u, e := url.Parse(base)
	if e != nil {
		return nil, errors.New("rating provider URL invalid")
	}
	q := u.Query()
	q.Set("apikey", cfg.APIKey)
	if cfg.Provider == "omdb" {
		q.Set("i", t.IDs.IMDB)
	}
	u.RawQuery = q.Encode()
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if e != nil {
		return nil, errors.New("rating request invalid")
	}
	resp, e := s.o.Client.Do(req)
	if e != nil {
		return nil, errors.New("rating provider unavailable")
	}
	defer resp.Body.Close()
	var badges []Badge
	switch {
	case resp.StatusCode == http.StatusNotFound && cfg.Provider == "mdblist":
		// MDBList does not know the title: a definite answer, cached like
		// one so the owner's quota is not spent asking again every retry.
	case resp.StatusCode != 200:
		return nil, fmt.Errorf("rating provider: HTTP %d", resp.StatusCode)
	default:
		data, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if e != nil {
			return nil, errors.New("rating response unreadable")
		}
		if cfg.Provider == "omdb" {
			badges, e = parseOMDb(data)
		} else {
			badges, e = parseMDBList(data)
		}
		if e != nil {
			return nil, e
		}
	}
	if badges == nil {
		badges = []Badge{}
	}
	raw, e := json.Marshal(badges)
	if e != nil {
		return nil, e
	}
	_, e = s.db.ExecContext(ctx, `INSERT INTO ratings_cache(item_key,provider,value,updated) VALUES(?,?,?,?) ON CONFLICT(item_key,provider) DO UPDATE SET value=excluded.value,updated=excluded.updated`, t.Key, cfg.Provider, string(raw), s.o.Now().Unix())
	return badges, e
}
func parseOMDb(data []byte) ([]Badge, error) {
	var response struct {
		Response string
		Error    string
		Ratings  []struct {
			Source string
			Value  string
		}
	}
	if json.Unmarshal(data, &response) != nil {
		return nil, errors.New("invalid OMDb response")
	}
	if response.Response == "False" {
		// A title OMDb does not have is a definite answer (no ratings),
		// cached like one; key, quota and server errors are retried.
		if msg := strings.ToLower(response.Error); strings.Contains(msg, "not found") || strings.Contains(msg, "incorrect imdb id") {
			return []Badge{}, nil
		}
		return nil, errors.New("OMDb lookup failed")
	}
	var out []Badge
	for _, r := range response.Ratings {
		source := ""
		scale := 100.
		switch r.Source {
		case "Internet Movie Database":
			source = "imdb"
			scale = 10
		case "Rotten Tomatoes":
			source = "tomatoes"
		case "Metacritic":
			source = "metacritic"
		default:
			continue
		}
		str := strings.TrimSuffix(strings.SplitN(r.Value, "/", 2)[0], "%")
		value, e := strconv.ParseFloat(str, 64)
		if e == nil && valid(value, scale) {
			out = append(out, Badge{Source: source, Value: value, Scale: scale})
		}
	}
	return out, nil
}
func valid(v, scale float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= scale }
func parseMDBList(data []byte) ([]Badge, error) {
	var response struct {
		Ratings []struct {
			Source string          `json:"source"`
			Value  json.RawMessage `json:"value"`
			Votes  int64           `json:"votes"`
		}
	}
	if json.Unmarshal(data, &response) != nil {
		return nil, errors.New("invalid MDBList response")
	}
	var out []Badge
	for _, r := range response.Ratings {
		scale := 100.
		switch r.Source {
		case "imdb":
			scale = 10
		case "letterboxd":
			scale = 5
		case "tomatoes", "popcorn", "metacritic":
		default:
			continue
		}
		value, e := strconv.ParseFloat(strings.Trim(string(r.Value), "\""), 64)
		if e == nil && valid(value, scale) {
			out = append(out, Badge{Source: r.Source, Value: value, Scale: scale, Votes: r.Votes})
		}
	}
	return out, nil
}

// Queue is bounded, deduplicated and nonblocking; Run owns the network worker.
func (s *Service) Queue(t core.Title) {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	if s.queued[t.Key] || s.o.Now().Before(s.retryAt[t.Key]) {
		return
	}
	select {
	case s.queue <- t:
		s.queued[t.Key] = true
	default:
	}
}
func (s *Service) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-s.queue:
			job, cancel := context.WithTimeout(ctx, 20*time.Second)
			_, err := s.Refresh(job, &t)
			cancel()
			s.qmu.Lock()
			delete(s.queued, t.Key)
			if err != nil {
				if len(s.retryAt) > 2048 {
					s.retryAt = map[core.RatingKey]time.Time{}
				}
				s.retryAt[t.Key] = s.o.Now().Add(15 * time.Minute)
			} else {
				delete(s.retryAt, t.Key)
			}
			s.qmu.Unlock()
		}
	}
}
