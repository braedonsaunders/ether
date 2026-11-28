package cardigann

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/langtag"
	"github.com/braedonsaunders/ether/internal/newznab"
)

// LoginLock serialises logins for one key across every Ether node sharing
// the database. unlock must be called once the login state is saved.
type LoginLock func(ctx context.Context, key string) (unlock func(), err error)

// Runner executes one configured indexer's definition. It is safe for
// concurrent use.
type Runner struct {
	def      *Definition
	id       string
	settings map[string]string
	site     *url.URL
	http     *httpSession
	sessions *sessionStore
	lock     LoginLock
	log      *slog.Logger
	now      func() time.Time
	// language is the release language stamped on this indexer's results
	// (core.Release.SourceLanguage): the owner's per-indexer override, else
	// the definition's `language`, canonicalised to a BCP 47 tag. Empty
	// when neither names one.
	language string
	clientID func() config.ClientIdentity

	mu            sync.Mutex
	loggedIn      bool
	version       int64 // version of the session row the jar holds
	loginFailedAt time.Time
	loginFailed   error
}

// runnerOptions carries a Runner's dependencies.
type runnerOptions struct {
	IndexerID   string
	SiteLink    string
	Settings    map[string]string
	HTTPClient  *http.Client
	FlareURL    func() string
	Sessions    *sessionStore
	Lock        LoginLock
	Logger      *slog.Logger
	Now         func() time.Time
	HostLimiter *newznab.HostLimiter
	// Language is the owner's per-indexer Release language override; empty
	// leaves the definition's own `language` in force (see Runner.language).
	Language       string
	ClientIdentity func() config.ClientIdentity
}

func newRunner(def *Definition, o runnerOptions) (*Runner, error) {
	link := def.SiteLink(o.SiteLink)
	site, err := url.Parse(link)
	if err != nil || (site.Scheme != "http" && site.Scheme != "https") || site.Host == "" {
		return nil, fmt.Errorf("site link %q is not an http(s) URL", link)
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sessions == nil {
		o.Sessions = &sessionStore{now: o.Now}
	}
	if o.Lock == nil {
		km := &keyedMutex{}
		o.Lock = km.lock
	}
	r := &Runner{def: def, id: o.IndexerID, settings: o.Settings, site: site, sessions: o.Sessions, lock: o.Lock, log: o.Logger, now: o.Now, clientID: o.ClientIdentity}
	r.language = canonicalLanguage(o.Language)
	if r.language == "" {
		r.language = canonicalLanguage(def.Language)
	}
	if r.id == "" {
		r.id = def.ID
	}
	r.http = newHTTPSession(def, o.HTTPClient, o.FlareURL, o.Logger)
	r.http.hosts = o.HostLimiter
	return r, nil
}

// Definition returns the runner's definition.
func (r *Runner) Definition() *Definition { return r.def }

// configVars maps the owner's settings to .Config values: checkboxes are
// "True" or "", selects default to their declared default, multi-selects
// are lists.
func (r *Runner) configVars() map[string]any {
	cfg := map[string]any{"sitelink": r.site.String()}
	for _, s := range r.def.Settings {
		val, set := r.settings[s.Name]
		switch s.Type {
		case "checkbox":
			if !set {
				val = s.Default
			}
			if truthy(val) {
				cfg[s.Name] = "True"
			} else {
				cfg[s.Name] = ""
			}
		case "multi-select":
			var list []string
			if set {
				for _, p := range strings.Split(val, ",") {
					if p = strings.TrimSpace(p); p != "" {
						list = append(list, p)
					}
				}
			} else {
				list = append(list, s.Defaults...)
				if len(list) == 0 && s.Default != "" {
					list = []string{s.Default}
				}
			}
			cfg[s.Name] = list
		default:
			if !set {
				val = s.Default
			}
			cfg[s.Name] = val
		}
	}
	// Values for settings the definition does not declare (older configs).
	for k, v := range r.settings {
		if _, ok := cfg[k]; !ok {
			cfg[k] = v
		}
	}
	return cfg
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

// canonicalLanguage folds a definition or owner-supplied language to its
// canonical BCP 47 tag ("pt-BR"), or "" when it names no known language.
func canonicalLanguage(v string) string {
	return langtag.CanonicalBCP47(v)
}

func (r *Runner) vars() vars { return baseVars(r.configVars(), r.now()) }

func (r *Runner) env(v vars) filterEnv { return filterEnv{vars: v, now: r.now(), log: r.log} }

// resolve makes a link absolute against base (the site link by default).
func (r *Runner) resolve(link string, base *url.URL) (*url.URL, error) {
	if base == nil {
		base = r.site
	}
	link = strings.TrimSpace(link)
	ref, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("bad link %q: %w", abbreviate(link), err)
	}
	return base.ResolveReference(ref), nil
}

func (r *Runner) headers(list []HeaderKV, v vars) http.Header {
	h := http.Header{}
	for _, kv := range list {
		for _, val := range kv.Values {
			if s, err := render(val, v, nil); err == nil && s != "" {
				h.Add(kv.Name, s)
			}
		}
	}
	return h
}

func (r *Runner) renderInputs(inputs []KV, v vars, dropEmpty bool) (form []KV, raw string, err error) {
	for _, in := range inputs {
		if in.Key == "$raw" {
			s, err := render(in.Value, v, r.http.encodeValue)
			if err != nil {
				return nil, "", err
			}
			raw += s
			continue
		}
		s, err := render(in.Value, v, nil)
		if err != nil {
			return nil, "", err
		}
		if dropEmpty && s == "" {
			continue
		}
		form = append(form, KV{Key: in.Key, Value: s})
	}
	return form, raw, nil
}

// mergeInputs overlays path inputs on the search inputs, keeping order.
func mergeInputs(base, over []KV) []KV {
	out := append([]KV(nil), base...)
	for _, o := range over {
		replaced := false
		for i := range out {
			if out[i].Key == o.Key && o.Key != "$raw" {
				out[i].Value, replaced = o.Value, true
				break
			}
		}
		if !replaced {
			out = append(out, o)
		}
	}
	return out
}
