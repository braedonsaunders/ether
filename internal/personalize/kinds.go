package personalize

import (
	"sort"
	"strings"

	"github.com/braedonsaunders/ether/internal/core"
)

// Row kinds. A kind is the stable, owner-facing identity of a row family:
// layout rules, insights and the bandit all work per kind. Hub ids are per
// row ("home.because.<key>"), kinds are not.
const (
	KindFavorites     = "favorites"
	KindContinue      = "continue-watching"
	KindUpNext        = "up-next"
	KindTop10         = "top-10"
	KindTop10Today    = "top-10-today"
	KindShowcase      = "genre-showcase"
	KindBecause       = "because-you-watched"
	KindPerson        = "more-from-person"
	KindTrendingTaste = "trending-in-your-taste"
	KindHiddenGems    = "hidden-gems"
	KindNewThisWeek   = "new-this-week"
	KindComfort       = "comfort-rewatches"
	KindDifferent     = "something-different"
	KindShortOnTime   = "short-on-time"
	KindFranchise     = "finish-the-franchise"
	KindLLM           = "llm"
	KindTrending      = "trending"
	// Crowd rows (crowd.go): public co-engagement over the viewer's
	// favourites, and its intersection with what is trending now.
	KindCrowd         = "fans-also-loved"
	KindCrowdTrending = "trending-with-fans"
	KindPopularServer = "popular-on-server"
	KindNewReleases   = "new-releases"
	KindTopRated      = "top-rated"
	KindPopular       = "popular"
	KindAiringToday   = "airing-today"
	KindTraktRecs     = "trakt-recommendations"
	KindTraktWatch    = "trakt-watchlist"
	KindRecentlyAdded = "recently-added" // built by the Plex front end in sections
	KindPlaylists     = "playlists"      // built by the Plex front end on Home

	// Movies → Recommended lenses (lenses.go): type-native rows that
	// explore the viewer's film taste from one angle each.
	KindTasteCluster  = "taste-cluster"      // "Heist Thrillers", "Sci-Fi Thrillers"
	KindEra           = "era-favourites"     // "Best of the '90s for You"
	KindWorld         = "world-cinema"       // "Korean Cinema for You"
	KindOccasion      = "occasion"           // "Date Night", "Family Night", "Under 100 Minutes"
	KindAcclaimed     = "acclaimed-unseen"   // acclaimed titles by fit
	KindTopGenre      = "top-rated-in-genre" // "Top-Rated Thrillers" in the viewer's genres
	KindBridge        = "taste-bridge"       // "Where <Seed A> Meets <Seed B>"
	KindStudio        = "more-from-studio"   // "More from A24"
	KindNewSeasons    = "new-seasons"        // TV Shows → Recommended (lenses_tv.go)
	KindFinishShow    = "finish-what-you-started"
	KindStartNew      = "start-something-new"
	KindBinge         = "complete-series"
	KindLimited       = "limited-series"
	KindAiringTaste   = "airing-for-you"
	KindNetwork       = "more-from-network"
	KindSticksLanding = "sticks-the-landing"
	KindStandalone    = "standalone-episodes"
	// KindCollectionPrefix + collection id is an owner collection row.
	KindCollectionPrefix = "collection:"
)

// kindInfo describes a row kind.
type kindInfo struct {
	kind        string
	title       string // generic title for the owner UI
	description string
	// prior is the expected play rate of a row of this kind before any
	// evidence (plays per row-day shown). It seeds the bandit.
	prior float64
	// slug is the hub id stem; dynamic rows append ".<id>".
	slug string
	// context is the Plex hub context stem ("hub.<section>.<context>").
	context string
	// cap is the most rows of this kind on one screen (0 = 1).
	cap int
	// dynamic kinds have one row per seed (person, title, franchise...).
	dynamic bool
	source  string
	// surfaces is where the row appears by default, with the row's prior
	// weight on that surface (surfaces.go). A surface not listed is not a
	// default home for the row, but an owner policy can still place the
	// row anywhere it fits.
	surfaces map[string]float64
	// fits lists the surfaces the row can ever appear on (nil = every
	// surface): a TV lens never fits Movies.
	fits []string
	// group is the Home Designer category (core.RowDescriptor.Group).
	group string
	// sectionCap is the most rows of this kind on a Movies or TV Shows
	// page (0 = cap).
	sectionCap int
	// lean classifies the row for the owner's tuning dials (tuning.go).
	lean rowLean
}

// Default surface weights.
var (
	onHome     = map[string]float64{surfHome: 1}
	onSections = map[string]float64{surfMovies: 1, surfShows: 1}
	onMovies   = map[string]float64{surfMovies: 1}
	onShows    = map[string]float64{surfShows: 1}
	everywhere = map[string]float64{surfHome: 1, surfMovies: 1, surfShows: 1}
	fitsMovies = []string{surfHome, surfMovies}
	fitsShows  = []string{surfHome, surfShows}
	fitsPages  = []string{surfMovies, surfShows}
)

var kinds = []kindInfo{
	{kind: KindContinue, title: "Continue Watching", description: "Titles the viewer is part-way through. Pin, schedule or hide this row with Home policies.",
		prior: 0.30, slug: "continue", context: "continue", source: core.RowSourcePlex, surfaces: onHome, group: groupPlex},
	{kind: KindUpNext, title: "Up Next", description: "The next episode of shows the viewer recently finished an episode of.",
		prior: 0.22, slug: "ondeck", context: "ondeck", surfaces: onHome, fits: fitsShows, group: groupPicks},
	{kind: KindTop10, title: "Top 10 for You", description: "The ten titles the taste model rates highest right now.",
		prior: 0.11, slug: "top10", context: "toppicks", surfaces: everywhere, group: groupPicks},
	{kind: KindTop10Today, title: "Top 10 Today", description: "The server's Top 10 today: trending now, catalogue popularity and what viewers here watch, with trustworthy ratings. Leads new viewers' Homes.",
		prior: 0.09, slug: "top10.today", context: "top10.today", surfaces: everywhere, group: groupHousehold, lean: leanFresh},
	{kind: KindShowcase, title: "Acclaimed <Genre>", description: "Acclaimed, popular titles of one broad genre. New viewers get three from different genre families, rotating daily, to learn their taste.",
		prior: 0.07, slug: "showcase", context: "showcase", cap: 3, dynamic: true, surfaces: map[string]float64{surfHome: 1, surfMovies: 0.9, surfShows: 0.9}, group: groupExplore, lean: leanExplore},
	{kind: KindBecause, title: "Because You Watched…", description: "Titles similar to something the viewer recently finished. One row per recent favourite, rotating daily; at most two on Home.",
		prior: 0.09, slug: "because", context: "because", cap: 2, sectionCap: 2, dynamic: true, surfaces: map[string]float64{surfHome: 1, surfMovies: 0.7, surfShows: 0.7}, group: groupPicks, lean: leanFamiliar},
	{kind: KindPerson, title: "More from…", description: "More work from a director, writer or actor the viewer keeps coming back to.",
		prior: 0.07, slug: "person", context: "person", cap: 2, sectionCap: 3, dynamic: true, surfaces: onSections, group: groupExplore, lean: leanFamiliar},
	{kind: KindTrendingTaste, title: "Trending in Your Taste", description: "This week's trending titles, filtered and ranked by the viewer's taste.",
		prior: 0.08, slug: "trending.taste", context: "trending.taste", surfaces: onSections, group: groupExplore, lean: leanFresh},
	{kind: KindHiddenGems, title: "Hidden Gems for You", description: "Highly rated, little-known titles that fit the viewer's taste.",
		prior: 0.06, slug: "hiddengems", context: "hiddengems", surfaces: onSections, group: groupExplore, lean: leanExplore},
	{kind: KindNewThisWeek, title: "New This Week", description: "Fresh releases in the viewer's favourite genres.",
		prior: 0.07, slug: "newthisweek", context: "newthisweek", surfaces: onHome, group: groupPicks, lean: leanFresh},
	{kind: KindComfort, title: "Comfort Rewatches", description: "Favourites the viewer has finished, rewatched or rated highly and not seen in a while.",
		prior: 0.05, slug: "comfort", context: "comfort", surfaces: onHome, group: groupPicks, lean: leanFamiliar | leanEvergreen},
	{kind: KindDifferent, title: "Something Different", description: "Exploration: acclaimed titles outside the viewer's usual genres.",
		prior: 0.04, slug: "different", context: "different", surfaces: map[string]float64{surfMovies: 0.8, surfShows: 0.8}, group: groupExplore, lean: leanExplore},
	{kind: KindShortOnTime, title: "Short on Time", description: "Shorter films and episodes, fitted to how long the viewer usually watches in this context.",
		prior: 0.06, slug: "shortontime", context: "shortontime", surfaces: onHome, group: groupPicks},
	{kind: KindFranchise, title: "Finish the Franchise", description: "The remaining films of a franchise the viewer has started. Off by default: it is usually one or two titles. Add it to a page to use it.",
		prior: 0.09, slug: "franchise", context: "franchise", cap: 2, dynamic: true, surfaces: map[string]float64{}, fits: fitsMovies, group: groupExplore, lean: leanFamiliar}, // off unless the owner places it: usually one or two titles
	{kind: KindLLM, title: "Themed Rows (AI)", description: "Natural-language themed rows from the owner's LLM, seeded by recent favourites. Off by default; add it to a page to use it.",
		prior: 0.06, slug: "llm", context: "llm", cap: 2, dynamic: true, surfaces: map[string]float64{}, group: groupPicks},
	{kind: KindCrowd, title: "Fans of What You Watch Also Loved", description: "What people who loved the viewer's favourites also loved: TMDB's crowd recommendations aggregated over up to 20 recent favourites, strongest where several favourites agree, plus what similar viewers here watched. Films and series separately on the Movies and TV pages.",
		prior: 0.09, slug: "crowd", context: "crowd", surfaces: map[string]float64{surfHome: 1, surfMovies: 0.9, surfShows: 0.9}, group: groupPicks},
	{kind: KindCrowdTrending, title: "Trending With Fans of What You Watch", description: "Titles trending right now (TMDB, and Trakt when it is set up) that fans of the viewer's favourites love: trendy, but personal.",
		prior: 0.08, slug: "crowd.trending", context: "crowd.trending", surfaces: everywhere, group: groupPicks, lean: leanFresh},
	{kind: KindTrending, title: "Trending Now", description: "What is trending on TMDB this week.",
		prior: 0.06, slug: "trending", context: "trending", surfaces: everywhere, group: groupExplore, lean: leanFresh},
	{kind: KindPopularServer, title: "Popular on This Server", description: "What other viewers on this server watched most in the last 30 days.",
		prior: 0.07, slug: "popular.server", context: "popular.server", surfaces: map[string]float64{surfHome: 1, surfMovies: 0.8, surfShows: 0.8}, group: groupHousehold},
	{kind: KindNewReleases, title: "New & Noteworthy", description: "The newest released titles in the catalogue.",
		prior: 0.05, slug: "newreleases", context: "recentlyreleased", surfaces: everywhere, group: groupExplore, lean: leanFresh},
	{kind: KindTopRated, title: "Top Rated", description: "TMDB's top rated titles.",
		prior: 0.045, slug: "toprated", context: "toprated", surfaces: onSections, group: groupExplore, lean: leanEvergreen},
	{kind: KindPopular, title: "Popular", description: "TMDB's most popular titles.",
		prior: 0.045, slug: "popular", context: "popular", surfaces: onSections, group: groupExplore},
	{kind: KindAiringToday, title: "Airing Today", description: "Shows with an episode airing today.",
		prior: 0.04, slug: "airing", context: "airing", surfaces: onShows, fits: fitsShows, group: groupShows, lean: leanFresh},
	{kind: KindTraktRecs, title: "Trakt Recommendations", description: "Personal recommendations from the viewer's connected Trakt account.",
		prior: 0.06, slug: "trakt.recommendations", context: "trakt", surfaces: map[string]float64{surfHome: 1, surfMovies: 0.8, surfShows: 0.8}, group: groupPicks},
	{kind: KindTraktWatch, title: "Your Trakt Watchlist", description: "The viewer's Trakt watchlist.",
		prior: 0.08, slug: "trakt.watchlist", context: "trakt.watchlist", surfaces: map[string]float64{surfHome: 1, surfMovies: 0.8, surfShows: 0.8}, group: groupPicks},
	{kind: KindRecentlyAdded, title: "Recently Added", description: "Built by the Plex front end at the top of each library section.",
		prior: 0.05, slug: "recentlyadded", context: "recentlyadded", source: core.RowSourcePlex, surfaces: onSections, fits: fitsPages, group: groupPlex},
	{kind: KindPlaylists, title: "Recent Playlists", description: "The viewer's playlists, built by the Plex front end at the end of Home.",
		prior: 0.03, slug: "playlists", context: "playlists", source: core.RowSourcePlex, surfaces: onHome, fits: []string{surfHome}, group: groupPlex},

	// ---- Movies → Recommended ----
	{kind: KindTasteCluster, title: "Your Taste Clusters", description: "Named rows built from the viewer's strongest genre and theme combinations (\"Heist Thrillers\", \"Sci-Fi Thrillers\"). Several distinct rows, films and series separately.",
		prior: 0.08, slug: "cluster", context: "cluster", cap: 1, sectionCap: 3, dynamic: true, surfaces: onSections, group: groupExplore},
	{kind: KindEra, title: "Best of the <Decade> for You", description: "Unseen films from the decade the viewer watches most, ranked by taste.",
		prior: 0.06, slug: "era", context: "era", dynamic: true, surfaces: onMovies, fits: fitsMovies, group: groupExplore, lean: leanEvergreen},
	{kind: KindWorld, title: "<Language> Cinema for You", description: "Films in a language the viewer's history shows they watch (or world cinema across several), ranked by taste.",
		prior: 0.06, slug: "world", context: "world", dynamic: true, surfaces: onMovies, fits: fitsMovies, group: groupExplore, lean: leanExplore},
	{kind: KindOccasion, title: "Occasions", description: "Date Night and Family Night when the viewer's history supports them, and Under 100 Minutes; each favoured at the times it suits.",
		prior: 0.06, slug: "occasion", context: "occasion", cap: 1, sectionCap: 2, dynamic: true, surfaces: onMovies, fits: fitsMovies, group: groupExplore},
	{kind: KindAcclaimed, title: "Acclaimed & Unseen", description: "Films with a trustworthy high rating the viewer hasn't seen, ranked by how well they fit.",
		prior: 0.06, slug: "acclaimed", context: "acclaimed", surfaces: onMovies, fits: fitsMovies, group: groupExplore, lean: leanEvergreen},
	{kind: KindTopGenre, title: "Top-Rated <Genre>", description: "The best-rated titles in each of the viewer's favourite genres, films and series separately.",
		prior: 0.05, slug: "toprated.genre", context: "toprated.genre", cap: 1, sectionCap: 2, dynamic: true, surfaces: onSections, group: groupExplore, lean: leanEvergreen},
	{kind: KindBridge, title: "Where <A> Meets <B>", description: "Multi-seed bridge rows connecting two distinct favourites in the viewer's history.",
		prior: 0.085, slug: "bridge", context: "bridge", cap: 1, sectionCap: 2, dynamic: true, surfaces: onSections, group: groupExplore, lean: leanExplore},
	{kind: KindStudio, title: "More from <Studio>", description: "More films from a studio or boutique label (A24, Neon, Studio Ghibli...) the viewer keeps watching.",
		prior: 0.065, slug: "studio", context: "studio", cap: 1, sectionCap: 2, dynamic: true, surfaces: onMovies, fits: fitsMovies, group: groupExplore, lean: leanFamiliar},

	// ---- TV Shows → Recommended ----
	{kind: KindNewSeasons, title: "New Seasons of Your Shows", description: "Shows the viewer watched that have a new season out they haven't started.",
		prior: 0.12, slug: "newseasons", context: "newseasons", surfaces: onShows, fits: fitsShows, group: groupShows, lean: leanFresh},
	{kind: KindFinishShow, title: "Finish What You Started", description: "Series the viewer started and stalled on for two weeks or more, closest to finished first.",
		prior: 0.10, slug: "finish", context: "finish", surfaces: onShows, fits: fitsShows, group: groupShows, lean: leanFamiliar},
	{kind: KindStartNew, title: "Start Something New", description: "Series the viewer hasn't started, ranked by fit: start with the pilot.",
		prior: 0.07, slug: "startnew", context: "startnew", surfaces: onShows, fits: fitsShows, group: groupShows, lean: leanExplore},
	{kind: KindBinge, title: "Complete Series to Binge", description: "Ended series with every season out, that fit the viewer's taste.",
		prior: 0.07, slug: "binge", context: "binge", surfaces: onShows, fits: fitsShows, group: groupShows, lean: leanEvergreen},
	{kind: KindSticksLanding, title: "Series That Stick the Landing", description: "Completed series whose finale and final season rated as high as or higher than where they began.",
		prior: 0.08, slug: "stickslanding", context: "stickslanding", surfaces: onShows, fits: fitsShows, group: groupShows, lean: leanEvergreen},
	{kind: KindLimited, title: "Limited Series for You", description: "One-season stories with an ending, ranked by fit.",
		prior: 0.06, slug: "limited", context: "limited", surfaces: onShows, fits: fitsShows, group: groupShows},
	{kind: KindAiringTaste, title: "Currently Airing for You", description: "Series in an active run (airing today, or returning and recently premiered) that match the viewer's taste.",
		prior: 0.07, slug: "airing.taste", context: "airing.taste", surfaces: onShows, fits: fitsShows, group: groupShows, lean: leanFresh},
	{kind: KindNetwork, title: "More from <Network>", description: "More series from a network or studio the viewer keeps watching.",
		prior: 0.06, slug: "network", context: "network", cap: 1, sectionCap: 2, dynamic: true, surfaces: onShows, fits: fitsShows, group: groupShows, lean: leanFamiliar},
	{kind: KindStandalone, title: "Standalone & Anthology Episodes", description: "Acclaimed standalone and anthology episodes that can be watched in one sitting with zero multi-season commitment.",
		prior: 0.065, slug: "standalone", context: "standalone", surfaces: onShows, fits: fitsShows, group: groupShows},
	{kind: KindFavorites, title: "Favorites", description: "Titles this viewer marked as favorites.", prior: 0.20, slug: "favorites", context: "favorites", surfaces: map[string]float64{}, group: groupPicks},
}

var kindByName = func() map[string]kindInfo {
	m := make(map[string]kindInfo, len(kinds))
	for _, k := range kinds {
		if k.cap == 0 {
			k.cap = 1
		}
		if k.source == "" {
			k.source = core.RowSourceEngine
		}
		if k.sectionCap == 0 {
			k.sectionCap = k.cap
		}
		if k.surfaces == nil {
			k.surfaces = everywhere
		}
		m[k.kind] = k
	}
	return m
}()

// infoFor returns the description of a kind; owner collection kinds share
// one generic description.
func infoFor(kind string) kindInfo {
	if k, ok := kindByName[kind]; ok {
		return k
	}
	if strings.HasPrefix(kind, KindCollectionPrefix) {
		return kindInfo{kind: kind, title: "Owner collection", description: "A collection designed in Collection Studio.",
			prior: 0.06, slug: "collection", context: "collection", cap: 1, sectionCap: 1, source: core.RowSourceOwner,
			surfaces: everywhere, group: groupCollections}
	}
	return kindInfo{kind: kind, title: kind, prior: 0.05, slug: kind, context: kind, cap: 1, sectionCap: 1, source: core.RowSourceEngine, surfaces: everywhere}
}

// banditKind is the kind used for the bandit's global prior pool: owner
// collections share one global prior but keep per-collection evidence.
func priorKind(kind string) string {
	if strings.HasPrefix(kind, KindCollectionPrefix) {
		return "collection"
	}
	return kind
}

// descriptors lists every row kind for the Home Designer, with the pages
// each can appear on and its designer group.
func descriptors(extra []core.RowDescriptor) []core.RowDescriptor {
	out := make([]core.RowDescriptor, 0, len(kinds)+len(extra))
	for _, k := range kinds {
		out = append(out, core.RowDescriptor{HubID: k.kind, Title: k.title, Kind: k.kind, Description: k.description,
			Sections: append([]string(nil), kindFits(k)...), Group: k.group})
	}
	sort.SliceStable(extra, func(i, j int) bool { return extra[i].Title < extra[j].Title })
	return append(out, extra...)
}

// sectionPrefix returns the hub id / context prefix for a section type.
func sectionPrefix(t core.MediaType) string {
	switch t {
	case core.TypeMovie:
		return "movie"
	case core.TypeShow:
		return "tv"
	}
	return "home"
}

// sectionName is the layout-rule section name for a section type.
func sectionName(t core.MediaType) string {
	switch t {
	case core.TypeMovie:
		return "movies"
	case core.TypeShow:
		return "shows"
	}
	return "home"
}
