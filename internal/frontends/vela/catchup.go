package vela

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) catchUpNow() time.Time {
	if i.host.Now != nil {
		return i.host.Now().UTC()
	}
	return time.Now().UTC()
}

func archiveFlagFingerprint(programmes []frontend.LiveProgramme, channelDays int, now time.Time) []byte {
	flags := make([]byte, len(programmes))
	for n, p := range programmes {
		ended, airing := frontend.ArchiveFlags(max(channelDays, p.ArchiveDays), p.Start, p.End, now)
		switch {
		case ended:
			flags[n] = 1
		case airing:
			flags[n] = 2
		}
	}
	return flags
}

func addArchiveFlags(wire object, days int, start, end, now time.Time) {
	ended, airing := frontend.ArchiveFlags(days, start, end, now)
	if ended {
		wire["catchUp"] = true
	}
	if airing {
		wire["startOver"] = true
	}
}

func (i *instance) catchUpFeature(r *http.Request, v *frontend.Viewer) object {
	days, minutes := 0, 60
	if i.host.HostConfig != nil {
		minutes = i.host.HostConfig().LiveTV.EffectiveTimeshiftMinutes()
	}
	if reader, ok := i.host.LiveTV.(frontend.LiveCatchUpCapability); ok && v != nil {
		days, _ = reader.LiveCatchUpMaxDays(r.Context(), v)
	} else if v != nil && i.host.LiveTV != nil {
		channels, err := i.host.LiveTV.Channels(r.Context(), v)
		if err == nil {
			for _, ch := range channels {
				days = max(days, ch.ArchiveDays)
			}
		}
	}
	return object{"available": days > 0, "maxDays": days, "timeshift": object{"available": minutes > 0, "maxMinutes": minutes}}
}

type catchUpTarget struct {
	ProgrammeID     string
	StartAt         *time.Time
	DurationSeconds *float64
	From            string
}

func (t catchUpTarget) specified() bool {
	return t.ProgrammeID != "" || t.StartAt != nil || t.DurationSeconds != nil || t.From != ""
}

func (t catchUpTarget) selection(channel string) (frontend.CatchUpRequest, bool) {
	req := frontend.CatchUpRequest{ChannelID: channel, ProgrammeID: t.ProgrammeID}
	if channel == "" {
		return req, false
	}
	if t.ProgrammeID != "" {
		req.StartOver = t.From == "start"
		return req, t.StartAt == nil && t.DurationSeconds == nil && (t.From == "" || req.StartOver)
	}
	if t.StartAt == nil || t.DurationSeconds == nil || t.From != "" {
		return req, false
	}
	duration, ok := seconds(*t.DurationSeconds)
	req.Start, req.Duration, req.ClampStart = *t.StartAt, duration, true
	return req, ok && duration > 0
}

type catchUpPlayback struct {
	info             frontend.CatchUpInfo
	tuneID, mediaKey string
	// savedAt is when the position was last saved as watch state.
	savedAt    time.Time
	savedState string
}

// catchUpSaveEvery spaces out watch-state saves while an airing plays or stays paused.
const catchUpSaveEvery = 10 * time.Second

// catchUpToSave returns the airing to save a position for, or false: only a
// played-back airing (not start-over or a time range) has library watch
// state, and while it plays a save is due every catchUpSaveEvery. Called with
// s.mu held.
func (s *playSession) catchUpToSave(state string, now time.Time) (frontend.CatchUpInfo, bool) {
	a := s.archive
	if a == nil || a.info.ChannelID == "" || a.info.ProgrammeID == "" || a.info.StartOver || state == "buffering" ||
		(state == "playing" || state == "paused" && a.savedState == state) && now.Sub(a.savedAt) < catchUpSaveEvery {
		return frontend.CatchUpInfo{}, false
	}
	a.savedAt, a.savedState = now, state
	return a.info, true
}

// saveCatchUp records a position in a played-back airing as its watch state,
// shared with the library front ends (resume and watched). Vela always starts
// an airing from the beginning, so it never reports "stopped": an early stop
// must not clear a resume point saved from another app. A position past the
// watched threshold still marks it watched.
func (i *instance) saveCatchUp(ctx context.Context, v *frontend.Viewer, info frontend.CatchUpInfo, position time.Duration, state string) {
	watch, ok := i.host.LiveTV.(frontend.CatchUpWatchState)
	if !ok || v == nil || !i.host.LiveTV.Enabled() {
		return
	}
	if state != "playing" {
		state = "paused"
	}
	err := watch.CatchUpProgress(ctx, v, frontend.CatchUpEpisodeID(info.ProgrammeID), position, 0, state)
	if err != nil && !errors.Is(err, frontend.ErrNotFound) {
		slog.Warn("catch-up progress not saved", "frontend", "vela", "err", err)
	}
}

func archiveMediaURL(sessionID, key, generation string) string {
	url := "/sessions/media/" + sessionID + "/archive/" + key + "/index.m3u8"
	if generation != "" {
		url += "?archive=" + generation
	}
	return url
}

func (i *instance) startCatchUp(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, token string, req frontend.CatchUpRequest, c client, caps frontend.ClientCaps) {
	host, ok := i.host.LiveTV.(frontend.LiveCatchUp)
	if !ok {
		i.unavailable(w, r)
		return
	}
	if !c.HLS && !c.MSE {
		problem(w, 422, "decoder", "unsupported_transport")
		return
	}
	dev := requestDevice(r)
	scope, err := i.grantScope(r.Context(), token, dev)
	if err != nil {
		fail(w, err, r)
		return
	}
	info, err := host.StartCatchUp(r.Context(), v, req, caps, dev.ID)
	if err != nil {
		i.failCatchUp(w, r, v, err)
		return
	}
	key := rand.Text()
	kind := "remux"
	if info.Decision.Kind == frontend.DecisionTranscode {
		kind = "transcode"
	}
	path := object{"id": "primary", "kind": kind, "transport": "hls", "readiness": "ready", "mimeType": "application/vnd.apple.mpegurl",
		"url": archiveMediaURL(info.ID, key, info.MediaGeneration), "timelineOriginSeconds": float64(0), "qualities": []object{}, "boundaries": []object{}}
	plan := object{"sessionId": info.ID, "title": info.Title, "revision": uint64(1), "sourceGeneration": uint64(1), "seekGeneration": uint64(1),
		"state": "ready", "positionSeconds": float64(0), "paths": []object{path}, "selectedPathId": "primary",
		"audioTracks": []object{}, "subtitleTracks": []object{}, "chapters": []object{}, "fallback": object{"state": "unsupported"}}
	s := &playSession{id: info.ID, client: c, declaredCaps: caps, scope: scope, viewer: v.ID, device: dev.ID,
		accessViewer: v, accessDevice: dev, plan: plan, source: 1, seek: 1, revision: 1, cursor: 1, signal: make(chan struct{}),
		archive: &catchUpPlayback{info: info, tuneID: info.ID, mediaKey: key}}
	s.setCatchUpRange()
	if c.Features.AdaptiveHLS {
		setAdaptiveUnavailable(path, "live_timeline", dev.Language)
	}
	if req.StartOver && i.host.LiveEventLink != nil {
		if progKey, ref, err := i.host.LiveEventLink.LiveEventForChannel(r.Context(), v, req.ChannelID); err == nil && ref != nil {
			s.liveProgrammeKey, s.liveEvent = progKey, cloneLiveEventRef(ref)
			s.plan["liveEvent"] = liveEventObject(ref)
		}
	}
	refreshStartOver(s, i.catchUpNow())
	i.registerPlaySession(s)
	send(w, http.StatusOK, plan)
	i.watchSession(v, dev, info.ID, scope, s)
}

func (s *playSession) setCatchUpRange() {
	a := s.archive.info
	s.plan["catchUp"] = object{"channelId": a.ChannelID, "programmeId": a.ProgrammeID, "startAt": a.Start.UTC(), "endAt": a.End.UTC()}
	if !a.StartOver {
		s.plan["durationSeconds"] = a.End.Sub(a.Start).Seconds()
	}
}

func refreshStartOver(s *playSession, now time.Time) bool {
	info := s.archive.info
	if !info.StartOver {
		return false
	}
	edgeAt := minArchiveTime(now.Truncate(time.Second), info.ProgrammeEnd)
	edge := max(0, edgeAt.Sub(info.Start).Seconds())
	live, _ := s.plan["live"].(object)
	behind := max(0, edge-s.position.Seconds())
	if live["liveEdgeSeconds"] == edge && live["behindLiveSeconds"] == behind && live["edgeAt"] == edgeAt {
		return false
	}
	s.plan["live"] = object{"mode": "startOver", "windowStartSeconds": float64(0), "liveEdgeSeconds": edge, "edgeAt": edgeAt, "behindLiveSeconds": behind}
	s.archive.info.End = edgeAt
	s.setCatchUpRange()
	if !now.Before(info.ProgrammeEnd) {
		s.plan["durationSeconds"] = info.ProgrammeEnd.Sub(info.Start).Seconds()
	}
	return true
}

func minArchiveTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (i *instance) failCatchUp(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, err error) {
	var capErr *frontend.ProviderConnectionCapError
	if errors.As(err, &capErr) {
		sendProviderConnectionCap(w, capErr)
		return
	}
	var archive *frontend.CatchUpError
	if errors.As(err, &archive) {
		status := http.StatusConflict
		issue := object{"class": "conflict", "code": archive.Code, "message": archive.Error(), "retry": object{"allowed": false}}
		switch archive.Code {
		case "catchup_outside_archive":
			issue["archiveDays"], issue["earliestAt"] = archive.ArchiveDays, archive.EarliestAt
		case "catchup_unavailable":
			status = http.StatusBadGateway
			issue["class"], issue["retry"] = "source", object{"allowed": true, "afterSeconds": 30}
			w.Header().Set("Retry-After", "30")
		}
		send(w, status, object{"error": issue})
		return
	}
	if errors.Is(err, frontend.ErrInvalidCatchUpRange) {
		problem(w, 400, "request", "invalid_target")
		return
	}
	i.failFor(w, v, err, r)
}

func (i *instance) serveCatchUpMedia(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, s *playSession, tail string) {
	key, file, ok := strings.Cut(tail, "/")
	s.mu.Lock()
	if !ok || s.archive == nil || key != s.archive.mediaKey || !s.ended.IsZero() || s.stopping {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	id, live := s.archive.tuneID, s.archive.info.ChannelID == ""
	s.mu.Unlock()
	done := s.beginMediaRead()
	defer done()
	if live {
		if media, ok := i.host.LiveTV.(interface {
			ServeLiveSessionMedia(http.ResponseWriter, *http.Request, *frontend.Viewer, string, string)
		}); ok {
			media.ServeLiveSessionMedia(w, r, v, id, file)
			return
		}
	}
	if file == "index.m3u8" {
		i.host.LiveTV.ServePlaylist(w, r, id, v)
		return
	}
	// Generated archive media filenames embed the output generation.
	if file != path.Base(file) || !strings.HasPrefix(file, "g-") || len(file) <= 35 || file[34] != '-' {
		http.NotFound(w, r)
		return
	}
	bare := file[35:]
	n := -1
	if bare != "init.mp4" {
		number, ext, ok := strings.Cut(bare, ".")
		var err error
		n, err = strconv.Atoi(number)
		if !ok || err != nil || n < 0 || (ext != "ts" && ext != "m4s") {
			http.NotFound(w, r)
			return
		}
	}
	i.host.LiveTV.ServeRelaySegment(w, r, id, n, v)
}

func (i *instance) stopPlayback(ctx context.Context, access frontend.SessionAccess, id string, s *playSession) error {
	s.mu.Lock()
	archive := s.archive
	var tuneID string
	if archive != nil {
		tuneID = archive.tuneID
	}
	s.mu.Unlock()
	if tuneID != "" {
		return i.host.LiveTV.Stop(ctx, tuneID, access.Viewer)
	}
	return i.host.Sessions.Stop(ctx, access, id)
}

func (i *instance) playbackKeepAlive(ctx context.Context, access frontend.SessionAccess, id string, s *playSession) error {
	s.mu.Lock()
	var tuneID string
	if s.archive != nil {
		tuneID = s.archive.tuneID
	}
	s.mu.Unlock()
	if tuneID != "" {
		return i.host.LiveTV.(frontend.LiveCatchUp).TouchLiveSession(ctx, access.Viewer, tuneID)
	}
	return i.host.Sessions.KeepAlive(ctx, access, id)
}

func (i *instance) catchUpCommand(w http.ResponseWriter, r *http.Request, v *frontend.Viewer, s *playSession, kind string, position *float64) {
	host := i.host.LiveTV.(frontend.LiveCatchUp)
	a := s.archive
	id := a.tuneID
	var err error
	event := "plan"
	switch kind {
	case "stop":
		s.stopping = true
		defer func() { s.stopping = false }()
		info, save := s.catchUpToSave("stopped", i.catchUpNow())
		position := s.position
		if !s.stopPort(w, func() {
			err = i.host.LiveTV.Stop(r.Context(), id, v)
			if save {
				i.saveCatchUp(r.Context(), v, info, position, "stopped")
			}
		}) {
			return
		}
		if err == nil || errors.Is(err, frontend.ErrNotFound) {
			err = nil
			s.ended = i.catchUpNow()
			s.clearDisconnectTimerLocked()
			s.plan["state"] = "ended"
			event = "ended"
		}
	case "pause", "resume":
		if !s.port(w, func() { err = host.TouchLiveSession(r.Context(), v, id) }) {
			return
		}
		if err == nil {
			refreshStartOver(s, i.catchUpNow())
			if kind == "pause" {
				s.plan["state"] = "paused"
			} else {
				s.plan["state"] = "playing"
			}
		}
	case "seek":
		if position == nil {
			problem(w, 400, "request", "invalid_position")
			return
		}
		at, ok := seconds(*position)
		if !ok {
			problem(w, 400, "request", "invalid_position")
			return
		}
		var next frontend.CatchUpInfo
		if !s.port(w, func() { next, err = host.SeekCatchUp(r.Context(), v, id, at) }) {
			return
		}
		if errors.Is(err, frontend.ErrInvalidCatchUpRange) {
			problem(w, 400, "request", "invalid_position")
			return
		}
		if err == nil {
			if next.MediaGeneration != a.info.MediaGeneration {
				selected := selectedPlaybackPath(s.plan, s.plan["paths"].([]object))
				selected["url"] = archiveMediaURL(s.id, a.mediaKey, next.MediaGeneration)
				selected["timelineOriginSeconds"] = next.Origin.Seconds()
			}
			a.info = next
			s.seek++
			s.position = at
			s.setCatchUpRange()
			refreshStartOver(s, i.catchUpNow())
		}
	case "goLive":
		if !a.info.StartOver {
			problem(w, 400, "request", "invalid_command")
			return
		}
		var live frontend.LiveTVSessionInfo
		channel := a.info.ChannelID
		if !s.port(w, func() {
			live, err = i.host.LiveTV.TuneWithSession(r.Context(), v, channel, s.declaredCaps, s.device)
		}) {
			return
		}
		if err == nil {
			a.tuneID, a.info = live.ID, frontend.CatchUpInfo{}
			a.mediaKey = rand.Text()
			s.channelID = channel
			delete(s.plan, "catchUp")
			delete(s.plan, "durationSeconds")
			s.plan["channelId"], s.plan["title"] = channel, live.Channel.Name
			s.plan["live"] = object{"mode": "adaptive"}
			s.plan["state"] = "ready"
			selected := selectedPlaybackPath(s.plan, s.plan["paths"].([]object))
			selected["url"] = archiveMediaURL(s.id, a.mediaKey, "")
			selected["timelineOriginSeconds"] = float64(0)
			s.source++
			s.seek++
			s.position = 0
			if !s.portSnapshot(w, func(copy *playSession) { i.refreshOutput(r.Context(), s.id, copy) }) {
				return
			}
			alignUnobservedLivePosition(s)
		}
	default:
		problem(w, 400, "request", "invalid_command")
		return
	}
	if err != nil {
		i.failCatchUp(w, r, v, err)
		return
	}
	s.publish(event)
	send(w, 200, s.plan)
}
