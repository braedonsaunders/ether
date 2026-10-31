package plex

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/plex/model"
	"github.com/braedonsaunders/ether/internal/sessions"
	"github.com/braedonsaunders/ether/internal/transcode"
)

// Decision codes. PMS documents the classes (1xxx ok, 2xxx general error,
// 3xxx direct-play error, 4xxx transcode error) but not every member; these
// are the values observed from real servers.
const (
	codeDirectPlayOK      = 1000
	codeConversionOK      = 1001
	codeGeneralError      = 2000
	codeDirectPlayError   = 3000
	codeTranscodeError    = 4000
	textDirectPlayOK      = "Direct play OK."
	textConversionOK      = "Direct play not available; Conversion OK."
	textDirectPlayDisable = "App cannot direct play this item. Direct play is disabled."
)

// User-visible playback failure messages.
const (
	msgNotAvailable = "Not available yet: no playable release of this title was found. Try again later."
	msgStreamLimit  = "Stream limit reached: stop another stream on this server and try again."
	msgPlayFailed   = "This title could not be played right now. Try again in a moment."
	// Setup problems: the owner can fix these, and "not available" would
	// send them looking for a missing release instead.
	msgNoSources        = "Ether has no sources to search: add a Stremio addon or Torznab indexer under Sources › Indexers."
	msgNoTorrentSources = "Ether streams through your debrid account but has no torrent indexer to search: add a Stremio addon or Torznab indexer under Sources › Indexers."
	msgDebridAuth       = "Your debrid service rejected Ether's API key: check the account under Sources › Debrid."
	msgNotCached        = "No cached release on your debrid account right now: every release found was uncached or blocked by the provider. Try again later."
	msgDownloadQueued   = "Not cached yet: Ether sent it to your debrid service to download. Try again in a few minutes."
)

func playbackMessage(err error) string {
	switch {
	case errors.Is(err, core.ErrUnreleased):
		return "This title has not been released yet. Playback is unavailable."
	case errors.Is(err, core.ErrDebridAuth):
		return msgDebridAuth
	case errors.Is(err, core.ErrNoTorrentSources):
		return msgNoTorrentSources
	case errors.Is(err, core.ErrNoSources):
		return msgNoSources
	case errors.Is(err, core.ErrDownloadQueued):
		return msgDownloadQueued
	case onlyNotCached(err):
		return msgNotCached
	case errors.Is(err, core.ErrNotAvailable):
		return msgNotAvailable
	case errors.Is(err, core.ErrStreamLimit):
		return msgStreamLimit
	case errors.Is(err, core.ErrBudgetExhausted):
		return "Indexers are temporarily at their limit. Ether will retry automatically; previously cached releases may still play."
	}
	return msgPlayFailed
}

// onlyNotCached reports whether every cause in err is core.ErrNotCached
// (uncached, blocked or add-refused on the debrid accounts). The resolver
// joins one error per failed candidate, so "every" means each joined branch;
// errors.Is alone would match a single uncached torrent among Usenet
// failures. A multi-%w error is treated the same way, which errs towards
// the general message.
func onlyNotCached(err error) bool {
	for err != nil {
		if err == core.ErrNotCached {
			return true
		}
		switch x := err.(type) {
		case interface{ Unwrap() []error }:
			errs := x.Unwrap()
			if len(errs) == 0 {
				return false
			}
			for _, e := range errs {
				if !onlyNotCached(e) {
					return false
				}
			}
			return true
		case interface{ Unwrap() error }:
			err = x.Unwrap()
		default:
			return false
		}
	}
	return false
}

// playRequest is a parsed decision/start request.
type playRequest struct {
	key         core.RatingKey
	collection  string
	caps        core.ClientCaps
	opts        core.DecideOptions
	sessionID   string // client transcode session id
	offset      time.Duration
	protocol    string
	audioSel    int              // stream position, -1 = default
	audioStream *core.StreamInfo // selected identity, remapped after probing
	subSel      int              // stream position, -1 = none/default
	subSet      bool
	// subStream is the subtitle picked by id (request or stored), to be
	// found again in the session's MediaInfo (resolvePlaySelection).
	subStream *core.StreamInfo
	// mediaIndex is the "Play Version" position the viewer picked
	// (0 = Auto / not sent); partReleaseID names the release directly
	// when the request addressed a variant's Part key.
	mediaIndex    int
	partReleaseID string
	// mde: the client runs its own media decision engine (hasMDE=1, sent
	// by current Plex Web, iOS and Apple TV apps).
	mde bool
	// listAllTracks: Android-family clients receive every probed track in
	// the decision response; recorded PMS decisions for web, iOS and Apple
	// TV are selected-only, so those clients keep that shape. Set from the
	// request context in decisionContainer; zeroed by sameSettings because
	// it is response shaping, not a playback setting (an asymmetric compare
	// would re-prepare and restart the encoder on every start).
	listAllTracks bool
}

// androidFamilyClient reports whether the decision lists every probed track.
// Plex's Android family does; web, iOS and Apple TV stay selected-only.
// That is the PMS response shape, selected by the registry quirk
// plex-list-all-tracks rather than a playback capability.
func androidFamilyClient(pr *Request) bool {
	if pr == nil {
		return false
	}
	return transcode.HasQuirk(pr.Product, pr.Platform, pr.Device, pr.Model, pr.Version, "plex-list-all-tracks")
}

// sameSettings compares playback controls, excluding the seek position (DASH
// spans the title) and device capabilities (clients may omit their profile on
// subsequent starts). Quality limits from the query are also in opts. Keep the
// original options since prepare adds owner limits and track preferences.
func (p playRequest) sameSettings(other playRequest) bool {
	p.offset, other.offset = 0, 0
	p.caps, other.caps = core.ClientCaps{}, core.ClientCaps{}
	p.listAllTracks, other.listAllTracks = false, false
	return reflect.DeepEqual(p, other)
}

func (s *Server) parsePlayRequest(r *http.Request) (playRequest, error) {
	pr := FromContext(r.Context())
	q := r.URL.Query()
	key := keyFromPath(q.Get("path"))
	if key == 0 {
		key = keyFromPath(q.Get("key"))
	}
	if key == 0 {
		key = keyFromPath(q.Get("ratingKey"))
	}
	// A "Play Version" pick may address the chosen Part key directly.
	var partReleaseID string
	if key == 0 {
		for _, v := range []string{q.Get("path"), q.Get("key")} {
			if partID, ok := partIDFromPath(v); ok {
				if k, rel, found := s.partTitle(partID); found {
					key, partReleaseID = k, rel
					break
				}
			}
		}
	}
	if key == 0 || !isPlayableKey(key) {
		return playRequest{}, fmt.Errorf("no playable item in request")
	}
	caps := s.clientCaps(pr)
	applyDecisionQuery(&caps, q)
	// The decision query can identify WAN even when the metadata request
	// looked local. Apply the remote ceiling before source resolution too.
	if limit := s.cfg().Transcode.RemoteMaxBitrateKbps; caps.Remote && limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
		caps.MaxBitrateKbps = limit
	}
	p := playRequest{key: key, caps: caps, audioSel: -1, subSel: -1, protocol: q.Get("protocol"), mde: q.Get("hasMDE") == "1", partReleaseID: partReleaseID}
	for _, raw := range []string{q.Get("path"), q.Get("key")} {
		if u, err := url.Parse(raw); err == nil {
			if c := u.Query().Get("collection"); c != "" {
				p.collection = c
				break
			}
		}
	}
	// "Play Version" selection: position in the rendered Media list.
	// partIndex is always 0 (one Part per Media) and ignored.
	if n, err := strconv.Atoi(q.Get("mediaIndex")); err == nil && n > 0 {
		p.mediaIndex = n
	}
	if p.protocol == "" {
		p.protocol = "hls"
	}
	p.opts = core.DecideOptions{
		DirectPlay:     q.Get("directPlay") != "0",
		DirectStream:   q.Get("directStream") != "0",
		MaxBitrateKbps: caps.MaxBitrateKbps,
		MaxHeight:      parseResolutionHeight(q.Get("videoResolution")),
		AudioStream:    -1,
		SubtitleStream: -1,
	}
	if vb, err := strconv.Atoi(q.Get("videoBitrate")); err == nil && vb > 0 && (p.opts.MaxBitrateKbps == 0 || vb < p.opts.MaxBitrateKbps) {
		p.opts.MaxBitrateKbps = vb
	}
	switch q.Get("subtitles") {
	case "burn":
		p.opts.BurnSubtitles = true
	case "none":
		p.subSet = true
	}
	// Explicit stream ids in the request win over stored selections.
	var userID int64
	if pr.User != nil {
		userID = pr.User.ID
	}
	sel := s.selections.get(userID, int64(key))
	if sel.audioSet {
		p.audioSel, p.audioStream = sel.audio, sel.audioStream
	}
	if sel.subtitleSet {
		p.subSel, p.subSet, p.subStream = sel.subtitle, true, sel.subtitleStream
	}
	if id, err := strconv.ParseInt(q.Get("audioStreamID"), 10, 64); err == nil {
		binding, rerr := s.resolveStream(sessions.WithPlexServe(r.Context(), pr.Remote, ""), pr.User, id)
		if rerr != nil && !streamMiss(rerr) {
			return playRequest{}, rerr
		}
		if rerr == nil && binding.Key == key && binding.Info.Type == "audio" {
			info := binding.Info
			p.audioSel, p.audioStream = binding.Index, &info
		}
	}
	if v := q.Get("subtitleStreamID"); v != "" {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			if id == 0 {
				p.subSel, p.subSet, p.subStream = -1, true, nil
			} else {
				binding, rerr := s.resolveStream(sessions.WithPlexServe(r.Context(), pr.Remote, ""), pr.User, id)
				if rerr != nil && !streamMiss(rerr) {
					return playRequest{}, rerr
				}
				if rerr == nil && binding.Key == key && binding.Info.Type == "subtitle" {
					info := binding.Info
					p.subSel, p.subSet, p.subStream = binding.Index, true, &info
				}
			}
		}
	}
	p.sessionID = firstNonEmpty(q.Get("session"), q.Get("transcodeSessionId"), pr.SessionID)
	if p.sessionID == "" {
		p.sessionID = sanitizeSessionID(pr.ClientID + "-" + key.String())
	}
	p.sessionID = sanitizeSessionID(p.sessionID)
	p.offset = durationParam(q.Get("offset"), time.Second)
	return p, nil
}

func isPlayableKey(k core.RatingKey) bool {
	switch k.Kind() {
	case core.KindMovie, core.KindEpisode, core.KindLocal:
		return true
	}
	return false
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// sanitizeSessionID keeps session ids path-safe.
func sanitizeSessionID(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s) && len(b) < 128; i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' {
			b = append(b, c)
		}
	}
	return string(b)
}

// applySelections maps stream positions to DecideOptions stream indexes.
func applySelections(p *playRequest, info core.MediaInfo, langs []string) {
	audio := p.audioSel
	if audio < 0 || audio >= len(info.Streams) {
		audio = pickAudio(info, langs)
	}
	if audio >= 0 && audio < len(info.Streams) && info.Streams[audio].Type == "audio" {
		p.opts.AudioStream = info.Streams[audio].Index
	}
	if p.subSet && p.subSel >= 0 && p.subSel < len(info.Streams) && info.Streams[p.subSel].Type == "subtitle" {
		st := info.Streams[p.subSel]
		if !st.External {
			p.opts.SubtitleStream = st.Index
		}
	}
	p.audioSel = audio
}

// prepare resolves the title for this client and makes the decision.
func (s *Server) prepare(ctx context.Context, pr *Request, p *playRequest) (*core.Title, *core.PlaySession, core.Decision, error) {
	requested := *p
	if s.d.Playback == nil {
		return nil, nil, core.Decision{}, errors.New("playback not configured")
	}
	t, err := s.d.Catalog.Get(ctx, p.key)
	if err != nil || (t != nil && !t.Released.IsZero() && t.Released.After(s.now())) {
		if title, _, e := s.lookupPlaceholder(ctx, pr, p.key, p.collection); e == nil {
			return title, nil, core.Decision{}, core.ErrUnreleased
		}
		if err == nil {
			err = core.ErrNotFound
		}
	}
	if err != nil {
		return nil, nil, core.Decision{}, err
	}
	// Policy only: discovery evidence never blocks a play attempt.
	if !s.playableTitle(ctx, pr.User, t) {
		return nil, nil, core.Decision{}, core.ErrNotFound
	}
	if policy, ok := s.d.AccessPolicy.(core.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, p.caps.Remote, s.now()) {
		return nil, nil, core.Decision{}, core.ErrUnauthorized
	}
	if policy, ok := s.d.AccessPolicy.(core.BitrateAccessPolicy); ok {
		if limit := policy.BitrateLimit(pr.User); limit > 0 && (p.caps.MaxBitrateKbps == 0 || limit < p.caps.MaxBitrateKbps) {
			p.caps.MaxBitrateKbps = limit
			p.opts.MaxBitrateKbps = limit
		}
	}
	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		return t, nil, core.Decision{}, core.ErrUnauthorized
	}
	var ps *core.PlaySession
	dec, err := s.openPlexPlan(ctx, pr, p, func(ctx context.Context) (sessions.PlexAdmission, error) {
		var prepErr error
		ps, prepErr = s.preparePlayback(ctx, pr, p)
		if prepErr != nil {
			return sessions.PlexAdmission{}, prepErr
		}
		// Tracks cached or picked since the session's media was enriched (a
		// subtitle search pick mid-playback) join its streams for this
		// decision; text sidecars never enter the transcoder's input.
		if info := s.withCachedSidecars(ctx, pr.User, t, ps.Info); len(info.Streams) != len(ps.Info.Streams) {
			cp := *ps
			cp.Info = info
			ps = &cp
		}
		// The chosen sidecar has to be on this media before track resolution.
		// Otherwise a language already present in the probe drops it, and the
		// decision either selects the other track or no subtitle at all.
		ps = ensureSelectedSidecar(ps, p.subStream)
		resolvePlaySelection(p, ps.Info)
		s.applyStreamPreferences(ctx, pr, p, t, ps.Info)
		applySelections(p, ps.Info, s.audioLanguages(pr.User))
		p.caps.TranscodeAudioCodecs = streamingAudioTarget(pr.ProfileExtra, p.protocol)
		return sessions.PlexAdmission{
			Source: ps, Media: ps.Info, Caps: p.caps, Options: p.opts, PreferRemux: p.mde, Protocol: p.protocol,
			RemoteMaxBitrateKbps: s.cfg().Transcode.RemoteMaxBitrateKbps,
		}, nil
	})
	if err != nil {
		return t, nil, core.Decision{}, err
	}
	if v := ps.Info.Video(); v != nil {
		s.log.Debug("playback media decision", "key", p.key, "client", pr.ClientID, "device", pr.Device, "model", pr.Model, "platformVersion", pr.PlatformVersion, "video", v.Codec, "videoProfile", v.Profile, "height", v.Height, "bitDepth", v.BitDepth, "transfer", v.ColorTransfer, "dvProfile", v.DolbyVisionProfile, "dvLevel", v.DolbyVisionLevel, "dvCompatibility", v.DolbyVisionCompatibilityID, "kind", dec.Kind, "outputContainer", dec.Container, "videoCopy", dec.VideoCopy, "audioCopy", dec.AudioCopy, "hdrCap", p.caps.HDR10, "dvCap", p.caps.DolbyVision, "profileExtra", pr.ProfileExtra)
	}
	subtitle := -1
	if p.subSet && p.subSel >= 0 && p.subSel < len(ps.Info.Streams) && ps.Info.Streams[p.subSel].Type == "subtitle" {
		subtitle = p.subSel
	}
	s.playbackOwners.Bind(pr.User.ID, pr.ClientID, p.key, pr.SessionID, ps.ID, true, s.now())
	s.transcodes.put(&transcodeEntry{
		id: p.sessionID, playID: ps.ID, playbackID: pr.SessionID, userID: pr.User.ID, clientID: pr.ClientID, key: p.key,
		requested: requested,
		decision:  dec, info: ps.Info, subtitle: subtitle, caps: p.caps, opts: p.opts, offset: p.offset, updatedAt: s.now(),
	})
	return t, ps, dec, nil
}

func onDemandSession(ps *core.PlaySession) bool {
	return ps.Resolution != nil && !strings.HasPrefix(ps.Resolution.Release.ID, "local:")
}

// handleDecision is /video/:/transcode/universal/decision.
func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	if s.handleLiveTVPlayback(w, r) {
		return
	}
	if s.handleExtraPlayback(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	if chi.URLParam(r, "ttype") != "" && chi.URLParam(r, "ttype") != "video" {
		s.writeEmpty(w, r)
		return
	}
	p, err := s.parsePlayRequest(r)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	unlockEncoder, lockErr := s.lockEncoder(ctx, p.sessionID)
	if lockErr != nil {
		return
	}
	defer unlockEncoder()

	t, ps, dec, err := s.prepare(ctx, pr, &p)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) && t == nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if ctx.Err() != nil {
			return
		}
		s.log.Info("playback decision failed", "key", p.key, "user", pr.User.Username, "reason", playbackMessage(err), "err", err)
		// The client follows a failed decision with start.mpd/start.m3u8
		// for the same session: answer it with this error, not a second
		// full resolve that fails the same way.
		s.transcodes.fail(p.sessionID, pr.User.ID, pr.ClientID, p.key, err, s.now())
		s.write(w, r, s.decisionError(ctx, pr, t, err, p))
		return
	}
	s.write(w, r, s.decisionContainer(ctx, pr, t, ps, dec, p))
}

// decisionError is the decision response for a failed resolve: no Media,
// and the reason in every *DecisionText so whichever field a client shows
// carries the message instead of a spinner.
func (s *Server) decisionError(ctx context.Context, pr *Request, t *core.Title, err error, p playRequest) *model.MediaContainer {
	msg := playbackMessage(err)
	mc := libraryContainer()
	if t != nil {
		mc = s.itemContainer(t.Type)
		md := s.metadata(ctx, pr, t, renderOpts{detail: true})
		decisionItem(&md) // shaped and typed like a successful decision's item
		mc.Metadata = []model.Metadata{md}
	}
	mc.Extra = []model.Attr{
		model.A("directPlayDecisionCode", codeDirectPlayError),
		model.A("directPlayDecisionText", msg),
		model.A("generalDecisionCode", codeGeneralError),
		model.A("generalDecisionText", msg),
		model.A("transcodeDecisionCode", codeTranscodeError),
		model.A("transcodeDecisionText", msg),
	}
	if p.mde {
		// MDE clients read mdeDecision* too (Plex Web shows the first
		// *DecisionText whose code is >= 2000).
		mc.Extra = append(mc.Extra, model.A("mdeDecisionCode", codeGeneralError), model.A("mdeDecisionText", msg))
	}
	mc.Extra = append(mc.Extra, model.A("resourceSession", firstNonEmpty(pr.ClientID, p.sessionID)))
	mc.Size = len(mc.Metadata)
	mc.SetDecision()
	return mc
}

// decisionItem reshapes a detail item the way PMS's decision responses
// carry it: attributes alphabetical with string-typed ids, no Image or Guid
// children, tag attributes alphabetical with string ids, and
// UltraBlurColors as an array in JSON.
func decisionItem(md *model.Metadata) {
	md.SetAlphabetical()
	md.Image = nil
	md.Guid = nil
	if md.UltraBlurColors != nil {
		u := *md.UltraBlurColors
		u.SetAlphabetical()
		md.UltraBlurList = []model.UltraBlurColors{u}
		md.UltraBlurColors = nil
	}
	for i := range md.Rating_ {
		md.Rating_[i].SetAlphabetical()
	}
	for _, tags := range [][]model.Tag{md.Genre, md.Country, md.Collection, md.Director, md.Writer, md.Role, md.Producer, md.Label, md.Similar} {
		for i := range tags {
			tags[i].SetAlphabetical()
		}
	}
}

func (s *Server) decisionContainer(ctx context.Context, pr *Request, t *core.Title, ps *core.PlaySession, dec core.Decision, p playRequest) *model.MediaContainer {
	mc := s.itemContainer(t.Type)
	md := s.metadata(ctx, pr, t, renderOpts{detail: true})
	decisionItem(&md)
	// With hasMDE=1 and direct play allowed, the client's own decision
	// engine decides: PMS answers with the source media as direct play and
	// only an mdeDecisionCode. Ether's own decision is kept for start.m3u8
	// in case the client then asks for a stream.
	mdeDirect := p.mde && p.opts.DirectPlay && len(p.opts.AudioFilters) == 0 && !(onDemandSession(ps) && p.opts.DirectStream && dec.Kind != core.DecisionDirectPlay)
	shown := dec
	if mdeDirect {
		shown = core.Decision{Kind: core.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: ps.Info.Container}
	}
	p.listAllTracks = androidFamilyClient(pr)
	md.Media = []model.Media{s.decisionMedia(t, ps, shown, p, pr.User)}
	s.addProbedStreamFeatures(ctx, t.Key, ps.Info, &md)
	if md.Media[0].Duration > 0 {
		md.Duration = md.Media[0].Duration
	}
	mc.Metadata = []model.Metadata{md}
	mc.Size = 1
	mc.SetDecision()
	switch {
	case mdeDirect:
		mc.Extra = []model.Attr{
			model.A("mdeDecisionCode", codeDirectPlayOK),
			model.A("mdeDecisionText", textDirectPlayOK),
		}
	case dec.Kind == core.DecisionDirectPlay:
		mc.Extra = []model.Attr{
			model.A("directPlayDecisionCode", codeDirectPlayOK),
			model.A("directPlayDecisionText", textDirectPlayOK),
			model.A("generalDecisionCode", codeDirectPlayOK),
			model.A("generalDecisionText", textDirectPlayOK),
		}
	default:
		dpText := textDirectPlayDisable
		if p.opts.DirectPlay {
			dpText = "App cannot direct play this item."
			if dec.Reason != "" {
				dpText += " " + dec.Reason
			}
		}
		mc.Extra = []model.Attr{
			model.A("directPlayDecisionCode", codeDirectPlayError),
			model.A("directPlayDecisionText", dpText),
			model.A("generalDecisionCode", codeConversionOK),
			model.A("generalDecisionText", textConversionOK),
			model.A("transcodeDecisionCode", codeConversionOK),
			model.A("transcodeDecisionText", textConversionOK),
		}
	}
	// PMS echoes the client identifier as the resource session.
	mc.Extra = append(mc.Extra, model.A("resourceSession", firstNonEmpty(pr.ClientID, p.sessionID)))
	return mc
}

// decisionMedia renders the Media with decision attributes.
func (s *Server) decisionMedia(t *core.Title, ps *core.PlaySession, dec core.Decision, p playRequest, u *core.User) model.Media {
	stamp := ps.StartedAt.Unix()
	if stamp <= 0 {
		stamp = 1
	}
	info := ps.Info
	m := buildMedia(t, info, stamp)
	if ps.Resolution != nil && s.d.Trickplay != nil && s.d.Trickplay.TrickplayAvailable(ps.Resolution.Release.ID) {
		m.Part[0].Indexes = "sd"
	}
	m.Selected = model.Ptr(true)
	m.SetAlphabetical()
	part := &m.Part[0]
	part.Selected = model.Ptr(true)
	part.SetAlphabetical()
	sel := selection{audio: p.audioSel, audioSet: p.audioSel >= 0, subtitle: p.subSel, subtitleSet: p.subSet}
	streams := s.buildStreams(t.Key, info, sel, u)
	for i := range streams {
		streams[i].SetAlphabetical()
		if streams[i].StreamType == 1 {
			// Decisions shorten the video title to the resolution part
			// ("1080p"); the extended title keeps the codec.
			if short, _, ok := strings.Cut(streams[i].DisplayTitle, " ("); ok {
				streams[i].DisplayTitle = short
			}
		}
	}

	selectedOnly := info.Estimated || !p.listAllTracks
	if dec.Kind == core.DecisionDirectPlay {
		return directPlayMedia(m, streams, selectedOnly)
	}
	applyTranscodeMedia(&m, dec, p.protocol)
	var kept []model.Stream
	for _, st := range streams {
		binding, rerr := s.resolveStream(context.Background(), u, st.ID)
		if rerr != nil && !streamMiss(rerr) {
			continue
		}
		if shaped, keep := applyStreamDecision(st, binding.Info, dec, selectedOnly); keep {
			kept = append(kept, shaped)
		}
	}
	part.Stream = kept
	if p.protocol == core.ProtocolDASH {
		dashDecisionMedia(&m)
	}
	return m
}

// directPlayMedia preserves stream ordering. For Android-family clients
// every audio and subtitle stays in the list and selected marks the one
// that will play: Plex Android builds its playback track menu from this
// response, so dropping the alternates leaves the menu empty. Other
// clients keep the recorded PMS shape (video + selected tracks), which
// TestDecisionConformsToPMS and the goldens pin.
func directPlayMedia(m model.Media, streams []model.Stream, selectedOnly bool) model.Media {
	part := &m.Part[0]
	part.Decision = "directplay"
	var kept []model.Stream
	for _, st := range streams {
		if selectedOnly && st.StreamType != 1 && (st.Selected == nil || !*st.Selected) {
			continue
		}
		st.Location = "direct"
		kept = append(kept, st)
	}
	part.Stream = kept
	return m
}

// applyTranscodeMedia describes the output container and media dimensions.
func applyTranscodeMedia(m *model.Media, dec core.Decision, protocol string) {
	part := &m.Part[0]
	// Describe the actual HLS segment container so native MDE does not see
	// HEVC paired with the MPEG-TS container it cannot use.
	m.SetTranscodeOrder()
	part.SetTranscodeOrder()
	m.AspectRatio = 0
	m.Protocol = protocol
	m.Container = "mpegts"
	if dec.Container == transcode.ContainerFMP4 {
		m.Container = "mp4"
	} else if dec.Container == transcode.ContainerMatroska {
		m.Container = "mkv"
	}
	part.Container = m.Container
	part.Protocol = protocol
	part.Decision = "transcode"
	part.Key = ""
	part.File = ""
	part.Size = 0
	m.OptimizedForStreaming, m.Has64bitOffsets = nil, nil
	part.OptimizedForStreaming, part.Has64bitOffsets = nil, nil
	if r := m.VideoResolution; r != "" && r != "4k" && r != "sd" {
		m.VideoResolution = r + "p"
	}
	if !dec.VideoCopy {
		if dec.VideoCodec != "" {
			m.VideoCodec = plexCodec(dec.VideoCodec)
		}
		if dec.MaxHeight > 0 && m.Height > dec.MaxHeight {
			if m.Height > 0 {
				m.Width = m.Width * dec.MaxHeight / m.Height
			}
			m.Height = dec.MaxHeight
			m.VideoResolution = videoResolution(m.Width, m.Height) + "p"
		}
		if dec.BitrateKbps > 0 {
			m.Bitrate = dec.BitrateKbps
		}
	}
	if !dec.AudioCopy && dec.AudioCodec != "" {
		m.AudioCodec = plexCodec(dec.AudioCodec)
	}
	part.Bitrate, part.Width, part.Height = m.Bitrate, m.Width, m.Height
}

// applyStreamDecision shapes one already-resolved stream without registry
// I/O. When selectedOnly is false (Android-family decisions), an unselected
// stream stays in the list bare: it keeps its index and gets no decision or
// location, the same shape its streams carry in library metadata. That is
// the shape under review against a real PMS Android capture; the selected
// stream's shape never changes.
func applyStreamDecision(st model.Stream, src core.StreamInfo, dec core.Decision, selectedOnly bool) (model.Stream, bool) {
	idx := st.Index
	st.Index = nil // segments carry their own stream layout
	switch st.StreamType {
	case 1:
		st.Decision = "copy"
		if dec.StripDolbyVision {
			st.DOVIPresent, st.DOVIProfile, st.DOVILevel, st.DOVIVersion = nil, 0, 0, ""
			st.DOVIBLCompatID, st.DOVIBLPresent, st.DOVIELPresent, st.DOVIRPUPresent = nil, nil, nil, nil
			base := src
			base.DolbyVision = false
			st.DisplayTitle = videoDisplayTitle(base)
			st.ExtendedDisplayTitle = st.DisplayTitle
		}
		if !dec.VideoCopy {
			// A transcoded stream is new: PMS drops the source's
			// profile and chroma details and reports 8-bit output.
			st.Decision = "transcode"
			if dec.VideoCodec != "" {
				st.Codec = plexCodec(dec.VideoCodec)
			}
			if dec.MaxHeight > 0 && st.Height > dec.MaxHeight {
				st.Width = st.Width * dec.MaxHeight / max(st.Height, 1)
				st.Height = dec.MaxHeight
			}
			if dec.BitrateKbps > 0 {
				st.Bitrate = dec.BitrateKbps
			}
			st.Profile, st.ChromaSubsampling, st.ScanType = "", "", ""
			st.DOVIPresent, st.DOVIProfile = nil, 0
			st.DOVIBLCompatID, st.DOVIBLPresent, st.DOVIELPresent, st.DOVIRPUPresent = nil, nil, nil, nil
			st.DOVILevel, st.DOVIVersion, st.Level = 0, "", 0
			if st.Codec == "h264" || dec.ToneMap {
				st.BitDepth = 8
			}
			if dec.ToneMap {
				st.ColorTrc, st.ColorPrimaries, st.ColorSpace = "bt709", "", ""
			}
		}
		st.Location = "segments-av"
	case 2:
		if st.Selected == nil || !*st.Selected {
			if selectedOnly {
				return st, false
			}
			st.Index = idx
			return st, true
		}
		st.Decision = "copy"
		if !dec.AudioCopy {
			st.Decision = "transcode"
			if dec.AudioCodec != "" {
				st.Codec = plexCodec(dec.AudioCodec)
			}
			// Source-only details go, as in PMS's transcoded audio.
			st.Profile, st.AudioChannelLayout, st.SamplingRate, st.Title = "", "", 0, ""
		}
		st.Location = "segments-av"
	case 3:
		if st.Selected == nil || !*st.Selected {
			if selectedOnly {
				return st, false
			}
			st.Index = idx
			return st, true
		}
		switch {
		case dec.BurnSubtitles:
			st.Decision = "burn"
			st.Location = "segments-video"
		case src.External:
			st.Decision = "copy"
			st.Location = "sidecar"
		default:
			st.Decision = "copy"
			st.Location = "sidecar"
			if st.Key == "" {
				st.Key = "/library/streams/" + strconv.FormatInt(st.ID, 10)
			}
			if st.Format == "" {
				st.Format = st.Codec
			}
		}
	}
	return st, true
}

// handleStartHLS is /video/:/transcode/universal/start.m3u8.
func (s *Server) handleStartHLS(w http.ResponseWriter, r *http.Request) {
	if s.handleLiveTVPlayback(w, r) {
		return
	}
	if s.handleExtraPlayback(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	if s.d.Transcoder == nil || s.d.Playback == nil {
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	}
	p, err := s.parsePlayRequest(r)
	if err != nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	unlockEncoder, lockErr := s.lockEncoder(ctx, p.sessionID)
	if lockErr != nil {
		return
	}
	defer unlockEncoder()

	if s.playbackOwners.Stale(pr.User.ID, pr.ClientID, p.key, pr.SessionID) {
		writeHTTPError(w, http.StatusGone)
		return
	}

	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	if err, failed := s.transcodes.takeFailure(p.sessionID, pr.User.ID, p.key, s.now()); failed {
		s.log.Info("start playback failed", "key", p.key, "reason", playbackMessage(err), "err", err, "decision", "remembered")
		s.writeErr(w, r, err)
		return
	}
	entry, ok := s.transcodes.get(p.sessionID)
	var ps *core.PlaySession
	if ok && entry.key == p.key && entry.userID == pr.User.ID {
		ps, ok = s.d.Playback.Session(entry.playID)
	}
	if !ok || ps == nil || !p.sameSettings(entry.requested) {
		// A client can change quality or tracks in start itself, without a
		// preceding decision request. Reapply the normal policy and decision
		// path before deciding whether a running encoder can be reused.
		if _, ps, _, err = s.prepare(ctx, pr, &p); err != nil {
			s.log.Info("start playback failed", "key", p.key, "reason", playbackMessage(err), "err", err)
			s.writeErr(w, r, err)
			return
		}
		entry, _ = s.transcodes.get(p.sessionID)
	} else if dec, err := s.publishBrokerPlayback(ctx, pr, replayPlay(p, entry), ps); err != nil {
		s.log.Info("playback broker publish failed", "key", p.key, "err", err)
		writeHTTPError(w, http.StatusServiceUnavailable)
		return
	} else {
		entry.decision = dec
	}
	s.playbackOwners.Bind(pr.User.ID, pr.ClientID, p.key, pr.SessionID, ps.ID, true, s.now())
	entry.playbackID = pr.SessionID
	s.transcodes.put(entry)

	dec := sessions.HLSDecision(entry.decision, p.opts.DirectPlay, func() core.Decision {
		resolvePlaySelection(&p, ps.Info)
		applySelections(&p, ps.Info, s.audioLanguages(pr.User))
		return s.d.Transcoder.Decide(ps.Info, p.caps, p.opts)
	}, func(dec core.Decision) core.Decision {
		if len(entry.caps.TranscodeAudioCodecs) > 0 && dec.AudioCodec != "" && !slices.Contains(entry.caps.TranscodeAudioCodecs, dec.AudioCodec) {
			opts := entry.opts
			opts.DirectPlay = false
			opts.DirectStream = true
			return s.d.Transcoder.Decide(ps.Info, entry.caps, opts)
		}
		return dec
	})
	if existing, ok := s.d.Transcoder.Session(p.sessionID); ok && existing != nil {
		if entry.started && entry.offset == p.offset {
			s.notePlexEncoder(p.sessionID, frontend.StreamHLS, dec, p.offset)
			unlockEncoder()
			unlockOwner()
			s.writeMasterPlaylist(w, r, p.sessionID, existing, ps.Info, dec)
			return
		}
		s.d.Transcoder.Stop(p.sessionID)
	}
	hs, err := s.d.Transcoder.StartHLS(context.WithoutCancel(ctx), core.HLSRequest{
		SessionID:   p.sessionID,
		InputURL:    s.d.Playback.InputURL(ps.ID),
		Info:        ps.Info,
		Decision:    dec,
		StartOffset: p.offset,
	})
	if err != nil {
		s.log.Warn("start HLS session", "session", p.sessionID, "key", p.key, "err", err)
		writeHTTPError(w, http.StatusInternalServerError)
		return
	}
	entry.decision, entry.offset, entry.started, entry.updatedAt = dec, p.offset, true, s.now()
	s.transcodes.put(entry)
	s.notePlexEncoder(p.sessionID, frontend.StreamHLS, dec, p.offset)
	unlockEncoder()
	unlockOwner()
	s.writeMasterPlaylist(w, r, p.sessionID, hs, ps.Info, dec)
}

func (s *Server) writeMasterPlaylist(w http.ResponseWriter, r *http.Request, sessionID string, hs core.HLSSession, info core.MediaInfo, dec core.Decision) {
	entry, _ := s.transcodes.get(sessionID)
	// A ladder-capable session advertises every rung; the client's native
	// ABR (AVPlayer, hls.js) steps between them and the session starts a
	// rung's encoder lazily on its first segment request. The primary
	// rung keeps the historic "base" URI.
	if ls, ok := hs.(core.HLSLadderSession); ok {
		if rungs := ls.Rungs(); len(rungs) > 1 {
			var b strings.Builder
			b.WriteString("#EXTM3U\n")
			for _, rg := range rungs {
				line := fmt.Sprintf("#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=%d", rg.Bandwidth)
				if rg.AverageBandwidth > 0 {
					line += fmt.Sprintf(",AVERAGE-BANDWIDTH=%d", rg.AverageBandwidth)
				}
				if rg.Width > 0 && rg.Height > 0 {
					line += fmt.Sprintf(",RESOLUTION=%dx%d", rg.Width, rg.Height)
				}
				if rg.Codecs != "" {
					line += fmt.Sprintf(",CODECS=%q", rg.Codecs)
				}
				if rg.FrameRate > 0 {
					line += fmt.Sprintf(",FRAME-RATE=%.3f", rg.FrameRate)
				}
				uri := streamGrantURL("session/"+sessionID+"/"+rg.ID+"/index.m3u8", entry.streamToken)
				b.WriteString(line + "\n" + uri + "\n")
			}
			body := b.String()
			h := w.Header()
			h.Set("Content-Type", "application/vnd.apple.mpegurl")
			h.Set("Cache-Control", "no-cache")
			h.Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = io.WriteString(w, body)
			}
			return
		}
	}
	bw := info.BitrateKbps
	if !dec.VideoCopy && dec.BitrateKbps > 0 {
		bw = dec.BitrateKbps
	}
	if bw <= 0 {
		bw = 8000
	}
	line := fmt.Sprintf("#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=%d", bw*1000)
	if v := info.Video(); v != nil && v.Width > 0 && v.Height > 0 {
		wd, ht := v.Width, v.Height
		if !dec.VideoCopy && dec.MaxHeight > 0 && ht > dec.MaxHeight {
			wd, ht = wd*dec.MaxHeight/ht, dec.MaxHeight
		}
		line += fmt.Sprintf(",RESOLUTION=%dx%d", wd, ht)
		if v.FrameRate > 0 {
			line += fmt.Sprintf(",FRAME-RATE=%.3f", v.FrameRate)
		}
	}
	line += transcode.HLSVariantAttributes(info, dec)
	body := "#EXTM3U\n" + line + "\n" + streamGrantURL("session/"+sessionID+"/base/index.m3u8", entry.streamToken) + "\n"
	h := w.Header()
	h.Set("Content-Type", "application/vnd.apple.mpegurl")
	h.Set("Cache-Control", "no-cache")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, body)
	}
}

// handleTranscodeSession serves session/{id}/base/index.m3u8 and segments.
func (s *Server) handleTranscodeSession(w http.ResponseWriter, r *http.Request) {
	if s.d.Transcoder == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	id, ok := routeSessionID(r)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	hs, ok := s.d.Transcoder.Session(id)
	if !ok || hs == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.transcodes.touch(id, s.now())
	rest := chi.URLParam(r, "*")
	name := rest
	if i := strings.LastIndexByte(rest, '/'); i >= 0 {
		name = rest[i+1:]
	}
	// ABR ladder: session/{id}/{rung}/index.m3u8 and .../{rung}/{seg}.ts.
	// The primary rung is "base"; sessions without a ladder fall through
	// to the historic single-rung handling.
	if ls, isLadder := hs.(core.HLSLadderSession); isLadder {
		if rungID, _, found := strings.Cut(rest, "/"); found {
			for _, rg := range ls.Rungs() {
				if rg.ID != rungID {
					continue
				}
				if strings.HasSuffix(name, ".m3u8") {
					s.serveStreamManifest(w, r, id, func(w http.ResponseWriter, r *http.Request) {
						ls.ServeRungPlaylist(w, r, rungID)
					})
					return
				}
				n, ok := segmentNumber(name)
				if !ok {
					writeHTTPError(w, http.StatusNotFound)
					return
				}
				ls.ServeRungSegment(w, r, rungID, n)
				return
			}
		}
	}
	if strings.HasSuffix(name, ".m3u8") {
		s.serveStreamManifest(w, r, id, hs.ServePlaylist)
		return
	}
	n, ok := segmentNumber(name)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	hs.ServeSegment(w, r, n)
}

// segmentNumber extracts the segment index from names like "00012.ts",
// "12.ts", "segment-12.m4s".
func segmentNumber(name string) (int, bool) {
	if name == transcode.InitSegmentName || name == transcode.MatroskaHeaderName {
		return transcode.InitSegment, true
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	end := len(base)
	start := end
	for start > 0 && base[start-1] >= '0' && base[start-1] <= '9' {
		start--
	}
	if start == end {
		return 0, false
	}
	n, err := strconv.Atoi(base[start:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

func (s *Server) handleTranscodePing(w http.ResponseWriter, r *http.Request) {
	if id := sanitizeSessionID(r.URL.Query().Get("session")); id != "" {
		if e, ok := s.transcodes.get(id); ok && e.userID == FromContext(r.Context()).User.ID {
			s.transcodes.touch(id, s.now())
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleTranscodeStop(w http.ResponseWriter, r *http.Request) {
	id := sanitizeSessionID(r.URL.Query().Get("session"))
	pr := FromContext(r.Context())
	if e, ok := s.transcodes.get(id); id != "" && ok && e.userID == pr.User.ID {
		unlockOwner, err := s.lockPlaybackOwner(r.Context(), e.userID, e.clientID)
		if err != nil {
			return
		}
		defer unlockOwner()
		unlockEncoder, err := s.lockEncoder(r.Context(), id)
		if err != nil {
			return
		}
		defer unlockEncoder()
		current, ok := s.transcodes.get(id)
		if !ok || current.userID != e.userID || current.clientID != e.clientID {
			w.WriteHeader(http.StatusOK)
			return
		}
		e = current
		if s.playbackOwners.Stale(e.userID, e.clientID, e.key, pr.SessionID) && e.playbackID != pr.SessionID {
			w.WriteHeader(http.StatusOK)
			return
		}
		shared := false
		if s.d.Transcoder != nil && e.playID != "" {
			for _, other := range s.transcodes.byPlayID(e.playID) {
				if other != id {
					if _, active := s.d.Transcoder.Session(other); active {
						shared = true
						break
					}
				}
			}
		}
		// A replacement decision owns the source before its encoder starts.
		// Retire a superseded encoder without releasing that prepared source,
		// including stops that omit the external playback identifier.
		superseded := s.playbackOwners.Stale(e.userID, e.clientID, e.key, e.playbackID)
		// Another encoder, or a replacement decision, still owns this source.
		// Broker Stop would release it. The last encoder stops the broker
		// session that owns the source.
		if !superseded && !shared {
			if lookup, ok := s.playbackBroker.(plexPlaybackLookup); ok && pr.User != nil {
				sid, lookupErr := lookup.PlexSessionForPlayback(r.Context(), s.plexAccess(pr), e.key, pr.SessionID, id)
				if lookupErr == nil && sid != "" {
					playID, known := lookup.PlexSessionSource(sid)
					if sid == id || (known && playID == e.playID) {
						_ = s.playbackBroker.Stop(r.Context(), s.plexAccess(pr), sid)
					}
				}
			}
		}
		if s.d.Transcoder != nil {
			s.d.Transcoder.Stop(id)
		}
		s.transcodes.remove(id)
	}
	w.WriteHeader(http.StatusOK)
}

// handleTranscodeSessions is /transcode/sessions.
func (s *Server) handleTranscodeSessions(w http.ResponseWriter, r *http.Request) {
	mc := &model.MediaContainer{}
	u := FromContext(r.Context()).User
	for _, e := range s.sessions.list() {
		if !u.IsOwner && e.user.ID != u.ID {
			continue
		}
		if e.transcodeID == "" {
			continue
		}
		if te, ok := s.transcodes.get(e.transcodeID); ok {
			mc.TranscodeSess = append(mc.TranscodeSess, transcodeSessionModel(te))
		}
	}
	mc.Size = len(mc.TranscodeSess)
	s.write(w, r, mc)
}

func transcodeSessionModel(te *transcodeEntry) model.TranscodeSess {
	ts := model.TranscodeSess{
		Key:        "/transcode/sessions/" + te.id,
		Context:    "streaming",
		Protocol:   "hls",
		Container:  "mpegts",
		Duration:   ms(te.info.Duration),
		Progress:   -1,
		TimeStamp:  float64(te.updatedAt.Unix()),
		AudioCodec: plexCodec(te.decision.AudioCodec),
		VideoCodec: plexCodec(te.decision.VideoCodec),
	}
	if v := te.info.Video(); v != nil {
		ts.SourceVideoCodec = plexCodec(v.Codec)
	}
	for _, st := range te.info.Streams {
		if st.Type == "audio" {
			ts.SourceAudioCodec = plexCodec(st.Codec)
			break
		}
	}
	ts.VideoDecision, ts.AudioDecision = "copy", "copy"
	if !te.decision.VideoCopy {
		ts.VideoDecision = "transcode"
	} else {
		ts.VideoCodec = ts.SourceVideoCodec
	}
	if !te.decision.AudioCopy {
		ts.AudioDecision = "transcode"
	} else {
		ts.AudioCodec = ts.SourceAudioCodec
	}
	if te.decision.BurnSubtitles {
		ts.SubtitleDecision = "burn"
	}
	return ts
}

// ---------------------------------------------------------------------------
// Direct play

// handlePart serves /library/parts/{part}/{changestamp}/{file} with Range
// support through Playback.ServeRange. A variant Part id names a specific
// release of the title ("Play Version" direct play).
func (s *Server) handlePart(w http.ResponseWriter, r *http.Request) {
	if s.handleExtraPart(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	id, err := strconv.ParseInt(chi.URLParam(r, "part"), 10, 64)
	if err != nil || s.d.Playback == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	key, releaseID, found := s.partTitle(id)
	if !found {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	title, titleErr := s.d.Catalog.Get(ctx, key)
	if titleErr != nil || !s.playableTitle(ctx, pr.User, title) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if policy, ok := s.d.AccessPolicy.(core.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, pr.ClientCaps(0).Remote, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	ps, ok := s.d.Playback.SessionFor(pr.User.ID, pr.ClientID, key)
	// A live session on a different release does not satisfy a variant part.
	if ok && ps != nil && releaseID != "" && (ps.Resolution == nil || ps.Resolution.Release.ID != releaseID) {
		ok = false
	}
	if !ok || ps == nil {
		t, err := s.d.Catalog.Get(ctx, key)
		if err == nil && !s.titleVisible(pr.User, t) {
			err = core.ErrNotFound
		}
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		caps := s.clientCaps(pr)
		ps, err = s.prepareForRelease(ctx, pr.User, key, caps, releaseID)
		if err != nil {
			s.log.Info("direct play failed", "key", key, "reason", playbackMessage(err), "err", err)
			s.writeErr(w, r, err)
			return
		}
	}
	if r.Method != http.MethodHead && r.URL.Query().Get("download") != "1" {
		s.playbackOwners.Bind(pr.User.ID, pr.ClientID, key, pr.SessionID, ps.ID, false, s.now())
		sessionID := pr.SessionID
		if sessionID == "" {
			sessionID = sanitizeSessionID(pr.ClientID + "-" + key.String())
		}
		partPlay := playRequest{key: key, caps: s.declaredCaps(pr), sessionID: sanitizeSessionID(sessionID), audioSel: -1, subSel: -1}
		if partPlay.caps.ClientID == "" {
			partPlay.caps.ClientID = pr.ClientID
		}
		partPlay.opts.DirectPlay = true
		partPlay.opts.DirectStream = true
		// A part range of a playback that is already on the broker must not
		// open a second session. That open supersedes the live encoder.
		already := false
		if lookup, ok := s.playbackBroker.(plexPlaybackLookup); ok {
			if sid, lookupErr := lookup.PlexSessionForPlayback(ctx, s.plexAccess(pr), key, "", ""); lookupErr == nil && sid != "" {
				if playID, ok := lookup.PlexSessionSource(sid); ok && playID == ps.ID {
					already = true
				}
			}
		}
		if !already {
			if _, err := s.publishBrokerPlayback(ctx, pr, &partPlay, ps); err != nil {
				s.log.Info("direct play broker publish failed", "key", key, "err", err)
				s.writeErr(w, r, err)
				return
			}
		}
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", "attachment; filename=\""+strings.ReplaceAll(chi.URLParam(r, "file"), "\"", "")+"\"")
	}
	unlockOwner()
	s.d.Playback.ServeRange(w, r, ps.ID)
}

// handlePartSelect is PUT /library/parts/{part}?audioStreamID=&subtitleStreamID=.
func (s *Server) handlePartSelect(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	id, err := strconv.ParseInt(chi.URLParam(r, "part"), 10, 64)
	if err != nil || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	// Stream selections are per title, even when addressed at a variant part.
	if k, _, found := s.partTitle(id); found {
		id = int64(k)
	}
	q := r.URL.Query()
	ctx := sessions.WithPlexServe(r.Context(), pr.Remote, "")
	var audio *sessions.PlexStreamBinding
	if v := q.Get("audioStreamID"); v != "" {
		if sid, err := strconv.ParseInt(v, 10, 64); err == nil {
			binding, rerr := s.resolveStream(ctx, pr.User, sid)
			if rerr != nil && !streamMiss(rerr) {
				writeHTTPError(w, http.StatusUnauthorized)
				return
			}
			if rerr == nil && int64(binding.Key) == id && binding.Info.Type == "audio" {
				audio = &binding
			}
		}
	}
	clearSub := false
	var sub *sessions.PlexStreamBinding
	if v := q.Get("subtitleStreamID"); v != "" {
		if sid, err := strconv.ParseInt(v, 10, 64); err == nil {
			if sid == 0 {
				clearSub = true
			} else {
				binding, rerr := s.resolveStream(ctx, pr.User, sid)
				if rerr != nil && !streamMiss(rerr) {
					writeHTTPError(w, http.StatusUnauthorized)
					return
				}
				if rerr == nil && int64(binding.Key) == id && binding.Info.Type == "subtitle" {
					sub = &binding
				}
			}
		}
	}
	s.selections.update(pr.User.ID, id, func(sel *selection) {
		if audio != nil {
			info := audio.Info
			sel.audio, sel.audioSet, sel.audioStream = audio.Index, true, &info
		}
		if clearSub {
			sel.subtitle, sel.subtitleSet, sel.subtitleStream = -1, true, nil
		} else if sub != nil {
			info := sub.Info
			sel.subtitle, sel.subtitleSet, sel.subtitleStream = sub.Index, true, &info
		}
	})
	s.rememberStreamRequest(r, pr.User, core.RatingKey(id))
	w.WriteHeader(http.StatusOK)
}

var subtitleContentTypes = map[string]string{
	"srt": "application/x-subrip",
	"vtt": "text/vtt",
	"ass": "text/x-ssa",
	"ssa": "text/x-ssa",
}

// handleStream serves external (sidecar) subtitle streams:
// /library/streams/{id}[.ext][?format=srt|vtt|webvtt]. Cached OpenSubtitles
// tracks are UTF-8 (any encoding= parameter is satisfied) and served in
// either format; other sidecars are served as stored.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "stream")
	raw, _, _ = strings.Cut(raw, ".")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if !s.acceptanceCurrent(r) {
		writeHTTPError(w, http.StatusUnauthorized)
		return
	}
	pr := FromContext(r.Context())
	format := subtitleFormat(r.URL.Query().Get("format"))
	if _, ext, ok := strings.Cut(chi.URLParam(r, "stream"), "."); ok {
		format = subtitleFormat(ext)
	}
	if s.boundStreams == nil || pr == nil || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	binding, rerr := s.resolveStream(r.Context(), pr.User, id)
	if rerr != nil {
		if errors.Is(rerr, core.ErrUnauthorized) {
			writeHTTPError(w, http.StatusUnauthorized)
			return
		}
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if binding.Info.Type == "subtitle" && !binding.Info.External {
		s.serveEmbeddedStream(w, r, binding.Key, binding.Info)
		return
	}
	receipt, _ := acceptanceFrom(r.Context())
	reader, info, err := s.boundStreams.OpenPlexSidecar(sessions.WithPlexServe(r.Context(), pr.Remote, format), s.plexStreamAccess(pr.User), receipt, id)
	if err != nil || reader == nil {
		if reader != nil {
			_ = reader.Close()
		}
		if errors.Is(err, core.ErrUnauthorized) {
			writeHTTPError(w, http.StatusUnauthorized)
			return
		}
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	defer reader.Close()
	ct := subtitleContentTypes[plexCodec(info.Codec)]
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	_, _ = io.Copy(w, io.LimitReader(reader, 32<<20))
}
