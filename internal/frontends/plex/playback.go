package plex

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/braedonsaunders/ether/internal/frontend/kit/plexparts"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
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
	msgNotCached        = "No cached release is available on your debrid service."
	// Every release is a torrent no debrid account holds, with the owner's
	// "Cached torrents only" on: nothing downloads until it is turned off.
	msgNotCachedOnly       = "No cached copy. Turn off Cached torrents only to download it."
	msgNotCachedOnlyNoNZB  = "No cached copy or Usenet release. Turn off Cached torrents only to download it."
	msgNotCachedOnlyBadNZB = "No cached copy; Usenet releases are incomplete. Turn off Cached torrents only to download it."
	msgNotCachedNoNZB      = "No cached copy or Usenet release is available."
	msgNotCachedBadNZB     = "No cached copy; Usenet releases are incomplete."
	msgDownloadRefused     = "Not cached. The download could not be started; check the Ether log for the reason."
	msgDownloadPaused      = "Not cached. Ether temporarily paused downloads after debrid service refusals. Try again later."
	msgDownloadLimit       = "Not cached. Your debrid account has reached its active download limit."
	msgVideoTranscodingOff = "This title needs a video transcode to play on this device, and video transcoding is turned off."
)

// playbackMessageFor is playbackMessage with the wording for a viewer whose
// own account has video transcoding switched off.
func (s *Server) playbackMessageFor(pr *Request, err error) string {
	if pr != nil {
		if _, msg, _ := frontend.PlaybackPending(err, pr.Language); msg != "" {
			return msg
		}
		if msg := frontend.VideoEncoderCapacityMessage(err, pr.Language); msg != "" {
			return msg
		}
		if _, _, msg := frontend.PlaybackSourceProblem(err, pr.Language); msg != "" {
			return msg
		}
	}
	if pr != nil && pr.User != nil && errors.Is(err, frontend.ErrVideoTranscodingDisabled) {
		return frontend.VideoTranscodingOffMessage(s.policy(), pr.User)
	}
	return playbackMessage(err)
}

func playbackMessage(err error) string {
	if _, msg, _ := frontend.PlaybackPending(err); msg != "" {
		return msg
	}
	if msg := frontend.VideoEncoderCapacityMessage(err); msg != "" {
		return msg
	}
	if _, _, msg := frontend.PlaybackSourceProblem(err); msg != "" {
		return msg
	}
	var bedtime *frontend.ErrBedtime
	if errors.As(err, &bedtime) {
		return bedtime.Error()
	}
	var reqLang *frontend.RequiredAudioLanguageError
	if errors.As(err, &reqLang) {
		return reqLang.Error()
	}
	switch {
	case errors.Is(err, frontend.ErrVideoTranscodingDisabled):
		return msgVideoTranscodingOff
	case errors.Is(err, frontend.ErrUnreleased):
		return "This title has not been released yet. Playback is unavailable."
	case errors.Is(err, frontend.ErrDebridAuth):
		return msgDebridAuth
	case errors.Is(err, frontend.ErrNoTorrentSources):
		return msgNoTorrentSources
	case errors.Is(err, frontend.ErrNoSources):
		return msgNoSources
	case errors.As(err, new(*frontend.UncachedError)), onlyNotCached(err):
		return notCachedMessage(err)
	case errors.Is(err, frontend.ErrArchiveUnstreamable):
		return "This archive format or compression method cannot be streamed."
	case errors.Is(err, context.DeadlineExceeded):
		return "The source timed out. Try again later."
	case errors.Is(err, frontend.ErrArticleMissing), errors.Is(err, frontend.ErrArticleCorrupt), errors.Is(err, frontend.ErrReleaseRejected):
		return "The source is incomplete or cannot be played."
	case errors.Is(err, frontend.ErrSourceUnavailable):
		return "The source could not be opened or is incomplete. Try again later."
	case errors.Is(err, frontend.ErrNotAvailable):
		return msgNotAvailable
	case errors.Is(err, frontend.ErrStreamLimit):
		return msgStreamLimit
	case errors.Is(err, frontend.ErrJobsBusy):
		return frontend.JobsBusyMessage(err)
	}
	return msgPlayFailed
}

// writeStartErr answers a failed start.m3u8, start.mpd or part request.
// End the media request and queue the reason as
// terminationText for the client's next timeline, as for a start stall.
func (s *Server) writeStartErr(w http.ResponseWriter, r *http.Request, pr *Request, err error) {
	if r.Context().Err() != nil {
		return
	}
	if frontend.WritePlaybackPending(w, err, notificationLanguage(r)) {
		return
	}
	s.log.Warn("playback start failed", "reason", playbackMessage(err), "err", err)
	if pr != nil && pr.User != nil {
		s.setTermination(trackID(pr.User.ID, pr.ClientID, pr.SessionID), s.playbackMessageFor(pr, err))
	}
	if frontend.WritePlaybackSourceProblem(w, err, notificationLanguage(r)) {
		return
	}
	// A completed attempt is terminal. Retry-After on a 503 makes media
	// loaders retry while waiting for a timeline they may never send.
	// PMS start.* documents 400/403/404 for unsuccessful starts.
	status := http.StatusBadRequest
	w.Header().Del("Retry-After")
	switch {
	case errors.Is(err, frontend.ErrNotFound), errors.Is(err, frontend.ErrNotAvailable):
		status = http.StatusNotFound
	case errors.Is(err, frontend.ErrUnauthorized):
		status = http.StatusForbidden
	}
	writePlaybackStatus(w, status)
}

// PMS does not specify an error body for start responses. Do not send HTML
// where the client expected a playlist, manifest, or media bytes.
func writePlaybackStatus(w http.ResponseWriter, status int) {
	w.Header().Del("Content-Type")
	w.Header().Del("Content-Encoding")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(status)
}

// notCachedMessage words a failure whose every cause is frontend.ErrNotCached:
// the account's download limit or a refused download first (the owner can
// act on those), then what the resolver said about the owner's setting and
// Usenet (*frontend.UncachedError), the same reason Releases › History records.
func notCachedMessage(err error) string {
	switch {
	case errors.Is(err, frontend.ErrDownloadPaused):
		return msgDownloadPaused
	case errors.Is(err, frontend.ErrDownloadLimit):
		return msgDownloadLimit
	case errors.Is(err, frontend.ErrDownloadRefused):
		return msgDownloadRefused
	}
	var ue *frontend.UncachedError
	if !errors.As(err, &ue) {
		return msgNotCached
	}
	switch {
	case ue.CachedOnly && ue.Usenet == frontend.UsenetNone:
		return msgNotCachedOnlyNoNZB
	case ue.CachedOnly && ue.Usenet == frontend.UsenetFailed:
		return msgNotCachedOnlyBadNZB
	case ue.CachedOnly:
		return msgNotCachedOnly
	case ue.Usenet == frontend.UsenetNone:
		return msgNotCachedNoNZB
	case ue.Usenet == frontend.UsenetFailed:
		return msgNotCachedBadNZB
	}
	return msgNotCached
}

// onlyNotCached reports whether every cause in err is frontend.ErrNotCached
// (uncached, blocked or add-refused on the debrid accounts). The resolver
// joins one error per failed candidate, so "every" means each joined branch;
// errors.Is alone would match a single uncached torrent among Usenet
// failures. A multi-%w error is treated the same way, which errs towards
// the general message.
func onlyNotCached(err error) bool {
	for err != nil {
		if err == frontend.ErrNotCached {
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

// requestClient retains the client profile sent on /decision so a native media
// loader's subsequent start.* request (which often omits X-Plex-* profile
// headers) keeps the negotiated container and player identity.
type requestClient struct {
	product            string
	version            string
	platform           string
	platformVersion    string
	device             string
	model              string
	profileName        string
	profileExtra       string
	clientCapabilities string
}

func requestClientFrom(pr *Request) requestClient {
	if pr == nil {
		return requestClient{}
	}
	return requestClient{
		product:            pr.Product,
		version:            pr.Version,
		platform:           pr.Platform,
		platformVersion:    pr.PlatformVersion,
		device:             pr.Device,
		model:              pr.Model,
		profileName:        pr.ProfileName,
		profileExtra:       pr.ProfileExtra,
		clientCapabilities: pr.ClientCapabilities,
	}
}

// playRequest is a parsed decision/start request.
type playRequest struct {
	client         requestClient
	manualSubtitle bool
	key            frontend.RatingKey
	collection     string
	caps           frontend.ClientCaps
	sourceLimits   *frontend.ClientCaps
	opts           frontend.DecideOptions
	sessionID      string // client transcode session id
	offset         time.Duration
	offsetSet      bool
	protocol       string
	copyTimestamps bool
	audioSel       int                  // stream position, -1 = default
	audioStream    *frontend.StreamInfo // selected identity, remapped after probing
	subSel         int                  // stream position, -1 = none/default
	subSet         bool
	// subNamed: this request's subtitleStreamID named the subtitle (or
	// None). Otherwise the selection is the stored or preferred one, which
	// is burned only when the client was shown it selected (#313).
	// Response shaping, zeroed by sameSettings.
	subNamed bool
	// The initial decision ignored the empty menu's Off echo.
	coldSubtitleOff bool
	// subStream is the subtitle picked by id (request or stored), to be
	// found again in the session's MediaInfo (resolvePlaySelection).
	subStream     *frontend.StreamInfo
	profileResult *deviceProfileResult
	// mediaIndex is the "Play Version" position the viewer picked
	// (0 = Auto / not sent); partReleaseID names the release directly
	// when the request addressed a variant's Part key.
	mediaIndex    int
	partReleaseID string
	// requestedPart is the Part id in the request path, when the client
	// addressed a Play Version part directly.
	requestedPart int64
	// mde: the client runs its own media decision engine (hasMDE=1, sent
	// by current Plex Web, iOS and Apple TV apps).
	mde bool
	// ownerBitrateCap: the server's per-user bitrate access policy lowered
	// MaxBitrateKbps; unlike a client transcode quality hint, an owner policy
	// still governs source selection when directPlay=1.
	ownerBitrateCap bool
	// listAllTracks: Android-family clients receive every probed track in
	// the decision response; recorded PMS decisions for web, iOS and Apple
	// TV are selected-only. On-demand subtitles are listed independently
	// because their tracks may only become known at Play. Set from the
	// request context in decisionContainer; zeroed by sameSettings because
	// it is response shaping, not a playback setting (an asymmetric compare
	// would re-prepare and restart the encoder on every start).
	listAllTracks bool
	// variantRelease is the release a Play Version pick resolved to ("" =
	// Auto) and mediaID the Media id the session was admitted on; both are
	// set by prepare and zeroed by sameSettings.
	variantRelease string
	mediaID        int64
	// subtitles is the client's subtitles= delivery mode (auto, burn,
	// none, sidecar, embedded, segmented). It never changes which track
	// is selected: PMS keeps the part's selected subtitle under every mode
	// and only marks how it is delivered (none: decision="ignore", the
	// player draws the track itself; sidecar: location="sidecar-subs").
	subtitles string
	// softText: the client is Plex's HTML player (Plex Web and the apps
	// built on it), which draws a text subtitle only from the ASS stream
	// it reads at /subtitles/:/transcode/universal/start (direct play) or
	// /video/:/transcode/universal/subtitles (conversion). Response
	// shaping, zeroed by sameSettings.
	softText bool
	// A fresh HLS decision can stage changed settings while the old output
	// continues to serve URLs already issued to the player.
	restartEncoder bool
}

// androidFamilyClient reports whether the decision lists every probed track.
// Plex's Android family and smart-TV HTML apps do; web, iOS and Apple TV stay
// selected-only. That is the PMS response shape, selected by the registry quirk
// plex-list-all-tracks rather than a playback capability.
func androidFamilyClient(pr *Request) bool {
	if pr == nil {
		return false
	}
	return frontend.HasQuirk(pr.Product, pr.Platform, pr.Device, pr.Model, pr.Version, "plex-list-all-tracks")
}

// plexSmartTVClient identifies official Plex HTML TV apps (LG webOS, Samsung
// Tizen, VIDAA, Vizio, Smart TVs, Xbox, PlayStation, Fetch TV, X1) whose
// players build their direct-play audio/subtitle menus from the pre-play
// metadata item rather than re-fetching metadata after playback starts.
func plexSmartTVClient(pr *Request) bool {
	if pr == nil || browserClient(pr) || libraryToolClient(pr) {
		return false
	}
	product := strings.ToLower(strings.TrimSpace(pr.Product))
	platform := strings.ToLower(strings.TrimSpace(pr.Platform))
	switch product {
	case "plex for lg", "plex for samsung", "plex for vidaa", "plex for vizio",
		"plex for smart tvs", "plex for xbox", "plex for playstation",
		"plex for playstation 4", "plex for playstation 5",
		"plex for fetch tv", "plex for x1":
		return true
	}
	switch platform {
	case "webos", "tizen", "vidaa", "xbox", "playstation":
		return true
	}
	return strings.EqualFold(strings.TrimSpace(pr.ProfileName), "HTML TV App")
}

// plexAndroidClient identifies the official Android phone and TV players.
// Plezy, Web and Apple players retain their existing contracts.
func plexAndroidClient(pr *Request) bool {
	if pr == nil {
		return false
	}
	product := strings.ToLower(pr.Product)
	return product == "plex for android" || product == "plex for android (mobile)" ||
		(strings.HasPrefix(product, "plex for android") && (strings.Contains(product, "(tv)") || strings.Contains(strings.ToLower(pr.Device), "shield")))
}

// plexIOSClient identifies the official Plex iOS and iPadOS players.
func plexIOSClient(pr *Request) bool {
	if pr == nil {
		return false
	}
	product := strings.ToLower(strings.TrimSpace(pr.Product))
	platform := strings.ToLower(strings.TrimSpace(pr.Platform))
	return product == "plex for ios" || (product == "plex" && (platform == "ios" || platform == "ipados"))
}

// plexMobileOrTVBedrockClient identifies the official Android and iOS players
// built on Plex's Bedrock player, which populate their subtitle picker from the
// pre-play detail/queue item and do not re-fetch metadata once playback starts.
func plexMobileOrTVBedrockClient(pr *Request) bool {
	return plexAndroidClient(pr) || plexIOSClient(pr)
}

// androidImageSubtitle is retained for native file playback, but must be burned
// once playback is converted: our segmented muxer carries text, not images.
func androidImageSubtitle(pr *Request, info frontend.MediaInfo, opts frontend.DecideOptions) bool {
	if !plexAndroidClient(pr) {
		return false
	}
	for _, st := range info.Streams {
		if st.Type == "subtitle" && !st.External && st.Index == opts.SubtitleStream {
			return frontend.IsImageSubtitle(st.Codec)
		}
	}
	return false
}

// keepsSelectedSubtitle retains tracks rendered by the client from Matroska,
// including native bitmap tracks. subtitles=none disables server rendering,
// not the selected track the client reads from the segments.
func keepsSelectedSubtitle(p *playRequest, info frontend.MediaInfo) bool {
	if p.protocol == frontend.ProtocolDASH {
		return false
	}
	if p.protocol != frontend.ProtocolHTTP && frontend.HLSContainer(p.caps, "", "") != frontend.ContainerMatroska {
		return false
	}
	for _, st := range info.Streams {
		if st.Type == "subtitle" && st.Index == p.opts.SubtitleStream {
			return !st.External && (frontend.IsTextSubtitle(st.Codec) ||
				(frontend.IsImageSubtitle(st.Codec) && !frontend.BurnsSubtitle(st, p.caps, p.opts)))
		}
	}
	return false
}

// browserClient reports Plex Web. Its HTML player never reads a text track
// out of the file or the segments: it renders soft subtitles with libjass
// from a separate ASS stream, and asks for an ASS track to be burned
// (subtitles=burn) under its default "Automatic" burn setting.
func browserClient(pr *Request) bool {
	return pr != nil && strings.Contains(strings.ToLower(pr.Product), "plex web")
}

// sameSettings compares playback controls, excluding the seek position (DASH
// spans the title) and device capabilities (clients may omit their profile on
// subsequent starts). Quality limits from the query are also in opts. Keep the
// original options since prepare adds owner limits and track preferences.
func (p playRequest) sameSettings(other playRequest) bool {
	p.client, other.client = requestClient{}, requestClient{}
	p.profileResult, other.profileResult = nil, nil
	p.offset, other.offset = 0, 0
	p.offsetSet, other.offsetSet = false, false
	p.caps, other.caps = frontend.ClientCaps{}, frontend.ClientCaps{}
	p.sourceLimits, other.sourceLimits = nil, nil
	p.listAllTracks, other.listAllTracks = false, false
	p.variantRelease, other.variantRelease = "", ""
	p.mediaID, other.mediaID = 0, 0
	p.softText, other.softText = false, false
	p.subNamed, other.subNamed = false, false
	p.coldSubtitleOff, other.coldSubtitleOff = false, false
	p.restartEncoder, other.restartEncoder = false, false
	// The burn option is compared in opts. Of the remaining modes, only
	// none changes what the encoder delivers.
	if (p.subtitles == "none") == (other.subtitles == "none") {
		p.subtitles, other.subtitles = "", ""
	}
	return reflect.DeepEqual(p, other)
}

// sessionEntryForStart finds the decision binding a start or subtitle request
// continues. Native media loaders (ExoPlayer, AVPlayer) often omit X-Plex-*
// profile headers or generate a fresh transcode session id under the same
// armed viewer/client playback attempt.
func (s *Server) sessionEntryForStart(pr *Request, sessionID string, key frontend.RatingKey) (*transcodeEntry, bool) {
	if pr == nil || pr.User == nil || key == 0 {
		return nil, false
	}
	if sessionID != "" {
		if e, ok := s.transcodes.get(sessionID); ok && e.failErr == nil && e.userID == pr.User.ID && e.key == key && (pr.ClientID == "" || e.clientID == "" || e.clientID == pr.ClientID) {
			return e, true
		}
	}
	if pr.ClientID == "" {
		return nil, false
	}
	now := s.now()
	var best *transcodeEntry
	for _, id := range s.transcodes.forClient(pr.User.ID, pr.ClientID, key) {
		e, ok := s.transcodes.get(id)
		if !ok || e.failErr != nil || e.playID == "" || e.userID != pr.User.ID || e.clientID != pr.ClientID || e.key != key {
			continue
		}
		if !e.updatedAt.IsZero() && now.Sub(e.updatedAt) >= playIdentityTTL {
			continue
		}
		if pr.SessionID != "" && e.playbackID != "" && e.playbackID != pr.SessionID {
			continue
		}
		if best == nil || e.updatedAt.After(best.updatedAt) {
			best = e
		}
	}
	return best, best != nil
}

// mergeSessionProfile fills client profile fields omitted by native media
// loaders from the session's decision binding, without altering authentication
// or ownership identifiers (ClientID, SessionID, diag).
func mergeSessionProfile(pr *Request, entry *transcodeEntry) *Request {
	if pr == nil || entry == nil {
		return pr
	}
	rc := entry.requested.client
	cp := *pr
	// Native media loaders can omit the playback header. Republishing the
	// decision from start must keep it on the same broker playback.
	cp.SessionID = cmp.Or(cp.SessionID, entry.playbackID)
	cp.Product = cmp.Or(cp.Product, rc.product, entry.caps.Product)
	cp.Version = cmp.Or(cp.Version, rc.version, entry.caps.Version)
	cp.Platform = cmp.Or(cp.Platform, rc.platform, entry.caps.Platform)
	cp.PlatformVersion = cmp.Or(cp.PlatformVersion, rc.platformVersion)
	cp.Device = cmp.Or(cp.Device, rc.device, entry.caps.Device)
	cp.Model = cmp.Or(cp.Model, rc.model)
	cp.ProfileName = cmp.Or(cp.ProfileName, rc.profileName)
	cp.ProfileExtra = cmp.Or(cp.ProfileExtra, rc.profileExtra)
	cp.ClientCapabilities = cmp.Or(cp.ClientCapabilities, rc.clientCapabilities)
	return &cp
}

// entryAudioPosition returns the slice index in entry.info.Streams of the
// audio track selected for a probed decision. Estimated decisions return -1 so
// a subsequent start re-resolves against real probed tracks.
func entryAudioPosition(entry *transcodeEntry) int {
	if entry == nil || entry.info.Estimated || entry.opts.AudioStream < 0 {
		return -1
	}
	for i, st := range entry.info.Streams {
		if st.Type == "audio" && st.Index == entry.opts.AudioStream {
			return i
		}
	}
	return -1
}

// entryExpectedStart normalizes a decision's requested controls to the stream
// identities resolved onto entry.info, so a start request that omits or
// defaults audioStreamID/subtitleStreamID matches the decision's selection.
func entryExpectedStart(entry *transcodeEntry) playRequest {
	if entry == nil {
		return playRequest{}
	}
	req := entry.requested
	if pos := entryAudioPosition(entry); pos >= 0 {
		st := entry.info.Streams[pos]
		req.audioSel, req.audioStream = pos, &st
	}
	if entry.subtitle >= 0 && entry.subtitle < len(entry.info.Streams) && entry.info.Streams[entry.subtitle].Type == "subtitle" {
		st := entry.info.Streams[entry.subtitle]
		req.subSel, req.subSet, req.subStream = entry.subtitle, true, &st
	} else if entry.subtitle < 0 && req.subSet {
		req.subSel, req.subStream = -1, nil
	}
	return req
}

// sessionWithInfo retains sidecar streams attached during decision preparation
// when reusing an underlying playback source session, without downgrading a
// probed session back to estimated metadata.
func sessionWithInfo(ps *frontend.PlaySession, info frontend.MediaInfo) *frontend.PlaySession {
	if ps == nil || len(info.Streams) == 0 || (info.Estimated && !ps.Info.Estimated) {
		return ps
	}
	cp := *ps
	cp.Info = info
	if len(cp.Info.Keyframes) == 0 && len(ps.Info.Keyframes) > 0 {
		cp.Info.Keyframes = ps.Info.Keyframes
	}
	return &cp
}

func (s *Server) parsePlayRequest(r *http.Request) (playRequest, error) {
	pr := FromContext(r.Context())
	if pr.ProfileName == "Raspberry PI" && resolvedPlayerProfile(pr) == "" {
		return playRequest{}, fmt.Errorf("profile name must identify a registered client profile")
	}
	if strings.Count(pr.ProfileExtra, "(") != strings.Count(pr.ProfileExtra, ")") {
		return playRequest{}, fmt.Errorf("malformed client profile directives")
	}
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
	sessionID := cmp.Or(q.Get("session"), q.Get("transcodeSessionId"), pr.SessionID)
	if sessionID == "" {
		sessionID = sanitizeSessionID(pr.ClientID + "-" + key.String())
	}
	sessionID = sanitizeSessionID(sessionID)
	if sessionID == "" {
		sessionID = newSessionID()
	}
	var entry *transcodeEntry
	var hasEntry bool
	capsPR := pr
	if !strings.HasSuffix(r.URL.Path, "/decision") {
		if entry, hasEntry = s.sessionEntryForStart(pr, sessionID, key); hasEntry {
			capsPR = mergeSessionProfile(pr, entry)
		}
	}
	caps := s.clientCapsForKey(capsPR, key)
	if s.dvBaseOnly(capsPR, key) {
		caps.DolbyVisionBaseOnly = true // this start stalled in Dolby Vision
	}
	if s.segmentedSurroundOff(capsPR) {
		caps.SegmentedSurroundOff = true // a stream stalled on copied surround audio
	}
	// Save device/source limits before the quality slider sets output limits.
	sourceLimits := caps
	sourceLimits.MaxBitrateKbps = 0
	sourceLimits.DefaultRemoteBitrate = false
	applyDecisionQuery(&caps, q)
	// Remote bitrate ceilings apply only when the authenticated source address
	// is remote. This ceiling constrains delivery, including direct play.
	if limit := s.cfg().Transcode.RemoteMaxBitrateKbps; caps.Remote && limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
		if caps.MaxBitrateKbps == 0 {
			caps.DefaultRemoteBitrate = true
		}
		caps.MaxBitrateKbps = limit
	}
	if caps.Remote && s.host.RemoteUploadCap != nil {
		allowTC := !s.cfg().Transcode.VideoTranscodingOff() && !caps.NoVideoTranscode
		if limit, fromBudget := s.host.RemoteUploadCap(r.Context(), sessionID, caps.Remote, allowTC, caps.MaxBitrateKbps, 0); fromBudget && limit > 0 && (caps.MaxBitrateKbps == 0 || limit < caps.MaxBitrateKbps) {
			if caps.MaxBitrateKbps == 0 {
				caps.DefaultRemoteBitrate = true
			}
			caps.MaxBitrateKbps = limit
		}
	}
	p := playRequest{client: requestClientFrom(capsPR), copyTimestamps: q.Get("copyts") == "1", key: key, caps: caps, sourceLimits: &sourceLimits, sessionID: sessionID, audioSel: -1, subSel: -1, protocol: q.Get("protocol"), mde: q.Get("hasMDE") == "1", partReleaseID: partReleaseID}
	// Plex for Roku sends hasMDE=1 on /decision only. A start that omits the
	// parameter continues that decision in the same mode; otherwise it no
	// longer matches the stored decision and re-plans with the legacy profile.
	if hasEntry && !q.Has("hasMDE") {
		p.mde = entry.requested.mde
	}
	for _, raw := range []string{q.Get("path"), q.Get("key")} {
		if partID, ok := partIDFromPath(raw); ok {
			p.requestedPart = partID
			break
		}
	}
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
	tt := chi.URLParam(r, "ttype")
	if p.protocol == "" {
		switch {
		case strings.HasSuffix(r.URL.Path, "/start.mpd"):
			p.protocol = frontend.ProtocolDASH
		case strings.HasSuffix(r.URL.Path, "/start.m3u8"):
			p.protocol = "hls"
		case strings.HasSuffix(r.URL.Path, "/start") || chi.URLParam(r, "ext") != "":
			p.protocol = frontend.ProtocolHTTP
		case tt == "audio" || tt == "music":
			p.protocol = frontend.ProtocolHTTP
		default:
			p.protocol = "hls"
		}
	}
	if tt == "audio" || tt == "music" {
		// Containers from video transcode targets do not describe music
		// output. The music target is applied after the source is probed.
		p.caps.HLSContainers = nil
		if rawContainer := strings.ToLower(strings.TrimSpace(q.Get("container"))); rawContainer != "" {
			p.caps.HLSContainers = []string{rawContainer}
		}
	}
	p.opts = frontend.DecideOptions{
		DirectPlay:     q.Get("directPlay") != "0",
		DirectStream:   q.Get("directStream") != "0",
		MaxBitrateKbps: caps.MaxBitrateKbps,
		MaxHeight:      parseResolutionHeight(q.Get("videoResolution")),
		AudioStream:    -1,
		SubtitleStream: -1,
		Protocol:       p.protocol,
	}
	if vb, err := strconv.Atoi(q.Get("videoBitrate")); err == nil && vb > 0 && (p.opts.MaxBitrateKbps == 0 || vb < p.opts.MaxBitrateKbps) {
		p.opts.MaxBitrateKbps = vb
	}
	p.subtitles = strings.ToLower(q.Get("subtitles"))
	if p.subtitles == "burn" {
		p.opts.BurnSubtitles = true
	}
	// Explicit stream ids in the request win over stored selections.
	// PMS treats -1 as the default selector (keep the part's selected stream).
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
	rawAudio := q.Get("audioStreamID")
	audioDefaultOrOmitted := rawAudio == "" || rawAudio == "-1"
	if id, err := strconv.ParseInt(rawAudio, 10, 64); err == nil && id > 0 {
		binding, rerr := s.resolveIssuedStream(frontend.WithStreamServe(r.Context(), pr.Remote, ""), pr.User, pr.ClientID, id)
		if rerr != nil && !streamMiss(rerr) {
			return playRequest{}, rerr
		}
		if rerr == nil && binding.Key == key && binding.Info.Type == "audio" {
			if binding.Estimated {
				// Pre-probe placeholder or estimated audio IDs carry no real
				// track identity: treat them as no explicit choice so the
				// viewer's language preferences apply to the real tracks.
				audioDefaultOrOmitted = true
			} else {
				info := binding.Info
				p.audioSel, p.audioStream = binding.Index, &info
			}
		}
	}
	rawSub := q.Get("subtitleStreamID")
	if rawSub == "0" && hasEntry && !entry.started && entry.requested.coldSubtitleOff && !sel.subtitleSet {
		rawSub = ""
	}
	subDefaultOrOmitted := rawSub == "" || rawSub == "-1"
	if rawSub != "" {
		if id, err := strconv.ParseInt(rawSub, 10, 64); err == nil {
			switch {
			case id == 0:
				p.subSel, p.subSet, p.subStream, p.subNamed = -1, true, nil, true
			case id > 0:
				binding, rerr := s.resolveIssuedStream(frontend.WithStreamServe(r.Context(), pr.Remote, ""), pr.User, pr.ClientID, id)
				if rerr != nil && !streamMiss(rerr) {
					return playRequest{}, rerr
				}
				if rerr == nil && binding.Key == key && binding.Info.Type == "subtitle" {
					info := binding.Info
					p.subSel, p.subSet, p.subStream, p.subNamed = binding.Index, true, &info, true
				}
			}
		}
	}
	rawSubStream := p.subStream
	inheritedSub := false
	if hasEntry {
		if audioDefaultOrOmitted && !sel.audioSet {
			if pos := entryAudioPosition(entry); pos >= 0 {
				st := entry.info.Streams[pos]
				p.audioSel, p.audioStream = pos, &st
			} else if entry.requested.audioSel >= 0 || entry.requested.audioStream != nil {
				p.audioSel, p.audioStream = entry.requested.audioSel, entry.requested.audioStream
			}
		} else if p.audioSel >= 0 && p.audioStream != nil && !entry.info.Estimated {
			if pos := frontend.ResolveAudio(p.audioStream, p.audioSel, entry.info); pos >= 0 && pos < len(entry.info.Streams) {
				st := entry.info.Streams[pos]
				p.audioSel, p.audioStream = pos, &st
			}
		}
		if subDefaultOrOmitted && !p.subNamed && !sel.subtitleSet {
			if entry.subtitle >= 0 && entry.subtitle < len(entry.info.Streams) && entry.info.Streams[entry.subtitle].Type == "subtitle" {
				st := entry.info.Streams[entry.subtitle]
				p.subSel, p.subSet, p.subStream = entry.subtitle, true, &st
				inheritedSub = true
			} else if entry.subtitle < 0 && entry.requested.subSet && entry.requested.subStream == nil {
				p.subSel, p.subSet, p.subStream = -1, true, nil
				inheritedSub = true
			}
		} else if p.subSet && p.subStream != nil && (!p.subStream.External || slices.ContainsFunc(entry.info.Streams, func(s frontend.StreamInfo) bool {
			return s.External && s.ExternalURL == p.subStream.ExternalURL
		})) {
			if pos := resolveSubtitle(p.subStream, p.subSel, entry.info); pos >= 0 && pos < len(entry.info.Streams) {
				st := entry.info.Streams[pos]
				p.subSel, p.subStream = pos, &st
			}
		}
	}
	echoPR := pr
	if hasEntry && pr != nil && pr.ClientID == "" && entry.clientID != "" {
		cp := *pr
		cp.ClientID = entry.clientID
		echoPR = &cp
	}
	// A client commonly echoes the track Ether marked selected. That is
	// not a manual veto of the viewer's automatic-download rule. Off is
	// always explicit; PUT choices also carry a durable manual intent.
	if inheritedSub {
		p.manualSubtitle = entry.requested.manualSubtitle
	} else {
		echo := s.subtitleSelectionEcho(echoPR, key, rawSubStream) || (p.subStream != rawSubStream && s.subtitleSelectionEcho(echoPR, key, p.subStream))
		p.manualSubtitle = p.subSet && !echo
	}
	p.offset = durationParam(q.Get("offset"), time.Second)
	p.offsetSet = q.Has("offset")
	return p, nil
}

// resumeStartHint chooses a speculative start position when a start request
// omitted offset. An explicit offset on the preceding /decision (including 0
// for "Play from Beginning") takes precedence over saved watch state so
// clients that send offset on decision only are never overridden.
func (s *Server) resumeStartHint(ctx context.Context, pr *Request, p playRequest, entry, priorEntry *transcodeEntry) time.Duration {
	if p.offsetSet || p.offset != 0 {
		return 0
	}
	switch {
	case entry != nil && entry.requested.offsetSet:
		return entry.offset
	case priorEntry != nil && !priorEntry.started && priorEntry.requested.offsetSet:
		return priorEntry.offset
	case s.state() != nil:
		if st, err := s.state().Get(ctx, pr.User.ID, p.key); err == nil {
			return st.ViewOffset
		}
	}
	return 0
}

func (p *playRequest) sourceDecisionContext(ctx context.Context) context.Context {
	if p.audioSel >= 0 || p.audioStream != nil || p.subSel >= 0 || p.subStream != nil {
		return ctx
	}
	return frontend.WithSourceDecision(ctx, frontend.SourceDecision{Options: p.opts})
}

// sourceSelectionCaps keeps request quality limits at delivery selection. Device
// decoder limits and owner quality rules still govern which source may open.
// An HD output request may softly prefer a suitable HD source on new admission.
func (p *playRequest) sourceSelectionCaps() frontend.ClientCaps {
	caps := p.caps
	if p.sourceLimits != nil {
		caps.MaxHeight = p.sourceLimits.MaxHeight
		caps.ExplicitMaxHeight = p.sourceLimits.ExplicitMaxHeight
		caps.MaxHeightSource = p.sourceLimits.MaxHeightSource
		caps.MaxBitrateKbps = p.sourceLimits.MaxBitrateKbps
		caps.DefaultRemoteBitrate = p.sourceLimits.DefaultRemoteBitrate
	}
	// SD delivery caps always shape the encode, never source selection.
	// A client transcode quality hint (maxVideoBitrate) also never caps
	// direct-play source selection when the client requested directPlay=1;
	// owner bitrate access policies still apply.
	if (!p.opts.DirectPlay || p.ownerBitrateCap) && !p.caps.DefaultRemoteBitrate && p.caps.MaxBitrateKbps >= 1500 {
		caps.MaxBitrateKbps = p.caps.MaxBitrateKbps
		caps.DefaultRemoteBitrate = false
	} else if p.caps.NoVideoTranscode && p.caps.MaxBitrateKbps > 0 {
		// When video transcoding is off for this viewer, any bitrate ceiling
		// must guide release selection instead of failing after admission.
		caps.MaxBitrateKbps = p.caps.MaxBitrateKbps
		caps.DefaultRemoteBitrate = p.caps.DefaultRemoteBitrate
	}
	if p.opts.MaxHeight >= 720 && !p.opts.DirectPlay && !p.opts.DirectStream {
		caps.PreferredSourceHeight = p.opts.MaxHeight
	}
	return caps
}

func isPlayableKey(k frontend.RatingKey) bool {
	if k.IsPluginKey() {
		return true
	}
	switch k.Kind() {
	case frontend.KindMovie, frontend.KindEpisode, frontend.KindLocal:
		return true
	}
	return false
}

// sanitizeSessionID keeps session ids path-safe.
func newSessionID() string {
	return rand.Text()
}

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
func applySelections(p *playRequest, info frontend.MediaInfo, langs []string) {
	audio := p.audioSel
	if audio < 0 || audio >= len(info.Streams) {
		audio = pickAudio(info, langs)
	}
	if audio >= 0 && audio < len(info.Streams) && info.Streams[audio].Type == "audio" {
		p.opts.AudioStream = info.Streams[audio].Index
	}
	// Rebuilding a plan must clear the prior selection when the viewer
	// switches subtitles off or their language is absent on this source.
	p.opts.SubtitleStream = -1
	if p.subSet && p.subSel >= 0 && p.subSel < len(info.Streams) && info.Streams[p.subSel].Type == "subtitle" {
		st := info.Streams[p.subSel]
		if !st.External || (p.opts.BurnTextSubtitles && st.ExternalURL != "") {
			p.opts.SubtitleStream = st.Index
		}
	}
	p.audioSel = audio
}

// prepare resolves the title for this client and makes the decision.
func (s *Server) prepare(ctx context.Context, pr *Request, p *playRequest) (*frontend.Title, *frontend.PlaySession, frontend.Decision, error) {
	requested := *p
	if s.playback() == nil {
		return nil, nil, frontend.Decision{}, errors.New("playback not configured")
	}
	t, err := s.metadataTitle(ctx, pr, p.key)
	if err != nil || (t != nil && t.IsFutureRelease(s.now())) {
		if title, _, e := s.lookupPlaceholder(ctx, pr, p.key, p.collection); e == nil {
			return title, nil, frontend.Decision{}, frontend.ErrUnreleased
		}
		if err == nil {
			err = frontend.ErrNotFound
		}
	}
	if err != nil {
		return nil, nil, frontend.Decision{}, err
	}
	// Policy only: discovery evidence never blocks a play attempt.
	if !s.pluginAwarePlayable(ctx, pr.User, t) {
		return nil, nil, frontend.Decision{}, frontend.ErrNotFound
	}
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, p.caps.Remote, s.now()) {
		return nil, nil, frontend.Decision{}, frontend.ErrUnauthorized
	}
	s.stampViewerTranscoding(pr.User, &p.caps)
	if policy, ok := s.policy().(frontend.BitrateAccessPolicy); ok {
		if limit := policy.BitrateLimit(pr.User); limit > 0 && (p.caps.MaxBitrateKbps == 0 || limit < p.caps.MaxBitrateKbps) {
			p.caps.MaxBitrateKbps = limit
			p.caps.DefaultRemoteBitrate = false
			p.opts.MaxBitrateKbps = limit
			p.ownerBitrateCap = true
		}
	}
	explicitBitrateCap := !p.caps.DefaultRemoteBitrate && p.opts.MaxBitrateKbps > 0
	s.observeClientAdmission(pr, p.caps)
	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		return t, nil, frontend.Decision{}, frontend.ErrUnauthorized
	}
	// The version list this client was issued stays pinned while the
	// session is admitted, so a page render racing a background refill
	// cannot move the Media the request's mediaIndex names.
	ik := s.playIdentityFor(pr, p.key)
	s.playIDs.beginAdmission(ik, s.now())
	var ps *frontend.PlaySession
	admitted := false
	defer func() {
		release := ""
		if admitted && ps.Resolution != nil {
			release = ps.Resolution.Release.ID
		}
		s.playIDs.endAdmission(ik, p.mediaID, release, admitted, s.now())
		if admitted {
			s.trackRestarts.admitted(ik, p.variantRelease, release, s.now())
		}
	}()
	dec, err := s.openPlexPlan(ctx, pr, p, func(ctx context.Context) (frontend.PlanAdmission, error) {
		var prepErr error
		ps, prepErr = s.preparePlayback(ctx, pr, p)
		if prepErr != nil {
			return frontend.PlanAdmission{}, prepErr
		}
		addon := ps.Resolution != nil && ps.Resolution.Release.IsAddonHTTPStream()
		// A cold client can echo None from a Part that had no subtitle
		// tracks. Defer to viewer rules until it has a real menu to choose from.
		if addon && !ps.Info.Estimated && p.subNamed && p.subSel < 0 && p.subStream == nil &&
			!s.playIDs.listedSubtitles(ik, s.now()) && !s.selections.get(pr.User.ID, int64(p.key)).subtitleSet {
			requested.coldSubtitleOff, requested.manualSubtitle = true, false
			p.subNamed, p.subSet, p.manualSubtitle = false, false, false
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
		// With no subtitle in the request, a sidecar this client was shown
		// selected (a cached OpenSubtitles file) is what it expects to see.
		// Cached sidecars omit a language the file embeds, so without this the
		// rules pick the embedded track (often an image subtitle the client
		// was never shown) and the shown-only burn rule then plays None.
		// An embedded text track of the same language still wins: it needs
		// no burn and plays as before. A remembered Off wins over the shown
		// sidecar: the viewer turned subtitles off for this title.
		if !p.subNamed && !p.subSet {
			if shown, _ := s.playIDs.shownSubtitles(ik, s.now()); len(shown) > 0 {
				for i := range shown {
					want := shown[i]
					if !want.External || want.ExternalURL == "" || frontend.IsImageSubtitle(want.Codec) {
						continue
					}
					if slices.ContainsFunc(ps.Info.Streams, func(st frontend.StreamInfo) bool {
						return st.Type == "subtitle" && !st.External && !frontend.IsImageSubtitle(st.Codec) && sameLanguage(st.Language, want.Language)
					}) {
						continue
					}
					if !s.rememberedSubtitleOff(ctx, pr, p.key) {
						p.subStream, p.subSel, p.subSet = &want, -1, true
					}
					break
				}
			}
		}
		if p.subNamed && p.subStream != nil && p.subStream.External {
			cp := *ps
			cp.Info = infoWithExactSidecar(ps.Info, p.subStream)
			ps = &cp
		} else {
			ps = ensureSelectedSidecar(ps, p.subStream)
		}
		// "burn" explicitly asks the server to render text as well as image
		// subtitles, including a selected sidecar.
		p.opts.BurnTextSubtitles = p.subtitles == "burn"
		resolvePlaySelection(p, ps.Info)
		s.applyStreamPreferences(ctx, pr, p, t, ps.Info)
		applySelections(p, ps.Info, s.audioLanguages(pr.User))
		s.selectLegacyDecoderAudio(pr, p, ps.Info)
		if !addon && p.subtitles == "none" && !keepsSelectedSubtitle(p, ps.Info) && !androidImageSubtitle(pr, ps.Info, p.opts) {
			// The client handles the selected track itself.
			p.opts.SubtitleStream = -1
		}
		if androidImageSubtitle(pr, ps.Info, p.opts) {
			dec := frontend.DecideTranscode(ps.Info, p.caps, p.opts, frontend.TranscodePolicy{})
			// Source-change remuxing is also a conversion, even when MDE
			// initially allowed direct play of the original file. Skip the
			// proactive Usenet burn when the client can direct-play PGS itself.
			converting := !p.opts.DirectPlay || ps.RequiresRemux
			if onDemandSession(ps) && p.mde && p.opts.DirectStream && ps.Resolution != nil {
				// A preliminary direct-play decision can still become an optional
				// source-safety remux in the broker. Exempt only the native paths
				// that sessions.Decide also keeps out of that remux.
				native := false
				if v := ps.Info.Video(); v != nil && dec.Kind == frontend.DecisionDirectPlay {
					targets := streamingVideoTarget(pr.ProfileExtra, p.protocol)
					if len(p.caps.VideoCodecs) > 0 {
						targets = slices.DeleteFunc(targets, func(c string) bool { return !p.caps.SupportsVideo(c) })
					}
					native = (v.DolbyVision && strings.EqualFold(p.caps.Platform, "Android") && frontend.SupportsDVProfile(p.caps, v.DolbyVisionProfile)) ||
						(v.Codec != "" && len(targets) > 0 && !slices.Contains(targets, v.Codec))
				}
				protocol := ps.Resolution.Release.Protocol
				converting = converting || (!native && protocol != frontend.ProtocolHTTP && protocol != frontend.ProtocolTorrent)
			}
			// A quality/codec limit can require conversion even though the
			// client allowed direct play.
			if converting || dec.Kind != frontend.DecisionDirectPlay {
				p.opts.BurnSubtitles = true
			}
		}
		if !addon {
			s.burnOnlyShownSubtitle(pr, p, ps.Info)
		}
		if ps.Info.Video() == nil {
			applyMusicDirectPlay(&p.caps, pr.ProfileExtra, ps.Info, p.opts.AudioStream)
			if len(p.caps.HLSContainers) == 0 && len(p.caps.TranscodeAudioCodecs) == 0 {
				targetContainers, targetCodecs := streamingMusicTarget(pr.ProfileExtra, p.protocol)
				if len(targetContainers) > 0 {
					p.caps.HLSContainers = targetContainers
				}
				if len(targetCodecs) > 0 {
					p.caps.TranscodeAudioCodecs = targetCodecs
				}
			} else if p.protocol == frontend.ProtocolHTTP && len(p.caps.HLSContainers) == 0 && len(p.caps.TranscodeAudioCodecs) > 0 {
				switch p.caps.TranscodeAudioCodecs[0] {
				case "mp3":
					p.caps.HLSContainers = []string{"mp3"}
				case "opus", "vorbis":
					p.caps.HLSContainers = []string{"ogg"}
				case "flac":
					p.caps.HLSContainers = []string{"flac"}
				case "aac":
					p.caps.HLSContainers = []string{"mp4"}
				case "wav":
					p.caps.HLSContainers = []string{"wav"}
				}
			}
		} else {
			p.caps.TranscodeVideoCodecs = streamingVideoTarget(pr.ProfileExtra, p.protocol)
			if len(p.caps.VideoCodecs) > 0 && len(p.caps.TranscodeVideoCodecs) > 0 {
				var kept []string
				for _, c := range p.caps.TranscodeVideoCodecs {
					if p.caps.SupportsVideo(c) {
						kept = append(kept, c)
					}
				}
				p.caps.TranscodeVideoCodecs = kept
			}
			p.caps.TranscodeAudioCodecs = streamingAudioTarget(pr.ProfileExtra, p.protocol)
			if len(p.caps.AudioCodecs) > 0 && len(p.caps.TranscodeAudioCodecs) > 0 {
				var kept []string
				for _, c := range p.caps.TranscodeAudioCodecs {
					if p.caps.SupportsAudio(c) {
						kept = append(kept, c)
					}
				}
				p.caps.TranscodeAudioCodecs = kept
			}
		}
		if err := s.applyDeviceProfile(pr, t, ps, p); err != nil {
			return frontend.PlanAdmission{}, err
		}
		// A quality re-decision sends directStream=0 against the previous Media.
		// When source selection chose a release that already fits the explicit
		// cap, allow Decide to remux video if codec and container rules permit.
		if !p.opts.DirectPlay && !p.opts.DirectStream && explicitBitrateCap && p.opts.MaxBitrateKbps > 0 &&
			ps.Info.BitrateKbps > 0 && ps.Info.BitrateKbps <= p.opts.MaxBitrateKbps &&
			(p.opts.MaxHeight == 0 || ps.Info.Video() == nil || ps.Info.Video().Height <= p.opts.MaxHeight) {
			p.opts.DirectStream = true
		}
		if s.svc().support != nil && s.svc().support.Recording() {
			ev := s.supportEvent(pr, frontend.DiagnosticKindClientCapabilities)
			ev.Client = s.supportClientFacts(pr, p.caps, "output")
			ev.Client.OutputProtocol = p.protocol
			s.svc().support.Observe(ev)
		}
		return frontend.PlanAdmission{
			Source: ps, Media: ps.Info, Caps: p.caps, Options: p.opts, PreferRemux: p.mde, Protocol: p.protocol,
			RemoteMaxBitrateKbps: s.cfg().Transcode.RemoteMaxBitrateKbps,
		}, nil
	})
	if err != nil {
		return t, nil, frontend.Decision{}, err
	}
	admitted = true
	s.logClientIdentity(pr, p.caps)
	if v := ps.Info.Video(); v != nil {
		s.log.Debug("playback media decision", "key", p.key, "client", pr.ClientID, "device", pr.Device, "model", pr.Model, "platformVersion", pr.PlatformVersion, "video", v.Codec, "videoProfile", v.Profile, "height", v.Height, "bitDepth", v.BitDepth, "transfer", v.ColorTransfer, "dvProfile", v.DolbyVisionProfile, "dvLevel", v.DolbyVisionLevel, "dvCompatibility", v.DolbyVisionCompatibilityID, "kind", dec.Kind, "outputContainer", dec.Container, "videoCopy", dec.VideoCopy, "audioCopy", dec.AudioCopy, "hdrCap", p.caps.HDR10, "dvCap", p.caps.DolbyVision, "profileExtra", pr.ProfileExtra)
	}
	subtitle := -1
	if p.subSet && p.subSel >= 0 && p.subSel < len(ps.Info.Streams) && ps.Info.Streams[p.subSel].Type == "subtitle" {
		subtitle = p.subSel
	}
	s.observePlaybackDecision(ctx, pr, p, ps)
	s.playbackOwners.Bind(pr.User.ID, pr.ClientID, p.key, pr.SessionID, ps.ID, true, s.now())
	next := &transcodeEntry{
		id: p.sessionID, playID: ps.ID, playbackID: pr.SessionID, userID: pr.User.ID, clientID: pr.ClientID, key: p.key,
		requested: requested,
		decision:  dec, info: ps.Info, subtitle: subtitle, caps: p.caps, opts: p.opts, offset: p.offset, updatedAt: s.now(),
	}
	if old, ok := s.transcodes.get(next.id); ok && old.started &&
		old.requested.protocol == "hls" && next.requested.protocol == "hls" &&
		old.key == next.key && old.userID == next.userID && old.clientID == next.clientID &&
		(old.playbackID == next.playbackID || next.playbackID == "") {
		next.started, next.offset, next.playbackID = true, old.offset, old.playbackID
		next.requested.restartEncoder = old.requested.restartEncoder || old.playID != next.playID ||
			!next.requested.sameSettings(entryExpectedStart(old)) || !reflect.DeepEqual(next.opts, old.opts)
	}
	s.transcodes.put(next)
	return t, ps, dec, nil
}

func onDemandSession(ps *frontend.PlaySession) bool {
	return ps.Resolution != nil && !strings.HasPrefix(ps.Resolution.Release.ID, "local:")
}

// handleDecision is /video/:/transcode/universal/decision.
func (s *Server) handleDecision(w http.ResponseWriter, r *http.Request) {
	if s.refuseCapability(w, r) {
		return
	}
	if s.handleLiveTVPlayback(w, r) {
		return
	}
	if s.handleExtraPlayback(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	if arm, err := s.parsePlayRequest(r); err == nil {
		owner := decisionOwner(pr)
		owner.Generation = arm.sessionID
		s.decisionArms.Arm(owner, arm.key)
		if isPlayableKey(arm.key) {
			s.recentClients.recordTitle(r, pr, arm.key, s.now())
		}
	}
	// A slow source search must not hold the owner gates (timeline and stop
	// share them) or outlast the client's request timeout.
	if t, err := s.awaitSource(ctx, r, pr); err != nil {
		if ctx.Err() != nil {
			return
		}
		if frontend.WritePlaybackPending(w, err, notificationLanguage(r)) {
			return
		}
		s.log.Warn("playback decision failed before admission", "user", pr.User.Username, "reason", playbackMessage(err), "err", err)
		p, _ := s.parsePlayRequest(r)
		// start.m3u8/start.mpd follow a failed decision for the same session:
		// answer them with this error rather than a second full resolve.
		s.transcodes.fail(p.sessionID, pr.User.ID, pr.ClientID, p.key, err, s.now())
		s.write(w, r, s.decisionError(ctx, pr, t, err, p))
		return
	}
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	if tt := chi.URLParam(r, "ttype"); tt != "" && tt != "video" && tt != "music" && tt != "audio" {
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
		var profileErr *profileDecisionError
		if errors.As(err, &profileErr) && profileErr.code == 0 {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		if errors.Is(err, errDecisionMediaIndex) {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		if errors.Is(err, frontend.ErrNotFound) && t == nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if frontend.WritePlaybackPending(w, err, notificationLanguage(r)) {
			return
		}
		s.log.Warn("playback decision failed", "key", p.key, "user", pr.User.Username, "reason", playbackMessage(err), "err", err)
		// The client follows a failed decision with start.mpd/start.m3u8
		// for the same session: answer it with this error, not a second
		// full resolve that fails the same way.
		s.transcodes.fail(p.sessionID, pr.User.ID, pr.ClientID, p.key, err, s.now())
		s.write(w, r, s.decisionError(ctx, pr, t, err, p))
		return
	}
	mc := s.decisionContainer(ctx, pr, t, ps, dec, p)
	audio, subtitles, selectedSubtitle := 0, 0, int64(0)
	if len(mc.Metadata) > 0 && len(mc.Metadata[0].Media) > 0 {
		chosenMedia := mc.Metadata[0].Media[0]
		for _, m := range mc.Metadata[0].Media {
			if m.Selected != nil && *m.Selected {
				chosenMedia = m
				break
			}
		}
		if len(chosenMedia.Part) > 0 {
			for _, st := range chosenMedia.Part[0].Stream {
				switch st.StreamType {
				case 2:
					audio++
				case 3:
					subtitles++
					if st.Selected != nil && *st.Selected {
						selectedSubtitle = st.ID
					}
				}
			}
		}
	}
	s.log.Debug("playback decision tracks", "key", p.key, "client", pr.ClientID,
		"session", p.sessionID, "mediaIndex", p.mediaIndex, "mediaID", p.mediaID,
		"directPlay", p.opts.DirectPlay, "directStream", p.opts.DirectStream,
		"maxBitrate", p.opts.MaxBitrateKbps, "maxHeight", p.opts.MaxHeight,
		"audioStream", p.audioSel, "subtitles", p.subtitles, "subtitleStream", p.subSel,
		"audioTracks", audio, "subtitleTracks", subtitles, "selectedSubtitle", selectedSubtitle)
	s.write(w, r, mc)
}

// decisionSourceWait delegates the deadline to the shared playback service
// (PrepareTimeout) and the HTTP request context. A short adapter timer turned
// an in-flight source into a terminal decision error, which native clients do
// not automatically retry. No recorded PMS pending-decision response or exact
// native client timeout is established by our conformance fixtures.
const decisionSourceWait time.Duration = 0

// awaitSource joins the shared source flight outside the owner gates. Explicit
// version picks and requests this cannot pre-check go straight on. A dropped
// request stops waiting; the manager owns cancellation and continuation grace.
func (s *Server) awaitSource(ctx context.Context, r *http.Request, pr *Request) (*frontend.Title, error) {
	pw, ok := s.playback().(frontend.PlaybackPrepareWithin)
	if !ok || pr == nil || pr.User == nil {
		return nil, nil
	}
	if tt := chi.URLParam(r, "ttype"); tt != "" && tt != "video" && tt != "music" && tt != "audio" {
		return nil, nil
	}
	p, err := s.parsePlayRequest(r)
	if err != nil || p.key == 0 || p.key.IsPluginKey() || p.partReleaseID != "" || p.mediaIndex > 0 || !isPlayableKey(p.key) || s.catalog() == nil {
		return nil, nil
	}
	t, err := s.metadataTitle(ctx, pr, p.key)
	if err != nil || t == nil || !s.pluginAwarePlayable(ctx, pr.User, t) {
		return nil, nil
	}
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, p.caps.Remote, s.now()) {
		return nil, nil
	}
	s.stampViewerTranscoding(pr.User, &p.caps)
	if policy, ok := s.policy().(frontend.BitrateAccessPolicy); ok {
		if limit := policy.BitrateLimit(pr.User); limit > 0 && (p.caps.MaxBitrateKbps == 0 || limit < p.caps.MaxBitrateKbps) {
			p.caps.MaxBitrateKbps = limit
			p.caps.DefaultRemoteBitrate = false
			p.ownerBitrateCap = true
		}
	}
	if p.caps.ClientID == "" {
		p.caps.ClientID = pr.ClientID
	}
	// Wait for the result instead of converting slow preparation to an error.
	// A genuine failure is answered now so admission does not repeat the search.
	// A restart after a track change waits for the release being watched.
	release, exact := s.restartRelease(s.playIdentityFor(pr, p.key), "")
	if _, err := pw.PrepareWithin(p.sourceDecisionContext(ctx), decisionSourceWait, pr.User, p.key, p.sourceSelectionCaps(), release, exact); err != nil {
		return t, err
	}
	return nil, nil
}

// decisionError is the decision response for a failed resolve: no Media,
// and the reason in every *DecisionText so whichever field a client shows
// carries the message instead of a spinner.
func (s *Server) decisionError(ctx context.Context, pr *Request, t *frontend.Title, err error, p playRequest) *model.MediaContainer {
	var profileErr *profileDecisionError
	if errors.As(err, &profileErr) {
		return profileErrorContainer(pr, p, profileErr)
	}
	msg := s.playbackMessageFor(pr, err)
	s.setTermination(trackID(pr.User.ID, pr.ClientID, pr.SessionID), msg)
	mc := libraryContainer()
	if t != nil {
		mc = s.titleContainer(ctx, t)
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
	mc.Extra = append(mc.Extra, model.A("resourceSession", cmp.Or(pr.SessionID, pr.ClientID)))
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

func (s *Server) decisionContainer(ctx context.Context, pr *Request, t *frontend.Title, ps *frontend.PlaySession, dec frontend.Decision, p playRequest) *model.MediaContainer {
	mc := s.titleContainer(ctx, t)
	md := s.metadata(ctx, pr, t, renderOpts{detail: true, media: true})
	decisionItem(&md)
	// With hasMDE=1 and direct play allowed, the client's own decision
	// engine decides: PMS answers with the source media as direct play and
	// only an mdeDecisionCode. Ether's own decision is kept for start.m3u8
	// in case the client then asks for a stream.
	//
	// An on-demand title's metadata is an estimate until it is probed, so
	// the client's engine may have judged it playable from streams it
	// does not have (an AC3 track estimated as AAC for a browser). When
	// Ether's own decision must convert a stream for this client, say so
	// instead of "Direct play OK", whatever directStream says (#246).
	mustConvert := dec.Kind == frontend.DecisionTranscode || (dec.Kind != frontend.DecisionDirectPlay && (!dec.VideoCopy || !dec.AudioCopy))
	pm4k := strings.EqualFold(pr.Product, "PM4K") || strings.EqualFold(pr.Product, "Plex for Kodi")
	// The owner's "Convert Dolby and DTS to AAC" rule outranks the client's
	// engine: a surround track it converted is never answered direct play.
	ownerConverts := p.caps.ConvertSurroundAudio && dec.Kind != frontend.DecisionDirectPlay && !dec.AudioCopy
	// A quality the viewer picked below the source (maxVideoBitrate or
	// videoResolution in this request) rules direct play out even for an
	// MDE client: Plex for Mac re-asks with directPlay=1 after a quality
	// change, and answering direct play makes it keep ranging the original
	// part instead of calling start (#420).
	belowSource := !dec.VideoCopy && qualityBelowSource(p.caps, ps.Info)
	mdeDirect := p.mde && p.opts.DirectPlay && !belowSource && !dec.StripDolbyVision && !dec.BurnSubtitles && (!pm4k || dec.Kind == frontend.DecisionDirectPlay) && len(p.opts.AudioFilters) == 0 && !ownerConverts &&
		!(onDemandSession(ps) && (mustConvert || (p.opts.DirectStream && dec.Kind != frontend.DecisionDirectPlay)))
	shown := dec
	if mdeDirect {
		shown = frontend.Decision{Kind: frontend.DecisionDirectPlay, VideoCopy: true, AudioCopy: true, Container: ps.Info.Container}
	}
	p.listAllTracks = androidFamilyClient(pr)
	p.softText = browserClient(pr)
	if plexAndroidClient(pr) && shown.BurnSubtitles && p.subtitles == "none" {
		p.subtitles = "burn"
	}
	if ps.Resolution != nil && ps.Resolution.Release.IsAddonHTTPStream() && p.subtitles == "none" &&
		p.subSet && p.subSel >= 0 && p.subSel < len(ps.Info.Streams) {
		if track := ps.Info.Streams[p.subSel]; track.External || (shown.Kind != frontend.DecisionDirectPlay && shown.Container != frontend.ContainerMatroska) {
			p.subtitles = "sidecar"
		}
	}
	chosen := s.decisionMedia(t, ps, shown, p, pr.User)
	if pr.configuredSlots && len(md.Media) > 0 {
		replaced := false
		for i := range md.Media {
			md.Media[i].Selected = nil
			if len(md.Media[i].Part) > 0 {
				md.Media[i].Part[0].Selected = nil
			}
			if md.Media[i].ID == chosen.ID {
				md.Media[i] = chosen
				replaced = true
			}
		}
		if !replaced {
			md.Media = []model.Media{chosen}
		}
	} else {
		md.Media = []model.Media{chosen}
	}
	s.addProbedStreamFeatures(ctx, t.Key, ps.Info, &md)
	if chosen.Duration > 0 {
		md.Duration = chosen.Duration
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
	case dec.Kind == frontend.DecisionDirectPlay:
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
		if p.profileResult != nil && p.profileResult.reason != "" {
			dpText = p.profileResult.reason
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
	if pm4k && mdeDirect {
		mc.Extra = append(mc.Extra, model.A("directPlayDecisionCode", codeDirectPlayOK), model.A("directPlayDecisionText", textDirectPlayOK), model.A("generalDecisionCode", codeDirectPlayOK), model.A("generalDecisionText", textDirectPlayOK))
	}
	if pm4k && p.mde && !mdeDirect {
		code, text := codeConversionOK, textConversionOK
		if dec.Kind == frontend.DecisionDirectPlay {
			code, text = codeDirectPlayOK, textDirectPlayOK
		}
		mc.Extra = append(mc.Extra, model.A("mdeDecisionCode", code), model.A("mdeDecisionText", text))
	}
	// PMS echoes the client identifier as the resource session.
	mc.Extra = append(mc.Extra, model.A("resourceSession", cmp.Or(pr.SessionID, pr.ClientID)))
	return mc
}

// qualityBelowSource reports whether the request's own quality limits
// (applyDecisionQuery) are below the source: a bitrate cap under the
// source bitrate, or an explicit resolution under the source height.
func qualityBelowSource(caps frontend.ClientCaps, info frontend.MediaInfo) bool {
	if caps.MaxBitrateKbps > 0 && !caps.DefaultRemoteBitrate && info.BitrateKbps > caps.MaxBitrateKbps {
		return true
	}
	if v := info.Video(); v != nil && caps.ExplicitMaxHeight && caps.MaxHeight > 0 && v.Height > caps.MaxHeight {
		return true
	}
	return false
}

// decisionMedia renders the Media with decision attributes.
func (s *Server) decisionMedia(t *frontend.Title, ps *frontend.PlaySession, dec frontend.Decision, p playRequest, u *frontend.User) model.Media {
	stamp := ps.StartedAt.Unix()
	if stamp <= 0 {
		stamp = 1
	}
	info := ps.Info
	m := buildMedia(t, info, stamp)
	// The decision names the Media the session was admitted on, with the
	// Part key and stream ids that Media was issued (play_identity.go).
	var ik playIdentityKey
	if p.mediaID != 0 && u != nil {
		ik = playIdentityKey{userID: u.ID, clientID: p.caps.ClientID, key: t.Key}
		if p.mediaID != int64(t.Key) {
			m.ID = p.mediaID
			m.Part[0].ID = p.mediaID
			m.Part[0].Key = mediaPartKey(p.mediaID, stamp, info)
		}
		m.Part[0].Key = s.playIDs.partKey(ik, m.ID, m.Part[0].Key, s.now())
	}
	if ps.OriginalRevision != "" {
		m.Part[0].Key += "?sourceRevision=" + ps.OriginalRevision
	}
	if ps.Resolution != nil && s.trickplayAvailable(frontend.TrickplayCacheKey(ps)) {
		m.Part[0].Indexes = "sd"
	}
	m.Selected = model.Ptr(true)
	m.SetAlphabetical()
	part := &m.Part[0]
	part.Selected = model.Ptr(true)
	part.SetAlphabetical()
	sel := selection{audio: p.audioSel, audioSet: p.audioSel >= 0, subtitle: p.subSel, subtitleSet: p.subSet}
	streams := s.issuedStreams(t.Key, ik, m.ID, info, sel, u)
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

	// Only the first video elementary stream is played. A separate Dolby
	// Vision enhancement stream remains probe metadata, not a second player track.
	videoSeen := false
	playableStreams := streams[:0]
	for _, st := range streams {
		if st.StreamType == 1 {
			if videoSeen {
				continue
			}
			videoSeen = true
		}
		playableStreams = append(playableStreams, st)
	}
	streams = playableStreams
	// Android builds its picker from this response even while some source
	// facts remain estimated. Preserve every known track across quality changes.
	selectedOnly := !p.listAllTracks
	// Local files already publish their complete tracks before Play. On-demand
	// versions may discover audio and subtitle tracks only now; publish the
	// whole probed audio menu and subtitle menu so non-Android clients (such as
	// Apple TV) see the real tracks and selection rather than keeping a
	// pre-probe placeholder.
	allOnDemand := onDemandSession(ps)
	selectedAudioOnly := selectedOnly && !(allOnDemand && !info.Estimated)
	selectedSubtitleOnly := selectedOnly && !allOnDemand
	if dec.Kind == frontend.DecisionDirectPlay {
		return directPlayMedia(m, streams, selectedAudioOnly, selectedSubtitleOnly, p.subtitles)
	}
	applyTranscodeMedia(&m, dec, p.protocol, selectedAudioStream(info, dec), info.Video())
	var kept []model.Stream
	for _, st := range streams {
		binding, rerr := s.resolveIssuedStream(context.Background(), u, p.caps.ClientID, st.ID)
		if rerr != nil && !streamMiss(rerr) {
			continue
		}
		if shaped, keep := applyStreamDecision(st, binding.Info, dec, selectedAudioOnly, selectedSubtitleOnly, p.listAllTracks, p.subtitles, p.softText); keep {
			if plexAndroidClient(&Request{Product: p.caps.Product, Device: p.caps.Device}) && shaped.Decision == "burn" && frontend.IsImageSubtitle(binding.Info.Codec) && p.protocol == frontend.ProtocolHLS {
				shaped.Location = "segments-av"
				shaped.Extra = append(shaped.Extra, model.A("burn", "1"))
			}
			kept = append(kept, shaped)
		}
	}
	part.Stream = kept
	if !dec.VideoCopy && dec.BitrateKbps > 0 {
		audioBitrate := 0
		for _, st := range kept {
			if st.StreamType == 2 && st.Decision != "" && st.Bitrate > 0 {
				audioBitrate = st.Bitrate
				break
			}
		}
		if audioBitrate > 0 && dec.BitrateKbps > audioBitrate {
			for i := range kept {
				if kept[i].StreamType == 1 && kept[i].Decision == "transcode" {
					kept[i].Bitrate = dec.BitrateKbps - audioBitrate
				}
			}
		}
	}
	if p.protocol == frontend.ProtocolDASH {
		dashDecisionMedia(&m)
	}
	if p.protocol == frontend.ProtocolHTTP {
		for i := range kept {
			if kept[i].Location == "segments-av" || kept[i].Location == "segments-video" {
				kept[i].Location = "direct"
			}
		}
	}
	return m
}

// directPlayMedia preserves stream ordering. For Android-family clients
// every audio stays in the list and selected marks the one that will play.
// On-demand playback lists all subtitle tracks: an unresolved version has no
// embedded tracks before Play, so this response populates its track picker.
// Other clients retain the recorded PMS shape for audio (selected only).
//
// The selected subtitle stays selected whatever subtitles= says, and its
// row names the delivery as PMS 1.42 does for a direct play: subtitles=none
// is decision="ignore" with no location (the player draws the track from
// the file itself; Plex for Android and Android TV send it), sidecar is
// location="sidecar-subs" (Plex Web, which then reads the cues from
// /subtitles/:/transcode/universal/start). An image subtitle only reaches
// direct play when the client can draw it; otherwise Decide burns it and
// this function is not used.
func directPlayMedia(m model.Media, streams []model.Stream, selectedOnly, selectedSubtitleOnly bool, subtitles string) model.Media {
	part := &m.Part[0]
	part.Decision = "directplay"
	var kept []model.Stream
	for _, st := range streams {
		if (selectedOnly && st.StreamType == 2 || selectedSubtitleOnly && st.StreamType == 3) && (st.Selected == nil || !*st.Selected) {
			continue
		}
		st.Location = "direct"
		if st.StreamType == 3 && st.Selected != nil && *st.Selected {
			switch subtitles {
			case "none":
				st.Decision, st.Location = "ignore", ""
			case "sidecar":
				st.Location = "sidecar-subs"
			}
		}
		kept = append(kept, st)
	}
	part.Stream = kept
	return m
}

func textSidecarCodec(codec string) bool {
	switch strings.ToLower(codec) {
	case "srt", "subrip", "ass", "ssa", "vtt", "webvtt", "mov_text", "text", "microdvd", "subviewer", "sami", "ttml", "stl":
		return true
	default:
		return false
	}
}

// applyTranscodeMedia describes the output container and media dimensions.
func applyTranscodeMedia(m *model.Media, dec frontend.Decision, protocol string, srcAudio, srcVideo *frontend.StreamInfo) {
	part := &m.Part[0]
	// Describe the actual HLS segment container so native MDE does not see
	// HEVC paired with the MPEG-TS container it cannot use.
	m.SetTranscodeOrder()
	part.SetTranscodeOrder()
	m.AspectRatio = 0
	m.Protocol = protocol
	m.Container = "mpegts"
	if dec.Container == frontend.ContainerFMP4 {
		m.Container = "mp4"
	} else if dec.Container == frontend.ContainerMatroska {
		m.Container = "mkv"
	}
	part.Container = m.Container
	part.Protocol = protocol
	if protocol == frontend.ProtocolHTTP {
		// Plex Desktop's Matroska stream carries container=mkv and no protocol
		// attribute; audio-only HTTP transcodes carry their target audio
		// container (mp3, mp4/m4a, aac, ogg, flac, wav) instead.
		m.Protocol = ""
		part.Protocol = ""
		if srcVideo != nil {
			m.Container = "mkv"
			part.Container = "mkv"
		} else if c := plexContainer(dec.Container); c != "" {
			m.Container = c
			part.Container = c
		} else {
			m.Container = "mp4"
			part.Container = "mp4"
		}
	}
	part.Decision = "transcode"
	part.Key = ""
	part.File = ""
	part.Size = 0
	m.OptimizedForStreaming, m.Has64bitOffsets = nil, nil
	part.OptimizedForStreaming, part.Has64bitOffsets = nil, nil
	srcH := m.Height
	if r := m.VideoResolution; r != "" && r != "4k" && r != "sd" {
		m.VideoResolution = r + "p"
	}
	if !dec.VideoCopy {
		if dec.VideoCodec != "" {
			m.VideoCodec = plexCodec(dec.VideoCodec)
		}
		m.Width, m.Height = transcodeOutputSize(srcVideo, dec)
		m.VideoResolution = transcodeVideoResolution(m.Width, m.Height, srcH, dec)
		if dec.BitrateKbps > 0 {
			m.Bitrate = dec.BitrateKbps
		}
	}
	if !dec.AudioCopy {
		if dec.AudioCodec != "" {
			m.AudioCodec = plexCodec(dec.AudioCodec)
		}
		srcCh := m.AudioChannels
		if srcAudio != nil && srcAudio.Channels > 0 {
			srcCh = srcAudio.Channels
		}
		if ch := transcodeAudioChannels(dec, srcCh); ch > 0 {
			m.AudioChannels = ch
		}
		if srcVideo == nil && dec.BitrateKbps > 0 {
			m.Bitrate = dec.BitrateKbps
		}
	}
	part.Bitrate, part.Width, part.Height = m.Bitrate, m.Width, m.Height
}

// applyStreamDecision shapes one already-resolved stream without registry
// I/O. Unselected audio is kept when selectedOnly is false (Android-family
// decisions); subtitles are kept when selectedSubtitleOnly is false (also
// on-demand versions). These rows keep their source index with no decision or
// location, as in library metadata. Selected tracks describe their delivery.
// The Android subtitle row retains an index because Plex for Android turns a
// missing index into -1 and disables the text track (#191).
//
// The selected subtitle row follows PMS 1.42 (recorded PMS 1.42.2 capture):
// subtitles=none is decision="ignore" with nothing delivered (except a
// text track a Matroska run carries, which keeps the copy row); a burned text
// track is decision="burn" burn="1" location="segments-av"; Plex Web's text
// track is converted to ASS (codec/container/format "ass",
// decision="transcode", location="segments-subs") and read from
// /video/:/transcode/universal/subtitles; a text track copied into
// Matroska segments keeps its format.
func applyStreamDecision(st model.Stream, src frontend.StreamInfo, dec frontend.Decision, selectedAudioOnly, selectedSubtitleOnly, keepSubtitleIndex bool, subtitles string, softText bool) (model.Stream, bool) {
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
			st.Width, st.Height = transcodeOutputSize(&src, dec)
			// The encode is square-pixel; the source's pixel aspect
			// must not be reported on it.
			st.PixelAspectRatio, st.Anamorphic = "", nil
			if dec.BitrateKbps > 0 {
				st.Bitrate = dec.BitrateKbps
			}
			st.Profile, st.ChromaSubsampling, st.ScanType = "", "", ""
			st.CodedWidth, st.CodedHeight = 0, 0
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
			if selectedAudioOnly {
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
			ch := transcodeAudioChannels(dec, src.Channels)
			if ch > 0 {
				st.Channels = ch
			}
			if br := transcodeAudioBitrate(dec, ch); br > 0 {
				st.Bitrate = br
			}
			// Source-only details go, as in PMS's transcoded audio.
			st.Profile, st.AudioChannelLayout, st.SamplingRate, st.Title = "", "", 0, ""
		}
		st.Location = "segments-av"
	case 3:
		if st.Selected == nil || !*st.Selected {
			if selectedSubtitleOnly {
				return st, false
			}
			st.Index = idx
			return st, true
		}
		text := textSidecarCodec(st.Codec)
		// Matroska clients read native text and bitmap tracks from the
		// segments even when they request subtitles=none.
		inSegments := (text || (!src.External && frontend.IsImageSubtitle(src.Codec))) && dec.Container == frontend.ContainerMatroska && !dec.BurnSubtitles && dec.SubtitleStream >= 0 && dec.SubtitleStream == src.Index
		switch {
		case subtitles == "none" && !inSegments && !dec.BurnSubtitles:
			// subtitles=none means "the player draws the track itself"
			// and nothing delivers it. A track the encoder is burning
			// into the picture is the exception: Plex for Android and
			// Android TV send subtitles=none with the same request that
			// converts an image subtitle (PGS), and PMS still reports
			// that track decision="burn" so the player knows the cues
			// are in the video (#191). Swallowing it here left the
			// player showing no subtitle at all.
			st.Decision, st.Location = "ignore", ""
			return st, true
		case dec.BurnSubtitles && text:
			st.Decision = "burn"
			st.Location = "segments-av"
			st.Format = ""
			st.Extra = append(st.Extra, model.A("burn", "1"))
			return st, true
		case dec.BurnSubtitles:
			st.Decision = "burn"
			st.Location = "segments-video"
		case softText && text && dec.Container != frontend.ContainerMatroska:
			st.Codec, st.Format = "ass", "ass"
			st.Decision = "transcode"
			st.Location = "segments-subs"
			st.Extra = append(st.Extra, model.A("container", "ass"))
			return st, true
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
		// Plex tvOS reads the selected track from the Matroska segments and does
		// not request a sidecar. The transcoder muxes that track on the
		// same clock as the video; a sidecar key would describe a file
		// this player never opens.
		if st.Location == "sidecar" && inSegments {
			st.Decision = "copy"
			st.Location = "segments-av"
			st.Key = ""
			st.Format = st.Codec
			// Plex for Android copies this selected row into the player on
			// the way into playback. The engine turns a missing index into
			// -1, and the track selector treats -1 as Disabled, so the
			// subtitle muxed into the segment is never drawn even though
			// the row stays selected in the panel (#191). Measured on
			// 2026.22.0: "Selecting subtitle stream with index -1" while
			// the panel showed the embedded SRT. A direct play matches
			// this source index to the extractor id. On a transcode the
			// app replaces any present index with 2, which is the output
			// index of this mux: one video, one audio, then this subtitle.
			// Other clients keep the index off the selected row.
			if keepSubtitleIndex {
				st.Index = idx
			}
		}
	}
	return st, true
}

// handleStartHLS is /video/:/transcode/universal/start.m3u8.
func (s *Server) handleStartHLS(w http.ResponseWriter, r *http.Request) {
	if id, ok := liveManifestRefetchFrom(r.Context()); ok {
		// A native player refetching the Live TV playlist it was just handed
		// (see authenticateLiveManifestRefetch).
		s.serveLiveManifestRefetch(w, r, id)
		return
	}
	if s.refuseCapability(w, r) {
		return
	}
	if s.handleLiveTVPlayback(w, r) {
		return
	}
	if s.handleExtraPlayback(w, r) {
		return
	}
	if !s.admitStart(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	if s.transcoder() == nil || s.playback() == nil {
		writePlaybackStatus(w, http.StatusServiceUnavailable)
		return
	}
	p, err := s.parsePlayRequest(r)
	if err != nil {
		writePlaybackStatus(w, http.StatusBadRequest)
		return
	}
	s.logPlaybackStart(r, p)
	if !s.decisionArmed(pr, p.key) {
		// admitStart decided; a stop since then ended this delivery.
		writePlaybackStatus(w, http.StatusBadRequest)
		return
	}
	unlockEncoder, lockErr := s.lockEncoder(ctx, p.sessionID)
	if lockErr != nil {
		return
	}
	defer unlockEncoder()

	if s.playbackOwners.Stale(pr.User.ID, pr.ClientID, p.key, pr.SessionID) {
		writePlaybackStatus(w, http.StatusGone)
		return
	}

	if !s.transcodes.claim(p.sessionID, pr.User.ID, pr.ClientID, s.now()) {
		writePlaybackStatus(w, http.StatusForbidden)
		return
	}
	// A slow decision leaves acquisition running. Join that shared flight
	// below; only a completed source failure should short-circuit start.
	if err, failed := s.transcodes.takeFailure(p.sessionID, pr.User.ID, p.key, s.now()); failed && !errors.Is(err, frontend.ErrPrepareSlow) && !errors.Is(err, frontend.ErrPreparing) && !errors.Is(err, frontend.ErrJobsBusy) {
		s.log.Info("start playback failed", "key", p.key, "reason", playbackMessage(err), "err", err, "decision", "remembered")
		s.writeStartErr(w, r, pr, err)
		return
	}
	priorEntry, _ := s.sessionEntryForStart(pr, p.sessionID, p.key)
	startPR := mergeSessionProfile(pr, priorEntry)
	entry, ok := s.transcodes.get(p.sessionID)
	var ps *frontend.PlaySession
	if ok && entry.key == p.key && entry.userID == pr.User.ID {
		ps, ok = s.playback().Session(entry.playID)
		if ok && ps != nil {
			if entry.info.Estimated && !ps.Info.Estimated {
				ok = false
			} else {
				ps = sessionWithInfo(ps, entry.info)
			}
		}
	}
	if !ok || ps == nil || !p.sameSettings(entryExpectedStart(entry)) {
		// A client can change quality or tracks in start itself, without a
		// preceding decision request. Reapply the normal policy and decision
		// path before deciding whether a running encoder can be reused.
		if _, ps, _, err = s.prepare(ctx, startPR, &p); err != nil {
			if ctx.Err() != nil {
				return // the player dropped this request; its preparation carries on
			}
			s.log.Info("start playback failed", "key", p.key, "reason", playbackMessage(err), "err", err)
			s.writeStartErr(w, r, pr, err)
			return
		}
		entry, _ = s.transcodes.get(p.sessionID)
		startPR = mergeSessionProfile(pr, entry)
	} else if dec, err := s.publishBrokerPlayback(ctx, startPR, replayPlay(p, entry), ps); err != nil {
		s.writeStartErr(w, r, pr, err)
		return
	} else {
		entry.decision = dec
		startPR = mergeSessionProfile(pr, entry)
	}
	s.playbackOwners.Bind(pr.User.ID, pr.ClientID, p.key, pr.SessionID, ps.ID, true, s.now())
	preserveStartIdentity(entry, pr, priorEntry)
	s.transcodes.put(entry)

	dec := frontend.HLSDecision(entry.decision, p.opts.DirectPlay, func() frontend.Decision {
		opts := entry.opts
		opts.DirectPlay = false
		if androidImageSubtitle(startPR, ps.Info, opts) {
			opts.BurnSubtitles = true
		}
		caps := entry.caps
		if s.cfg().Transcode.VideoTranscodingOff() {
			caps.NoVideoTranscode = true
		}
		return s.transcoder().Decide(ps.Info, caps, opts)
	}, func(dec frontend.Decision) frontend.Decision {
		// A client may request HLS after accepting an MDE direct-play
		// decision (notably stable HTTP/debrid files). HLSDecision's legacy
		// fallback is MPEG-TS, which cannot carry the selected text track.
		// Use the negotiated Matroska/Android target even on this late handoff.
		caps := entry.caps
		if s.cfg().Transcode.VideoTranscodingOff() {
			caps.NoVideoTranscode = true
		}
		if plexAndroidClient(startPR) || slices.Contains(caps.HLSContainers, frontend.ContainerMatroska) {
			dec.Container = frontend.HLSContainer(caps, dec.VideoCodec, dec.AudioCodec)
			if androidImageSubtitle(startPR, ps.Info, entry.opts) {
				opts := entry.opts
				opts.DirectPlay, opts.BurnSubtitles = false, true
				return s.transcoder().Decide(ps.Info, caps, opts)
			}
		}
		videoOK := !(ps.Info.Video() != nil && caps.EffectiveAlwaysEncodeVideo()) && (dec.VideoCodec == "" || frontend.SegmentedVideoCopyOK(caps, dec.VideoCodec))
		audioOK := dec.AudioCodec == "" || frontend.SegmentedAudioCopyOK(caps, dec.AudioCodec)
		if !videoOK || !audioOK {
			opts := entry.opts
			opts.DirectPlay = false
			opts.DirectStream = videoOK
			return s.transcoder().Decide(ps.Info, caps, opts)
		}
		return dec
	})
	if existing, ok := s.plexHLSSession(p.sessionID); ok && existing != nil {
		if entry.started && !entry.requested.restartEncoder && entry.offset == p.offset {
			dec = liveSessionDecision(existing, dec)
			s.rebindSessionTranscode(entry)
			s.notePlexEncoder(p.sessionID, frontend.StreamHLS, dec, p.offset)
			unlockEncoder()
			unlockOwner()
			s.writeMasterPlaylist(w, r, p.sessionID, existing, ps.Info, dec)
			return
		}
		s.stopPlexHLS(p.sessionID)
	}
	var resumeHint time.Duration
	if dec.VideoCopy {
		resumeHint = s.resumeStartHint(ctx, pr, p, entry, priorEntry)
	}
	hs, err := s.startPlexHLS(ctx, pr.User, ps.ID, p.sessionID, frontend.HLSRequest{
		SoftSubtitleStream:  sessionSoftSubtitle(entry, startPR, dec),
		TextSubtitleStreams: s.plexTextTracks(startPR, ps.Info, dec),
		SessionID:           p.sessionID,
		ReleaseID:           ps.SourceReleaseID(),
		InputURL:            s.playback().InputURL(ps.ID),
		NoVideoTranscode:    entry.caps.NoVideoTranscode,
		Info:                ps.Info,
		Decision:            dec,
		StartOffset:         ps.SourcePosition(p.offset),
		ResumeHint:          resumeHint,
	})
	if err != nil {
		s.writeStartErr(w, r, pr, err)
		return
	}
	dec = liveSessionDecision(hs, dec)
	s.bindTranscodeStarted(entry, dec, p.offset)
	s.notePlexEncoder(p.sessionID, frontend.StreamHLS, dec, p.offset)
	unlockEncoder()
	unlockOwner()
	s.writeMasterPlaylist(w, r, p.sessionID, hs, ps.Info, dec)
}

// singleRungHLSClient reports smart-TV HTML apps (Plex for LG / webOS and
// Plex for VIDAA) whose native HLS demuxer wedges in a seek loop if ABR
// switches across shared-encoder TS resolutions mid-stream (#408). Explicit
// Plex quality-menu changes still renegotiate start.m3u8.
func singleRungHLSClient(r *http.Request, entry *transcodeEntry) bool {
	product, platform := "", ""
	if pr := FromContext(r.Context()); pr != nil {
		product, platform = pr.Product, pr.Platform
	}
	if entry != nil {
		if product == "" {
			product = entry.caps.Product
		}
		if platform == "" {
			platform = entry.caps.Platform
		}
	}
	switch strings.ToLower(strings.TrimSpace(product)) {
	case "plex for lg", "plex for vidaa":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "webos", "vidaa":
		return true
	}
	return false
}

func (s *Server) writeMasterPlaylist(w http.ResponseWriter, r *http.Request, sessionID string, hs frontend.HLSSession, info frontend.MediaInfo, dec frontend.Decision) {
	if err := s.saveDistributedBinding(r.Context(), sessionID); err != nil {
		http.Error(w, "playback unavailable", 503)
		return
	}
	entry, _ := s.transcodes.get(sessionID)
	subtitleTags, subtitleGroup := s.textRenditionTags(r, sessionID, hs, entry, info, dec)
	// A ladder-capable session advertises every rung; the client's native
	// ABR (AVPlayer, hls.js) steps between them and the session starts a
	// rung's encoder lazily on its first segment request. The primary
	// rung keeps the historic "base" URI.
	baseBandwidth := 0
	if ls, ok := hs.(frontend.HLSLadderSession); ok {
		rungs := ls.Rungs()
		if singleRungHLSClient(r, entry) && len(rungs) > 1 {
			rungs = rungs[:1]
		}
		if len(rungs) == 1 {
			baseBandwidth = rungs[0].Bandwidth
		}
		if len(rungs) > 1 {
			var b strings.Builder
			b.WriteString("#EXTM3U\n" + subtitleTags)
			for _, rg := range rungs {
				line := fmt.Sprintf("#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=%d", rg.Bandwidth)
				if rg.AverageBandwidth > 0 {
					line += fmt.Sprintf(",AVERAGE-BANDWIDTH=%d", rg.AverageBandwidth)
				}
				if rg.Width > 0 && rg.Height > 0 {
					line += fmt.Sprintf(",RESOLUTION=%dx%d", rg.Width, rg.Height)
				}
				// Not reachable for Plex Android Matroska sessions: Matroska HLS
				// is cut to a single rung before the session exists
				// (transcode/manager.go, distributed_support.go), so those
				// masters go through HLSVariantAttributes below, which omits
				// CODECS on purpose. If Matroska ever gets a real ladder, this
				// line must follow the same rule (see hls_attributes.go).
				if rg.Codecs != "" {
					line += fmt.Sprintf(",CODECS=%q", rg.Codecs)
				}
				if rg.FrameRate > 0 {
					line += fmt.Sprintf(",FRAME-RATE=%.3f", rg.FrameRate)
				}
				uri := s.mediaURL("session/"+sessionID+"/"+rg.ID+"/index.m3u8", sessionID, entry.streamToken)
				b.WriteString(line + subtitleGroup + "\n" + uri + "\n")
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
	if dec.VideoCopy {
		// BANDWIDTH is the peak segment rate; a copy's segments swing
		// above the container average.
		var segDur time.Duration
		if sd, ok := hs.(frontend.SegmentDurationSession); ok {
			segDur = sd.SegmentDuration()
		}
		bw = frontend.HLSCopyPeakKbps(info, segDur)
	} else if dec.BitrateKbps > 0 {
		bw = dec.BitrateKbps
	}
	// A single-rung session still owns measured copy peaks and ramp rate
	// controls. Keep the legacy attribute layout but use its actual budget.
	if baseBandwidth > 0 {
		bw = (baseBandwidth + 999) / 1000
	}
	if bw <= 0 {
		bw = 8000
	}
	line := fmt.Sprintf("#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=%d", bw*1000)
	if v := info.Video(); v != nil && v.Width > 0 && v.Height > 0 {
		wd, ht := transcodeOutputSize(v, dec)
		line += fmt.Sprintf(",RESOLUTION=%dx%d", wd, ht)
		if v.FrameRate > 0 {
			line += fmt.Sprintf(",FRAME-RATE=%.3f", v.FrameRate)
		}
	}
	// Plex for Android's Matroska transport gets no CODECS or
	// SUPPLEMENTAL-CODECS here, deliberately: with them the app drops the muxed
	// subtitle track (listed, never drawn). The reason is in
	// transcode.HLSVariantAttributes.
	line += frontend.HLSVariantAttributes(info, dec)
	body := "#EXTM3U\n" + subtitleTags + line + subtitleGroup + "\n" + s.mediaURL("session/"+sessionID+"/base/index.m3u8", sessionID, entry.streamToken) + "\n"
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
	if s.transcoder() == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	id, ok := routeSessionID(r)
	if !ok {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if s.forwardPlexMedia(w, r, id) {
		return
	}
	hs, ok := s.plexHLSSession(id)
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
	if dir, _, found := strings.Cut(rest, "/"); found && strings.HasPrefix(dir, "text-") {
		index, err := strconv.Atoi(strings.TrimPrefix(dir, "text-"))
		source, supported := hs.(playbackTextSubtitles)
		if err != nil || !supported || !source.HasTextSubtitle(index) {
			http.NotFound(w, r)
			return
		}
		if name == "index.m3u8" {
			s.serveStreamManifest(w, r, id, func(w http.ResponseWriter, r *http.Request) { source.ServeTextSubtitlePlaylist(w, r, index) })
		} else if n, ok := segmentNumber(name); ok && strings.HasSuffix(name, ".vtt") {
			source.ServeTextSubtitleSegment(w, r, index, n)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	// ABR ladder: session/{id}/{rung}/index.m3u8 and .../{rung}/{seg}.ts.
	// The primary rung is "base"; sessions without a ladder fall through
	// to the historic single-rung handling.
	if ls, isLadder := hs.(frontend.HLSLadderSession); isLadder {
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
				cw := s.newCountWriter(w, r)
				ls.ServeRungSegment(cw, r, rungID, n)
				cw.finish()
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
	cw := s.newCountWriter(w, r)
	hs.ServeSegment(cw, r, n)
	cw.finish()
}

// segmentNumber extracts the segment index from names like "00012.ts",
// "12.ts", "segment-12.m4s".
func segmentNumber(name string) (int, bool) {
	if strings.HasPrefix(name, "g-") {
		return frontend.ParseSegmentName(name)
	}
	if name == frontend.InitSegmentName || name == frontend.MatroskaHeaderName {
		return frontend.InitSegment, true
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

// The control endpoints distinguish an omitted session from an empty one.
func transcodeControlSession(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if !q.Has("session") {
		writeHTTPError(w, http.StatusBadRequest)
		return false
	}
	if q.Get("session") == "" {
		writeHTTPError(w, http.StatusNotFound)
		return false
	}
	return true
}

func (s *Server) handleTranscodePing(w http.ResponseWriter, r *http.Request) {
	if s.refuseCapability(w, r) {
		return
	}
	if !transcodeControlSession(w, r) {
		return
	}
	if id := sanitizeSessionID(r.URL.Query().Get("session")); id != "" {
		if e, ok := s.transcodes.get(id); ok && e.userID == FromContext(r.Context()).User.ID {
			s.transcodes.touch(id, s.now())
			if s.transcoder() != nil {
				if hs, ok := s.plexHLSSession(id); ok {
					if t, ok := hs.(frontend.TouchableSession); ok {
						t.Touch()
					}
				}
			}
		}
	}
	if e, ok := s.transcodes.get(sanitizeSessionID(r.URL.Query().Get("session"))); !ok || e.userID != FromContext(r.Context()).User.ID {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleTranscodeStop(w http.ResponseWriter, r *http.Request) {
	if s.refuseCapability(w, r) {
		return
	}
	if !transcodeControlSession(w, r) {
		return
	}
	id := sanitizeSessionID(r.URL.Query().Get("session"))
	pr := FromContext(r.Context())
	if e, ok := s.transcodes.get(id); !ok || e.userID != pr.User.ID {
		if pr.User != nil && s.transcodes.wasStoppedBy(id, pr.User.ID, s.now()) {
			w.WriteHeader(http.StatusOK)
			return
		}
		if s.svc().liveTV != nil && pr.User != nil && id != "" {
			if liveID, le, ok := s.liveManifests.lookup(id); ok && le.userID == pr.User.ID && (s.liveTVEnabled() || s.isCatchUpSession(pr.User, liveID)) {
				s.liveManifests.dropByID(liveID)
				_ = s.svc().liveTV.Stop(r.Context(), liveID, pr.User)
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		writeHTTPError(w, http.StatusNotFound)
		return
	}
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
		// Preserve the failure and the last client playhead even if stop
		// removes the encoder before a terminal timeline is delivered.
		if s.transcoder() != nil {
			if session, ok := s.plexHLSSession(id); ok {
				if failed, ok := session.(frontend.ProgressiveFailureSession); ok && failed.ProgressiveFailed() {
					s.rememberProgressiveStop(r.Context(), e)
				}
			}
		}
		shared := false
		if s.transcoder() != nil && e.playID != "" {
			for _, other := range s.transcodes.byPlayID(e.playID) {
				if other != id {
					if _, active := s.plexHLSSession(other); active {
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
		s.trackRestarts.stopped(playIdentityKey{userID: e.userID, clientID: e.clientID, key: e.key}, s.liveRelease(e.userID, e.clientID, e.key), s.now())
		// Another encoder, or a replacement decision, still owns this source.
		// Broker Stop would release it. The last encoder stops the broker
		// session that owns the source.
		if !superseded && !shared {
			s.endVersionFlow(r.Context(), playIdentityKey{userID: e.userID, clientID: e.clientID, key: e.key})
			if lookup := s.brokerRuntime(); lookup != nil && pr.User != nil {
				sid, lookupErr := lookup.PlexSessionForPlayback(r.Context(), s.plexAccess(pr), e.key, pr.SessionID, id)
				if lookupErr == nil && sid != "" {
					playID, known := lookup.PlexSessionSource(sid)
					if sid == id || (known && playID == e.playID) {
						if b := s.broker(); b != nil {
							_ = b.Stop(r.Context(), s.plexAccess(pr), sid)
						}
					}
				}
			}
		}
		if s.transcoder() != nil {
			s.stopPlexHLS(id)
		}
		s.dropTranscode(id)
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
		if te, ok := s.activeTranscodeForSession(e); ok {
			mc.TranscodeSess = append(mc.TranscodeSess, s.servedTranscodeSessionModel(te))
		}
	}
	mc.Size = len(mc.TranscodeSess)
	s.write(w, r, mc)
}

// handleTranscodeSessionStatus is /transcode/sessions/{session}, the key every
// TranscodeSession carries. PMS clients poll it for one session's state;
// a desktop app's embedded server does so while it pulls a download
// stream from this server. Viewers see only their own sessions.
func (s *Server) handleTranscodeSessionStatus(w http.ResponseWriter, r *http.Request) {
	u := FromContext(r.Context()).User
	te, ok := s.transcodes.get(chi.URLParam(r, "session"))
	if !ok || u == nil || (!u.IsOwner && te.userID != u.ID) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	s.write(w, r, &model.MediaContainer{Size: 1, TranscodeSess: []model.TranscodeSess{s.servedTranscodeSessionModel(te)}})
}

// preserveStartIdentity keeps the decision's playback and client identity when
// a native player omits X-Plex-Session-Identifier or X-Plex-Client-Identifier
// on start.m3u8/start.mpd/start.
func preserveStartIdentity(entry *transcodeEntry, pr *Request, prior *transcodeEntry) {
	if entry == nil {
		return
	}
	if pr != nil && pr.SessionID != "" {
		entry.playbackID = pr.SessionID
	} else if entry.playbackID == "" && prior != nil {
		entry.playbackID = prior.playbackID
	}
	if entry.clientID == "" {
		if pr != nil && pr.ClientID != "" {
			entry.clientID = pr.ClientID
		} else if prior != nil {
			entry.clientID = prior.clientID
		}
	}
}

func liveSessionDecision(hs frontend.HLSSession, fallback frontend.Decision) frontend.Decision {
	if hs == nil {
		return fallback
	}
	if src, ok := hs.(frontend.DecisionSession); ok {
		if actual := src.Decision(); actual.Kind != "" {
			return mergeLiveDecision(fallback, actual)
		}
	}
	return fallback
}

func (s *Server) transcodeEntryLive(te *transcodeEntry) bool {
	if te == nil || !te.started {
		return false
	}
	if s.transcoder() != nil || s.broker() != nil {
		_, ok := s.plexHLSSession(te.id)
		return ok
	}
	return true
}

func (s *Server) activeTranscodeForSession(e playEntry) (*transcodeEntry, bool) {
	if e.node != "" || e.user.ID == 0 || e.key == 0 {
		return nil, false
	}
	return s.activeTranscodeForClient(e.user.ID, e.client.ClientID, e.client.SessionID, e.key, e.transcodeID)
}

func (s *Server) activeTranscodeForClient(userID int64, clientID, playbackID string, key frontend.RatingKey, preferredID string) (*transcodeEntry, bool) {
	if userID == 0 || key == 0 {
		return nil, false
	}
	var candidates []*transcodeEntry
	seen := map[string]bool{}
	consider := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		te, ok := s.transcodes.get(id)
		if !ok || te.userID != userID || te.key != key || !s.transcodeEntryLive(te) {
			return
		}
		if clientID != "" && te.clientID != "" && te.clientID != clientID {
			return
		}
		if te.playbackID != "" {
			if playbackID != "" && te.playbackID != playbackID {
				return
			}
			if s.playbackOwners.Stale(userID, clientID, key, te.playbackID) {
				return
			}
		}
		candidates = append(candidates, te)
	}
	consider(preferredID)
	if clientID != "" {
		for _, id := range s.transcodes.forClient(userID, clientID, key) {
			consider(id)
		}
	}
	if len(candidates) == 0 && clientID != "" {
		var clientPlayID string
		if pb := s.playback(); pb != nil {
			if ps, ok := pb.SessionFor(userID, clientID, key); ok && ps != nil {
				clientPlayID = ps.ID
			}
		}
		for _, id := range s.transcodes.forClient(userID, "", key) {
			if te, ok := s.transcodes.get(id); ok && clientPlayID != "" && te.playID != "" && te.playID != clientPlayID {
				continue
			}
			consider(id)
		}
	}
	if len(candidates) == 0 {
		return nil, false
	}
	best := candidates[0]
	for _, te := range candidates[1:] {
		if betterActiveTranscode(te, best, playbackID, preferredID) {
			best = te
		}
	}
	return best, true
}

func betterActiveTranscode(a, b *transcodeEntry, playbackID, preferredID string) bool {
	if playbackID != "" {
		aExact, bExact := a.playbackID == playbackID, b.playbackID == playbackID
		if aExact != bExact {
			return aExact
		}
	}
	if !a.updatedAt.Equal(b.updatedAt) {
		return a.updatedAt.After(b.updatedAt)
	}
	if preferredID != "" {
		aPref, bPref := a.id == preferredID, b.id == preferredID
		if aPref != bPref {
			return aPref
		}
	}
	return a.id > b.id
}

type liveTranscodeState struct {
	info              frontend.MediaInfo
	decision          frontend.Decision
	protocol          string
	container         string
	speed             float64
	throttled         bool
	complete          bool
	progressTime      time.Duration
	minOffset         time.Duration
	maxOffset         time.Duration
	hwRequested       bool
	hwDecoding        string
	hwEncoding        string
	hwFullPipeline    bool
	hasHWFullPipeline bool
}

func mergeLiveDecision(base, live frontend.Decision) frontend.Decision {
	if live.Kind == "" {
		return base
	}
	out := live
	if out.Container == "" {
		out.Container = base.Container
	}
	if !out.AudioCopy && out.AudioCodec == "" {
		out.AudioCodec = base.AudioCodec
	}
	if out.AudioStream == 0 && base.AudioStream > 0 && live.Container == "" && live.Reason == "" {
		out.AudioStream = base.AudioStream
	}
	if out.SubtitleStream == 0 && base.SubtitleStream != 0 && live.Container == "" && live.Reason == "" {
		out.SubtitleStream = base.SubtitleStream
	}
	return out
}

func (s *Server) liveTranscode(te *transcodeEntry) liveTranscodeState {
	state := liveTranscodeState{
		info:     te.info,
		decision: te.decision,
		protocol: "hls",
	}
	if te.started {
		switch cmp.Or(te.opts.Protocol, te.requested.protocol) {
		case frontend.ProtocolDASH:
			state.protocol = frontend.ProtocolDASH
		case frontend.ProtocolHTTP:
			state.protocol = frontend.ProtocolHTTP
		default:
			state.protocol = frontend.ProtocolHLS
		}
		state.container = te.decision.Container
	}
	if pb := s.playback(); pb != nil {
		var ps *frontend.PlaySession
		if live, ok := pb.Session(te.playID); ok && live != nil {
			ps = live
		} else if live, ok := pb.SessionFor(te.userID, te.clientID, te.key); ok && live != nil {
			ps = live
		}
		if ps != nil && len(ps.Info.Streams) > 0 {
			state.info = ps.Info
		}
	}
	hs, ok := s.plexHLSSession(te.id)
	if ok && hs != nil {
		if src, ok := hs.(frontend.DeliveryInfoSession); ok {
			if info := src.Info(); len(info.Streams) > 0 || info.Duration > 0 || info.BitrateKbps > 0 {
				state.info = info
			}
		}
		if src, ok := hs.(frontend.DecisionSession); ok {
			state.decision = mergeLiveDecision(state.decision, src.Decision())
		}
		if src, ok := hs.(frontend.ProtocolSession); ok {
			if p := src.Protocol(); p != "" {
				state.protocol = p
			}
		}
		if src, ok := hs.(frontend.ContainerSession); ok {
			if c := src.Container(); c != "" {
				state.container = c
			}
		}
		var hasStatus bool
		if src, ok := hs.(frontend.EncoderStatusSession); ok {
			if st := src.EncoderStatus(); st != nil {
				hasStatus = true
				state.decision = mergeLiveDecision(state.decision, st.Decision)
				if st.Speed > 0 {
					state.speed = math.Round(st.Speed*10) / 10
				}
				state.throttled = st.Phase == "paused"
				state.complete = st.Phase == "finished"
				state.progressTime = max(st.ReadyUntil, st.OutputTime)
				if st.ReadyUntil > 0 {
					state.minOffset = st.MinOffsetAvailable
					state.maxOffset = st.ReadyUntil
				}
				if !state.decision.VideoCopy {
					switch st.Processor {
					case "gpu":
						state.hwRequested = true
						state.hwEncoding = st.HWEncoder
						if state.hwEncoding == "" && s.transcoder() != nil {
							state.hwEncoding = hwAccelEncoderToken(s.transcoder().HWAccel().Name)
						}
						state.hwDecoding = st.HWDecoder
						if st.Filters == "gpu" || st.Filters == "cpu" {
							state.hwFullPipeline = st.Filters == "gpu"
							state.hasHWFullPipeline = true
						}
						if state.hwDecoding == "" && state.hwFullPipeline && state.hwEncoding != "" {
							state.hwDecoding = hwDecoderForEncoder(state.hwEncoding)
						}
					case "cpu":
						state.hwRequested = false
					}
				}
			}
		}
		if !hasStatus && !state.decision.VideoCopy {
			if hwSrc, ok := hs.(frontend.HWAcceleratedSession); ok && hwSrc.HWAccelerated() {
				state.hwRequested = true
				if s.transcoder() != nil {
					state.hwEncoding = hwAccelEncoderToken(s.transcoder().HWAccel().Name)
				}
			}
		}
	}
	if state.container == "" {
		state.container = state.decision.Container
	}
	return state
}

// servedTranscodeSessionModel is the TranscodeSession of what is actually
// being served: initial start, quality change, ABR rung switch, failover, or
// adaptive copy-to-encode.
func (s *Server) servedTranscodeSessionModel(te *transcodeEntry) model.TranscodeSess {
	return buildTranscodeSessionModel(te, s.liveTranscode(te))
}

// sessionTranscodeMedia renders the active conversion/remux Media for
// /status/sessions once start.m3u8/start.mpd/start has actually started
// (te.started). Tautulli and Plex Dashboard inspect Part@decision on the
// session's Media to distinguish Direct Play (Part@decision omitted) from
// Direct Stream (Part@decision="transcode" with VideoDecision="copy").
func (s *Server) sessionTranscodeMedia(t *frontend.Title, te *transcodeEntry, viewer *Request) (model.Media, bool) {
	if t == nil || te == nil || !te.started || viewer == nil || viewer.User == nil {
		return model.Media{}, false
	}
	live := s.liveTranscode(te)
	ps := &frontend.PlaySession{Info: live.info, StartedAt: t.UpdatedAt}
	if pb := s.playback(); pb != nil {
		if psLive, ok := pb.Session(te.playID); ok && psLive != nil {
			ps = psLive
		} else if psLive, ok := pb.SessionFor(te.userID, te.clientID, te.key); ok && psLive != nil {
			ps = psLive
		}
	}
	if len(live.info.Streams) > 0 {
		cp := *ps
		cp.Info = live.info
		ps = &cp
	}
	dec := live.decision
	if dec.Kind == "" || dec.Kind == frontend.DecisionDirectPlay {
		if !dec.VideoCopy {
			dec.Kind = frontend.DecisionTranscode
		} else {
			dec.Kind = frontend.DecisionRemux
		}
	}
	p := te.requested
	p.key = t.Key
	p.caps = te.caps
	if p.caps.ClientID == "" {
		p.caps.ClientID = te.clientID
	}
	if live.protocol != "" {
		p.protocol = live.protocol
	} else if p.protocol == "" {
		if te.opts.Protocol != "" {
			p.protocol = te.opts.Protocol
		} else {
			p.protocol = frontend.ProtocolHLS
		}
	}
	ik := s.playIdentityFor(viewer, t.Key)
	if pin := s.playIDs.pinned(ik, true, s.now()); pin.selected != 0 {
		p.mediaID = pin.selected
	} else if p.mediaID == 0 {
		p.mediaID = int64(t.Key)
	}
	p.audioSel = -1
	for i, st := range ps.Info.Streams {
		if st.Type == "audio" && st.Index == dec.AudioStream {
			p.audioSel = i
			break
		}
	}
	if te.subtitle >= 0 && te.subtitle < len(ps.Info.Streams) && ps.Info.Streams[te.subtitle].Type == "subtitle" {
		p.subSel, p.subSet = te.subtitle, true
	} else {
		p.subSel, p.subSet = -1, false
	}
	p.listAllTracks = androidFamilyClient(viewer)
	p.softText = browserClient(viewer)
	if plexAndroidClient(viewer) && dec.BurnSubtitles && p.subtitles == "none" {
		p.subtitles = "burn"
	}
	return s.decisionMedia(t, ps, dec, p, viewer.User), true
}

func transcodeSessionModel(te *transcodeEntry) model.TranscodeSess {
	live := liveTranscodeState{info: te.info, decision: te.decision, protocol: "hls"}
	if te.started {
		switch cmp.Or(te.opts.Protocol, te.requested.protocol) {
		case frontend.ProtocolDASH:
			live.protocol = frontend.ProtocolDASH
		case frontend.ProtocolHTTP:
			live.protocol = frontend.ProtocolHTTP
		default:
			live.protocol = frontend.ProtocolHLS
		}
		live.container = te.decision.Container
	}
	return buildTranscodeSessionModel(te, live)
}

func buildTranscodeSessionModel(te *transcodeEntry, live liveTranscodeState) model.TranscodeSess {
	dec := live.decision
	info := live.info
	ts := model.TranscodeSess{
		Key:        "/transcode/sessions/" + te.id,
		Throttled:  live.throttled,
		Complete:   live.complete,
		Context:    "streaming",
		Protocol:   cmp.Or(live.protocol, "hls"),
		Container:  "mpegts",
		Duration:   ms(info.Duration),
		Progress:   -1,
		Speed:      live.speed,
		TimeStamp:  float64(te.updatedAt.Unix()),
		AudioCodec: plexCodec(dec.AudioCodec),
		VideoCodec: plexCodec(dec.VideoCodec),
	}
	if live.complete {
		ts.Progress = 100
	} else if info.Duration > 0 && live.progressTime > 0 {
		pct := float64(live.progressTime) / float64(info.Duration) * 100
		if pct > 100 {
			pct = 100
		}
		ts.Progress = math.Round(pct*10) / 10
	}
	if live.maxOffset > 0 {
		ts.MinOffsetAvailable = math.Round(live.minOffset.Seconds()*1000) / 1000
		ts.MaxOffsetAvailable = math.Round(live.maxOffset.Seconds()*1000) / 1000
	}
	if ts.Protocol == frontend.ProtocolDASH {
		ts.Container = "mp4"
	} else if te.started || live.container != "" {
		switch live.container {
		case frontend.ContainerMatroska:
			ts.Container = "mkv"
		case frontend.ContainerFMP4, "mp4":
			ts.Container = "mp4"
		case frontend.ContainerMPEGTS:
			ts.Container = "mpegts"
		default:
			if ts.Protocol == frontend.ProtocolHTTP {
				ts.Container = "mkv"
			} else {
				ts.Container = "mpegts"
			}
		}
	}
	v := info.Video()
	if v != nil {
		ts.SourceVideoCodec = plexCodec(v.Codec)
		if v.Width > 0 && v.Height > 0 {
			ts.Width, ts.Height = transcodeOutputSize(v, dec)
		}
	}
	a := selectedAudioStream(info, dec)
	if a != nil {
		ts.SourceAudioCodec = plexCodec(a.Codec)
	}
	ts.VideoDecision, ts.AudioDecision = "copy", "copy"
	if !dec.VideoCopy {
		ts.VideoDecision = "transcode"
		ts.TranscodeHwRequested = live.hwRequested
		if live.hwDecoding != "" {
			ts.TranscodeHwDecoding = live.hwDecoding
			ts.TranscodeHwDecodingTitle = hwAccelTitle(live.hwDecoding)
		}
		if live.hwEncoding != "" {
			ts.TranscodeHwEncoding = live.hwEncoding
			ts.TranscodeHwEncodingTitle = hwAccelTitle(live.hwEncoding)
		}
		if live.hasHWFullPipeline {
			ts.TranscodeHwFullPipeline = model.Ptr(live.hwFullPipeline)
		}
	} else {
		ts.VideoCodec = ts.SourceVideoCodec
	}
	if !dec.AudioCopy {
		ts.AudioDecision = "transcode"
		srcCh := 0
		if a != nil {
			srcCh = a.Channels
		}
		if srcCh > 0 || dec.AudioChannels > 0 {
			ts.AudioChannels = transcodeAudioChannels(dec, srcCh)
		}
	} else {
		ts.AudioCodec = ts.SourceAudioCodec
		if a != nil && a.Channels > 0 {
			ts.AudioChannels = a.Channels
		}
	}
	if dec.BurnSubtitles {
		ts.SubtitleDecision = "burn"
	} else if te.started && te.subtitle >= 0 && te.subtitle < len(info.Streams) && info.Streams[te.subtitle].Type == "subtitle" {
		sub := info.Streams[te.subtitle]
		codec := plexCodec(sub.Codec)
		text := textSidecarCodec(codec)
		inSegments := text && dec.Container == frontend.ContainerMatroska && dec.SubtitleStream >= 0 && dec.SubtitleStream == sub.Index
		softText := browserClient(&Request{Product: te.caps.Product})
		switch {
		case te.requested.subtitles == "none" && !inSegments:
		case softText && text && dec.Container != frontend.ContainerMatroska:
			ts.SubtitleDecision = "transcode"
		default:
			ts.SubtitleDecision = "copy"
		}
	}
	return ts
}

func hwAccelEncoderToken(accel string) string {
	switch strings.ToLower(strings.TrimSpace(accel)) {
	case "videotoolbox":
		return "videotoolbox"
	case "nvenc":
		return "nvenc"
	case "vaapi":
		return "vaapi"
	case "qsv":
		return "qsv"
	default:
		return ""
	}
}

func hwDecoderForEncoder(enc string) string {
	switch enc {
	case "videotoolbox":
		return "videotoolbox"
	case "nvenc":
		return "nvdec"
	case "vaapi":
		return "vaapi"
	case "qsv":
		return "vaapi"
	default:
		return ""
	}
}

func hwAccelTitle(token string) string {
	switch strings.ToLower(token) {
	case "videotoolbox":
		return "Apple VideoToolbox"
	case "nvdec":
		return "NVIDIA NVDEC"
	case "nvenc":
		return "NVIDIA NVENC"
	case "vaapi":
		return "VAAPI"
	case "qsv":
		return "Intel Quick Sync Video"
	case "d3d11va":
		return "Direct3D 11"
	case "dxva2":
		return "DXVA2"
	default:
		return ""
	}
}

func evenDim(n int) int {
	if n%2 != 0 {
		n--
	}
	if n < 2 {
		n = 2
	}
	return n
}

// transcodeOutputSize is the picture size a decision delivers from v: the
// source's coded size for a copy, and the encoder's square-pixel size
// (sized by display aspect, so an anamorphic source reports its display
// dimensions) for an encode.
func transcodeOutputSize(v *frontend.StreamInfo, dec frontend.Decision) (int, int) {
	if v == nil {
		if dec.MaxHeight > 0 {
			return evenDim(dec.MaxHeight * 16 / 9), evenDim(dec.MaxHeight)
		}
		return 0, 0
	}
	if dec.VideoCopy {
		return v.Width, v.Height
	}
	return frontend.TranscodeOutputSize(v, dec)
}

func transcodeVideoResolution(w, h, srcH int, dec frontend.Decision) string {
	res := videoResolution(w, h)
	if !dec.VideoCopy && dec.MaxHeight > 0 && srcH > dec.MaxHeight {
		switch dec.MaxHeight {
		case 480, 576, 720, 1080, 2160:
			if frontend.ResolutionTier(w, h) > dec.MaxHeight {
				res = videoResolution(0, dec.MaxHeight)
			}
		}
	}
	if res != "" && res != "4k" && res != "sd" {
		return res + "p"
	}
	return res
}

func transcodeResolutionLabel(w, h, srcH int, dec frontend.Decision) string {
	res := transcodeVideoResolution(w, h, srcH, dec)
	switch res {
	case "", "sd":
		return "SD"
	case "4k":
		return "4K"
	case "2k":
		return "2K"
	case "2.7k":
		return "2.7K"
	default:
		return res
	}
}

func selectedAudioStream(info frontend.MediaInfo, dec frontend.Decision) *frontend.StreamInfo {
	var first *frontend.StreamInfo
	for i := range info.Streams {
		st := &info.Streams[i]
		if st.Type != "audio" {
			continue
		}
		if first == nil {
			first = st
		}
		if dec.AudioStream >= 0 && st.Index == dec.AudioStream {
			return st
		}
	}
	return first
}

func transcodeAudioChannels(dec frontend.Decision, srcChannels int) int {
	if dec.AudioChannels > 0 {
		return dec.AudioChannels
	}
	ch := 2
	if srcChannels > 0 {
		ch = srcChannels
	}
	switch strings.ToLower(dec.AudioCodec) {
	case "ac3", "eac3":
		if ch > 6 {
			return 6
		}
		return ch
	}
	if ch > 2 {
		return 2
	}
	return ch
}

func transcodeAudioBitrate(dec frontend.Decision, channels int) int {
	if dec.AudioBitrateKbps > 0 {
		return dec.AudioBitrateKbps
	}
	switch strings.ToLower(dec.AudioCodec) {
	case "ac3":
		if channels > 2 {
			return 640
		}
		return 256
	case "eac3":
		if channels > 2 {
			return 768
		}
		return 384
	case "":
		return 0
	default:
		if channels > 2 {
			return 384
		}
		return 192
	}
}

// ---------------------------------------------------------------------------
// Direct play

// handlePart serves /library/parts/{part}/{changestamp}/{file} with Range
// support through Playback.ServeRange. A variant Part id names a specific
// release of the title ("Play Version" direct play).
func (s *Server) handlePart(w http.ResponseWriter, r *http.Request) {
	// Validate before acquisition; padded binary integers are accepted by PMS.
	download := false
	if values, present := r.URL.Query()["download"]; present {
		raw := values[0]
		raw = strings.TrimPrefix(raw, "+")
		raw = strings.TrimLeft(raw, "0")
		if raw != "" && raw != "1" || values[0] == "" || values[0] == "+" {
			writeHTTPError(w, http.StatusBadRequest)
			return
		}
		download = raw == "1"
	}
	if s.handleExtraPart(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	if s.playbackTerminated(pr) {
		writePlaybackStatus(w, http.StatusServiceUnavailable)
		return
	}
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	id, err := strconv.ParseInt(chi.URLParam(r, "part"), 10, 64)
	if err != nil || s.playback() == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	key, releaseID, found := s.partTitle(id)
	if !found {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	title, titleErr := s.metadataTitle(ctx, pr, key)
	if titleErr != nil || !s.pluginAwarePlayable(ctx, pr.User, title) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	if policy, ok := s.policy().(frontend.PlaybackAccessPolicy); ok && !policy.CanPlay(pr.User, pr.ClientCaps(0).Remote, s.now()) {
		writeHTTPError(w, http.StatusForbidden)
		return
	}
	var (
		dlPolicy         frontend.DownloadLimitPolicy
		downloadReserved bool
		downloadServed   bool
	)
	if download {
		// Plex Web's Download button is a plain part read; only the viewer
		// policy (an owner override or plex.tv allowSync=0) refuses it. The
		// download queue being off is a different feature.
		if p, ok := s.policy().(frontend.DownloadAccessPolicy); ok && !p.AllowDownloads(pr.User) {
			writeHTTPError(w, http.StatusForbidden)
			return
		}
		if r.Method != http.MethodHead {
			if p, ok := s.policy().(frontend.DownloadLimitPolicy); ok && pr.User != nil {
				dlPolicy = p
				estCaps := s.clientCapsForKey(pr, key)
				if mh := s.cfg().Downloads.MaxHeight; mh > 0 && (estCaps.MaxHeight == 0 || estCaps.MaxHeight > mh) {
					estCaps.MaxHeight = mh
				}
				if mh := p.DownloadAllowance(pr.User).MaxHeight; mh > 0 && (estCaps.MaxHeight == 0 || estCaps.MaxHeight > mh) {
					estCaps.MaxHeight = mh
				}
				est := s.estimateDownloadBytes(ctx, title, estCaps)
				dec := p.AdmitDownload(ctx, pr.User, key, est)
				if !dec.Allowed {
					if wait := int(time.Until(dec.ResetAt).Seconds()); wait > 0 {
						w.Header().Set("Retry-After", strconv.Itoa(wait))
					}
					http.Error(w, frontend.DownloadLimitMessage(dec), http.StatusTooManyRequests)
					return
				}
				downloadReserved = dec.Reserved
				defer func() {
					if downloadReserved && !downloadServed {
						dlPolicy.ReleaseDownloadReservation(context.Background(), pr.User, key)
					}
				}()
			}
		}
	}
	// Clients may fall back from a failed decision to the original part URL.
	// Do not launch the same failed acquisition again during that attempt.
	if releaseID == "" && pr.ClientID != "" {
		for _, id := range s.transcodes.forClient(pr.User.ID, pr.ClientID, 0) {
			if pr.SessionID != "" && id != sanitizeSessionID(pr.SessionID) {
				continue // a distinct playback attempt must be allowed to retry
			}
			if err, failed := s.transcodes.takeFailure(id, pr.User.ID, key, s.now()); failed && !errors.Is(err, frontend.ErrPrepareSlow) && !errors.Is(err, frontend.ErrPreparing) && !errors.Is(err, frontend.ErrJobsBusy) {
				s.writePartErr(w, r, pr, err)
				return
			}
		}
	}
	source := s.plexSources().Part(pr.User.ID, pr.ClientID, key, releaseID)
	ps, ok := source.Session, source.Matches
	live, anonymousContinuation := source.PreviousID, source.AnonymousContinuation
	if anonymousContinuation {
		if confirm := s.brokerRuntime(); confirm != nil {
			viewer, err := confirm.ConfirmAdmission(ctx, pr.User, pr.ClientCaps(0).Remote)
			if err != nil || viewer == nil || viewer.ID != pr.User.ID || !s.pluginAwarePlayable(ctx, viewer, title) {
				writeHTTPError(w, http.StatusForbidden)
				return
			}
		}
	}
	// A clientless part read is a native player (Samsung AVPlay, Plezy's libmpv)
	// that drops Plex headers and carries only a token on the URL. Before
	// adopting the generic "Native player" fallback, restore omitted capability
	// headers from this viewer's recent pre-play metadata or control request so
	// direct-play source selection uses the real device's capabilities.
	if !ok || ps == nil {
		s.recentClients.apply(r, pr, key, s.now())
	}
	if pr.ClientID == "" && (!ok || ps == nil) {
		pr.adoptNativeDevice(r)
	}
	if !ok || ps == nil {
		t, err := s.metadataTitle(ctx, pr, key)
		if err == nil && !t.Key.IsPluginKey() && !s.titleVisible(pr.User, t) {
			err = frontend.ErrNotFound
		}
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		caps := s.clientCapsForKey(pr, key)
		if download && releaseID == "" {
			if mh := s.cfg().Downloads.MaxHeight; mh > 0 && (caps.MaxHeight == 0 || caps.MaxHeight > mh) {
				caps.MaxHeight = mh
			}
			if dlPolicy != nil && pr.User != nil {
				if mh := dlPolicy.DownloadAllowance(pr.User).MaxHeight; mh > 0 && (caps.MaxHeight == 0 || caps.MaxHeight > mh) {
					caps.MaxHeight = mh
				}
			}
		}
		s.observeClientAdmission(pr, caps)
		// A restart after a track change keeps the release being watched.
		prepareRelease, exact := s.restartRelease(s.playIdentityFor(pr, key), releaseID)
		ps, err = s.prepareForRelease(ctx, pr.User, key, caps, prepareRelease, exact)
		if err != nil {
			s.log.Info("direct play failed", "key", key, "reason", playbackMessage(err), "err", err)
			s.writePartErr(w, r, pr, err)
			return
		}
		// The picked version fell back: the playback kept the session that
		// already plays the automatic choice, so this read continues it.
		ok = live != "" && ps.ID == live
		if !ok {
			s.observePlaybackDecision(ctx, pr, &playRequest{key: key, caps: caps, partReleaseID: releaseID, requestedPart: id}, ps)
		}
	}
	if !anonymousContinuation && r.Method != http.MethodHead && !download {
		// A part read is the playing Media for clients that skip the
		// decision; a decision's admission keeps its selection unless this
		// read prepared another release.
		media := int64(key)
		if releaseID != "" {
			media = id
		}
		release := ""
		if ps.Resolution != nil {
			release = ps.Resolution.Release.ID
		}
		s.playIDs.admit(s.playIdentityFor(pr, key), media, release, !ok, s.now())
		if !ok {
			s.trackRestarts.admitted(s.playIdentityFor(pr, key), releaseID, release, s.now())
		}
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
		applyMusicDirectPlay(&partPlay.caps, pr.ProfileExtra, ps.Info, -1)
		// A part range of a playback that is already on the broker must not
		// open a second session. That open supersedes the live encoder.
		already := false
		if lookup := s.brokerRuntime(); lookup != nil {
			if sid, lookupErr := lookup.PlexSessionForPlayback(ctx, s.plexAccess(pr), key, "", ""); lookupErr == nil && sid != "" {
				if playID, ok := lookup.PlexSessionSource(sid); ok && playID == ps.ID {
					already = true
				}
			}
		}
		// A plugin kind key opens through the shared KindPlayback pipeline; when
		// no broker is mounted (standalone runtime, fixture) it is served
		// directly from the prepared kind session without a broker publish.
		if !already && (s.broker() != nil || !key.IsPluginKey()) {
			if _, err := s.publishBrokerPlayback(ctx, pr, &partPlay, ps); err != nil {
				s.log.Info("direct play broker publish failed", "key", key, "err", err)
				s.writeErr(w, r, err)
				return
			}
		}
	}
	if download {
		if dlPolicy != nil && pr.User != nil && r.Method != http.MethodHead {
			if actual := preparedDownloadBytes(ps.Info, frontend.DecideOptions{}); actual > 0 {
				if rec := dlPolicy.ReconcileDownloadBytes(ctx, pr.User, key, actual); !rec.Allowed {
					if wait := int(time.Until(rec.ResetAt).Seconds()); wait > 0 {
						w.Header().Set("Retry-After", strconv.Itoa(wait))
					}
					http.Error(w, frontend.DownloadLimitMessage(rec), http.StatusTooManyRequests)
					return
				}
			}
		}
		filename := chi.URLParam(r, "file")
		if decoded, err := url.PathUnescape(filename); err == nil {
			filename = decoded
		}
		w.Header().Set("Content-Disposition", contentDisposition(filename))
	}
	unlockOwner()
	// Direct delivery hands the client the provider's URL for a file it has
	// already been allowed to play. A download keeps Ether's file name, and a
	// HEAD stays with the relay so the client's size probe is answered by the
	// session that knows the container.
	if !download && r.Method == http.MethodGet && s.partDirectDelivery(w, r, ps.ID) {
		return
	}
	downloadServed = true
	cw := s.newCountWriter(w, r)
	q := parts.Request(r)
	if download {
		q = q.WithContext(frontend.WithDownloadRange(q.Context()))
	}
	var recoverOriginal func()
	if reader, ok := s.broker().(frontend.OriginalRangeReader); ok && !download {
		if rt := s.brokerRuntime(); rt != nil {
			access := s.plexAccess(pr)
			if id, err := rt.PlexSessionForSource(q.Context(), access, key, ps.ID); err == nil {
				request := reader.OriginalRange(q.Context(), access, id, releaseID, q.URL.Query().Get("sourceRevision"))
				if recover := request.Lost; recover != nil {
					request.Lost = func() { recoverOriginal = recover }
					q = q.WithContext(frontend.WithOriginalRange(q.Context(), request))
				}
			}
		}
	}
	s.plexSources().ServePart(cw, q, ps.ID)
	cw.finish()
	if recoverOriginal != nil {
		recoverOriginal()
	}
}

// Part acquisition failures distinguish an absent provider file from a failed
// upstream transport; the conversion start routes retain their own contract.
func (s *Server) writePartErr(w http.ResponseWriter, r *http.Request, pr *Request, err error) {
	if errors.Is(err, frontend.ErrNotFound) || errors.Is(err, frontend.ErrNotAvailable) || errors.Is(err, frontend.ErrUnauthorized) || errors.Is(err, frontend.ErrStreamLimit) || errors.Is(err, frontend.ErrPrepareSlow) || errors.Is(err, frontend.ErrPreparing) || errors.Is(err, frontend.ErrDownloadQueued) || errors.Is(err, frontend.ErrJobsBusy) || errors.Is(err, frontend.ErrArchiveUnstreamable) || errors.As(err, new(*frontend.IndexersPausedError)) {
		s.writeStartErr(w, r, pr, err)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	if pr != nil && pr.User != nil {
		s.setTermination(trackID(pr.User.ID, pr.ClientID, pr.SessionID), s.playbackMessageFor(pr, err))
	}
	writePlaybackStatus(w, http.StatusBadGateway)
}

// handlePartSelect is PUT or POST /library/parts/{part}?audioStreamID=&subtitleStreamID=.
// The stored subtitle is the title's choice. The part id only finds that
// title: Play Version part ids move between releases as the list reorders.
//
// GET /library/metadata is built for that request from this choice. Nothing
// caches the rendered body, and the response is Cache-Control: no-cache, so
// a re-read sees the new selection. Apple TV checks the menu radio from the
// PUT immediately and leaves the detail chip on its previous label until
// that screen loads again. Plex Media Server's client does the same; the
// chip is not a stale Ether document.
func (s *Server) handlePartSelect(w http.ResponseWriter, r *http.Request) {
	pr := FromContext(r.Context())
	rawID, err := strconv.ParseInt(chi.URLParam(r, "part"), 10, 64)
	if err != nil || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	// Resolve and authorize the part before interpreting either selector.
	key, _, found := s.partTitle(rawID)
	if !found || (!key.IsPluginKey() && s.catalog() == nil) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	title, err := s.metadataTitle(r.Context(), pr, key)
	if err != nil || title == nil || (key.IsPluginKey() && !s.canSeePluginTitle(pr.User, title)) || (!key.IsPluginKey() && !s.titleVisible(pr.User, title)) {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	id := int64(key)
	q := r.URL.Query()
	// These two selection parameters deliberately have different PMS grammars.
	audioID, audioErr := strconv.ParseInt(q.Get("audioStreamID"), 10, 64)
	subID, subErr := strconv.ParseInt(q.Get("subtitleStreamID"), 10, 64)
	if (q.Has("audioStreamID") && audioErr == nil && (audioID == 0 || audioID < -1)) ||
		(q.Has("subtitleStreamID") && subErr == nil && subID < -1) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	audioDefault := q.Has("audioStreamID") && audioErr == nil && audioID == -1
	subDefault := q.Has("subtitleStreamID") && subErr == nil && subID == -1
	ctx := frontend.WithStreamServe(r.Context(), pr.Remote, "")
	var audio *frontend.StreamBinding
	if v := q.Get("audioStreamID"); v != "" && !audioDefault {
		if sid, err := strconv.ParseInt(v, 10, 64); err == nil {
			if sid == 0 {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			binding, rerr := s.resolveIssuedStream(ctx, pr.User, pr.ClientID, sid)
			if rerr != nil && !streamMiss(rerr) {
				writeHTTPError(w, http.StatusUnauthorized)
				return
			}
			if rerr != nil || binding.Info.Type != "audio" || binding.Key != key {
				writeHTTPError(w, http.StatusBadRequest)
				return
			}
			audio = &binding
		}
	}
	clearSub := q.Has("subtitleStreamID") && (subErr != nil || subID == 0)
	var sub *frontend.StreamBinding
	if v := q.Get("subtitleStreamID"); v != "" && !subDefault {
		if sid, err := strconv.ParseInt(v, 10, 64); err == nil {
			if sid == 0 {
				clearSub = true
			} else {
				binding, rerr := s.resolveIssuedStream(ctx, pr.User, pr.ClientID, sid)
				if rerr != nil && !streamMiss(rerr) {
					writeHTTPError(w, http.StatusUnauthorized)
					return
				}
				if rerr != nil || binding.Info.Type != "subtitle" || binding.Key != key || (audio != nil && audio.Key != binding.Key) {
					writeHTTPError(w, http.StatusBadRequest)
					return
				}
				sub = &binding
			}
		}
	}
	if q.Has("subtitleStreamID") {
		if err := s.recordManualSubtitle(r.Context(), pr.User.ID, key, !subDefault); err != nil {
			writeHTTPError(w, http.StatusServiceUnavailable)
			return
		}
	}
	s.selections.update(pr.User.ID, id, func(sel *selection) {
		if audioDefault || (audio != nil && audio.Estimated) {
			sel.audio, sel.audioSet, sel.audioStream = 0, false, nil
		}
		if subDefault {
			sel.subtitle, sel.subtitleSet, sel.subtitleStream, sel.subtitlePart = 0, false, nil, 0
		}
		if audio != nil && !audio.Estimated {
			info := audio.Info
			sel.audio, sel.audioSet, sel.audioStream = audio.Index, true, &info
		}
		if clearSub {
			sel.subtitle, sel.subtitleSet, sel.subtitleStream, sel.subtitlePart = -1, true, nil, 0
		} else if sub != nil {
			info := sub.Info
			if sel.subtitleStream == nil || sel.subtitleStream.ExternalURL != info.ExternalURL {
				sel.subtitlePart = 0
			}
			sel.subtitle, sel.subtitleSet, sel.subtitleStream = sub.Index, true, &info
		}
	})
	if clearSub || sub != nil {
		// This client now shows the track it picked (burnOnlyShownSubtitle).
		var picked *frontend.StreamInfo
		if sub != nil {
			picked = &sub.Info
		}
		s.playIDs.pickSubtitle(s.playIdentityFor(pr, frontend.RatingKey(id)), picked, s.now())
	}
	if !key.IsPluginKey() && (audio != nil || audioDefault || sub != nil || clearSub || subDefault) {
		// The client restarts playback for the new track; that restart
		// keeps the release being watched (track_restart.go).
		s.trackRestarts.pin(s.playIdentityFor(pr, key), s.liveRelease(pr.User.ID, pr.ClientID, key), s.now())
	}
	s.rememberStreamRequest(r, pr.User, frontend.RatingKey(id))
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
		status := http.StatusNotFound
		if r.Method == http.MethodPut || r.Method == http.MethodDelete {
			status = http.StatusBadRequest
		}
		writeHTTPError(w, status)
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
	if s.streams() == nil || pr == nil || pr.User == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	binding, rerr := s.resolveIssuedStream(r.Context(), pr.User, pr.ClientID, id)
	if rerr != nil {
		if errors.Is(rerr, frontend.ErrUnauthorized) {
			writeHTTPError(w, http.StatusUnauthorized)
			return
		}
		status := http.StatusNotFound
		if r.Method == http.MethodPut {
			status = http.StatusBadRequest
		}
		writeHTTPError(w, status)
		return
	}
	if binding.Info.Type == "lyrics" {
		s.serveLyricStream(w, r, pr.User, binding.Key)
		return
	}
	// Embedded tracks cannot be removed from the media bytes. PMS accepts
	// stream PUTs without changing the track; reads still use Ether's
	// extraction and sidecar paths.
	switch r.Method {
	case http.MethodPut:
		w.WriteHeader(http.StatusOK)
		return
	case http.MethodDelete:
		if !binding.Info.External {
			writeHTTPError(w, http.StatusForbidden)
			return
		}
		subs := s.svc().subtitles
		if subs == nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		if err := subs.DeleteSubtitle(r.Context(), binding.Key, pr.User, binding.Info); err != nil {
			writeHTTPError(w, http.StatusNotFound)
			return
		}
		s.selections.update(pr.User.ID, int64(binding.Key), func(sel *selection) {
			if sel.subtitleStream != nil && sel.subtitleStream.ExternalURL == binding.Info.ExternalURL {
				sel.subtitle, sel.subtitleSet, sel.subtitleStream, sel.subtitlePart = -1, true, nil, 0
			}
		})
		if tp := s.svc().trackPrefs; tp != nil {
			if err := tp.ForgetChoice(r.Context(), pr.User.ID, binding.Key, "subtitle"); err != nil {
				s.log.Warn("clear deleted subtitle preference", "error", err)
			}
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	if binding.Info.Type == "subtitle" && !binding.Info.External {
		s.serveEmbeddedStream(w, r, binding.Key, binding.Info)
		return
	}
	receipt, _ := acceptanceFrom(r.Context())
	rt := s.streams()
	if rt == nil {
		writeHTTPError(w, http.StatusNotFound)
		return
	}
	reader, info, err := rt.OpenSidecar(frontend.WithStreamServe(r.Context(), pr.Remote, format), s.plexStreamAccess(pr.User), receipt, id)
	if err != nil || reader == nil {
		if reader != nil {
			_ = reader.Close()
		}
		if errors.Is(err, frontend.ErrUnauthorized) {
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

// logPlaybackStart records only parsed playback controls, never URL queries,
// headers, paths, or capabilities that may carry credentials.
func (s *Server) logPlaybackStart(r *http.Request, p playRequest) {
	endpoint := "start.m3u8"
	if strings.HasSuffix(r.URL.Path, "/start.mpd") {
		endpoint = "start.mpd"
	}
	part, _ := strconv.Atoi(r.URL.Query().Get("partIndex"))
	protocol := p.protocol
	switch protocol {
	case "hls", "dash", "http":
	default:
		protocol = "unknown"
	}
	subtitles := r.URL.Query().Get("subtitles")
	switch subtitles {
	case "auto", "burn", "none":
	default:
		subtitles = ""
	}
	s.log.Debug("playback start controls", "endpoint", endpoint,
		"offset", p.offset, "offsetPresent", r.URL.Query().Has("offset"), "directPlay", p.opts.DirectPlay, "directStream", p.opts.DirectStream,
		"mediaIndex", p.mediaIndex, "partIndex", part, "protocol", protocol,
		"subtitles", subtitles, "subtitleStream", p.subSel, "session", p.sessionID)
}
