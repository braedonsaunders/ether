package plex

// Sidecar (external) subtitles: listing cached OpenSubtitles tracks before a
// play session exists, keeping a viewer's choice of one across the
// estimate/probe change, the viewer-driven subtitle search
// (GET/PUT /library/metadata/{key}/subtitles) and the transcode sidecar
// endpoint (/video/:/transcode/universal/subtitles).
//
// Sidecar files are downloaded only when the viewer picks a search result.
// Detail pages and playback list whatever is already cached (SubtitleCatalog);
// a live session lists its enriched streams plus any track picked since.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/experience"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/subtitles"
)

// SubtitleCatalog is optionally implemented by Deps.Subtitles: the sidecars
// already cached for a title and viewer, without network access, skipping
// streams embedded already lists (by ExternalURL) or covers (by language).
type SubtitleCatalog interface {
	CachedSidecars(ctx context.Context, t *core.Title, u *core.User, embedded []core.StreamInfo) []core.StreamInfo
}

// SubtitleOffer is one subtitle search result.
type SubtitleOffer struct {
	ID              int64  // provider file id, passed back to DownloadSubtitle
	Language        string // ISO 639-1
	Title           string // release or file name
	Provider        string // "OpenSubtitles"
	Score           int
	HearingImpaired bool
	Forced          bool
	PerfectMatch    bool // matched the playing file's hash or release
}

// SubtitleSearch is optionally implemented by Deps.Subtitles: Plex's
// on-demand subtitle search and pick.
type SubtitleSearch interface {
	SearchSubtitles(ctx context.Context, t *core.Title, u *core.User, lang, release string) ([]SubtitleOffer, error)
	// DownloadSubtitle fetches an offer returned by SearchSubtitles for t
	// and returns its sidecar stream; core.ErrNotFound for unknown offers.
	DownloadSubtitle(ctx context.Context, t *core.Title, u *core.User, id int64) (core.StreamInfo, error)
}

// SubtitleTimeline is optionally implemented by Deps.Subtitles: a sidecar
// with its cues moved back by offset, for a transcode that starts there.
type SubtitleTimeline interface {
	OpenFormatAt(id, format string, offset time.Duration) (io.ReadCloser, string, error)
}

// ensureSelectedSidecar keeps a viewer's chosen external subtitle on the
// play session when the probe has no external file of that language.
// Cached sidecars omit a language the file already embeds, so an embedded
// PGS (or another track the client cannot draw) would otherwise win the
// decision and Continue Watching would show no SRT. A probed external of
// the same language is left alone: the stored choice follows that release
// sidecar instead of pinning the earlier cached copy beside it.
func ensureSelectedSidecar(ps *core.PlaySession, want *core.StreamInfo) *core.PlaySession {
	if ps == nil || want == nil || want.Type != "subtitle" || !want.External || want.ExternalURL == "" {
		return ps
	}
	for _, st := range ps.Info.Streams {
		if st.Type != "subtitle" || !st.External {
			continue
		}
		if st.ExternalURL == want.ExternalURL || sameLanguage(st.Language, want.Language) {
			return ps
		}
	}
	cp := *ps
	streams := make([]core.StreamInfo, len(ps.Info.Streams)+1)
	copy(streams, ps.Info.Streams)
	streams[len(ps.Info.Streams)] = *want
	cp.Info.Streams = streams
	return &cp
}

// withCachedSidecars appends the viewer's cached sidecars that info does not
// already list. It never touches the network.
func (s *Server) withCachedSidecars(ctx context.Context, u *core.User, t *core.Title, info core.MediaInfo) core.MediaInfo {
	c, ok := s.d.Subtitles.(SubtitleCatalog)
	if !ok || t == nil {
		return info
	}
	extra := c.CachedSidecars(ctx, t, u, info.Streams)
	if len(extra) == 0 {
		return info
	}
	out := info
	out.Streams = append(append(make([]core.StreamInfo, 0, len(info.Streams)+len(extra)), info.Streams...), extra...)
	return out
}

// resolveSubtitle finds a chosen subtitle stream in info: the same sidecar
// source, the same embedded position, else a track of the same language
// (same kind and forced flag preferred). -1 when info has none.
func resolveSubtitle(want *core.StreamInfo, pos int, info core.MediaInfo) int {
	return experience.ResolveSubtitle(want, pos, info)
}

// resolveAudio retains the requested language across estimates and releases.
// Within that language prefer the same codec, channel layout and track label;
// position breaks ties but can never override a language mismatch.
func resolveAudio(want *core.StreamInfo, pos int, info core.MediaInfo) int {
	if want == nil {
		return -1
	}
	return experience.ResolveAudio(want, pos, info)
}

// resolveSelection maps stored track identities onto the current positions. A
// choice that info cannot satisfy is dropped, so viewer preferences apply.
func resolveSelection(sel selection, info core.MediaInfo) selection {
	if sel.audioSet && sel.audioStream != nil {
		sel.audio = resolveAudio(sel.audioStream, sel.audio, info)
		sel.audioSet = sel.audio >= 0
	}
	if !sel.subtitleSet || sel.subtitleStream == nil {
		return sel
	}
	if p := resolveSubtitle(sel.subtitleStream, sel.subtitle, info); p >= 0 {
		sel.subtitle = p
	} else {
		sel.subtitle, sel.subtitleSet = -1, false
	}
	return sel
}

// resolvePlaySelection is resolveSelection for a play request.
func resolvePlaySelection(p *playRequest, info core.MediaInfo) {
	out, err := experience.TranslatePlexTrackIdentity(experience.PlexIssuedIdentity{
		Audio: p.audioStream, AudioPosition: p.audioSel, AudioSet: p.audioSel >= 0,
		Subtitle: p.subStream, SubtitlePosition: p.subSel, SubtitleSet: p.subSet,
	}, info)
	if err != nil {
		return
	}
	p.audioSel = out.Audio
	p.subSel, p.subSet = out.Subtitle, out.SubtitleSet
	if !p.subSet {
		p.subSel = -1
	}
}

// subtitleFormat maps a requested subtitle format to one Ether serves.
func subtitleFormat(v string) string {
	switch strings.ToLower(strings.TrimPrefix(v, ".")) {
	case "vtt", "webvtt":
		return "vtt"
	}
	return "srt"
}

// subtitleLanguage is the language a subtitle search defaults to: the
// viewer's first preferred subtitle language, else English.
func (s *Server) subtitleLanguage(ctx context.Context, u *core.User) string {
	if u == nil {
		return "en"
	}
	if s.d.TrackPreferences != nil {
		if rules, err := s.d.TrackPreferences.GetRules(ctx, u.ID); err == nil && len(rules.SubtitleLanguages) > 0 {
			return rules.SubtitleLanguages[0]
		}
	}
	if len(u.SubtitleLanguages) > 0 {
		return u.SubtitleLanguages[0]
	}
	return "en"
}

// subtitleTitle resolves the playable title of a /library/metadata/{key}/
// subtitles request the viewer may see.
func (s *Server) subtitleTitle(r *http.Request) (*core.Title, bool) {
	keys, err := parseKeys(chi.URLParam(r, "key"))
	if err != nil || len(keys) != 1 || !isPlayableKey(keys[0]) || s.d.Catalog == nil {
		return nil, false
	}
	t, err := s.d.Catalog.Get(r.Context(), keys[0])
	if err != nil || t == nil || !s.visibleTitle(r.Context(), FromContext(r.Context()).User, t) {
		return nil, false
	}
	return t, true
}

// offerKeyPrefix is the key of a title's subtitle search results:
// /library/metadata/{ratingKey}/subtitles/{offer id}. Clients pass it back
// verbatim in PUT .../subtitles?key=.
func offerKeyPrefix(t *core.Title) string {
	return "/library/metadata/" + strconv.FormatInt(int64(t.Key), 10) + "/subtitles/"
}

// searchLevel parses Plex's hearingImpaired/forced search options:
// 0 prefer without, 1 prefer with, 2 only with, 3 only without.
func searchLevel(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > 3 {
		return 0
	}
	return n
}

func keepOffer(level int, has bool) bool {
	switch level {
	case 2:
		return has
	case 3:
		return !has
	}
	return true
}

func preferOffer(level int, has bool) bool {
	if level == 1 {
		return has
	}
	return !has
}

// handleSubtitleSearch is GET /library/metadata/{key}/subtitles
// ?language=&hearingImpaired=&forced=: Plex's subtitle "Search". Results
// are subtitle Streams (streamType 3) whose key a PUT downloads. Without
// a configured subtitle service, or when the provider fails, the answer is
// an empty container so the client stops searching.
func (s *Server) handleSubtitleSearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	t, ok := s.subtitleTitle(r)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	mc := libraryContainer()
	searcher, ok := s.d.Subtitles.(SubtitleSearch)
	if !ok {
		s.write(w, r, mc)
		return
	}
	q := r.URL.Query()
	lang := strings.TrimSpace(q.Get("language"))
	if lang == "" {
		lang = s.subtitleLanguage(ctx, pr.User)
	}
	release := ""
	if pr.User != nil && s.d.Playback != nil {
		if ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, t.Key); ok && ps != nil && ps.Resolution != nil {
			release = ps.Resolution.Release.Name
		}
	}
	offers, err := searcher.SearchSubtitles(ctx, t, pr.User, lang, release)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Info("subtitle search failed", "key", t.Key, "language", lang, "err", err)
		}
		s.write(w, r, mc)
		return
	}
	hi, forced := searchLevel(q.Get("hearingImpaired")), searchLevel(q.Get("forced"))
	var preferred, rest []model.Stream
	prefix := offerKeyPrefix(t)
	for _, o := range offers {
		if !keepOffer(hi, o.HearingImpaired) || !keepOffer(forced, o.Forced) {
			continue
		}
		display := subtitleDisplayTitle(core.StreamInfo{Codec: "srt", Language: o.Language, Forced: o.Forced})
		st := model.Stream{
			ID:                   slotHash(t.Key, "offer/"+strconv.FormatInt(o.ID, 10)),
			StreamType:           3,
			Codec:                "srt",
			Key:                  prefix + strconv.FormatInt(o.ID, 10),
			Forced:               boolPtrIf(o.Forced),
			Language:             languageName(o.Language),
			LanguageTag:          languageTag(o.Language),
			LanguageCode:         languageCode3(o.Language),
			Format:               "srt",
			Title:                o.Title,
			DisplayTitle:         display,
			ExtendedDisplayTitle: extendedTitle(core.StreamInfo{Title: o.Title, Language: o.Language}, display, "SRT"),
			Extra: []model.Attr{
				model.A("hearingImpaired", o.HearingImpaired),
				model.A("perfectMatch", o.PerfectMatch),
				model.A("providerTitle", o.Provider),
				model.A("score", o.Score),
			},
		}
		if preferOffer(hi, o.HearingImpaired) && preferOffer(forced, o.Forced) {
			preferred = append(preferred, st)
		} else {
			rest = append(rest, st)
		}
	}
	mc.Stream = append(preferred, rest...)
	mc.Size = len(mc.Stream)
	s.write(w, r, mc)
}

// handleSubtitlePick is PUT /library/metadata/{key}/subtitles?key=: download
// a search result, add it to the title as an external subtitle stream and
// select it for the viewer.
func (s *Server) handleSubtitlePick(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	t, ok := s.subtitleTitle(r)
	if !ok || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	searcher, ok := s.d.Subtitles.(SubtitleSearch)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	raw, found := strings.CutPrefix(r.URL.Query().Get("key"), offerKeyPrefix(t))
	id, err := strconv.ParseInt(raw, 10, 64)
	if !found || err != nil || id <= 0 {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	st, err := searcher.DownloadSubtitle(ctx, t, pr.User, id)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if ctx.Err() == nil {
			s.log.Warn("subtitle download failed", "key", t.Key, "offer", id, "err", err)
		}
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	s.selections.update(pr.User.ID, int64(t.Key), func(sel *selection) {
		sel.subtitle, sel.subtitleSet, sel.subtitleStream = -1, true, &st
	})
	if s.d.TrackPreferences != nil {
		if err := s.d.TrackPreferences.Remember(ctx, pr.User.ID, t.Key, "subtitle", &st); err != nil {
			s.log.Warn("save stream preference", "error", err)
		}
	}
	w.WriteHeader(http.StatusOK)
}

// handleTranscodeSubtitles is /video/:/transcode/universal/subtitles: the
// selected subtitle of a transcode session. External sidecars are served
// with their cues moved back by the session's start offset (the HLS
// timeline starts there), as SRT or, with format=vtt|webvtt, WebVTT.
// Embedded text tracks are extracted from the session's source as WebVTT
// when the transcoder supports it; image subtitles and unknown sessions
// get an empty container.
func (s *Server) handleTranscodeSubtitles(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	q := r.URL.Query()
	p, perr := s.parsePlayRequest(r)
	sessionID := p.sessionID
	if perr != nil {
		sessionID = sanitizeSessionID(firstNonEmpty(q.Get("session"), q.Get("transcodeSessionId"), pr.SessionID))
	}
	entry, ok := s.transcodes.get(sessionID)
	if !ok || pr.User == nil || entry.userID != pr.User.ID || (pr.ClientID != "" && entry.clientID != "" && entry.clientID != pr.ClientID) || entry.key == 0 || (perr == nil && p.key != entry.key) {
		s.writeEmpty(w, r)
		return
	}
	pos := entry.subtitle
	if perr == nil && q.Get("subtitleStreamID") != "" {
		resolvePlaySelection(&p, entry.info)
		pos = p.subSel
	}
	offset := entry.offset
	if perr == nil && q.Get("offset") != "" {
		offset = p.offset
	}
	if pos < 0 || pos >= len(entry.info.Streams) {
		s.writeEmpty(w, r)
		return
	}
	st := entry.info.Streams[pos]
	if st.Type != "subtitle" {
		s.writeEmpty(w, r)
		return
	}
	if !st.External {
		// Embedded track: extract it from the playing source so a subtitle
		// picked in-player renders without restarting the session.
		body, actual, ok := s.extractEmbeddedSubtitle(r.Context(), entry.playID, st, subtitleFormat(q.Get("format")), offset)
		if !ok {
			s.writeEmpty(w, r)
			return
		}
		w.Header().Set("Content-Type", subtitleContentTypes[actual])
		_, _ = w.Write(body)
		return
	}
	if !strings.HasPrefix(st.ExternalURL, "ether-subtitle:") {
		s.writeEmpty(w, r)
		return
	}
	timeline, ok := s.d.Subtitles.(SubtitleTimeline)
	if !ok {
		s.writeEmpty(w, r)
		return
	}
	title, err := s.d.Catalog.Get(r.Context(), entry.key)
	if err != nil || !s.visibleTitle(r.Context(), pr.User, title) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	reader, actual, err := timeline.OpenFormatAt(st.ExternalURL, subtitleFormat(q.Get("format")), offset)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	defer reader.Close()
	w.Header().Set("Content-Type", subtitleContentTypes[actual])
	_, _ = io.Copy(w, io.LimitReader(reader, 32<<20))
}

func (s *Server) extractEmbeddedSubtitle(ctx context.Context, playID string, st core.StreamInfo, format string, offset time.Duration) ([]byte, string, bool) {
	ex, ok := s.d.Transcoder.(core.SubtitleExtractor)
	if !ok || s.d.Playback == nil || playID == "" {
		return nil, "", false
	}
	input := s.d.Playback.InputURL(playID)
	if input == "" {
		return nil, "", false
	}
	var buf bytes.Buffer
	if err := ex.ExtractSubtitle(ctx, input, st.Index, st.Codec, &buf); err != nil || buf.Len() == 0 {
		return nil, "", false
	}
	body := buf.Bytes()
	if format != "vtt" {
		// ExtractSubtitle always emits WebVTT; re-render as SRT.
		cues, perr := subtitles.ParseCues(string(body))
		if perr != nil {
			return nil, "", false
		}
		if offset > 0 {
			shifted, serr := subtitles.Shift(cues, -offset, 1)
			if serr != nil {
				return nil, "", false
			}
			cues = shifted
		}
		body = []byte(subtitles.RenderSRT(cues))
		format = "srt"
	} else if offset > 0 {
		cues, perr := subtitles.ParseCues(string(body))
		if perr != nil {
			return nil, "", false
		}
		shifted, serr := subtitles.Shift(cues, -offset, 1)
		if serr != nil {
			return nil, "", false
		}
		body = []byte(subtitles.RenderVTT(shifted))
	}
	return body, format, true
}

func (s *Server) serveEmbeddedStream(w http.ResponseWriter, r *http.Request, key core.RatingKey, info core.StreamInfo) {
	pr := FromContext(r.Context())
	if pr == nil || pr.User == nil || info.Type != "subtitle" || s.d.Playback == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	var playID string
	var offset time.Duration
	var bestAt time.Time
	for _, id := range s.transcodes.ids() {
		e, ok := s.transcodes.get(id)
		if !ok || e.userID != pr.User.ID || e.key != key || e.playID == "" {
			continue
		}
		if pr.ClientID != "" && e.clientID != "" && e.clientID != pr.ClientID {
			continue
		}
		if playID == "" || e.updatedAt.After(bestAt) {
			playID, offset, bestAt = e.playID, e.offset, e.updatedAt
		}
	}
	if playID == "" && pr.ClientID != "" {
		if ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, key); ok && ps != nil {
			playID = ps.ID
		}
	}
	if playID == "" {
		for _, ps := range s.d.Playback.Sessions() {
			if ps.UserID == pr.User.ID && ps.Key == key {
				playID = ps.ID
				break
			}
		}
	}
	q := r.URL.Query()
	if q.Get("offset") != "" {
		if sec, err := strconv.ParseFloat(q.Get("offset"), 64); err == nil && sec >= 0 {
			offset = time.Duration(sec * float64(time.Second))
		}
	}
	format := subtitleFormat(q.Get("format"))
	if _, ext, ok := strings.Cut(chi.URLParam(r, "stream"), "."); ok && q.Get("format") == "" {
		format = subtitleFormat(ext)
	}
	body, actual, ok := s.extractEmbeddedSubtitle(r.Context(), playID, info, format, offset)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", subtitleContentTypes[actual])
	_, _ = w.Write(body)
}
