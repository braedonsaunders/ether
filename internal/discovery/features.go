// Package discovery owns shared discovery enrichment and its bounded worker.
package discovery

import (
	"context"
	"fmt"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/llm"
	"github.com/braedonsaunders/ether/internal/overlays"
	"github.com/braedonsaunders/ether/internal/personalize"
	"github.com/braedonsaunders/ether/internal/ratings"
	"github.com/braedonsaunders/ether/internal/watchlist"
	"sync"
	"time"
)

// Features is optional. Viewers use these features through ordinary
// Plex metadata, Home, search and continue-watching endpoints.
type Features struct {
	Watchlist *watchlist.Service
	Ratings   *ratings.Service
	Overlays  *overlays.Service
	Personal  *personalize.Discovery
	LLM       *llm.Service
	Localize  func(context.Context, *core.Title, string) (*core.Title, error)
	once      sync.Once
	mu        sync.Mutex
	queue     chan discoveryJob
	cache     map[string]discoveryEntry
	pending   map[string]bool
}
type discoveryJob struct {
	key   string
	title core.Title
	user  core.User
	lang  string
	recap bool
	state core.State
}
type discoveryEntry struct {
	title *core.Title
	recap string
	at    time.Time
}

func (d *Features) init() {
	d.once.Do(func() {
		d.queue = make(chan discoveryJob, 128)
		d.cache = map[string]discoveryEntry{}
		d.pending = map[string]bool{}
	})
}
func (d *Features) cached(job discoveryJob) discoveryEntry {
	d.init()
	d.mu.Lock()
	defer d.mu.Unlock()
	v := d.cache[job.key]
	if time.Since(v.at) < 6*time.Hour {
		return v
	}
	if !d.pending[job.key] {
		select {
		case d.queue <- job:
			d.pending[job.key] = true
		default:
		}
	}
	return v
}

// Run is the sole metadata background worker, bounded and stopped with the app.
func (d *Features) Run(ctx context.Context) {
	if d == nil {
		return
	}
	d.init()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-d.queue:
			bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
			var v discoveryEntry
			var e error
			if job.recap {
				v.recap, e = d.LLM.SeasonRecap(bounded, &job.user, job.title.Key, job.state)
			} else {
				v.title, e = d.Localize(bounded, &job.title, job.lang)
			}
			cancel()
			d.mu.Lock()
			delete(d.pending, job.key)
			v.at = time.Now()
			if e != nil {
				v.title = nil
				v.recap = ""
				v.at = time.Now().Add(-6*time.Hour + 5*time.Minute)
			}
			if len(d.cache) >= 256 {
				for k := range d.cache {
					delete(d.cache, k)
					break
				}
			}
			d.cache[job.key] = v
			d.mu.Unlock()
		}
	}
}

// CachedLocalized queues localization without delaying catalogue enumeration.
func (d *Features) CachedLocalized(t *core.Title, lang string) *core.Title {
	return d.cached(discoveryJob{key: fmt.Sprintf("lang:%s:%s:%d", t.Key.String(), lang, t.UpdatedAt.Unix()), title: *t, lang: lang}).title
}
func (d *Features) CachedRecap(user *core.User, t *core.Title, state core.State, now time.Time) string {
	return d.cached(discoveryJob{key: fmt.Sprintf("recap:%d:%d:%d", user.ID, t.Key, now.Unix()/3600), title: *t, user: *user, recap: true, state: state}).recap
}
