package resolver

import (
	"testing"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

func TestLanguagePreferencesAcceptISOCodesAndRegionalVariants(t *testing.T) {
	s := New(&fakeIndexer{}, nil, nil, testCfg(), Options{Now: func() time.Time { return time.Unix(1800000000, 0) }})
	defer s.Close()
	base := withParsed(rel("language", "The.Signal.1999.1080p.WEB-DL.H264-GRP", 4000))
	score := func(audio, subs []string, prefs ...string) float64 {
		r := base
		r.Info.Languages, r.Info.SubtitleLanguages = audio, subs
		return s.score(r, core.QualityProfile{PreferredLanguages: prefs}, matrixTitle())
	}
	one := func(l string) []string { return []string{l} }
	// The owner UI asks for ISO codes; they must match parsed names.
	if score(one("french"), nil, "fr") <= score(one("english"), nil, "fr") {
		t.Fatal(`preference "fr" did not favour a French release`)
	}
	latino := score(one(releaseparse.LanguageSpanishLatino), nil, "es-419")
	castilian := score(one("spanish"), nil, "es-419")
	english := score(one("english"), nil, "es-419")
	if !(latino > castilian && castilian > english) {
		t.Fatalf("es-419: latino %.1f castilian %.1f english %.1f", latino, castilian, english)
	}
	// A plain Spanish preference takes either variant.
	if score(one(releaseparse.LanguageSpanishLatino), nil, "es") != score(one("spanish"), nil, "es") {
		t.Fatal("plain Spanish preference distinguished variants")
	}
	if score(one(releaseparse.LanguagePortugueseBrazil), nil, "pt-BR") <= score(one("portuguese"), nil, "pt-BR") {
		t.Fatal("pt-BR preference did not favour Brazilian Portuguese")
	}
	// VOSTFR is French subtitles over the original audio: it never counts as
	// French audio, so a French-audio release still wins, and it is not
	// penalised as foreign audio either.
	vostfr := score(nil, one("french"), "fr")
	if french := score(one("french"), nil, "fr"); vostfr >= french {
		t.Fatalf("VOSTFR %.1f scored like French audio %.1f", vostfr, french)
	}
	if neutral := score(nil, nil, "fr"); vostfr <= neutral {
		t.Fatalf("French subtitles earned nothing: %.1f vs %.1f", vostfr, neutral)
	}
	if german := score(one("german"), one("french"), "fr"); german >= score(nil, nil, "fr") {
		t.Fatal("French subtitles offset a German dub's audio penalty")
	}
}

func TestDubSuitabilityMatchesRegionalPreference(t *testing.T) {
	r := core.Release{Name: "Movie.2019.LATINO.DUBBED.1080p.WEB-DL-GRP"}
	title := &core.Title{Language: "en"}
	if reason := sourceSuitability(r, title, core.QualityProfile{PreferredLanguages: []string{"es-MX"}}, testCfg().Resolver, false); reason != "" {
		t.Fatalf("Latino dub rejected for an es-MX viewer: %s", reason)
	}
	if reason := sourceSuitability(r, title, core.QualityProfile{PreferredLanguages: []string{"de"}}, testCfg().Resolver, false); reason == "" {
		t.Fatal("Latino dub accepted for a German-preferring profile")
	}
}

func TestAbsoluteEpisodeIdentity(t *testing.T) {
	title := &core.Title{Type: core.TypeEpisode, Title: "Episode 1071", ShowTitle: "One Piece"}
	sreq := core.SearchRequest{Type: core.TypeEpisode, Title: "One Piece", Season: 21, Episode: 180, AbsoluteEpisode: 1071, Anime: true}
	match := withParsed(rel("abs", "[SubsPlease] One Piece - 1071 (1080p) [ABCD1234].mkv", 1400))
	if reason := rejectIdentity(match, title, sreq); reason != "" {
		t.Fatalf("absolute 1071 rejected: %s", reason)
	}
	other := withParsed(rel("abs2", "[SubsPlease] One Piece - 1070 (1080p) [ABCD1234].mkv", 1400))
	if reason := rejectIdentity(other, title, sreq); reason != "season/episode mismatch" {
		t.Fatalf("absolute 1070 for 1071: %q", reason)
	}
	// Without a catalogue count, only the first season's numbers are known.
	sreq.AbsoluteEpisode = 0
	if reason := rejectIdentity(match, title, sreq); reason != "season/episode mismatch" {
		t.Fatalf("unknown absolute number accepted: %q", reason)
	}
	first := core.SearchRequest{Type: core.TypeEpisode, Title: "One Piece", Season: 1, Episode: 7}
	if reason := rejectIdentity(withParsed(rel("s1", "[G] One Piece - 07 (1080p)", 1400)), title, first); reason != "" {
		t.Fatalf("season 1 absolute rejected: %s", reason)
	}
	// Numeric movie titles stay movies.
	movie := &core.Title{Type: core.TypeMovie, Title: "300", Year: 2006}
	mreq := core.SearchRequest{Type: core.TypeMovie, Title: "300", Year: 2006}
	if reason := rejectIdentity(withParsed(rel("m", "300.2006.1080p.BluRay.x264-GRP", 8000)), movie, mreq); reason != "" {
		t.Fatalf("numeric movie title rejected: %s", reason)
	}
}

func TestSearchRequestCarriesAbsoluteEpisodeAndAnime(t *testing.T) {
	ep := &core.Title{Key: core.EpisodeKey(37854, 21, 180), Type: core.TypeEpisode, ShowTitle: "One Piece", Title: "x",
		Season: 21, Episode: 180, AbsoluteEpisode: 1071, Language: "ja", Genres: []string{"Animation", "Action"}}
	sreq := searchRequestFor(ep)
	if sreq.AbsoluteEpisode != 1071 || !sreq.Anime {
		t.Fatalf("got absolute %d anime %v", sreq.AbsoluteEpisode, sreq.Anime)
	}
	western := &core.Title{Key: core.EpisodeKey(1, 2, 3), Type: core.TypeEpisode, ShowTitle: "Cartoon", Language: "en", Genres: []string{"Animation"}}
	if searchRequestFor(western).Anime {
		t.Fatal("English animation treated as anime")
	}
	keyword := &core.Title{Key: core.EpisodeKey(1, 2, 3), Type: core.TypeEpisode, ShowTitle: "Co-production", Language: "en", Keywords: []string{"anime"}}
	if !searchRequestFor(keyword).Anime {
		t.Fatal("TMDB anime keyword ignored")
	}
}

// httpRoutingReleases: a well-grabbed Usenet 1080p leads on score; an
// addon's HTTP stream of the same quality tier sits a few points behind.
func httpRoutingReleases() []core.Release {
	rs := routingReleases("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-OTHER")
	h := rel("http", "The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-ADDON", 8000)
	h.Grabs = 0
	h.Protocol = core.ProtocolHTTP
	h.StreamURL = "https://addon.example/resolve/opaque"
	h.DebridCached = []string{"realdebrid"} // an addon's label, not proof
	return []core.Release{rs[0], h}
}

func TestHTTPStreamsRouteWithDebridPreference(t *testing.T) {
	for _, tc := range []struct {
		pref  string
		first string
	}{
		{PreferDebrid, "http"},
		{PreferUsenet, "usenet"},
		// Auto never treats an addon's cached label as known-cached.
		{PreferAuto, "usenet"},
	} {
		cfg := testCfg()
		cfg.Resolver.SourcePreference = tc.pref
		// No debrid account is configured: HTTP streams need none.
		cfg.Debrid.Providers = nil
		svc := New(&fakeIndexer{releases: httpRoutingReleases()}, fakeHealth{}, nil, cfg, Options{})
		svc.SetSourceSignals(cachedSet{})
		got := resolveOrder(t, svc, routingTitle(true))
		if got[0] != tc.first || len(got) != 2 {
			t.Errorf("%s: order %v, want %s first and both kept", tc.pref, got, tc.first)
		}
		svc.Close()
	}
}

func TestHTTPAndTorrentShareDebridRouting(t *testing.T) {
	rs := append(httpRoutingReleases(), routingReleases("The.Signal.1999.1080p.WEB-DL.DDP5.1.H.264-OTHER")[1])
	cfg := testCfg()
	cfg.Resolver.SourcePreference = PreferUsenet
	svc := New(&fakeIndexer{releases: rs}, fakeHealth{}, nil, cfg, Options{})
	defer svc.Close()
	svc.SetSourceSignals(cachedSet{hashA: true})
	got := resolveOrder(t, svc, routingTitle(true))
	if got[0] != "usenet" || len(got) != 3 {
		t.Fatalf("order %v, want usenet first with both debrid-class sources kept", got)
	}
	if !resumeSourceAllowedFor(PreferDebrid, core.ProtocolHTTP) || resumeSourceAllowedFor(PreferUsenet, core.ProtocolHTTP) {
		t.Fatal("HTTP streams must resume under a debrid preference only")
	}
}

// resumeSourceAllowedFor mirrors playback's resume rule through routing's
// source classification.
func resumeSourceAllowedFor(pref, protocol string) bool {
	src := protocolOf(core.Release{Protocol: protocol})
	switch pref {
	case PreferDebrid:
		return src == core.ProtocolTorrent
	case PreferUsenet:
		return src == core.ProtocolUsenet
	}
	return true
}

func TestBrowserReleaseDropsStreamCredentials(t *testing.T) {
	r := core.Release{ID: "x", Name: "n", Protocol: core.ProtocolHTTP, StreamURL: "https://addon.example/KEY/play",
		StreamHeaders: map[string]string{"Authorization": "Bearer KEY"}, StreamOrigin: "https://addon.example", NZBURL: "https://ix/api?apikey=K"}
	got := browserRelease(r)
	if got.StreamURL != "" || got.StreamHeaders != nil || got.NZBURL != "" {
		t.Fatalf("credentials kept: %+v", got)
	}
	if got.StreamOrigin != r.StreamOrigin || got.Protocol != core.ProtocolHTTP {
		t.Fatalf("non-secret fields lost: %+v", got)
	}
}
