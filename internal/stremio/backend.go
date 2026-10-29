package stremio

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/braedonsaunders/ether/internal/config"
	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/newznab"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

const (
	movieCategory = 2000
	tvCategory    = 5000
	// maxResults bounds the streams converted from one answer; addons
	// answer popular titles with a few hundred, best first.
	maxResults = 250
	// maxTrackers bounds the trackers carried into a magnet. Debrid
	// services resolve cached torrents by hash; trackers only help the
	// rare uncached add.
	maxTrackers = 8
	// manifestTTL is how long a fetched manifest's served types are reused.
	manifestTTL = time.Hour
	// skipWarnInterval throttles the skipped-streams warning: at most one
	// per Backend (hence per addon host) in the window.
	skipWarnInterval = time.Hour
)

// Backend is a newznab.Backend for one Stremio addon: the indexer Client
// keeps budgets, the result cache, rate-limit pauses and events around it.
type Backend struct {
	base   *url.URL
	client *http.Client

	mu           sync.Mutex
	manifest     *Manifest
	manifestAt   time.Time
	now          func() time.Time
	lastSkipWarn time.Time
	skipWarned   bool
}

var _ newznab.Backend = (*Backend)(nil)

// Service builds and keeps one Backend per addon URL, so a backend's
// manifest survives between searches.
type Service struct {
	client *http.Client

	mu       sync.Mutex
	backends map[string]*Backend
}

// NewService returns a Service. client nil uses a default client with a
// 20 s timeout.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &Service{client: client, backends: map[string]*Backend{}}
}

// Backend returns the backend for an addon indexer (newznab.Options.Backend).
func (s *Service) Backend(cfg config.Indexer) (newznab.Backend, error) {
	base, err := BaseURL(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("indexer %q: %w", cfg.Name, err)
	}
	key := base.String()
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.backends[key]
	if b == nil {
		b = &Backend{base: base, client: s.client, now: time.Now}
		s.backends[key] = b
	}
	return b, nil
}

// Test checks that the addon at cfg.URL can be searched.
func (s *Service) Test(ctx context.Context, cfg config.Indexer) error {
	_, err := FetchManifest(ctx, s.client, cfg.URL)
	return err
}

// Search implements newznab.Backend. Movies are looked up by IMDb id and
// episodes by IMDb id, season and episode; a season-level request (Episode
// 0) asks for the season's first episode and keeps whole-season packs.
// Titles without an IMDb id, and show-level requests, have no answer.
func (b *Backend) Search(ctx context.Context, req core.SearchRequest, reserve func(context.Context, string) error) ([]core.Release, error) {
	imdb := imdbID(req.IDs.IMDB)
	if imdb == "" {
		return nil, nil
	}
	var kind, id string
	packs := false
	switch req.Type {
	case core.TypeMovie:
		kind, id = "movie", imdb
	case core.TypeEpisode, core.TypeSeason:
		if req.Season <= 0 {
			return nil, nil
		}
		ep := req.Episode
		if ep <= 0 {
			ep, packs = 1, true
		}
		kind, id = "series", fmt.Sprintf("%s:%d:%d", imdb, req.Season, ep)
	default:
		return nil, nil
	}
	if !b.serves(ctx, kind) {
		return nil, nil
	}
	if err := reserve(ctx, "hit"); err != nil {
		return nil, err
	}
	var answer struct {
		Streams []stream `json:"streams"`
	}
	if err := getJSON(ctx, b.client, b.base, "/stream/"+kind+"/"+url.PathEscape(id)+".json", maxStreams, &answer); err != nil {
		var he *httpError
		if errors.As(err, &he) && (he.status == http.StatusTooManyRequests || he.status == http.StatusServiceUnavailable && he.retry != "") {
			return nil, &newznab.RateLimitError{RetryAfter: he.retry, Err: err}
		}
		return nil, err
	}
	rels, skippedNoHash, skippedAmbiguous := convert(answer.Streams, req, packs)
	b.logSkipped(skippedNoHash, skippedAmbiguous)
	return rels, nil
}

// logSkipped warns about hash-caused skips at most once per
// skipWarnInterval per Backend; later searches inside the window are
// demoted to Debug. Só o host: a URL do addon pode carregar credenciais
// na path/query.
func (b *Backend) logSkipped(skippedNoHash, skippedAmbiguous int) {
	if skippedNoHash == 0 && skippedAmbiguous == 0 {
		return
	}
	now := b.now()
	b.mu.Lock()
	throttled := b.skipWarned && now.Sub(b.lastSkipWarn) < skipWarnInterval
	if !throttled {
		b.lastSkipWarn, b.skipWarned = now, true
	}
	b.mu.Unlock()
	if throttled {
		slog.Default().Debug("stremio: streams skipped without a usable info hash", "host", b.base.Host, "skippedNoHash", skippedNoHash, "skippedAmbiguous", skippedAmbiguous)
		return
	}
	slog.Default().Warn("stremio: streams skipped without a usable info hash", "host", b.base.Host, "skippedNoHash", skippedNoHash, "skippedAmbiguous", skippedAmbiguous)
}

// serves reports whether the addon answers kind, from a manifest fetched
// at most hourly. An unreadable manifest does not block the search: the
// stream request itself is the authority.
func (b *Backend) serves(ctx context.Context, kind string) bool {
	b.mu.Lock()
	m, at := b.manifest, b.manifestAt
	b.mu.Unlock()
	if m == nil || b.now().Sub(at) > manifestTTL {
		var fetched Manifest
		mctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := getJSON(mctx, b.client, b.base, "/manifest.json", maxManifest, &fetched)
		cancel()
		if err != nil {
			return true
		}
		b.mu.Lock()
		b.manifest, b.manifestAt, m = &fetched, b.now(), &fetched
		b.mu.Unlock()
	}
	return m.serves(kind)
}

// FetchNZB implements newznab.Backend: addon results are torrents.
func (b *Backend) FetchNZB(_ context.Context, r core.Release, _ func(context.Context, string) error) ([]byte, error) {
	return nil, fmt.Errorf("stremio: %q is a torrent release; it streams through a debrid provider", r.Name)
}

// ResolveTorrent implements newznab.Backend. Every addon result already
// carries its info hash and magnet.
func (b *Backend) ResolveTorrent(_ context.Context, r core.Release, _ func(context.Context, string) error) (core.Release, error) {
	if r.InfoHash == "" {
		return r, fmt.Errorf("stremio: %q has no info hash: %w", r.Name, core.ErrReleaseRejected)
	}
	return r, nil
}

// stream is the part of a Stremio stream object Ether reads.
type stream struct {
	Name          string   `json:"name"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	InfoHash      string   `json:"infoHash"`
	FileIdx       *int     `json:"fileIdx"`
	URL           string   `json:"url"`
	Sources       []string `json:"sources"`
	BehaviorHints struct {
		Filename  string `json:"filename"`
		VideoSize int64  `json:"videoSize"`
	} `json:"behaviorHints"`
}

// guidSuffix is the per-file part of a release GUID: the file index when
// the addon gives one, else the requested episode for series.
func guidSuffix(s *stream, req core.SearchRequest, packs bool) string {
	switch {
	case packs:
		return ""
	case s.FileIdx != nil && *s.FileIdx >= 0:
		return "/" + strconv.Itoa(*s.FileIdx)
	case req.Type != core.TypeMovie:
		return fmt.Sprintf("/s%de%d", req.Season, req.Episode)
	}
	return ""
}

// convert turns addon streams into torrent releases, best first as the
// addon ordered them. skippedNoHash counts only streams whose sole cause
// of discard is a missing hash: the name/pack filters (pure, so running
// them first does not change which releases come out, nor their order)
// run before the hash check, streams with conflicting hash labels count
// separately as skippedAmbiguous, and duplicate no-hash streams (same
// chosen name, URL and GUID suffix — the key the release seen-map would
// use, minus the hash itself) count at most once. Including streams
// beyond maxResults: the count walks every stream, but only the first
// maxResults valid ones become releases.
func convert(streams []stream, req core.SearchRequest, packs bool) (out []core.Release, skippedNoHash, skippedAmbiguous int) {
	out = make([]core.Release, 0, min(len(streams), maxResults))
	seen := make(map[string]bool, len(streams))
	seenSkipped := make(map[string]bool, len(streams))
	for i := range streams {
		s := &streams[i]
		text := s.Description
		if text == "" {
			text = s.Title
		}
		torrent, file := names(text, s.BehaviorHints.Filename)
		var name string
		if packs {
			if !isSeasonPack(torrent, req.Season) {
				continue
			}
			name = torrent
		} else if name = pickName(req, file, torrent); name == "" {
			continue
		}
		hash, ambiguous := streamHash(s)
		if ambiguous || hash == "" {
			key := name + "\x00" + s.URL + "\x00" + guidSuffix(s, req, packs)
			if ambiguous {
				key = "ambiguous\x00" + key
			}
			if !seenSkipped[key] {
				seenSkipped[key] = true
				if ambiguous {
					skippedAmbiguous++
				} else {
					skippedNoHash++
				}
			}
			continue
		}
		if len(out) == maxResults {
			continue
		}
		guid := "btih:" + hash + guidSuffix(s, req, packs)
		if seen[guid] {
			continue
		}
		seen[guid] = true
		size := s.BehaviorHints.VideoSize
		if size <= 0 {
			size = parseSize(text)
		}
		rel := core.Release{
			GUID:      guid,
			Name:      name,
			Size:      size,
			Seeders:   parseSeeders(text),
			Info:      describe(name, file, torrent),
			Protocol:  core.ProtocolTorrent,
			InfoHash:  hash,
			MagnetURL: newznab.MagnetFor(hash, firstNonEmpty(torrent, name), streamTrackers(s)),
			Category:  movieCategory,
		}
		if req.Type != core.TypeMovie {
			rel.Category = tvCategory
		}
		rel.DebridCached, rel.DebridUncached = debridHints(s.Name)
		out = append(out, rel)
	}
	return out, skippedNoHash, skippedAmbiguous
}

// imdbID normalises an IMDb id to "tt" + digits, or "".
func imdbID(id string) string {
	id = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(id)), "tt")
	if id == "" || len(id) > 10 {
		return ""
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return "tt" + id
}

// streamHash merges the hashes a stream labels explicitly: its infoHash
// field, its URL and its sources. Distinct labels conflict: ambiguous
// reports it and "" wins, rather than a guess — a conflict is not an
// absence, so callers can count it apart from a merely missing hash.
// Uma fonte rotulada internamente em conflito (ambiguous) também impõe
// "" — mesmo com um hash válido em outra fonte.
// Valores inválidos, sim, são ausência e não bloqueiam um hash válido.
// Unlabelled hex in names, titles, descriptions or file names never counts.
func streamHash(s *stream) (hash string, ambiguous bool) {
	urlHash, urlAmbiguous := hashFromURL(s.URL)
	if urlAmbiguous {
		return "", true
	}
	srcHash, srcAmbiguous := hashFromSources(s.Sources)
	if srcAmbiguous {
		return "", true
	}
	hash = newznab.NormalizeInfoHash(s.InfoHash)
	for _, candidate := range []string{urlHash, srcHash} {
		if candidate == "" {
			continue
		}
		if hash != "" && candidate != hash {
			return "", true
		}
		hash = candidate
	}
	return hash, false
}

// magnetValues parses a magnet URI's query, tolerating unescaped junk in
// other parameters (same tolerance as newznab.InfoHashFromMagnet).
func magnetValues(magnet string) url.Values {
	q := magnet[strings.IndexByte(magnet, ':')+1:]
	q = strings.TrimPrefix(q, "?")
	values, err := url.ParseQuery(q)
	if err != nil {
		values = url.Values{}
		for _, part := range strings.Split(q, "&") {
			if k, v, ok := strings.Cut(part, "="); ok {
				if uv, uerr := url.QueryUnescape(v); uerr == nil {
					v = uv
				}
				values.Add(k, v)
			}
		}
	}
	return values
}

// hashFromMagnet extracts the v1 info hash from a magnet URI's
// xt=urn:btih: parameters. ambiguous reports two or more distinct valid
// hashes inside the same magnet; invalid xt values are absence.
func hashFromMagnet(magnet string) (hash string, ambiguous bool) {
	if !strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
		return "", false
	}
	for _, xt := range magnetValues(magnet)["xt"] {
		if lower := strings.ToLower(xt); strings.HasPrefix(lower, "urn:btih:") {
			candidate := newznab.NormalizeInfoHash(xt[len("urn:btih:"):])
			if candidate == "" {
				continue
			}
			if hash != "" && candidate != hash {
				return "", true
			}
			hash = candidate
		}
	}
	return hash, false
}

// magnetTrackers extracts a magnet URI's tr= announce URLs.
func magnetTrackers(magnet string) []string {
	if !strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
		return nil
	}
	var out []string
	for key, values := range magnetValues(magnet) {
		if !strings.EqualFold(key, "tr") {
			continue
		}
		for _, tr := range values {
			if tr = strings.TrimSpace(tr); tr != "" {
				out = append(out, tr)
			}
		}
	}
	return out
}

// streamMagnets collects every original magnet a stream carries: its URL
// itself, a ?magnet= query value, or a magnet among its sources.
func streamMagnets(s *stream) []string {
	var magnets []string
	raw := strings.TrimSpace(s.URL)
	if strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		magnets = append(magnets, raw)
	} else if u, err := url.Parse(raw); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		for key, values := range u.Query() {
			if strings.EqualFold(key, "magnet") {
				magnets = append(magnets, values...)
			}
		}
	}
	for _, source := range s.Sources {
		if source = strings.TrimSpace(source); strings.HasPrefix(strings.ToLower(source), "magnet:") {
			magnets = append(magnets, source)
		}
	}
	return magnets
}

// hashFromSources reads "dht:<hash>" sources and magnet sources.
// ambiguous reports an internal conflict — between sources, or inside a
// single magnet source — distinguishing it from absence ("", false):
// invalid values are absence and never make a source ambiguous.
func hashFromSources(sources []string) (hash string, ambiguous bool) {
	for _, source := range sources {
		source = strings.TrimSpace(source)
		var candidate string
		if strings.HasPrefix(strings.ToLower(source), "dht:") {
			candidate = newznab.NormalizeInfoHash(source[len("dht:"):])
		} else {
			var magnetAmbiguous bool
			candidate, magnetAmbiguous = hashFromMagnet(source)
			if magnetAmbiguous {
				return "", true
			}
		}
		if candidate == "" {
			continue
		}
		if hash != "" && candidate != hash {
			return "", true
		}
		hash = candidate
	}
	return hash, false
}

// hashFromURL reads a magnet URL, explicit hash query fields (including a
// magnet carried in ?magnet=) or the known Torrentio-style
// /resolve/{provider}/{credential}/{hash}/... route. ambiguous reports an
// internal conflict within the URL itself (e.g. ?info_hash=A&info_hash=B,
// or a magnet with two distinct xt hashes), distinguishing it from
// absence ("", false): invalid values are absence. Arbitrary hexadecimal
// URL values may be credentials or opaque IDs, not torrent identities.
func hashFromURL(raw string) (hash string, ambiguous bool) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		return hashFromMagnet(raw)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	for key, values := range u.Query() {
		switch strings.ToLower(key) {
		case "infohash", "info_hash", "hash", "btih":
			for _, value := range values {
				if candidate := newznab.NormalizeInfoHash(value); candidate != "" {
					if hash != "" && candidate != hash {
						return "", true
					}
					hash = candidate
				}
			}
		case "magnet":
			for _, value := range values {
				candidate, magnetAmbiguous := hashFromMagnet(value)
				if magnetAmbiguous {
					return "", true
				}
				if candidate != "" {
					if hash != "" && candidate != hash {
						return "", true
					}
					hash = candidate
				}
			}
		}
	}
	if hash != "" {
		return hash, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] != "resolve" {
			continue
		}
		switch strings.ToLower(parts[i+1]) {
		case config.DebridRealDebrid, config.DebridAllDebrid, config.DebridPremiumize,
			config.DebridTorBox, config.DebridDebridLink, config.DebridOffcloud:
			return newznab.NormalizeInfoHash(parts[i+3]), false
		}
	}
	return "", false
}

// videoExt reports whether a file name ends in a video container.
func videoExt(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".mkv", ".mp4", ".m4v", ".avi", ".ts", ".m2ts", ".mov", ".wmv", ".webm", ".mpg", ".mpeg":
		return true
	}
	return false
}

// names splits a stream's text into the torrent name and the video file's
// name (without directories or extension). Addons put the torrent name on
// the first line and, for a file inside a pack, the file's path on the
// next; statistics lines (seeders, size, source, languages) follow.
func names(text, filename string) (torrent, file string) {
	for _, line := range strings.Split(text, "\n") {
		if isStatsLine(line) {
			continue
		}
		if line = trimDecoration(line); line == "" {
			continue
		}
		if torrent == "" {
			torrent = line
			continue
		}
		if file == "" && videoExt(line) {
			file = line
		}
	}
	if filename != "" {
		file = filename
	}
	if file != "" {
		file = path.Base(strings.ReplaceAll(file, "\\", "/"))
		if videoExt(file) {
			file = strings.TrimSuffix(file, path.Ext(file))
		}
	}
	if torrent != "" && videoExt(torrent) {
		if file == "" {
			file = strings.TrimSuffix(path.Base(torrent), path.Ext(torrent))
		}
		torrent = strings.TrimSuffix(torrent, path.Ext(torrent))
	}
	return torrent, file
}

// trimDecoration removes the emoji and symbols addons lead lines with
// ("📂 Name", "📄 file.mkv").
func trimDecoration(line string) string {
	line = strings.TrimSpace(line)
	return strings.TrimSpace(strings.TrimLeftFunc(line, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '[' && r != '('
	}))
}

// isStatsLine reports an addon's statistics or flags line.
func isStatsLine(line string) bool {
	for _, marker := range []string{"👤", "💾", "⚙️", "⚙", "🔎", "🌐", "🌎", "🔗", "📦", "🇬🇧"} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	// A line of only flags or separators.
	return strings.IndexFunc(line, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) < 0
}

// pickName chooses the name that describes the video best: the file's own
// name when it identifies the title (and episode), else the torrent's. A
// file in a pack names its episode and its own quality; a foreign-language
// torrent name often names neither.
func pickName(req core.SearchRequest, file, torrent string) string {
	score := func(name string) int {
		if name == "" {
			return -1
		}
		info := releaseparse.Parse(name)
		n := 0
		if releaseparse.TitleMatches(info, req.Title) || (req.OriginalTitle != "" && releaseparse.TitleMatches(info, req.OriginalTitle)) {
			n += 4
		}
		if req.Type != core.TypeMovie && req.Episode > 0 && releaseparse.EpisodeMatches(info, req.Season, req.Episode) {
			n += 2
		}
		if info.Height > 0 {
			n++
		}
		return n
	}
	fs, ts := score(file), score(torrent)
	switch {
	case fs < 0 && ts < 0:
		return ""
	case fs >= ts:
		return file
	default:
		return torrent
	}
}

// describe parses name and fills the quality facts it lacks from the other
// of the stream's names: a pack's episode file often carries only the
// episode, its torrent name the resolution, source, codecs and group.
func describe(name, file, torrent string) core.ReleaseInfo {
	info := releaseparse.Parse(name)
	other := torrent
	if name == torrent {
		other = file
	}
	if other == "" || other == name {
		return info
	}
	o := releaseparse.Parse(other)
	if info.Height == 0 {
		info.Height = o.Height
	}
	if info.Source == core.SourceUnknown {
		info.Source = o.Source
	}
	if info.VideoCodec == "" {
		info.VideoCodec = o.VideoCodec
	}
	if info.AudioCodec == "" {
		info.AudioCodec, info.Channels = o.AudioCodec, o.Channels
	}
	if info.Channels == "" {
		info.Channels = o.Channels
	}
	if info.Year == 0 {
		info.Year = o.Year
	}
	if info.Group == "" {
		info.Group = o.Group
	}
	if info.Edition == "" {
		info.Edition = o.Edition
	}
	if len(info.Languages) == 0 {
		info.Languages = o.Languages
	}
	info.HDR10 = info.HDR10 || o.HDR10
	info.DolbyVision = info.DolbyVision || o.DolbyVision
	info.Atmos = info.Atmos || o.Atmos
	info.Proper = info.Proper || o.Proper
	info.Repack = info.Repack || o.Repack
	return info
}

// isSeasonPack reports a torrent name covering the whole of season.
func isSeasonPack(name string, season int) bool {
	if name == "" {
		return false
	}
	info := releaseparse.Parse(name)
	if len(info.Episodes) > 0 {
		return false
	}
	if info.FullSeason {
		return true
	}
	for _, s := range info.Seasons {
		if s == season {
			return true
		}
	}
	return false
}

var (
	seedersRe = regexp.MustCompile(`👤\s*([\d,.]+)`)
	sizeRe    = regexp.MustCompile(`(?i)💾\s*([\d.,]+)\s*([KMGT]?)i?B\b`)
)

func parseSeeders(text string) int {
	m := seedersRe.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.NewReplacer(",", "", ".", "").Replace(m[1]))
	return n
}

func parseSize(text string) int64 {
	m := sizeRe.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
	if err != nil || v <= 0 {
		return 0
	}
	shift := map[string]uint{"": 0, "K": 10, "M": 20, "G": 30, "T": 40}[strings.ToUpper(m[2])]
	return int64(v * float64(uint64(1)<<shift))
}

// streamTrackers returns the announce URLs carried into the generated
// magnet: the tr= of every original magnet (URL, ?magnet= or sources)
// plus the stream's "tracker:" sources, deduplicated and capped at
// maxTrackers. Without an original magnet it is exactly trackers(sources).
func streamTrackers(s *stream) []string {
	var out []string
	add := func(tr string) {
		tr = strings.TrimSpace(tr)
		if tr == "" || len(out) == maxTrackers {
			return
		}
		for _, x := range out {
			if x == tr {
				return
			}
		}
		out = append(out, tr)
	}
	for _, magnet := range streamMagnets(s) {
		for _, tr := range magnetTrackers(magnet) {
			add(tr)
		}
	}
	for _, tr := range trackers(s.Sources) {
		add(tr)
	}
	return out
}

// trackers returns the announce URLs of a stream's "tracker:" sources.
func trackers(sources []string) []string {
	var out []string
	for _, s := range sources {
		if tr, ok := strings.CutPrefix(s, "tracker:"); ok && tr != "" {
			out = append(out, tr)
			if len(out) == maxTrackers {
				break
			}
		}
	}
	return out
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// hintRe matches a debrid service tag with its availability marker:
// "[RD+]" and "[RD download]", "[TB⚡]" and "[TB⬇️]", "RD ⏳". Torrentio and
// Comet tag Debrid-Link as DL and Offcloud as OC.
var hintRe = regexp.MustCompile(`(?i)\b(RD|TB|AD|PM|DL|OC)\s*(\+|⚡|download|⬇|⏳)`)

var hintKinds = map[string]string{
	"RD": config.DebridRealDebrid,
	"TB": config.DebridTorBox,
	"AD": config.DebridAllDebrid,
	"PM": config.DebridPremiumize,
	"DL": config.DebridDebridLink,
	"OC": config.DebridOffcloud,
}

// debridHints reads the cached markers an addon configured with a debrid
// key puts in a stream's name.
func debridHints(text string) (cached, uncached []string) {
	for _, idx := range hintRe.FindAllStringSubmatchIndex(text, -1) {
		tag, marker := strings.ToUpper(text[idx[2]:idx[3]]), text[idx[4]:idx[5]]
		if tag == "DL" && idx[2] > 0 && (text[idx[2]-1] == '-' || text[idx[2]-1] == '.') {
			continue // "WEB-DL+", "WEB.DL ⬇": a source tag, not Debrid-Link
		}
		kind := hintKinds[tag]
		switch marker {
		case "+", "⚡":
			if !contains(cached, kind) {
				cached = append(cached, kind)
			}
		default:
			if !contains(uncached, kind) {
				uncached = append(uncached, kind)
			}
		}
	}
	return cached, uncached
}
