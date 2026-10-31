package llm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
)

// Service turns model suggestions into catalogue titles and Home hubs.
//
// Every model call runs in a background goroutine (single-flighted per
// cache key, detached from the request context, bounded by
// ServiceOptions.CallTimeout). Callers only ever wait as long as their own
// context allows:
//
//   - ThemedRows never waits: it serves the cached rows (fresh or stale, or
//     nothing yet) and starts a refresh when needed.
//   - MoreLikeThis and ThemedRowsSync wait for the in-flight call until ctx
//     is done, then fall back to stale results. Bound ctx with a short
//     timeout on request paths; the result still lands in the cache.
//
// Failures back off (ErrorBackoff) so a dead endpoint is not hammered from
// every page view.
type Service struct {
	client  *Client
	catalog core.Catalog
	log     *slog.Logger
	ttl     time.Duration
	backoff time.Duration
	minRef  time.Duration
	timeout time.Duration
	max     int
	now     func() time.Time

	mu         sync.Mutex
	cache      map[string]*cacheEntry
	flights    map[string]chan struct{}
	generation uint64

	// Vibe refinement cache (vibe.go).
	vibeMu    sync.Mutex
	vibeCache map[string]vibeCacheEntry
}

type cacheEntry struct {
	hubs     []core.Hub
	titles   []core.Title
	sig      string    // input signature the value was computed from
	at       time.Time // last successful compute; zero = never
	failedAt time.Time
	err      error
}

func (e *cacheEntry) has() bool { return !e.at.IsZero() }

func (e *cacheEntry) touched() time.Time {
	if e.failedAt.After(e.at) {
		return e.failedAt
	}
	return e.at
}

// ServiceOptions build a Service.
type ServiceOptions struct {
	Client  *Client
	Catalog core.Catalog
	Logger  *slog.Logger
	// TTL is how long results are fresh (default 6h).
	TTL time.Duration
	// MinRefresh is the minimum age before themed rows are recomputed
	// because the user's history changed (default 15m).
	MinRefresh time.Duration
	// ErrorBackoff suppresses retries after a failed call (default 5m).
	ErrorBackoff time.Duration
	// CallTimeout bounds one background model call + resolution (default 3m).
	CallTimeout time.Duration
	// MaxEntries bounds the cache (default 1024); the least recently
	// computed entries are evicted.
	MaxEntries int
	Now        func() time.Time
}

// New builds a Service over a Client (share the Client if you also need raw Chat).
func New(opts ServiceOptions) *Service {
	if opts.TTL <= 0 {
		opts.TTL = 6 * time.Hour
	}
	if opts.MinRefresh <= 0 {
		opts.MinRefresh = 15 * time.Minute
	}
	if opts.ErrorBackoff <= 0 {
		opts.ErrorBackoff = 5 * time.Minute
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 3 * time.Minute
	}
	if opts.MaxEntries <= 0 {
		opts.MaxEntries = 1024
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
		if opts.Client != nil {
			opts.Logger = opts.Client.log
		}
	}
	return &Service{
		client: opts.Client, catalog: opts.Catalog, log: opts.Logger, ttl: opts.TTL,
		backoff: opts.ErrorBackoff, minRef: opts.MinRefresh, timeout: opts.CallTimeout,
		max: opts.MaxEntries, now: opts.Now,
		cache: map[string]*cacheEntry{}, flights: map[string]chan struct{}{},
	}
}

// Enabled reports whether the underlying client is on and a catalog is wired.
func (s *Service) Enabled() bool {
	return s != nil && s.client != nil && s.client.Enabled() && s.catalog != nil
}

// Chat is one completion. Language features use it for translations.
// Home recommendations keep using the cached Service methods.
func (s *Service) Chat(ctx context.Context, system, user string) (string, error) {
	if s == nil || s.client == nil {
		return "", fmt.Errorf("llm disabled")
	}
	return s.client.Chat(ctx, []Message{{Role: "system", Content: system}, {Role: "user", Content: user}}, false)
}

// Invalidate drops cached rows for a user (e.g. on an admin "refresh").
func (s *Service) Invalidate(userID int64) {
	s.mu.Lock()
	delete(s.cache, rowsKey(userID))
	s.mu.Unlock()
}

const systemPrompt = `You are a film and TV recommendation engine. Reply with JSON only, no prose. Only suggest real, released movies and TV shows, with their original release year. Never repeat a title the user has already watched.`

func describe(t core.Title) string {
	if t.Type == core.TypeEpisode && t.ShowTitle != "" {
		return fmt.Sprintf("%s (TV series)", t.ShowTitle)
	}
	typ := "movie"
	if t.Type == core.TypeShow {
		typ = "TV series"
	}
	parts := []string{t.Title}
	if t.Year > 0 {
		parts = append(parts, fmt.Sprintf("(%d, %s)", t.Year, typ))
	} else {
		parts = append(parts, "("+typ+")")
	}
	if len(t.Genres) > 0 {
		parts = append(parts, "- "+strings.Join(t.Genres, ", "))
	}
	return strings.Join(parts, " ")
}

// normTitle folds a title for comparison: lower case, "&" as "and",
// punctuation dropped, a leading article removed, whitespace collapsed.
func normTitle(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "&", " and "))
	var b strings.Builder
	space := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			space = false
		case !space && b.Len() > 0:
			b.WriteByte(' ')
			space = true
		}
	}
	out := strings.TrimSpace(b.String())
	for _, art := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(out, art) && len(out) > len(art) {
			out = out[len(art):]
			break
		}
	}
	return out
}

func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// matchRef picks the search result that is really the suggested title, or
// -1. Models misremember years by one and punctuation freely, but a title
// that matches neither name nor year is a different film and is dropped,
// so a hallucinated title cannot resolve to an arbitrary search hit.
func matchRef(ref TitleRef, results []core.Title) int {
	want := normTitle(ref.Title)
	if want == "" {
		return -1
	}
	best, bestScore := -1, 0
	for i, r := range results {
		score := 0
		names := []string{normTitle(r.Title), normTitle(r.OriginalTitle)}
		switch {
		case names[0] == want || (names[1] != "" && names[1] == want):
			score = 10
		case ref.Year != 0 && r.Year == ref.Year && (strings.Contains(names[0], want) || strings.Contains(want, names[0])):
			score = 5 // "Dune" vs "Dune: Part One" with the right year
		default:
			continue
		}
		switch {
		case ref.Year == 0 || r.Year == 0:
			score += 1
		case r.Year == ref.Year:
			score += 3
		case absInt(r.Year-ref.Year) == 1:
			score += 2
		default:
			score -= 6 // same name, different film (remake)
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

// resolve turns model refs into catalogue titles via Search. Duplicates,
// excluded keys and unresolvable refs drop out.
func (s *Service) resolve(ctx context.Context, refs []TitleRef, exclude map[core.RatingKey]bool, limit int) []core.Title {
	var out []core.Title
	seen := map[core.RatingKey]bool{}
	for _, ref := range refs {
		if len(out) >= limit || ctx.Err() != nil {
			break
		}
		var types []core.MediaType
		switch strings.ToLower(strings.TrimSpace(ref.Type)) {
		case "movie", "film":
			types = []core.MediaType{core.TypeMovie}
		case "show", "tv", "series", "tv show", "tv series":
			types = []core.MediaType{core.TypeShow}
		}
		results, err := s.catalog.Search(ctx, ref.Title, types, 5)
		if err != nil || len(results) == 0 {
			continue
		}
		i := matchRef(ref, results)
		if i < 0 {
			continue
		}
		t := results[i]
		if seen[t.Key] || exclude[t.Key] {
			continue
		}
		seen[t.Key] = true
		out = append(out, t)
	}
	return out
}

// needsRefresh reports whether e (may be nil) should be recomputed for sig.
func (s *Service) needsRefresh(e *cacheEntry, sig string, now time.Time) bool {
	if e == nil {
		return true
	}
	if !e.failedAt.IsZero() && e.failedAt.After(e.at) && now.Sub(e.failedAt) < s.backoff {
		return false
	}
	switch {
	case !e.has():
		return true
	case now.Sub(e.at) >= s.ttl:
		return true
	case sig != e.sig && now.Sub(e.at) >= s.minRef:
		return true
	}
	return false
}

// refresh starts (or joins) the background compute for key and returns a
// channel closed when it finishes. Call with s.mu held.
func (s *Service) refreshLocked(key, sig string, compute func(context.Context) ([]core.Hub, []core.Title, error)) chan struct{} {
	if ch, ok := s.flights[key]; ok {
		return ch
	}
	ch := make(chan struct{})
	s.flights[key] = ch
	generation := s.generation
	go func() {
		defer close(ch)
		ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
		defer cancel()
		hubs, titles, err := compute(ctx)
		now := s.now()
		s.mu.Lock()
		defer s.mu.Unlock()
		if generation != s.generation {
			return
		}
		delete(s.flights, key)
		e := s.cache[key]
		if e == nil {
			e = &cacheEntry{}
		}
		if err != nil {
			e.failedAt, e.err = now, err
			s.log.Debug("llm call failed", "key", key, "err", err)
		} else {
			e.hubs, e.titles, e.sig, e.at, e.failedAt, e.err = hubs, titles, sig, now, time.Time{}, nil
		}
		s.cache[key] = e
		s.evictLocked()
	}()
	return ch
}

func (s *Service) evictLocked() {
	for len(s.cache) > s.max {
		var oldest string
		var oldestAt time.Time
		for k, e := range s.cache {
			if _, busy := s.flights[k]; busy {
				continue
			}
			if oldest == "" || e.touched().Before(oldestAt) {
				oldest, oldestAt = k, e.touched()
			}
		}
		if oldest == "" {
			return
		}
		delete(s.cache, oldest)
	}
}

// MoreLikeThis asks the model for titles similar to one title and resolves
// them. It waits for the model only as long as ctx allows, then returns the
// stale result (or nil) while the call finishes into the cache.
func (s *Service) MoreLikeThis(ctx context.Context, title *core.Title, limit int) ([]core.Title, error) {
	if !s.Enabled() {
		return nil, nil
	}
	if title == nil {
		return nil, fmt.Errorf("nil title")
	}
	if limit <= 0 {
		limit = 12
	}
	key := fmt.Sprintf("more:%d:%d", int64(title.Key), limit)
	t := *title
	compute := func(ctx context.Context) ([]core.Hub, []core.Title, error) {
		prompt := fmt.Sprintf(`Suggest %d movies or TV shows similar to %s. Reply with JSON: {"titles":[{"title":"...","year":2000,"type":"movie","reason":"one short phrase"}]}`, limit+4, describe(t))
		content, err := s.client.Chat(ctx, []Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: prompt}}, true)
		if err != nil {
			return nil, nil, err
		}
		exclude := map[core.RatingKey]bool{t.Key: true}
		if t.ShowKey != 0 {
			exclude[t.ShowKey] = true
		}
		return nil, s.resolve(ctx, ParseTitles(content), exclude, limit), nil
	}
	return s.wait(ctx, key, "", compute, func(e *cacheEntry) []core.Title { return e.titles })
}

// wait serves a fresh entry, or joins/starts a compute and waits for it
// until ctx is done, falling back to stale values.
func (s *Service) wait(ctx context.Context, key, sig string, compute func(context.Context) ([]core.Hub, []core.Title, error), pick func(*cacheEntry) []core.Title) ([]core.Title, error) {
	s.mu.Lock()
	e := s.cache[key]
	if !s.needsRefresh(e, sig, s.now()) {
		var v []core.Title
		if e != nil {
			v = pick(e)
		}
		s.mu.Unlock()
		return v, nil
	}
	ch := s.refreshLocked(key, sig, compute)
	s.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e = s.cache[key]
	switch {
	case e != nil && e.has():
		return pick(e), nil
	case e != nil && e.err != nil:
		return nil, e.err
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	return nil, nil
}

func rowsKey(userID int64) string { return fmt.Sprintf("rows:%d", userID) }

func seedsSig(seeds []core.Title, limit int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d:", limit)
	for _, t := range seeds {
		fmt.Fprintf(&sb, "%d,", int64(t.Key))
	}
	return sb.String()
}

// computeRows does the blocking model call + resolution for themed rows.
func (s *Service) computeRows(ctx context.Context, user core.User, seeds, dislikes []core.Title, vetoGenres []string, limit int) ([]core.Hub, error) {
	if limit <= 0 {
		limit = 12
	}
	var watched []string
	exclude := map[core.RatingKey]bool{}
	for _, t := range seeds {
		watched = append(watched, describe(t))
		exclude[t.Key] = true
	}
	var avoided []string
	for _, t := range dislikes {
		avoided = append(avoided, describe(t))
		exclude[t.Key] = true
	}
	format := `Reply with JSON: {"rows":[{"title":"theme name","titles":[{"title":"...","year":2000,"type":"movie"}]}]}`
	var prompt string
	if len(seeds) == 0 {
		prompt = fmt.Sprintf("Create up to 3 themed rows of widely acclaimed movies and TV shows for a new viewer, each with a short evocative theme name and %d titles. %s", limit+4, format)
	} else {
		prompt = fmt.Sprintf("The user recently watched and enjoyed:\n- %s\n\nCreate up to 3 hyper-specific themed recommendation rows for them, each with a sharp evocative micro-genre name (like \"Slow-burn sci-fi with a twist\" or \"Competence porn: experts under pressure\") and %d titles they have not seen. Mix iconic essentials with acclaimed deeper cuts and international gems from any era rather than only obvious top-20 blockbusters. %s",
			strings.Join(watched, "\n- "), limit+4, format)
	}
	if len(avoided) > 0 {
		prompt += fmt.Sprintf("\n\nThey recently abandoned, disliked or passed on:\n- %s\nAvoid recommending titles with the same flaws, tone or sub-genre as those.", strings.Join(avoided, "\n- "))
	}
	if len(vetoGenres) > 0 {
		prompt += fmt.Sprintf(" Never include titles in these disliked genres: %s.", strings.Join(vetoGenres, ", "))
	}
	if user.MaxContentRating != "" {
		prompt += fmt.Sprintf(" Only include titles rated %s or milder.", user.MaxContentRating)
	}
	content, err := s.client.Chat(ctx, []Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: prompt}}, true)
	if err != nil {
		return nil, err
	}
	var hubs []core.Hub
	ids := map[string]int{}
	for _, row := range ParseRows(content) {
		items := s.resolve(ctx, row.Titles, exclude, limit)
		if len(items) == 0 {
			continue
		}
		title := strings.TrimSpace(row.Title)
		if title == "" {
			title = "Picked for You"
		}
		id := "llm." + slug(title)
		if n := ids[id]; n > 0 {
			id = fmt.Sprintf("%s-%d", id, n+1)
		}
		ids["llm."+slug(title)]++
		hubs = append(hubs, core.Hub{
			ID: id, Title: title, Context: "hub.home.llm", Style: "shelf", Promoted: true, Items: items,
		})
	}
	if len(hubs) == 0 {
		return nil, fmt.Errorf("llm themed rows: no resolvable titles in model output")
	}
	return hubs, nil
}

func (s *Service) rowsCompute(user *core.User, seeds, dislikes []core.Title, vetoGenres []string, limit int) func(context.Context) ([]core.Hub, []core.Title, error) {
	var u core.User
	if user != nil {
		u = *user
	}
	seedsCopy := append([]core.Title(nil), seeds...)
	dislikesCopy := append([]core.Title(nil), dislikes...)
	vetoCopy := append([]string(nil), vetoGenres...)
	return func(ctx context.Context) ([]core.Hub, []core.Title, error) {
		hubs, err := s.computeRows(ctx, u, seedsCopy, dislikesCopy, vetoCopy, limit)
		return hubs, nil, err
	}
}

// ThemedRows never blocks: it returns the user's cached rows (fresh or
// stale) and kicks off a background refresh when they are missing, expired,
// or were computed from different seeds more than MinRefresh ago. This is
// the shape the Home screen wants.
func (s *Service) ThemedRows(ctx context.Context, user *core.User, seeds []core.Title, limit int) ([]core.Hub, error) {
	if !s.Enabled() {
		return nil, nil
	}
	key, sig := rowsKey(userID(user)), seedsSig(seeds, limit)
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.cache[key]
	if s.needsRefresh(e, sig, s.now()) {
		s.refreshLocked(key, sig, s.rowsCompute(user, seeds, nil, nil, limit))
	}
	if e == nil || !e.has() {
		return nil, nil
	}
	return e.hubs, nil
}

// ThemedRowsSync is ThemedRows that waits (until ctx is done) for a needed
// refresh — for admin previews and warm-up jobs, not request paths.
func (s *Service) ThemedRowsSync(ctx context.Context, user *core.User, seeds []core.Title, limit int) ([]core.Hub, error) {
	return s.ThemedRowsContrastiveSync(ctx, user, seeds, nil, nil, limit)
}

// ThemedRowsContrastiveSync is ThemedRowsSync with negative evidence
// (abandoned/ignored titles and disliked genres) so the model steers away
// from what the viewer bounced off.
func (s *Service) ThemedRowsContrastiveSync(ctx context.Context, user *core.User, seeds, dislikes []core.Title, vetoGenres []string, limit int) ([]core.Hub, error) {
	if !s.Enabled() {
		return nil, nil
	}
	key, sig := rowsKey(userID(user)), seedsSig(append(append([]core.Title(nil), seeds...), dislikes...), limit)+"|"+strings.Join(vetoGenres, ",")
	s.mu.Lock()
	e := s.cache[key]
	if !s.needsRefresh(e, sig, s.now()) {
		s.mu.Unlock()
		if e == nil {
			return nil, nil
		}
		return e.hubs, nil
	}
	ch := s.refreshLocked(key, sig, s.rowsCompute(user, seeds, dislikes, vetoGenres, limit))
	s.mu.Unlock()
	select {
	case <-ch:
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e = s.cache[key]
	switch {
	case e != nil && e.has():
		return e.hubs, nil
	case e != nil && e.err != nil:
		return nil, e.err
	}
	return nil, ctx.Err()
}

func userID(u *core.User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}

func slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	lastDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "row"
	}
	if len(out) > 40 {
		out = strings.TrimRight(out[:40], "-")
	}
	return out
}

// UpdateConfig applies new endpoint/model credentials and invalidates results
// computed with previous settings. An old in-flight call cannot refill them.
func (s *Service) UpdateConfig(cfg config.LLM) {
	if s == nil || s.client == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client.mu.RLock()
	same := s.client.cfg == cfg
	s.client.mu.RUnlock()
	if same {
		return
	}
	s.client.UpdateConfig(cfg)
	s.generation++
	s.cache = map[string]*cacheEntry{}
	s.flights = map[string]chan struct{}{}
}
