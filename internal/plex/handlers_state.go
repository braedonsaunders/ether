package plex

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/plex/model"
)

// stateKey reads the item key from ratingKey= or key=.
func stateKey(r *http.Request) core.RatingKey {
	q := r.URL.Query()
	if k, err := core.ParseRatingKey(q.Get("ratingKey")); err == nil {
		return k
	}
	return keyFromPath(q.Get("key"))
}

func msParam(v string) time.Duration {
	return durationParam(v, time.Millisecond)
}

// durationParam parses a non-negative count of unit. NaN, infinities and
// values past the time.Duration range read as 0: converting them would
// yield an implementation-defined (typically negative) duration.
func durationParam(v string, unit time.Duration) time.Duration {
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || !(n >= 0) || n*float64(unit) >= float64(math.MaxInt64) {
		return 0
	}
	return time.Duration(n * float64(unit))
}

// handleTimeline is /:/timeline: playback progress from the client.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	if s.handleLiveTimeline(w, r) {
		return
	}
	ctx := r.Context()
	pr := FromContext(ctx)
	unlockOwner, locked := s.lockPlaybackRequest(w, r)
	if !locked {
		return
	}
	defer unlockOwner()

	q := r.URL.Query()
	key := stateKey(r)
	if key == 0 || pr.User == nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	s.rememberStreamRequest(r, pr.User, key)
	state := q.Get("state")
	// A late stop from an explicitly superseded playback must not stop the
	// replacement source or overwrite its progress. Unknown/omitted IDs keep
	// the historical behavior for clients with different request identities.
	if s.playbackOwners.Stale(pr.User.ID, pr.ClientID, key, pr.SessionID) {
		s.cleanSupersededEncoders(ctx, pr.User.ID, pr.ClientID, key, pr.SessionID)
		s.sessions.remove(trackID(pr.User.ID, pr.ClientID, pr.SessionID))
		s.writeEmpty(w, r)
		return
	}
	offset := msParam(q.Get("time"))
	duration := msParam(q.Get("duration"))
	user := pr.User
	s.log.Debug("playback timeline", "user", user.ID, "client", pr.ClientID, "key", key, "state", state, "position", offset, "duration", duration)

	if s.d.State != nil && key.Kind() != core.KindExtra {
		if err := s.d.State.Progress(ctx, user.ID, key, offset, duration, state); err != nil {
			s.log.Warn("record progress", "key", key, "err", err)
		}
	}
	var ps *core.PlaySession
	if s.d.Playback != nil {
		if p, ok := s.d.Playback.SessionFor(user.ID, pr.ClientID, key); ok {
			ps = p
			if state != "stopped" {
				s.ensureTrickplay(ctx, p, offset)
			}
			if t, ok := s.d.Playback.(SessionToucher); ok && (state == "playing" || state == "paused") {
				t.Touch(p.ID)
			}
		}
	}
	tids := s.transcodes.forClient(user.ID, pr.ClientID, key)
	for _, id := range tids {
		if e, ok := s.transcodes.get(id); ok && s.playbackOwners.Stale(user.ID, pr.ClientID, key, e.playbackID) {
			s.cleanSupersededEncoders(ctx, user.ID, pr.ClientID, key, e.playbackID)
			continue
		}
		s.transcodes.touch(id, s.now())
		if s.d.Transcoder != nil {
			if output, ok := s.d.Transcoder.Session(id); ok {
				if activity, ok := output.(core.SessionActivity); ok {
					activity.PlaybackState(state)
				}
			}
		}
	}
	pqItem, _ := strconv.ParseInt(q.Get("playQueueItemID"), 10, 64)
	if pqItem > 0 {
		s.queues.selectItem(user.ID, pqItem)
	}
	tid := trackID(user.ID, pr.ClientID, pr.SessionID)

	mc := &model.MediaContainer{}
	if reason, killed := s.takeTermination(tid); killed {
		mc.Extra = []model.Attr{model.A("terminationCode", 2006), model.A("terminationText", reason)}
		state = "stopped"
	}
	if state == "stopped" {
		if e, ok := s.sessions.remove(tid); ok {
			e.offset = offset
			s.notifyPlaying(e, "stopped")
			s.cl.poke()
		} else if s.cl != nil && ps == nil && len(tids) == 0 {
			// The client switched nodes mid-play: the node serving it ends it.
			s.cl.forwardStop(r.Context(), tid, offset)
		}
		if ps != nil {
			s.d.Playback.Stop(ps.ID)
			s.cancelTrickplayIfIdle(ps)
		}
		for _, id := range tids {
			unlock, err := s.lockEncoder(ctx, id)
			if err != nil {
				return
			}
			if e, ok := s.transcodes.get(id); ok && e.userID == user.ID && e.clientID == pr.ClientID && e.key == key {
				if s.d.Transcoder != nil {
					s.d.Transcoder.Stop(id)
				}
				s.transcodes.remove(id)
			}
			unlock()
		}
	} else {
		client := *pr
		client.User = nil
		fresh := !s.sessions.has(tid)
		if fresh && s.cl != nil {
			s.sessions.ensure(tid, s.cl.allocSessionKey())
		}
		var changed bool
		e := s.sessions.update(tid, func(e *playEntry) {
			changed = fresh || e.key != key || e.state != state
			if e.key != key {
				e.startedAt = s.now()
			}
			e.user = *user
			e.client = client
			e.key = key
			e.state = state
			e.offset = offset
			e.duration = duration
			e.playQueueItemID = pqItem
			e.transcodeID = ""
			if len(tids) > 0 {
				e.transcodeID = tids[0]
			}
			e.updatedAt = s.now()
		})
		s.notifyPlaying(e, state)
		if changed {
			s.cl.poke() // share starts and state changes now; positions ride the heartbeat
		}
	}
	if progress, ok := s.d.Playback.(core.PlaybackTimeline); ok {
		progress.PlaybackProgress(user.ID, pr.ClientID, key, offset, state)
	}
	if hook := s.d.Hooks.OnTimeline; hook != nil {
		u := *user
		s.goHook("OnTimeline", func() { hook(&u, key, offset, duration, state) })
	}
	s.write(w, r, mc)
}

// handleProgress is the legacy /:/progress?key=&time=&state=.
func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	key := stateKey(r)
	if key == 0 || pr.User == nil || s.d.State == nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	q := r.URL.Query()
	if err := s.d.State.Progress(ctx, pr.User.ID, key, msParam(q.Get("time")), 0, q.Get("state")); err != nil {
		s.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleScrobble(w http.ResponseWriter, r *http.Request)   { s.scrobble(w, r, true) }
func (s *Server) handleUnscrobble(w http.ResponseWriter, r *http.Request) { s.scrobble(w, r, false) }

func (s *Server) scrobble(w http.ResponseWriter, r *http.Request, watched bool) {
	ctx := r.Context()
	pr := FromContext(ctx)
	key := stateKey(r)
	if key == 0 || pr.User == nil || s.d.State == nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	// Containers mark every episode.
	keys := []core.RatingKey{key}
	if key.Kind() == core.KindShow || key.Kind() == core.KindSeason {
		if ks, err := s.expandKey(ctx, pr, key); err == nil && len(ks) > 0 {
			keys = ks
		}
	}
	for _, k := range keys {
		var err error
		if watched {
			err = s.d.State.Scrobble(ctx, pr.User.ID, k)
		} else {
			err = s.d.State.Unscrobble(ctx, pr.User.ID, k)
		}
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
	}
	t := core.TypeMovie
	switch key.Kind() {
	case core.KindShow:
		t = core.TypeShow
	case core.KindSeason:
		t = core.TypeSeason
	case core.KindEpisode:
		t = core.TypeEpisode
	}
	s.notifyTimeline(key, t, "", pr.User.ID)
	s.log.Info("watch state changed", "user", pr.User.ID, "client", pr.ClientID,
		"key", key, "watched", watched, "items", len(keys))
	if watched {
		if hook := s.d.Hooks.OnScrobble; hook != nil {
			u := *pr.User
			s.goHook("OnScrobble", func() { hook(&u, key) })
		}
	}
	w.WriteHeader(http.StatusOK)
}

// handleRate is /:/rate?key=&rating= (0-10, -1 clears).
func (s *Server) handleRate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	key := stateKey(r)
	if key == 0 || pr.User == nil || s.d.State == nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	rating, err := strconv.ParseFloat(r.URL.Query().Get("rating"), 64)
	if err != nil || math.IsNaN(rating) {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if rating < 0 {
		rating = 0
	}
	if rating > 10 {
		rating = 10
	}
	if rating > 0 {
		promoter := s.d.VibeSearch
		if promoter == nil {
			promoter, _ = s.d.Collections.(core.VibeSearcher)
		}
		if promoter != nil {
			if _, promoted, err := promoter.PromoteEphemeral(ctx, pr.User, key); err != nil {
				s.writeErr(w, r, err)
				return
			} else if promoted {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}
	if err := s.d.State.Rate(ctx, pr.User.ID, key, rating); err != nil {
		s.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleRemoveFromContinueWatching clears an item's resume point.
func (s *Server) handleRemoveFromContinueWatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	key := stateKey(r)
	if key == 0 || pr.User == nil || s.d.State == nil {
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	if err := s.discoveryRemoveContinue(ctx, pr, key); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if err := s.d.State.Progress(ctx, pr.User.ID, key, 0, 0, "stopped"); err != nil {
		s.writeErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// ---------------------------------------------------------------------------
// Sessions

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	entries := s.sessionEntries(ctx)
	sort.Slice(entries, func(i, j int) bool { return entries[i].sessionKey < entries[j].sessionKey })
	mc := &model.MediaContainer{}
	for _, e := range entries {
		if !pr.User.IsOwner && e.user.ID != pr.User.ID {
			continue
		}
		t, err := s.d.Catalog.Get(ctx, e.key)
		if err != nil {
			continue
		}
		viewer := &Request{User: &e.user, ClientID: e.client.ClientID, Product: e.client.Product, Platform: e.client.Platform}
		md := s.metadata(ctx, viewer, t, renderOpts{media: true})
		md.SessionKey = strconv.Itoa(e.sessionKey)
		md.ViewOffset = ms(e.offset)
		md.User = &model.SessionUser{ID: strconv.FormatInt(e.user.PlexID, 10), Thumb: e.user.Thumb, Title: e.user.Title}
		if md.User.Title == "" {
			md.User.Title = e.user.Username
		}
		state := e.state
		if state == "" {
			state = "playing"
		}
		title := e.client.DeviceName
		if title == "" {
			title = e.client.Device
		}
		md.Player = &model.Player{
			Address:           e.client.RemoteIP.String(),
			Device:            e.client.Device,
			MachineIdentifier: e.client.ClientID,
			Model:             e.client.Model,
			Platform:          e.client.Platform,
			PlatformVersion:   e.client.PlatformVersion,
			Product:           e.client.Product,
			State:             state,
			Title:             title,
			Vendor:            e.client.Vendor,
			Version:           e.client.Version,
			Local:             !e.client.Remote,
			UserID:            e.user.PlexID,
		}
		loc := "lan"
		if e.client.Remote {
			loc = "wan"
		}
		md.Session = &model.Session{ID: e.client.SessionID, Location: loc}
		if md.Session.ID == "" {
			md.Session.ID = e.id
		}
		if e.node != "" {
			md.TranscodeSession = e.remoteTS
		} else if e.transcodeID != "" {
			if te, ok := s.transcodes.get(e.transcodeID); ok {
				ts := transcodeSessionModel(te)
				md.TranscodeSession = &ts
			}
		}
		mc.Metadata = append(mc.Metadata, md)
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// handleHistory is /status/sessions/history/all (the caller's history).
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr := FromContext(ctx)
	mc := &model.MediaContainer{}
	if s.d.State == nil || pr.User == nil {
		s.write(w, r, mc)
		return
	}
	var since time.Time
	if v, err := strconv.ParseInt(r.URL.Query().Get("viewedAt>"), 10, 64); err == nil {
		since = time.Unix(v, 0)
	}
	_, size := pr.pageOf(0)
	hist, err := s.d.State.History(ctx, pr.User.ID, since, min(size, 500))
	if err != nil {
		s.writeErr(w, r, err)
		return
	}
	keys := make([]core.RatingKey, 0, len(hist))
	for _, h := range hist {
		keys = append(keys, h.Key)
	}
	titles := s.getTitles(ctx, dedupeKeys(keys))
	byKey := map[core.RatingKey]*core.Title{}
	for i := range titles {
		byKey[titles[i].Key] = &titles[i]
	}
	visible := map[core.RatingKey]bool{}
	for i, h := range hist {
		t, ok := byKey[h.Key]
		// History is rendered by hand, not through renderTitles: apply the
		// same visibility guard (the show's policy included) so titles
		// restricted after they were watched stay hidden.
		if !ok {
			continue
		}
		v, seen := visible[t.Key]
		if !seen {
			v = s.visibleTitle(ctx, pr.User, t)
			visible[t.Key] = v
		}
		if !v {
			continue
		}
		md := model.Metadata{
			RatingKey: t.Key.String(),
			Key:       "/library/metadata/" + t.Key.String(),
			Type:      string(t.Type),
			Title:     t.Title,
			Thumb:     thumbURL(t),
			Extra: []model.Attr{
				model.A("historyKey", "/status/sessions/history/"+strconv.Itoa(i+1)),
				model.A("viewedAt", h.ViewedAt.Unix()),
				model.A("accountID", pr.User.PlexID),
			},
		}
		if t.Type == core.TypeEpisode {
			md.GrandparentTitle = t.ShowTitle
			md.Index = model.Ptr(t.Episode)
			md.ParentIndex = model.Ptr(t.Season)
		}
		mc.Metadata = append(mc.Metadata, md)
	}
	mc.Size = len(mc.Metadata)
	s.write(w, r, mc)
}

// handleTerminate is the owner's "stop this stream" (sessionId, reason):
// delivered as terminationCode/Text on the client's next timeline.
func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := q.Get("sessionId")
	if sid == "" {
		// An empty id would match every session that has no session
		// identifier and stop an arbitrary one.
		writeHTTPError(w, http.StatusBadRequest)
		return
	}
	reason := q.Get("reason")
	if reason == "" {
		reason = "The server owner has ended this stream."
	}
	if s.terminateLocal(sid, reason) {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Another node may serve it: that node applies the kill.
	if s.cl != nil {
		for _, e := range s.cl.remoteEntries(r.Context()) {
			if matchesSession(e, sid) {
				s.cl.publish(topicTerminate, terminateEvent{Scope: "plex", ID: sid, Reason: reason, Node: e.node})
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}
	writeHTTPError(w, http.StatusNotFound)
}

func matchesSession(e playEntry, sid string) bool {
	return e.client.SessionID == sid || e.id == sid || strconv.Itoa(e.sessionKey) == sid
}

// terminateLocal ends a session this node tracks: the stop is delivered as
// terminationCode/Text on the client's next timeline and the source closes.
func (s *Server) terminateLocal(sid, reason string) bool {
	for _, e := range s.sessions.list() {
		if matchesSession(e, sid) {
			unlock, err := s.lockPlaybackOwner(s.ctx, e.user.ID, e.client.ClientID)
			if err != nil {
				return false
			}
			defer unlock()
			if s.playbackOwners.Stale(e.user.ID, e.client.ClientID, e.key, e.client.SessionID) {
				s.cleanSupersededEncoders(s.ctx, e.user.ID, e.client.ClientID, e.key, e.client.SessionID)
				return true
			}

			s.setTermination(e.id, reason)
			if s.d.Playback != nil {
				if ps, ok := s.d.Playback.SessionFor(e.user.ID, e.client.ClientID, e.key); ok {
					s.d.Playback.Stop(ps.ID)
				}
			}
			return true
		}
	}
	return false
}

// termination is an owner stream kill awaiting the client's next timeline.
type termination struct {
	reason string
	at     time.Time
}

// terminationTTL bounds how long a kill waits for its client: one that
// never reports again (closed app, lost network) would otherwise keep its
// entry forever.
const terminationTTL = 10 * time.Minute

func (s *Server) setTermination(tid, reason string) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	if s.terms == nil {
		s.terms = map[string]termination{}
	}
	s.terms[tid] = termination{reason: reason, at: s.now()}
}

func (s *Server) takeTermination(tid string) (string, bool) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	t, ok := s.terms[tid]
	if ok {
		delete(s.terms, tid)
	}
	return t.reason, ok
}

func (s *Server) expireTerminations(now time.Time, ttl time.Duration) {
	s.termMu.Lock()
	defer s.termMu.Unlock()
	for tid, t := range s.terms {
		if now.Sub(t.at) > ttl {
			delete(s.terms, tid)
		}
	}
}
