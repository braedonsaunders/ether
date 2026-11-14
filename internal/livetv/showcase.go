package livetv

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/store"
)

// Showcase channels ship with every installation: four decade networks whose
// programming is a recipe resolved against the owner's own catalogue. Seeding
// is additive and happens once per recipe; a deleted channel stays deleted.
// Until the catalogue can fill a channel it is saved disabled, so viewers
// never meet an empty network. A background refresh keeps each recipe's
// series and film pools current; an owner's edits always win.
func init() {
	store.Register("livetv", 16, `CREATE TABLE livetv_showcase(id TEXT PRIMARY KEY, channel_id TEXT NOT NULL, version INTEGER NOT NULL, recipe_hash TEXT NOT NULL, state TEXT NOT NULL, refreshed_ms BIGINT NOT NULL DEFAULT 0, attempted_ms BIGINT NOT NULL DEFAULT 0);`)
}

const (
	showcasePending = "pending"
	showcaseLive    = "live"
)

type showcaseRecipe struct {
	id      string
	version int
	number  string
	name    string
	lookID  string
	reelID  string
	decade  string
	seed    int64
	era     [2]int
	blocks  []Block
	filler  ProgramSource
}

var (
	everyDay = []int(nil)
	weekdays = []int{1, 2, 3, 4, 5}
)

// nonFiction never belongs in an era network's entertainment lineup.
var nonFiction = []string{"Reality", "Talk", "News", "Soap", "Documentary"}

func excluding(extra ...string) []string {
	return append(append([]string(nil), nonFiction...), extra...)
}

// series selects era television: premiere years, genres, minimum audience and
// episode length. Very long-running daily formats never match.
func series(from, to int, genres []string, votes, minMinutes, maxMinutes int) *core.Filter {
	return &core.Filter{MaxContentRating: "PG-13", Type: core.TypeShow, YearFrom: from, YearTo: to, Genres: genres, MinVotes: votes, MinRuntime: time.Duration(minMinutes) * time.Minute, MaxRuntime: time.Duration(maxMinutes) * time.Minute, MaxEpisodes: 400, Countries: []string{"US", "CA"}, Languages: []string{"en"}}
}

// rated caps a slot the way a broadcaster's standards desk would: daytime
// and children's slots stay family-friendly; late night is unrestricted.
func rated(f *core.Filter, max string) *core.Filter {
	f.MaxContentRating = max
	return f
}

func films(from, to int, genres []string, votes int) *core.Filter {
	return &core.Filter{MaxContentRating: "R", Type: core.TypeMovie, YearFrom: from, YearTo: to, Genres: genres, MinVotes: votes, MinRuntime: 75 * time.Minute, MaxRuntime: 165 * time.Minute, Languages: []string{"en"}}
}

func lineup(name string, f *core.Filter, exclude []string, shows, pool, rotate int) ProgramSource {
	return ProgramSource{Kind: "lineup", Name: name, Filter: f, ExcludeGenres: exclude, Shows: shows, Pool: pool, RotateDays: rotate}
}

func moviePool(name string, f *core.Filter, exclude []string, pool int) ProgramSource {
	return ProgramSource{Kind: "movies", Name: name, Filter: f, ExcludeGenres: exclude, Pool: pool, Order: "shuffle"}
}

func block(name string, start, end int, days []int, src ProgramSource) Block {
	b := Block{Name: name, Start: start * 60, End: end * 60, Days: days, Preset: "custom", Fit: "shift", Sources: []ProgramSource{src}}
	if src.Kind == "movies" {
		b.RepeatDays = 21
	}
	if b.End == 1440 {
		// The broadcast day ends at midnight: the last block stops at a whole
		// programme and short-form filler covers the minutes that remain.
		b.Fit = "pad"
	}
	return b
}

// showcaseRecipes are the shipped decade networks. Each era keeps the shape
// of a real schedule of its time: cartoons before school and on Saturday
// morning, syndicated reruns by day, an access-hour sitcom block, themed
// prime-time nights, a Friday and weekend movie, a Sunday marathon and a late
// movie. Every slot is a smart rule, never a list of titles.
func showcaseRecipes() []showcaseRecipe {
	comedy := []string{"Comedy"}
	hour := []string{"Drama", "Crime", "Mystery", "Action & Adventure"}
	crime := []string{"Crime", "Mystery"}
	action := []string{"Action & Adventure", "Sci-Fi & Fantasy", "Western"}
	scifi := []string{"Sci-Fi & Fantasy"}
	toons := []string{"Animation", "Kids"}
	family := []string{"Family"}
	blockbuster := []string{"Action", "Adventure", "Science Fiction", "Fantasy"}
	matinee := []string{"Adventure", "Family", "Comedy", "Fantasy"}
	sundayFilm := []string{"Drama", "Thriller", "Crime", "Adventure", "Romance"}
	lateFilm := []string{"Horror", "Thriller", "Mystery", "Science Fiction"}
	cartoonsOnly := excluding()
	adultToons := excluding("Kids", "Family")
	noToons := excluding("Animation", "Kids")
	noFantasy := excluding("Animation", "Kids", "Sci-Fi & Fantasy")
	sat, sun := []int{6}, []int{0}

	type era struct {
		id, number, name, look string
		reel, decade           string
		from, to               int
		votes, filmVotes       int
		lateNight              ProgramSource
		primeMon, primeWed     ProgramSource
		monName, wedName       string
		primeComedyExclude     []string
	}
	build := func(e era) showcaseRecipe {
		f, t := e.from, e.to
		v := e.votes
		r := showcaseRecipe{id: e.id, version: 3, number: e.number, name: e.name, lookID: e.look, reelID: e.reel, decade: e.decade, seed: int64(f), era: [2]int{f, t}}
		r.blocks = []Block{
			block("The Late Movie", 0, 2, everyDay, moviePool("Late movies", rated(films(f, t, lateFilm, e.filmVotes/2), ""), excluding("Animation", "Family"), 40)),
			block("All-night reruns", 2, 6, everyDay, lineup("Overnight dramas", series(f-4, t, append(append([]string(nil), hour...), "Sci-Fi & Fantasy"), v/2, 38, 70), noToons, 4, 8, 7)),
			block("Wake-up cartoons", 6, 9, weekdays, lineup("Morning cartoons", rated(series(f, t, toons, v/2, 8, 35), "PG"), cartoonsOnly, 6, 12, 7)),
			block("Daytime comedy reruns", 9, 12, weekdays, lineup("Syndicated sitcoms", rated(series(f-6, t-5, comedy, v, 18, 35), "PG"), noToons, 4, 8, 14)),
			block("Afternoon mysteries", 12, 15, weekdays, lineup("Afternoon detectives", series(f-6, t, crime, v, 38, 70), noToons, 3, 9, 7)),
			block("After school", 15, 17, weekdays, lineup("After-school cartoons", rated(series(f+3, t, append(append([]string(nil), toons...), "Family"), v/2, 8, 35), "PG"), cartoonsOnly, 4, 8, 7)),
			block("Sitcom hour", 17, 20, weekdays, lineup("Early-evening sitcoms", rated(series(f+2, t, comedy, v, 18, 35), "PG"), noToons, 6, 12, 14)),
			block(e.monName, 20, 23, []int{1}, e.primeMon),
			block("Comedy night", 20, 23, []int{2}, lineup("Tuesday comedies", series(f+4, t, comedy, v*2, 18, 35), e.primeComedyExclude, 6, 12, 4)),
			block(e.wedName, 20, 23, []int{3}, e.primeWed),
			block("Thursday comedy", 20, 23, []int{4}, lineup("Thursday comedies", series(f, t, comedy, v*4, 18, 35), e.primeComedyExclude, 6, 12, 4)),
			block("Friday night movie", 20, 23, []int{5}, moviePool("Friday movies", films(f, t, blockbuster, e.filmVotes), excluding("Animation", "Horror"), 40)),
			block("Saturday morning cartoons", 6, 12, sat, lineup("Saturday morning", rated(series(f, t, toons, v/2, 8, 35), "PG"), cartoonsOnly, 8, 16, 2)),
			block("Saturday matinee", 12, 15, sat, moviePool("Matinee movies", rated(films(f, t, matinee, e.filmVotes), "PG-13"), excluding("Horror"), 40)),
			block("Weekend adventure", 15, 20, sat, lineup("Adventure series", series(f-4, t, action, v, 38, 70), noToons, 5, 10, 2)),
			block("Saturday night movie", 20, 23, sat, moviePool("Saturday movies", films(f, t, blockbuster, e.filmVotes), excluding("Animation"), 40)),
			block("Sunday cartoons", 6, 9, sun, lineup("Sunday cartoons", rated(series(f, t, toons, v/2, 8, 35), "PG"), cartoonsOnly, 6, 12, 2)),
			block("Sunday marathon", 9, 13, sun, lineup("Marathon", series(f, t, append(append([]string(nil), comedy...), hour...), v*4, 18, 70), noToons, 1, 12, 1)),
			block("Sunday afternoon movie", 13, 17, sun, moviePool("Sunday afternoon movies", rated(films(f, t, matinee, e.filmVotes), "PG-13"), excluding("Horror"), 40)),
			block("Family hour", 17, 20, sun, lineup("Family comedies", rated(series(f, t, family, v, 18, 35), "PG"), noToons, 4, 8, 2)),
			block("Sunday night movie", 20, 23, sun, moviePool("Sunday movies", films(f, t, sundayFilm, e.filmVotes), excluding("Animation", "Horror"), 40)),
			block("Late night", 23, 24, everyDay, e.lateNight),
		}
		r.filler = lineup("Short cartoons", series(f-10, t, []string{"Animation", "Comedy", "Family", "Kids"}, 5, 3, 15), nil, 8, 8, 0)
		return r
	}
	recipes := []showcaseRecipe{
		build(era{id: "chroma-80", number: "1980", name: "CHROMA 80", look: "showcase-80", reel: "showcase-reel-80", decade: "1980s", from: 1978, to: 1989, votes: 40, filmVotes: 800,
			monName: "Action night", primeMon: lineup("Monday action", series(1978, 1989, action, 80, 38, 70), noToons, 3, 6, 4),
			wedName: "Drama night", primeWed: lineup("Wednesday dramas", series(1978, 1989, hour, 60, 38, 70), noFantasy, 3, 6, 4),
			primeComedyExclude: noToons,
			lateNight:          lineup("Late-night comedy", rated(series(1983, 1989, comedy, 60, 18, 35), ""), noToons, 2, 6, 7)}),
		build(era{id: "riot-90", number: "1990", name: "RIOT 90", look: "showcase-90", reel: "showcase-reel-90", decade: "1990s", from: 1988, to: 1999, votes: 60, filmVotes: 1000,
			monName: "Sci-fi night", primeMon: lineup("Monday sci-fi", series(1988, 1999, scifi, 120, 38, 70), noToons, 3, 6, 4),
			wedName: "Drama night", primeWed: lineup("Wednesday dramas", series(1988, 1999, hour, 100, 38, 70), noFantasy, 3, 6, 4),
			primeComedyExclude: noToons,
			lateNight:          lineup("Late-night animation", rated(series(1989, 1999, []string{"Animation"}, 150, 18, 35), ""), adultToons, 2, 6, 7)}),
		build(era{id: "orbit-00", number: "2000", name: "ORBIT 00", look: "showcase-00", reel: "showcase-reel-00", decade: "2000s", from: 1998, to: 2009, votes: 100, filmVotes: 1500,
			monName: "Crime time", primeMon: lineup("Monday crime", series(1998, 2009, crime, 200, 38, 70), noFantasy, 3, 6, 4),
			wedName: "Sci-fi night", primeWed: lineup("Wednesday sci-fi", series(1998, 2009, scifi, 200, 38, 70), noToons, 3, 6, 4),
			primeComedyExclude: excluding("Animation", "Kids", "Family"),
			lateNight:          lineup("Late-night animation", rated(series(1998, 2009, []string{"Animation"}, 300, 10, 35), ""), adultToons, 2, 6, 7)}),
		build(era{id: "shift-10", number: "2010", name: "SHIFT 10", look: "showcase-10", reel: "showcase-reel-10", decade: "2010s", from: 2008, to: 2019, votes: 150, filmVotes: 2000,
			monName: "Hero night", primeMon: lineup("Monday heroes", series(2008, 2019, []string{"Sci-Fi & Fantasy", "Action & Adventure"}, 300, 38, 70), noToons, 3, 6, 4),
			wedName: "Prestige drama", primeWed: lineup("Wednesday dramas", series(2008, 2019, []string{"Drama", "Crime"}, 600, 38, 70), noFantasy, 3, 6, 4),
			primeComedyExclude: excluding("Animation", "Kids", "Family"),
			lateNight:          lineup("Late-night animation", rated(series(2008, 2019, []string{"Animation"}, 450, 10, 35), ""), adultToons, 2, 6, 7)}),
	}
	for i := range recipes {
		decadeOnly(&recipes[i], i == 0)
	}
	return recipes
}

// decadeOnly keeps every rule of a recipe inside its own decade, so a series or
// film airs on one decade channel only: The Cleveland Show (2009) is ORBIT 00's,
// never also SHIFT 10's. Only the earliest network reaches back further, for
// the older reruns nobody else carries.
func decadeOnly(r *showcaseRecipe, earliest bool) {
	lo, err := strconv.Atoi(strings.TrimSuffix(r.decade, "s"))
	if err != nil {
		return
	}
	hi := lo + 9
	clamp := func(f *core.Filter) {
		if f == nil {
			return
		}
		if !earliest && f.YearFrom < lo {
			f.YearFrom = lo
		}
		if f.YearTo > hi {
			f.YearTo = hi
		}
	}
	for i := range r.blocks {
		for j := range r.blocks[i].Sources {
			clamp(r.blocks[i].Sources[j].Filter)
		}
	}
	clamp(r.filler.Filter)
}

// channel is the recipe's saved form. Its block IDs are stable so a later
// recipe version and the owner's edits can be compared.
func (r showcaseRecipe) channel(id, number, tz string, now time.Time) Channel {
	loc := zoneOf(tz)
	start := dayStart(now, loc)
	c := Channel{ID: id, Number: number, Name: r.name, Kind: "virtual", Group: "Decades", Enabled: true, Timezone: tz, Epoch: start.UTC(), Seed: r.seed, OffAir: true, Items: []Item{}, Transcode: true}
	look := &ChannelLook{LookID: r.lookID, Ident: true, UpNext: true, OffAir: true, Overlays: "always", Placement: "insert"}
	if r.reelID != "" {
		look.Breaks = "between"
		look.BreakPlacement = "between"
		look.BreakContent = "reel"
		look.ReelID = r.reelID
		if pattern, ok := EraBreakPattern(r.decade); ok {
			pattern.Inside = false
			look.BreakPattern = &pattern
		}
	}
	c.Look = look
	date := start.Format("2006-01-02")
	for i, b := range r.blocks {
		b.ID = fmt.Sprintf("%s-block-%d", id, i)
		b.Items = []Item{}
		b.Sources = append([]ProgramSource(nil), b.Sources...)
		for j := range b.Sources {
			b.Sources[j].ID = fmt.Sprintf("%s-source-0", b.ID)
			if j > 0 {
				b.Sources[j].ID = fmt.Sprintf("%s-source-%d", b.ID, j)
			}
			if b.Sources[j].Kind == "lineup" {
				b.Sources[j].StartDate = date
			}
		}
		c.Blocks = append(c.Blocks, b)
	}
	return c
}

// showcaseRecipeHash covers what an owner edits: identity, programming rules
// and presentation, never the resolved snapshots a refresh rewrites.
func showcaseRecipeHash(c Channel) string {
	strip := c
	strip.Items, strip.Filler, strip.Enabled = nil, nil, false
	strip.Blocks = nil
	for _, b := range c.Blocks {
		b.Items, b.Filler = nil, nil
		b.Sources = stripSnapshots(b.Sources)
		strip.Blocks = append(strip.Blocks, b)
	}
	strip.Sources = stripSnapshots(c.Sources)
	b, _ := json.Marshal(strip)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func stripSnapshots(list []ProgramSource) []ProgramSource {
	out := make([]ProgramSource, 0, len(list))
	for _, s := range list {
		s.Items = nil
		if s.Kind == "lineup" {
			s.Sources = nil
		} else {
			s.Sources = stripSnapshots(s.Sources)
		}
		out = append(out, s)
	}
	return out
}

// seedShowcase records and saves each recipe once. A recipe row outlives its
// channel: deleting the channel is the owner's answer and is never undone.
func (s *Service) seedShowcase(ctx context.Context) error {
	recipes := showcaseRecipes()
	var missing []showcaseRecipe
	for _, r := range recipes {
		var id string
		err := s.o.DB.QueryRowContext(ctx, `SELECT id FROM livetv_showcase WHERE id=?`, r.id).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			missing = append(missing, r)
		} else if err != nil {
			return err
		}
	}
	if len(missing) == 0 {
		return nil
	}
	existing, err := channels(ctx, s.o.DB)
	if err != nil {
		return err
	}
	used := map[string]bool{}
	tz := ""
	for _, c := range existing {
		used[c.Number] = true
		if tz == "" && c.Kind == "virtual" && c.Timezone != "" {
			tz = c.Timezone
		}
	}
	if tz == "" {
		tz = localZoneName()
	}
	now := s.o.Now()
	// One transaction for every missing recipe: livetv_channels is a viewer
	// policy input, so the seed advances the policy epoch once, not once per
	// channel.
	tx, err := s.o.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var added [][2]string
	for _, r := range missing {
		number := r.number
		for n, _ := strconv.Atoi(r.number); used[number]; n++ {
			number = strconv.Itoa(n + 1)
		}
		id := "showcase-" + r.id
		c := r.channel(id, number, tz, now)
		data, err := json.Marshal(c)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO livetv_showcase(id,channel_id,version,recipe_hash,state) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, r.id, id, r.version, showcaseRecipeHash(c), showcasePending)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			continue // another node seeded it
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO livetv_channels(id,data) VALUES(?,?) ON CONFLICT(id) DO NOTHING`, id, string(data)); err != nil {
			return err
		}
		used[number] = true
		added = append(added, [2]string{r.name, number})
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, a := range added {
		s.o.Logger.Info("showcase channel added", "channel", a[0], "number", a[1])
	}
	if len(added) > 0 {
		_ = s.refreshVirtualGuideIfWarm(ctx)
		s.bumpLineup()
		s.RefreshGraphics(ctx)
	}
	return nil
}

func (s *Service) showcaseLoop() {
	defer s.wg.Done()
	timer := s.newTimer(5 * time.Minute)
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	defer timer.Stop()
	for {
		wait := s.refreshShowcaseNext(s.ctx)
		timer.Reset(wait)
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
		}
	}
}

type showcaseRow struct {
	id, channelID, hash, state string
	version                    int
	refreshed, attempted       int64
}

func (s *Service) showcaseCatalogTotals(ctx context.Context) ([2]int, bool) {
	if s.o.Catalog == nil {
		return [2]int{}, false
	}
	movies, err1 := s.o.Catalog.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeMovie}, Limit: 1})
	shows, err2 := s.o.Catalog.Browse(ctx, core.BrowseQuery{Filter: core.Filter{Type: core.TypeShow}, Limit: 1})
	if err1 != nil || err2 != nil {
		return [2]int{}, false
	}
	return [2]int{movies.Total, shows.Total}, true
}

func (s *Service) refreshShowcase(ctx context.Context) {
	_ = s.refreshShowcaseNext(ctx)
}

func (s *Service) refreshShowcaseNext(ctx context.Context) time.Duration {
	rows, err := s.o.DB.QueryContext(ctx, `SELECT id,channel_id,version,recipe_hash,state,refreshed_ms,attempted_ms FROM livetv_showcase ORDER BY id`)
	if err != nil {
		return 5 * time.Minute
	}
	var list []showcaseRow
	for rows.Next() {
		var r showcaseRow
		if rows.Scan(&r.id, &r.channelID, &r.version, &r.hash, &r.state, &r.refreshed, &r.attempted) == nil {
			list = append(list, r)
		}
	}
	rows.Close()
	recipes := map[string]showcaseRecipe{}
	for _, r := range showcaseRecipes() {
		recipes[r.id] = r
	}
	now := s.o.Now()
	nextWait := 24 * time.Hour
	var totals [2]int
	totalsLoaded := false
	for _, row := range list {
		recipe, ok := recipes[row.id]
		if !ok || ctx.Err() != nil {
			continue
		}
		// Fill a pending channel as soon as the catalogue allows; afterwards
		// a daily refresh admits new episodes, series and films. Shipped recipe
		// version upgrades run immediately so existing installs pick up new
		// defaults without waiting out the daily interval.
		every := 24 * time.Hour
		if row.state == showcasePending {
			every = 15 * time.Minute
		}
		last := max(row.attempted, row.refreshed)
		if last > 0 && row.version >= recipe.version {
			if rem := time.UnixMilli(last).Add(every).Sub(now); rem > 0 {
				if rem < nextWait {
					nextWait = rem
				}
				continue
			}
		}
		if row.state == showcasePending && row.attempted > 0 && row.version >= recipe.version {
			if !totalsLoaded {
				totals, totalsLoaded = s.showcaseCatalogTotals(ctx)
			}
			if totalsLoaded {
				s.mu.Lock()
				prev, seen := s.showcasePendingTotals[row.id]
				s.mu.Unlock()
				if seen && prev == totals {
					if every < nextWait {
						nextWait = every
					}
					continue
				}
			}
		}
		if err := s.refreshShowcaseChannel(ctx, row, recipe); err != nil && ctx.Err() == nil {
			s.o.Logger.Debug("showcase refresh deferred", "channel", row.channelID, "error", err)
			if 5*time.Minute < nextWait {
				nextWait = 5 * time.Minute
			}
			continue
		}
		if totalsLoaded || row.state == showcasePending {
			if !totalsLoaded {
				totals, totalsLoaded = s.showcaseCatalogTotals(ctx)
			}
			if totalsLoaded {
				s.mu.Lock()
				if s.showcasePendingTotals == nil {
					s.showcasePendingTotals = map[string][2]int{}
				}
				s.showcasePendingTotals[row.id] = totals
				s.mu.Unlock()
			}
		}
		var updatedState string
		_ = s.o.DB.QueryRowContext(ctx, `SELECT state FROM livetv_showcase WHERE id=?`, row.id).Scan(&updatedState)
		if updatedState == showcasePending && 15*time.Minute < nextWait {
			nextWait = 15 * time.Minute
		}
	}
	if nextWait < time.Second {
		nextWait = time.Second
	}
	return nextWait
}

func (s *Service) channelBusy(id string) bool {
	s.mu.Lock()
	for _, ss := range s.sessions {
		if ss.info.Channel.ID == id {
			s.mu.Unlock()
			return true
		}
	}
	s.mu.Unlock()
	s.broadcasts.mu.Lock()
	defer s.broadcasts.mu.Unlock()
	for key := range s.broadcasts.outputs {
		if key.channelID == id {
			return true
		}
	}
	return false
}

type savedItemBreakMeta struct {
	durationMS            int64
	breakAvailableAfterMS int64
	breakPoints           []BreakPoint
	breakPointsMS         []int64
	breakReleaseID        string
	breakRevision         string
}

func collectChannelBreaks(c *Channel) map[core.RatingKey]savedItemBreakMeta {
	out := map[core.RatingKey]savedItemBreakMeta{}
	visitItems := func(items []Item) {
		for _, it := range items {
			if it.Key == 0 {
				continue
			}
			if len(it.BreakPoints) == 0 && len(it.BreakPointsMS) == 0 && it.BreakReleaseID == "" && it.BreakRevision == "" && it.BreakAvailableAfterMS == 0 {
				continue
			}
			out[it.Key] = savedItemBreakMeta{
				durationMS:            it.DurationMS,
				breakAvailableAfterMS: it.BreakAvailableAfterMS,
				breakPoints:           append([]BreakPoint(nil), it.BreakPoints...),
				breakPointsMS:         append([]int64(nil), it.BreakPointsMS...),
				breakReleaseID:        it.BreakReleaseID,
				breakRevision:         it.BreakRevision,
			}
		}
	}
	var visitSources func([]ProgramSource)
	visitSources = func(sources []ProgramSource) {
		for i := range sources {
			visitItems(sources[i].Items)
			visitSources(sources[i].Sources)
		}
	}
	visitItems(c.Items)
	visitItems(c.Filler)
	visitItems(c.Fallback)
	visitSources(c.Sources)
	for i := range c.Blocks {
		visitItems(c.Blocks[i].Items)
		visitItems(c.Blocks[i].Filler)
		visitSources(c.Blocks[i].Sources)
	}
	for i := range c.Pins {
		visitItems(c.Pins[i].Items)
		visitSources(c.Pins[i].Sources)
	}
	return out
}

func restoreChannelBreaks(c *Channel, saved map[core.RatingKey]savedItemBreakMeta) {
	if len(saved) == 0 {
		return
	}
	applyItems := func(items []Item) {
		for i := range items {
			meta, ok := saved[items[i].Key]
			if !ok || meta.durationMS != items[i].DurationMS {
				continue
			}
			if len(items[i].BreakPoints) == 0 && len(items[i].BreakPointsMS) == 0 && items[i].BreakReleaseID == "" {
				items[i].BreakAvailableAfterMS = meta.breakAvailableAfterMS
				items[i].BreakPoints = append([]BreakPoint(nil), meta.breakPoints...)
				items[i].BreakPointsMS = append([]int64(nil), meta.breakPointsMS...)
				items[i].BreakReleaseID = meta.breakReleaseID
				items[i].BreakRevision = meta.breakRevision
			}
		}
	}
	var applySources func([]ProgramSource)
	applySources = func(sources []ProgramSource) {
		for i := range sources {
			applyItems(sources[i].Items)
			applySources(sources[i].Sources)
		}
	}
	applyItems(c.Items)
	applyItems(c.Filler)
	applyItems(c.Fallback)
	applySources(c.Sources)
	for i := range c.Blocks {
		applyItems(c.Blocks[i].Items)
		applyItems(c.Blocks[i].Filler)
		applySources(c.Blocks[i].Sources)
	}
	for i := range c.Pins {
		applyItems(c.Pins[i].Items)
		applySources(c.Pins[i].Sources)
	}
}

func (s *Service) refreshShowcaseChannel(ctx context.Context, row showcaseRow, recipe showcaseRecipe) error {
	var raw string
	err := s.o.DB.QueryRowContext(ctx, `SELECT data FROM livetv_channels WHERE id=?`, row.channelID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		if row.version < recipe.version {
			_, _ = s.o.DB.ExecContext(ctx, `UPDATE livetv_showcase SET version=? WHERE id=?`, recipe.version, row.id)
		}
		return nil // removed by the owner
	}
	if err != nil {
		return err
	}
	if row.state != showcasePending && s.channelBusy(row.channelID) {
		// Never move a programme under a viewer; the next pass retries.
		return core.ErrNotAvailable
	}
	release, err := s.acquireRefresh(ctx, "showcase:"+row.id)
	if err != nil {
		return err
	}
	defer release()
	if _, err = s.o.DB.ExecContext(ctx, `UPDATE livetv_showcase SET attempted_ms=? WHERE id=?`, s.o.Now().UnixMilli(), row.id); err != nil {
		return err
	}
	var c Channel
	if err = json.Unmarshal([]byte(raw), &c); err != nil || c.Kind != "virtual" {
		return err
	}
	savedBreaks := collectChannelBreaks(&c)
	untouched := showcaseRecipeHash(c) == row.hash
	version := row.version
	if row.version < recipe.version {
		version = recipe.version
		if untouched {
			// A newer shipped recipe replaces one the owner never changed.
			anchor := s.o.Now()
			if !c.Epoch.IsZero() {
				anchor = c.Epoch.In(zoneOf(c.Timezone))
			}
			next := recipe.channel(c.ID, c.Number, c.Timezone, anchor)
			next.Enabled, next.Favorite, next.Epoch = c.Enabled, c.Favorite, c.Epoch
			c = next
		}
	}
	if err = s.expand(ctx, &c); err != nil {
		return err
	}
	// Short-form filler pads the minutes before midnight, where the broadcast
	// day ends on a whole programme. It lives on the padded block.
	if untouched {
		src := recipe.filler
		src.ID = c.ID + "-filler"
		if _, err := s.expandProgramSource(ctx, &src, 0); err == nil {
			filler := interleaveFiller(src.Sources)
			for i := range c.Blocks {
				if c.Blocks[i].Fit == "pad" && len(filler) > 0 {
					c.Blocks[i].Filler = filler
				}
			}
		}
	}
	restoreChannelBreaks(&c, savedBreaks)
	filled := 0
	for _, b := range c.Blocks {
		if len(b.Items) > 0 {
			filled++
		}
	}
	state := row.state
	if state == showcasePending {
		if len(c.Blocks) == 0 || filled*10 < len(c.Blocks)*7 {
			// Not enough of the catalogue yet; keep the channel off the lineup
			// without rewriting livetv_channels or bumping viewer_policy_epoch.
			if row.attempted == 0 {
				s.o.Logger.Info("showcase channel waiting for the catalogue", "channel", c.Name, "filledBlocks", filled, "blocks", len(c.Blocks))
			}
			if version != row.version {
				// A newer shipped recipe is written in full even while the
				// channel waits for the catalogue, so its stored definition
				// and recipe hash stay in step (an untouched channel keeps
				// receiving later upgrades).
				return s.writeShowcase(ctx, row, raw, c, version, state, untouched, false)
			}
			return nil
		}
		state = showcaseLive
		s.o.Logger.Info("showcase channel on air", "channel", c.Name, "filledBlocks", filled, "blocks", len(c.Blocks), "enabled", c.Enabled)
	}
	return s.writeShowcase(ctx, row, raw, c, version, state, untouched, true)
}

func interleaveFiller(series []ProgramSource) []Item {
	var out []Item
	for turn := 0; len(out) < 120; turn++ {
		added := false
		for _, s := range series {
			if turn < len(s.Items) && len(out) < 120 {
				out = append(out, s.Items[turn])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return out
}

func (s *Service) writeShowcase(ctx context.Context, row showcaseRow, raw string, c Channel, version int, state string, untouched, refreshed bool) error {
	if err := validateChannel(&c); err != nil {
		return err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if string(data) != raw {
		res, err := s.o.DB.ExecContext(ctx, `UPDATE livetv_channels SET data=? WHERE id=? AND CAST(data AS TEXT)=?`, string(data), c.ID, raw)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return fmt.Errorf("channel changed during refresh")
		}
		s.invalidate(c.ID, "")
		if err := s.refreshVirtualGuideIfWarm(ctx); err != nil {
			return err
		}
		s.bumpLineup()
		s.RefreshGraphics(ctx)
	}
	hash := row.hash
	if untouched && version != row.version {
		hash = showcaseRecipeHash(c)
	}
	refreshedMS := row.refreshed
	if refreshed {
		refreshedMS = s.o.Now().UnixMilli()
	}
	_, err = s.o.DB.ExecContext(ctx, `UPDATE livetv_showcase SET version=?,recipe_hash=?,state=?,refreshed_ms=? WHERE id=?`, version, hash, state, refreshedMS, row.id)
	return err
}

// localZoneName is the host's IANA zone, so dayparts follow the owner's
// clock: TZ, then the system zone link, then a well-known zone whose offsets
// match the host's through the year.
func localZoneName() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil && tz != "Local" {
			return tz
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			name := target[i+len("zoneinfo/"):]
			if _, err := time.LoadLocation(name); err == nil {
				return name
			}
		}
	}
	if b, err := os.ReadFile(filepath.Clean("/etc/timezone")); err == nil {
		name := strings.TrimSpace(string(b))
		if _, err := time.LoadLocation(name); err == nil && name != "" {
			return name
		}
	}
	year := time.Now().Year()
	probe := []time.Time{time.Date(year, 1, 15, 12, 0, 0, 0, time.UTC), time.Date(year, 4, 1, 12, 0, 0, 0, time.UTC), time.Date(year, 7, 15, 12, 0, 0, 0, time.UTC), time.Date(year, 10, 30, 12, 0, 0, 0, time.UTC), time.Date(year, 11, 15, 12, 0, 0, 0, time.UTC)}
	for _, name := range []string{"America/New_York", "America/Chicago", "America/Denver", "America/Phoenix", "America/Los_Angeles", "America/Anchorage", "Pacific/Honolulu", "America/Halifax", "America/St_Johns", "America/Sao_Paulo", "America/Mexico_City", "Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Berlin", "Europe/Helsinki", "Europe/Athens", "Europe/Moscow", "Africa/Johannesburg", "Asia/Dubai", "Asia/Kolkata", "Asia/Bangkok", "Asia/Singapore", "Asia/Shanghai", "Asia/Tokyo", "Asia/Seoul", "Australia/Perth", "Australia/Brisbane", "Australia/Adelaide", "Australia/Sydney", "Pacific/Auckland", "UTC"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		match := true
		for _, at := range probe {
			_, a := at.In(time.Local).Zone()
			_, b := at.In(loc).Zone()
			if a != b {
				match = false
				break
			}
		}
		if match {
			return name
		}
	}
	return ""
}

// AddShowcaseDefaults is an explicit owner action; startup never creates channels.
func (s *Service) AddShowcaseDefaults(ctx context.Context) error {
	// Explicitly adding the defaults can restore deleted channels. Automatic
	// refresh still respects their tombstones and never recreates them.
	if _, err := s.o.DB.ExecContext(ctx, `DELETE FROM livetv_showcase WHERE NOT EXISTS (SELECT 1 FROM livetv_channels WHERE livetv_channels.id=livetv_showcase.channel_id)`); err != nil {
		return err
	}
	if err := s.seedShowcase(ctx); err != nil {
		return err
	}
	for _, r := range showcaseRecipes() {
		c, err := s.Channel(ctx, "showcase-"+r.id)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if !c.Enabled {
			c.Enabled = true
			if _, err := s.SaveChannel(ctx, c); err != nil {
				return err
			}
		}
	}
	return nil
}
