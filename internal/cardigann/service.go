package cardigann

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/egress"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/safefetch"
)

// definitionRecheck is how long a node trusts its parsed copy of a
// definition before re-reading the database (another node may have
// updated it).
const definitionRecheck = time.Minute

// ErrUnknownDefinition reports a definition id that is not installed.
var ErrUnknownDefinition = errors.New("definition is not installed")

// Options configures a Service.
type Options struct {
	DB *sql.DB // nil keeps definitions and sessions in memory
	// HTTPClient supplies the transport and timeout for site, definitions
	// source and FlareSolverr requests (tests inject fakes).
	HTTPClient *http.Client
	Logger     *slog.Logger
	Now        func() time.Time
	// LoginLock serialises logins per indexer across nodes; nil uses an
	// in-process lock.
	LoginLock LoginLock
	Sites     config.IndexerSites
	// Sealer protects stored login cookies; nil stores them as given.
	Sealer config.FieldSealer
}

// Service manages installed definitions and the runners of configured
// built-in indexers.
type Service struct {
	defs     *defStore
	sessions *sessionStore
	tests    *testStore
	hc       *http.Client
	// sourceHC is hc guarded by safefetch, used only for definitions
	// source fetches (see fetchSource). Site/runner requests keep using
	// hc: their destinations come from owner-installed definitions and
	// owner-configured site links, not from a remote listing.
	sourceHC *http.Client
	log      *slog.Logger
	now      func() time.Time
	lock     LoginLock

	mu      sync.Mutex
	sites   config.IndexerSites
	parsed  map[string]*parsedDef
	runners map[string]*runnerEntry
	catalog *catalogCache
}

type parsedDef struct {
	def       *Definition
	warnings  []string
	hash      string
	stored    storedDefinition
	checkedAt time.Time
}

type runnerEntry struct {
	fingerprint string
	credentials string
	runner      *Runner
}

// New builds a Service.
func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTPClient == nil {
		o.HTTPClient = egress.Client(egress.CapIndexers, defaultHTTPTimeout)
	}
	if o.LoginLock == nil {
		km := &keyedMutex{}
		o.LoginLock = km.lock
	}
	return &Service{
		defs:     &defStore{db: o.DB},
		sessions: &sessionStore{db: o.DB, sealer: o.Sealer, now: o.Now},
		tests:    &testStore{db: o.DB},
		hc:       o.HTTPClient,
		sourceHC: (&safefetch.Guard{}).Client(o.HTTPClient),
		log:      o.Logger,
		now:      o.Now,
		lock:     o.LoginLock,
		sites:    o.Sites,
		parsed:   map[string]*parsedDef{},
		runners:  map[string]*runnerEntry{},
		catalog:  &catalogCache{},
	}
}

// ApplyConfig takes the definitions source and FlareSolverr settings from
// a config snapshot; subscribe it to config changes.
func (s *Service) ApplyConfig(cfg config.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sites.DefinitionsURL != cfg.IndexerSites.DefinitionsURL {
		s.catalog = &catalogCache{}
	}
	s.sites = cfg.IndexerSites
}

func (s *Service) siteSettings() config.IndexerSites {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sites
}

func (s *Service) flareURL() string { return s.siteSettings().FlareSolverrURL }

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// definition returns a parsed installed definition, re-reading the
// database at most once a minute.
func (s *Service) definition(ctx context.Context, id string) (*parsedDef, error) {
	if id == "" {
		return nil, fmt.Errorf("no definition selected: %w", ErrUnknownDefinition)
	}
	now := s.now()
	s.mu.Lock()
	p := s.parsed[id]
	fresh := p != nil && now.Sub(p.checkedAt) < definitionRecheck
	s.mu.Unlock()
	if fresh {
		return p, nil
	}
	stored, ok, err := s.defs.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.mu.Lock()
		delete(s.parsed, id)
		s.mu.Unlock()
		return nil, fmt.Errorf("%q: %w", id, ErrUnknownDefinition)
	}
	h := hashBytes(stored.YAML)
	if p != nil && p.hash == h {
		s.mu.Lock()
		p.checkedAt = s.now()
		s.mu.Unlock()
		return p, nil
	}
	def, warnings, err := Parse(stored.YAML)
	if err != nil {
		return nil, fmt.Errorf("installed definition %q: %w", id, err)
	}
	p = &parsedDef{def: def, warnings: warnings, hash: h, stored: stored, checkedAt: s.now()}
	s.mu.Lock()
	s.parsed[id] = p
	s.mu.Unlock()
	return p, nil
}

func mergedSettings(cfg config.Indexer) map[string]string {
	out := make(map[string]string, len(cfg.Settings)+len(cfg.SecretSettings))
	for k, v := range cfg.Settings {
		out[k] = v
	}
	for k, v := range cfg.SecretSettings {
		out[k] = v
	}
	return out
}

func mapFingerprint(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%q=%q;", k, m[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// credentialFingerprint covers the settings that identify the account, so
// changing them drops the stored session but changing a sort order does not.
func credentialFingerprint(cfg config.Indexer) string {
	ids := map[string]string{}
	for k, v := range cfg.Settings {
		lk := strings.ToLower(k)
		for _, part := range []string{"user", "email", "login", "cookie", "pass", "key", "token", "pid", "uid"} {
			if strings.Contains(lk, part) {
				ids[k] = v
			}
		}
	}
	return mapFingerprint(cfg.SecretSettings) + mapFingerprint(ids) + cfg.URL
}

func (s *Service) newRunner(cfg config.Indexer, p *parsedDef) (*Runner, error) {
	return newRunner(p.def, runnerOptions{
		IndexerID:  cfg.ID,
		SiteLink:   cfg.URL,
		Settings:   mergedSettings(cfg),
		HTTPClient: s.hc,
		FlareURL:   s.flareURL,
		Sessions:   s.sessions,
		Lock:       s.lock,
		Logger:     s.log.With("indexer", cfg.Name),
		Now:        s.now,
	})
}

// runner returns the cached runner for a configured indexer, rebuilding
// it when the definition, site link or settings changed. Changed
// credentials drop the stored session.
func (s *Service) runner(ctx context.Context, cfg config.Indexer) (*Runner, error) {
	p, err := s.definition(ctx, cfg.Definition)
	if err != nil {
		return nil, err
	}
	creds := credentialFingerprint(cfg)
	fp := p.hash + "|" + cfg.URL + "|" + mapFingerprint(cfg.Settings) + "|" + creds
	key := cfg.ID
	if key == "" {
		key = cfg.Name
	}
	s.mu.Lock()
	e := s.runners[key]
	s.mu.Unlock()
	if e != nil && e.fingerprint == fp {
		return e.runner, nil
	}
	r, err := s.newRunner(cfg, p)
	if err != nil {
		return nil, err
	}
	if e != nil && e.credentials != creds {
		if err := s.sessions.clear(ctx, r.id); err != nil {
			s.log.Warn("cardigann: could not clear session", "indexer", cfg.Name, "err", err)
		}
	}
	s.mu.Lock()
	s.runners[key] = &runnerEntry{fingerprint: fp, credentials: creds, runner: r}
	s.mu.Unlock()
	return r, nil
}

// backend adapts a Runner to newznab.Backend for one indexer config.
type backend struct {
	r         *Runner
	movieCats []int
	tvCats    []int
}

func (b backend) Search(ctx context.Context, req core.SearchRequest, reserve func(context.Context, string) error) ([]core.Release, error) {
	return b.r.Search(ctx, req, b.movieCats, b.tvCats, reserve)
}

func (b backend) FetchNZB(ctx context.Context, r core.Release, reserve func(context.Context, string) error) ([]byte, error) {
	return b.r.FetchNZB(ctx, r, reserve)
}

func (b backend) ResolveTorrent(ctx context.Context, r core.Release, reserve func(context.Context, string) error) (core.Release, error) {
	return b.r.ResolveTorrent(ctx, r, reserve)
}

// Backend implements newznab.Options.Backend for built-in indexers.
func (s *Service) Backend(cfg config.Indexer) (newznab.Backend, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := s.runner(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("indexer %q: %w", cfg.Name, err)
	}
	return backend{r: r, movieCats: cfg.MovieCategories, tvCats: cfg.TVCategories}, nil
}

// Test signs in with cfg (saved or not) and runs a sample search. With
// record set (a saved indexer) the result is kept for the indexer list.
func (s *Service) Test(ctx context.Context, cfg config.Indexer, record bool) TestResult {
	res := s.test(ctx, cfg)
	if record && cfg.ID != "" {
		msg := res.Error
		if res.OK {
			msg = fmt.Sprintf("%d results", res.Results)
		}
		if err := s.tests.put(ctx, TestStatus{IndexerID: cfg.ID, OK: res.OK, Message: msg, Results: res.Results, TestedAt: s.now()}); err != nil {
			s.log.Warn("cardigann: could not record test", "indexer", cfg.Name, "err", err)
		}
	}
	return res
}

func (s *Service) test(ctx context.Context, cfg config.Indexer) TestResult {
	p, err := s.definition(ctx, cfg.Definition)
	if err != nil {
		return TestResult{Error: err.Error()}
	}
	r, err := s.newRunner(cfg, p)
	if err != nil {
		return TestResult{Error: err.Error()}
	}
	res, _ := r.Test(ctx)
	if res.OK {
		// The cached runner adopts the session the test saved.
		s.mu.Lock()
		delete(s.runners, cfg.ID)
		s.mu.Unlock()
	}
	return res
}

// MigrateSecrets seals login cookies stored in plaintext, bound to each
// configured built-in indexer's site origin, drops plaintext cookies of
// indexers no longer configured, and returns how many rows it changed.
// Indexers whose definition cannot be loaded keep their row for a later
// run (reported in the error).
func (s *Service) MigrateSecrets(ctx context.Context, indexers []config.Indexer) (int, error) {
	if s.sessions.db == nil || s.sessions.sealer == nil {
		return 0, nil
	}
	origins := map[string]string{}
	for _, cfg := range indexers {
		if !cfg.IsBuiltin() {
			continue
		}
		id := cfg.ID
		p, err := s.definition(ctx, cfg.Definition)
		if err == nil {
			var site *url.URL
			if site, err = url.Parse(p.def.SiteLink(cfg.URL)); err == nil {
				if id == "" {
					id = p.def.ID
				}
				origins[id] = siteOrigin(site)
				continue
			}
		}
		// Unknown origin: keep the row (not "removed") for a later run.
		origins[id] = ""
	}
	n, waiting, err := s.sessions.migrate(ctx, origins)
	if err == nil && len(waiting) > 0 {
		err = fmt.Errorf("cardigann: plaintext sessions of %s left for a later run: definition unavailable", strings.Join(waiting, ", "))
	}
	return n, err
}

// Statuses returns the last test result of every tested indexer.
func (s *Service) Statuses(ctx context.Context) ([]TestStatus, error) {
	return s.tests.list(ctx)
}

// ---------------------------------------------------------------------------
// Definition management

// Summary describes an installed definition for the owner UI.
type Summary struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Language    string    `json:"language,omitempty"`
	Type        string    `json:"type,omitempty"`
	Protocol    string    `json:"protocol"`
	Links       []string  `json:"links"`
	Settings    []Setting `json:"settings"`
	Movies      bool      `json:"movies"`
	TV          bool      `json:"tv"`
	Login       string    `json:"login,omitempty"`
	Source      string    `json:"source"` // "upload" or "source"
	Warnings    []string  `json:"warnings,omitempty"`
	InstalledAt time.Time `json:"installedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

func summarize(def *Definition, warnings []string, stored storedDefinition) Summary {
	sum := Summary{
		ID: def.ID, Name: def.Name, Description: def.Description, Language: def.Language, Type: def.Type,
		Protocol: def.Protocol, Links: def.Links, Settings: def.Settings, Movies: def.SupportsMovies(), TV: def.SupportsTV(),
		Source: "upload", Warnings: warnings, InstalledAt: stored.InstalledAt, UpdatedAt: stored.UpdatedAt,
	}
	if sum.Settings == nil {
		sum.Settings = []Setting{}
	}
	if def.Login != nil {
		sum.Login = def.Login.Method
	}
	if stored.Origin != "" {
		sum.Source = "source"
	}
	return sum
}

// Definitions lists installed definitions by name.
func (s *Service) Definitions(ctx context.Context) ([]Summary, error) {
	rows, err := s.defs.list(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Summary, 0, len(rows))
	for _, row := range rows {
		def, warnings, err := Parse(row.YAML)
		if def == nil {
			s.log.Warn("cardigann: unreadable installed definition", "id", row.ID, "err", err)
			continue
		}
		if err != nil {
			warnings = append(warnings, err.Error())
		}
		out = append(out, summarize(def, warnings, row))
	}
	sort.SliceStable(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

// Definition returns one installed definition.
func (s *Service) Definition(ctx context.Context, id string) (Summary, error) {
	p, err := s.definition(ctx, id)
	if err != nil {
		return Summary{}, err
	}
	return summarize(p.def, p.warnings, p.stored), nil
}

// Install validates and stores a definition (replacing one with the same
// id). origin is the URL it came from ("" for an upload).
func (s *Service) Install(ctx context.Context, data []byte, origin, sha string) (Summary, error) {
	def, warnings, err := Parse(data)
	if err != nil {
		return Summary{}, err
	}
	now := s.now()
	row := storedDefinition{ID: def.ID, Name: def.Name, YAML: data, Origin: origin, SHA: sha, InstalledAt: now, UpdatedAt: now}
	if old, ok, err := s.defs.get(ctx, def.ID); err == nil && ok {
		row.InstalledAt = old.InstalledAt
	}
	if err := s.defs.put(ctx, row); err != nil {
		return Summary{}, err
	}
	s.mu.Lock()
	delete(s.parsed, def.ID)
	s.mu.Unlock()
	s.log.Info("cardigann: definition installed", "id", def.ID, "name", def.Name, "warnings", len(warnings))
	return summarize(def, warnings, row), nil
}

// Remove deletes an installed definition.
func (s *Service) Remove(ctx context.Context, id string) error {
	if _, ok, err := s.defs.get(ctx, id); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%q: %w", id, ErrUnknownDefinition)
	}
	if err := s.defs.delete(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.parsed, id)
	s.mu.Unlock()
	return nil
}
