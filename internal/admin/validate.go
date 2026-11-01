package admin

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/libraryfilter"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/releasepolicy"
)

// FieldError is one validation problem, addressed by dotted field path.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e FieldError) Error() string { return e.Field + ": " + e.Message }

type validator struct{ errs []FieldError }

func (v *validator) add(field, format string, args ...any) {
	v.errs = append(v.errs, FieldError{Field: field, Message: fmt.Sprintf(format, args...)})
}
func (v *validator) port(field string, n int, allowZero bool) {
	if allowZero && n == 0 {
		return
	}
	if n < 1 || n > 65535 {
		v.add(field, "must be a port between 1 and 65535")
	}
}
func (v *validator) httpURL(field, s string, required bool) {
	if s == "" {
		if required {
			v.add(field, "is required")
		}
		return
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		v.add(field, "must be an http(s) URL")
	}
}
func (v *validator) oneOf(field, s string, allowed ...string) {
	for _, a := range allowed {
		if s == a {
			return
		}
	}
	v.add(field, "must be one of: %s", strings.Join(allowed, ", "))
}

// clock accepts an empty value (default) or a 24-hour "HH:MM" time.
func (v *validator) clock(field, s string) {
	if s == "" {
		return
	}
	if _, err := time.Parse("15:04", s); err != nil || len(s) != 5 {
		v.add(field, "must be a 24-hour time like 03:00")
	}
}

// ValidateConfig checks a candidate config for the problems the owner can
// fix in the UI. It is intentionally permissive about empty optional
// sections (an unconfigured install must remain saveable mid-wizard) and
// strict about structurally broken values.
func ValidateConfig(cfg config.Config) []FieldError {
	v := &validator{}
	if err := cfg.Posters.Validate(); err != nil {
		v.add("posters", "%s", err.Error())
	}
	a := cfg.Availability
	if a.MovieEvidenceDays < 0 || a.MovieEvidenceDays > 366 {
		v.add("availability.movieEvidenceDays", "must be between 1 and 366 days (0 uses the default)")
	}
	if a.PollMinutes < 5 || a.PollMinutes > 1440 {
		v.add("availability.pollMinutes", "must be between 5 and 1440 minutes")
	}
	if a.MaxPages < 1 || a.MaxPages > 20 {
		v.add("availability.maxPages", "must be between 1 and 20")
	}
	for key, n := range map[string]int{"feedRequests": a.FeedRequests, "checkRequests": a.CheckRequests, "grabs": a.Grabs, "checks": a.Checks} {
		if n < 0 || n > 100000 {
			v.add("availability."+key, "must be between 0 and 100000 (0 disables this work)")
		}
	}
	if a.CheckBytes < 0 || a.CheckBytes > 100<<30 {
		v.add("availability.checkBytes", "must be between 0 and 100 GiB")
	}
	if a.ReservePercent < 0 || a.ReservePercent > 99 {
		v.add("availability.reservePercent", "must be between 0 and 99")
	}

	validateClientIdentity(v, cfg)

	if cfg.SharedCache != nil {
		shared := cfg
		shared.Cache, shared.SharedCache = *cfg.SharedCache, nil
		shared.Cache.Dir = cfg.Cache.Dir
		shared.Cache.Storage = ""
		for _, e := range ValidateConfig(shared) {
			if strings.HasPrefix(e.Field, "cache.") {
				v.add("sharedCache."+strings.TrimPrefix(e.Field, "cache."), "%s", e.Message)
			}
		}
	}
	if cfg.Cache.Storage != "" && cfg.Cache.Storage != "local" && cfg.Cache.Storage != "shared" {
		v.add("cache.storage", "must be local or shared")
	}
	if cfg.Cache.Storage == "shared" && (!filepath.IsAbs(cfg.Cache.Dir) || strings.Contains(cfg.Cache.Dir, "://")) {
		v.add("cache.dir", "use an absolute path to an already mounted NAS directory")
	}

	v.port("server.port", cfg.Server.Port, false)
	if _, err := frameOrigins(cfg.Server.WebFrameOrigins); err != nil {
		v.add("server.webFrameOrigins", "%s", err)
	}
	if strings.TrimSpace(cfg.Server.FriendlyName) == "" {
		v.add("server.friendlyName", "is required")
	}
	if strings.TrimSpace(cfg.Server.MoviesLibraryName) == "" {
		v.add("server.moviesLibraryName", "is required")
	}
	if strings.TrimSpace(cfg.Server.TVShowsLibraryName) == "" {
		v.add("server.tvShowsLibraryName", "is required")
	}
	if cfg.Server.LogLevel != "" {
		v.oneOf("server.logLevel", cfg.Server.LogLevel, "debug", "info", "warn", "error")
	}
	if cfg.Server.DefaultStreamLimit < 0 {
		v.add("server.defaultStreamLimit", "must be >= 0")
	}
	if cfg.Server.MaxStreams < 0 {
		v.add("server.maxStreams", "must be >= 0")
	}
	if cfg.Server.ClusterWeight < 0 || cfg.Server.ClusterWeight > 100 {
		v.add("server.clusterWeight", "must be between 0 and 100")
	}

	v.port("plex.publicPort", cfg.Plex.PublicPort, true)
	if cfg.Plex.SecureConnections != "" {
		v.oneOf("plex.secureConnections", cfg.Plex.SecureConnections, "required", "preferred", "disabled")
	}

	seenProv := map[string]bool{}
	for i, p := range cfg.Usenet.Providers {
		f := fmt.Sprintf("usenet.providers[%d]", i)
		if strings.TrimSpace(p.Host) == "" {
			v.add(f+".host", "is required")
		}
		v.port(f+".port", p.Port, false)
		if p.Connections < 0 || p.Connections > 500 {
			v.add(f+".connections", "must be between 0 and 500")
		}
		if p.ID != "" {
			if seenProv[p.ID] {
				v.add(f+".id", "duplicate provider id %q", p.ID)
			}
			seenProv[p.ID] = true
		}
	}

	seenIx := map[string]bool{}
	for i, ix := range cfg.Indexers {
		f := fmt.Sprintf("indexers[%d]", i)
		v.httpURL(f+".url", ix.URL, true)
		if ix.Kind != "" {
			v.oneOf(f+".kind", ix.Kind, config.IndexerKindNewznab, config.IndexerKindCardigann, config.IndexerKindStremio)
		}
		if ix.IsStremio() && ix.Protocol != "" && ix.Protocol != "torrent" {
			v.add(f+".protocol", "must be torrent for a Stremio addon")
		}
		if ix.IsBuiltin() && strings.TrimSpace(ix.Definition) == "" {
			v.add(f+".definition", "is required for a built-in site")
		}
		if ix.DailyLimit < 0 {
			v.add(f+".dailyLimit", "must be >= 0")
		}
		if ix.DailyGrabs < 0 {
			v.add(f+".dailyGrabs", "must be >= 0")
		}
		if ix.Protocol != "" {
			v.oneOf(f+".protocol", ix.Protocol, "usenet", "torrent")
		}
		if ix.ID != "" {
			if seenIx[ix.ID] {
				v.add(f+".id", "duplicate indexer id %q", ix.ID)
			}
			seenIx[ix.ID] = true
		}
	}

	v.httpURL("indexerSites.definitionsUrl", cfg.IndexerSites.DefinitionsURL, false)
	v.httpURL("indexerSites.flareSolverrUrl", cfg.IndexerSites.FlareSolverrURL, false)

	seenDebrid := map[string]bool{}
	for i, p := range cfg.Debrid.Providers {
		f := fmt.Sprintf("debrid.providers[%d]", i)
		v.oneOf(f+".kind", p.Kind, config.DebridKinds...)
		if p.Enabled && strings.TrimSpace(p.APIKey) == "" {
			v.add(f+".apiKey", "is required when the provider is enabled")
		}
		if p.ID != "" {
			if seenDebrid[p.ID] {
				v.add(f+".id", "duplicate provider id %q", p.ID)
			}
			seenDebrid[p.ID] = true
		}
	}
	if cfg.Debrid.ChunkBytes != 0 && (cfg.Debrid.ChunkBytes < 256<<10 || cfg.Debrid.ChunkBytes > 64<<20) {
		v.add("debrid.chunkBytes", "must be between 256 KiB and 64 MiB")
	}
	if cfg.Debrid.MaxConnections < 0 || cfg.Debrid.MaxConnections > 16 {
		v.add("debrid.maxConnections", "must be between 0 and 16")
	}
	if cfg.Debrid.ReadAheadChunks > 32 {
		v.add("debrid.readAheadChunks", "must be at most 32")
	}

	if cfg.Cache.PreloadBudgetBytes < 0 || (cfg.Cache.BudgetBytes > 0 && cfg.Cache.PreloadBudgetBytes > cfg.Cache.BudgetBytes) {
		v.add("cache.preloadBudgetBytes", "must fit inside the total cache budget")
	}
	if cfg.Cache.PreloadEnabled && (cfg.Cache.Mode == "off" || cfg.Cache.PreloadBudgetBytes <= 0) {
		v.add("cache.preloadEnabled", "requires persistent storage and a positive preload allocation")
	}
	if cfg.Cache.PreloadMaxTitles < 0 || cfg.Cache.PreloadMaxTitles > 5000 {
		v.add("cache.preloadMaxTitles", "must be between 0 and 5000")
	}
	if cfg.Cache.PreloadRateMiB < 0 || cfg.Cache.PreloadRateMiB > 10000 {
		v.add("cache.preloadRateMiB", "must be between 0 and 10000")
	}
	if cfg.Cache.PreloadMinFreeBytes < 0 {
		v.add("cache.preloadMinFreeBytes", "must not be negative")
	}
	if c := cfg.Cache.PreloadContent; c != "" && c != "mixed" && c != "movies" && c != "tv" {
		v.add("cache.preloadContent", "must be mixed, movies or tv")
	}
	if w := cfg.Cache.PreloadHouseholdWeight; w != nil && (*w < 0 || *w > 100) {
		v.add("cache.preloadHouseholdWeight", "must be between 0 and 100")
	}
	if s := cfg.Cache.PreloadHouseholdScope; s != "" && s != "home" && s != "all" {
		v.add("cache.preloadHouseholdScope", "must be home or all")
	}
	if n := cfg.Cache.PreloadEpisodesAhead; n < 0 || n > 24 {
		v.add("cache.preloadEpisodesAhead", "must be between 0 and 24 (0 uses 3)")
	}
	if n := cfg.Cache.PreloadHistoryDays; n < 0 || n > 730 {
		v.add("cache.preloadHistoryDays", "must be between 0 and 730 (0 uses 90)")
	}
	if cfg.Cache.PreloadSource != "" && cfg.Cache.PreloadSource != "popular" && cfg.Cache.PreloadSource != "recent" && cfg.Cache.PreloadSource != "smart" {
		v.add("cache.preloadSource", "must be smart, popular or recent")
	}
	switch cfg.Cache.Mode {
	case "", "demand", "adaptive", "off":
	default:
		v.add("cache.mode", "must be demand, adaptive or off")
	}
	if cfg.Cache.RetentionDays < 0 || cfg.Cache.RetentionDays > 3650 {
		v.add("cache.retentionDays", "must be between 0 and 3650")
	}
	if cfg.Cache.ReplayBoostHours < 0 || cfg.Cache.ReplayBoostHours > 720 {
		v.add("cache.replayBoostHours", "must be between 0 and 720")
	}
	if cfg.Cache.DetailWarmup != "" && cfg.Cache.DetailWarmup != "on" && cfg.Cache.DetailWarmup != "off" {
		v.add("cache.detailWarmup", "must be on or off")
	}
	if cfg.Cache.DetailBytes < 0 || cfg.Cache.DetailBytes > 64<<20 {
		v.add("cache.detailBytes", "must be between 0 and 64 MiB")
	}
	if cfg.Cache.InProgressPinPercent < 0 || cfg.Cache.InProgressPinPercent > 100 {
		v.add("cache.inProgressPinPercent", "must be between 0 and 100")
	}
	if cfg.Cache.InProgressPinMaxAge.D() < 0 {
		v.add("cache.inProgressPinMaxAge", "must not be negative")
	}
	if strings.TrimSpace(cfg.Cache.Dir) == "" {
		v.add("cache.dir", "is required")
	}
	if cfg.Cache.BudgetBytes < 0 {
		v.add("cache.budgetBytes", "must be >= 0")
	}
	if cfg.Cache.PrefetchBytes < 0 {
		v.add("cache.prefetchBytes", "must be >= 0")
	}
	if cfg.Cache.NextEpisodeAt < 0 || cfg.Cache.NextEpisodeAt > 1 {
		v.add("cache.nextEpisodeAt", "must be between 0 and 1")
	}

	if cfg.Catalog.MinPopularity < 0 {
		v.add("catalog.minPopularity", "must be >= 0")
	}
	if _, err := libraryfilter.Compile(cfg.Catalog.Exclusions); err != nil {
		v.add("catalog.exclusions", "%s", err.Error())
	}
	if cfg.Catalog.MinVotes < 0 {
		v.add("catalog.minVotes", "must be >= 0")
	}

	if cfg.Resolver.HealthSamples < 0 {
		v.add("resolver.healthSamples", "must be >= 0")
	}
	switch cfg.Resolver.SourcePreference {
	case "", "auto", "usenet", "debrid":
	default:
		v.add("resolver.sourcePreference", `must be "auto", "usenet" or "debrid"`)
	}
	if cfg.Resolver.SourceScoreMargin < 0 {
		v.add("resolver.sourceScoreMargin", "must be >= 0")
	}
	if cfg.Resolver.MaxCandidates < 0 {
		v.add("resolver.maxCandidates", "must be >= 0")
	}
	if n := cfg.Resolver.SameQualityAlternatives; n < 0 || n > 3 {
		v.add("resolver.sameQualityAlternatives", "must be between 0 and 3")
	}

	profiles := map[string]bool{}
	for i, p := range cfg.Quality.Profiles {
		f := fmt.Sprintf("quality.profiles[%d]", i)
		if strings.TrimSpace(p.Name) == "" {
			v.add(f+".name", "is required")
			continue
		}
		if profiles[p.Name] {
			v.add(f+".name", "duplicate profile name %q", p.Name)
		}
		profiles[p.Name] = true
		if _, err := releasepolicy.Compile(p.ReleaseRules); err != nil {
			v.add(f+".releaseRules", "%s", err)
		} else if msg := releasepolicy.Conflict(p.ReleaseRules); msg != "" {
			v.add(f+".releaseRules", "%s", msg)
		}
		if p.MinHeight < 0 || p.MaxHeight < 0 || (p.MaxHeight > 0 && p.MinHeight > p.MaxHeight) {
			v.add(f+".minHeight", "must be <= maxHeight")
		}
		if p.MinSizeMBPerMin < 0 || p.MaxSizeMBPerMin < 0 ||
			(p.MaxSizeMBPerMin > 0 && p.MinSizeMBPerMin > p.MaxSizeMBPerMin) {
			v.add(f+".minSizeMbPerMin", "must be <= maxSizeMbPerMin")
		}
		if p.EpisodeMinSizeMBPerMin < 0 || p.EpisodeMaxSizeMBPerMin < 0 ||
			(p.EpisodeMaxSizeMBPerMin > 0 && p.EpisodeMinSizeMBPerMin > p.EpisodeMaxSizeMBPerMin) {
			v.add(f+".episodeMinSizeMbPerMin", "must be <= episodeMaxSizeMbPerMin")
		}
	}
	if cfg.Quality.Default != "" && len(profiles) > 0 && !profiles[cfg.Quality.Default] {
		v.add("quality.default", "names a profile that does not exist")
	}
	if cfg.Quality.Remote != "" && len(profiles) > 0 && !profiles[cfg.Quality.Remote] {
		v.add("quality.remote", "names a profile that does not exist")
	}
	for i, r := range cfg.Quality.Rules {
		if r.Profile == "" {
			v.add(fmt.Sprintf("quality.rules[%d].profile", i), "is required")
		} else if len(profiles) > 0 && !profiles[r.Profile] {
			v.add(fmt.Sprintf("quality.rules[%d].profile", i), "names a profile that does not exist")
		}
		if r.MaxHeight < 0 {
			v.add(fmt.Sprintf("quality.rules[%d].maxHeight", i), "must be >= 0")
		}
	}

	if cfg.Transcode.HWAccel != "" {
		v.oneOf("transcode.hwAccel", cfg.Transcode.HWAccel, "auto", "none", "videotoolbox", "nvenc", "qsv", "vaapi")
	}
	if cfg.Transcode.MaxTranscodes < 0 {
		v.add("transcode.maxTranscodes", "must be >= 0")
	}
	if cfg.Transcode.RemoteMaxBitrateKbps < 0 {
		v.add("transcode.remoteMaxBitrateKbps", "must be >= 0")
	}
	rtx := cfg.Transcode.RTXVideo
	if rtx.Mode != "" {
		v.oneOf("transcode.rtxVideo.mode", rtx.Mode, "off", "auto", "on")
	}
	for _, limit := range []struct {
		name            string
		value, min, max int
	}{
		{"sourceMaxHeight", rtx.SourceMaxHeight, 144, 2160},
		{"targetMaxHeight", rtx.TargetMaxHeight, 144, 2160},
		{"maxConcurrent", rtx.MaxConcurrent, 1, 8},
	} {
		if limit.value != 0 && (limit.value < limit.min || limit.value > limit.max) {
			v.add("transcode.rtxVideo."+limit.name, "must be between %d and %d (0 uses the default)", limit.min, limit.max)
		}
	}
	switch rtx.Quality {
	case 0, 1, 2, 3, 4, 21, 23:
	default:
		v.add("transcode.rtxVideo.quality", "must be 1, 2, 3, 4, 21 or 23 (0 uses the default)")
	}

	if p, ok := config.LookupLLMProvider(cfg.LLM.Provider); !ok {
		v.add("llm.provider", "is not a supported provider")
	} else {
		if p.Local {
			v.httpURL("llm.baseUrl", cfg.LLM.BaseURL, cfg.LLM.Enabled && p.RequiresBaseURL)
		}
		if cfg.LLM.Enabled {
			if p.NeedsKey && strings.TrimSpace(cfg.LLM.APIKey) == "" {
				v.add("llm.apiKey", "is required for %s", p.Label)
			}
			if strings.TrimSpace(cfg.LLM.Model) == "" {
				v.add("llm.model", "is required when the language model is enabled")
			}
		}
	}
	if cfg.Simkl.Enabled && strings.TrimSpace(cfg.Simkl.ClientID) == "" {
		v.add("simkl.clientId", "is required when Simkl is enabled")
	}
	if cfg.Trakt.Enabled && strings.TrimSpace(cfg.Trakt.ClientID) == "" {
		v.add("trakt.clientId", "is required when Trakt is enabled")
	}
	if cfg.Updates.Channel != "" {
		v.oneOf("updates.channel", cfg.Updates.Channel, "stable", "beta")
	}
	if cfg.Updates.FeedURL != "" {
		v.httpURL("updates.feedUrl", cfg.Updates.FeedURL, false)
	}
	v.clock("updates.maintenanceStart", cfg.Updates.MaintenanceStart)
	v.clock("updates.maintenanceEnd", cfg.Updates.MaintenanceEnd)
	if cfg.Updates.MaxDrain < 0 {
		v.add("updates.maxDrain", "must not be negative")
	}
	return v.errs
}

// validateClientIdentity checks the global indexer identity policy and each
// indexer's override. Values are scoped: "same" is only a global grab value,
// an empty value inherits (or means ether), and a custom pair must be safe
// to put on the wire before it can be saved.
func validateClientIdentity(v *validator, cfg config.Config) {
	p := cfg.ClientIdentity
	identityValue(v, "clientIdentity.searchTv", p.SearchTV, false)
	identityValue(v, "clientIdentity.searchMovies", p.SearchMovies, false)
	identityValue(v, "clientIdentity.grabTv", p.GrabTV, true)
	identityValue(v, "clientIdentity.grabMovies", p.GrabMovies, true)
	pUsesCustom := p.SearchTV == newznab.IdentityCustom || p.SearchMovies == newznab.IdentityCustom ||
		p.GrabTV == newznab.IdentityCustom || p.GrabMovies == newznab.IdentityCustom
	validateCustomIdentity(v, "clientIdentity", p.UserAgent, p.Headers, pUsesCustom)
	for i, ix := range cfg.Indexers {
		f := fmt.Sprintf("indexers[%d]", i)
		identityValue(v, f+".searchIdentity", ix.SearchIdentity, false)
		identityValue(v, f+".grabIdentity", ix.GrabIdentity, false)
		usesCustom := ix.SearchIdentity == newznab.IdentityCustom || ix.GrabIdentity == newznab.IdentityCustom
		validateCustomIdentity(v, f, ix.UserAgent, ix.Headers, usesCustom)
	}
}

func identityValue(v *validator, field, name string, allowSame bool) {
	switch {
	case name == "":
	case name == config.IdentitySame:
		if !allowSame {
			v.add(field, "must be a preset; \"same\" is only valid for the global grab values")
		}
	case !newznab.ValidIdentity(name):
		v.oneOf(field, name, newznab.IdentityPresets()...)
	}
}

func validateCustomIdentity(v *validator, field, userAgent string, headers map[string]string, required bool) {
	if !required && userAgent == "" && len(headers) == 0 {
		return
	}
	// A user agent is required only by a selected custom identity; headers
	// kept from an earlier selection stay dormant until it is selected again.
	if required || userAgent != "" {
		if err := newznab.ValidateIdentityUserAgent(userAgent); err != nil {
			v.add(field+".userAgent", "%s", err)
		}
	}
	if err := newznab.ValidateIdentityHeaders(headers); err != nil {
		v.add(field+".headers", "%s", err)
	}
}
