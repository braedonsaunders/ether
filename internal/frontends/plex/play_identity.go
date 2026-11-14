package plex

// Play identities (#267). Plex Web and Plex for macOS open the player
// settings panel from the Media, Part and Stream ids they already hold:
// the /library/metadata item fetched before playback, the decision answer
// and /status/sessions. The panel throws when a later response drops one
// of those ids, renumbers a stream, or marks a different Media selected.
//
// An on-demand title changes underneath a playing client in four ways:
//   - the background search that fills in same-quality versions finishes,
//     or the cached search expires, so the resolver returns a different
//     version list (count, order or members);
//   - the playing release is probed, so its estimated tracks become the
//     real ones (an audio id hashes codec, channels and title);
//   - the session fails over to another release with other tracks and,
//     often, another container;
//   - the session starts, so the part key's changestamp moves from the
//     title's update time to the session start.
//
// A playIdentity is what one viewer's client was issued for one title. The
// version list is pinned when it is rendered. From the moment a decision
// starts admitting a session until that session ends it is frozen: renders
// reuse it instead of asking the resolver again, the Media the session was
// admitted on stays the selected one, part keys keep their first issued
// value, and each Media's stream ids carry over to the tracks they named.
// A probe or failover changes only this client's mapping from an issued id
// to the track it now names; the broker's title-wide table is never
// rewritten, because other sessions may resolve the same estimate to other
// tracks.

import (
	"sort"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

const (
	playIdentityTTL   = 30 * time.Minute
	maxPlayIdentities = 4096
	maxIssuedStreams  = 64 // per Media
)

type playIdentityKey struct {
	userID   int64
	clientID string
	key      frontend.RatingKey
}

func (k playIdentityKey) valid() bool { return k.userID != 0 && k.key != 0 }

func (s *Server) playIdentityFor(pr *Request, key frontend.RatingKey) playIdentityKey {
	if pr == nil || pr.User == nil {
		return playIdentityKey{}
	}
	return playIdentityKey{userID: pr.User.ID, clientID: pr.ClientID, key: key}
}

type playIdentity struct {
	// variants is the Play Version list last issued, Auto first; resolved
	// with no variants means the client was issued a single version.
	variants []frontend.ReleaseVariant
	resolved bool
	at       time.Time
	// frozen: a session is being admitted or is live. The list, selected
	// Media and part keys are not re-resolved until a render finds no
	// session and no admission in flight.
	flowUntil time.Time
	frozen    bool
	admitting int
	// selected is the Media id the session was admitted on (0 = unknown);
	// admittedRelease is the release it opened on, before any failover.
	selected        int64
	admittedRelease string
	// autoAligned is the admitted release the pinned Auto row was last
	// relabelled for (see alignAuto).
	autoAligned string
	media       map[int64]*issuedMedia
}

type issuedMedia struct {
	partKey string
	streams []issuedStream        // issue order
	alias   map[int64]int64       // a track's own id -> the id issued for it
	remap   map[int64]remapTarget // issued id -> the track it now names
	// rendered: a stream list was issued for this Media; subtitle is the
	// track its latest list marked selected (nil = None). A subtitle the
	// client was never shown selected is not burned (#313).
	rendered bool
	subtitle *frontend.StreamInfo
}

type issuedStream struct {
	id       int64
	typ      int
	lang     string
	codec    string
	channels int
	selected bool
	external bool
}

type remapTarget struct {
	index int
	info  frontend.StreamInfo
}

// pinnedVariants is a render's view of an identity.
type pinnedVariants struct {
	variants        []frontend.ReleaseVariant
	resolved        bool
	frozen          bool
	selected        int64
	admittedRelease string
	autoAligned     string
}

type playIdentities struct {
	mu sync.Mutex
	m  map[playIdentityKey]*playIdentity
}

func newPlayIdentities() *playIdentities {
	return &playIdentities{m: map[playIdentityKey]*playIdentity{}}
}

// entryLocked returns k's identity, creating it when there is room. A full
// table drops expired, then the oldest idle identities; it never drops a
// frozen one, and when every identity is frozen it creates nothing and the
// caller renders unpinned.
func (p *playIdentities) entryLocked(k playIdentityKey, now time.Time) *playIdentity {
	if e := p.m[k]; e != nil {
		return e
	}
	if len(p.m) >= maxPlayIdentities {
		var oldest playIdentityKey
		var oldestAt time.Time
		found := false
		for ek, e := range p.m {
			if e.frozen || e.admitting > 0 {
				continue
			}
			if now.Sub(e.at) > playIdentityTTL {
				delete(p.m, ek)
				continue
			}
			if !found || e.at.Before(oldestAt) {
				oldest, oldestAt, found = ek, e.at, true
			}
		}
		if len(p.m) >= maxPlayIdentities {
			if !found {
				return nil
			}
			delete(p.m, oldest)
		}
	}
	e := &playIdentity{at: now, media: map[int64]*issuedMedia{}}
	p.m[k] = e
	return e
}

// pinned returns the identity's list. live renders (a session exists)
// freeze it; a render without a session or admission thaws it, so the
// selected playback state can be cleared. The issued list remains valid
// through explicit pre-play (90 seconds pending) and live admission.
// Ordinary detail renders remain free to discover versions.
func (p *playIdentities) pinned(k playIdentityKey, live bool, now time.Time) pinnedVariants {
	if p == nil || !k.valid() {
		return pinnedVariants{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.m[k]
	if e == nil {
		return pinnedVariants{}
	}
	if live {
		e.frozen = true
		e.at = now
	} else if e.frozen && e.admitting == 0 && !now.Before(e.flowUntil) {
		// Thawing ends the freeze, not the issued list: a client restarting
		// playback (e.g. to select a just-downloaded subtitle, #389) stops
		// the session, re-reads the item and looks its held mediaIndex and
		// Part id up in the answer. Discarding the list here let that
		// re-read collapse to one Media when the resolver's cached search
		// had expired during the movie. The list still expires with the
		// identity (playIdentityTTL from last use, below).
		e.frozen, e.selected, e.admittedRelease, e.autoAligned = false, 0, "", ""
		e.flowUntil = time.Time{}
	}
	if !e.frozen && now.Sub(e.at) > playIdentityTTL {
		return pinnedVariants{}
	}
	e.at = now
	return pinnedVariants{variants: cloneVariants(e.variants), resolved: e.resolved, frozen: e.frozen, selected: e.selected, admittedRelease: e.admittedRelease, autoAligned: e.autoAligned}
}

// issueList remembers a real detail list for positional version choices.
// Ordinary cold/singleton renders remain unresolved and can gain a picker.
// Explicit pre-play/queue flows retain their list, including Auto, until
// admission, stop or pending-flow expiry (#339).
func (p *playIdentities) issueList(k playIdentityKey, vs []frontend.ReleaseVariant, live bool, now time.Time) {
	if p == nil || !k.valid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(k, now)
	if e == nil || (e.frozen && e.resolved) {
		return
	}
	// A re-resolve may grow the issued list (discovery completing), never
	// shrink it: the client may still hold the longer list's Media/Part
	// ids and mediaIndex positions (#389).
	if len(e.variants) >= 2 && len(vs) < len(e.variants) {
		e.at = now
		if live {
			e.frozen = true
		}
		return
	}
	e.variants = cloneVariants(vs)
	e.resolved = len(vs) >= 2 || e.frozen
	e.at = now
	if live {
		e.frozen = true
	}
}

// alignAuto replaces the pinned Auto row with the resolver's Auto for the
// admitted release (its label names the release that plays), once per
// admission. Auto's Media id is the rating key, so no id changes; a nil
// auto only records the attempt.
func (p *playIdentities) alignAuto(k playIdentityKey, auto *frontend.ReleaseVariant, release string) {
	if p == nil || !k.valid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.m[k]
	if e == nil || (e.admittedRelease != "" && e.admittedRelease != release) {
		return
	}
	e.autoAligned = release
	if auto != nil && len(e.variants) > 0 && e.variants[0].Auto {
		v := *auto
		v.Info.Streams = append([]frontend.StreamInfo(nil), auto.Info.Streams...)
		e.variants[0] = v
	}
}

// releases is the issued list's release ids (mediaIndex resolution).
func (p *playIdentities) releases(k playIdentityKey, now time.Time) ([]string, bool) {
	if p == nil || !k.valid() {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.m[k]
	if e == nil || !e.resolved || (!e.frozen && now.Sub(e.at) > playIdentityTTL) {
		return nil, false
	}
	// A singleton is also an issued list. In particular, do not resolve a
	// stale nonzero mediaIndex against newly arrived candidates.
	if len(e.variants) < 2 {
		return []string{""}, true
	}
	ids := make([]string, len(e.variants))
	for i, v := range e.variants {
		ids[i] = versionIdentity(v)
	}
	return ids, true
}

// beginAdmission freezes the identity while a decision admits a session,
// so a page render racing the admission cannot replace the list that the
// request's mediaIndex is resolved against.
func (p *playIdentities) beginAdmission(k playIdentityKey, now time.Time) {
	if p == nil || !k.valid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.entryLocked(k, now); e != nil {
		if !e.frozen && now.Sub(e.at) > playIdentityTTL {
			*e = playIdentity{at: now, media: map[int64]*issuedMedia{}}
		}
		e.admitting++
		e.frozen = true
		e.at = now
	}
}

// endAdmission records the Media the session was admitted on. A failed
// admission leaves the previous selection; the next render without a
// session thaws the identity.
func (p *playIdentities) endAdmission(k playIdentityKey, mediaID int64, release string, ok bool, now time.Time) {
	if p == nil || !k.valid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.m[k]
	if e == nil {
		return
	}
	if e.admitting > 0 {
		e.admitting--
	}
	if ok {
		e.selected, e.admittedRelease = mediaID, release
	}
	e.at = now
}

// admit records a session admitted outside a decision (a direct part
// read). An identity already admitted keeps its selection unless replace.
func (p *playIdentities) admit(k playIdentityKey, mediaID int64, release string, replace bool, now time.Time) {
	if p == nil || !k.valid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(k, now)
	if e == nil {
		return
	}
	e.frozen = true
	e.at = now
	if replace || e.selected == 0 {
		e.selected, e.admittedRelease = mediaID, release
	}
}

// partKey returns the Part key to render: while frozen the first key issued
// for this Media, otherwise the key just built (and remembers it).
func (p *playIdentities) partKey(k playIdentityKey, mediaID int64, built string, now time.Time) string {
	if p == nil || !k.valid() || built == "" {
		return built
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(k, now)
	if e == nil {
		return built
	}
	im := e.mediaLocked(mediaID)
	if e.frozen && im.partKey != "" {
		return im.partKey
	}
	im.partKey = built
	return built
}

func (e *playIdentity) mediaLocked(id int64) *issuedMedia {
	im := e.media[id]
	if im == nil {
		im = &issuedMedia{alias: map[int64]int64{}, remap: map[int64]remapTarget{}}
		e.media[id] = im
	}
	return im
}

// issueStreams rewrites out's ids so every id this client was issued for
// the Media keeps naming a track while one exists. out[i] renders
// info.Streams[src[i]] and carries that track's own (title-wide) id.
func (p *playIdentities) issueStreams(k playIdentityKey, mediaID int64, out []model.Stream, src []int, info frontend.MediaInfo, now time.Time) {
	if p == nil || !k.valid() || len(out) == 0 || len(out) != len(src) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(k, now)
	if e == nil {
		return
	}
	im := e.mediaLocked(mediaID)
	issued := make(map[int64]int, len(im.streams))
	for i, is := range im.streams {
		issued[is.id] = i
	}
	n := len(out)
	natural := make([]int64, n)
	final := make([]int64, n)
	used := make(map[int64]bool, n)
	for i := range out {
		natural[i] = out[i].ID
	}
	// A track keeps the id it was issued under, unless another track in
	// this render owns that id itself.
	owner := make(map[int64]int, n)
	for i := range out {
		owner[natural[i]] = i
	}
	for i := range out {
		a, ok := im.alias[natural[i]]
		if !ok || a == 0 || used[a] {
			continue
		}
		if j, owned := owner[a]; owned && j != i {
			continue
		}
		final[i], used[a] = a, true
	}
	// A track whose own id was issued keeps it.
	for i := range out {
		if final[i] != 0 || natural[i] == 0 {
			continue
		}
		if _, ok := issued[natural[i]]; ok && !used[natural[i]] {
			final[i], used[natural[i]] = natural[i], true
		}
	}
	// Issued ids that no longer name a track move to the new track that
	// best matches what they named (same type; language, codec, layout and
	// selection break ties). External sidecars keep their own ids: theirs
	// do not depend on the release.
	type pair struct {
		score, issued, out int
	}
	var pairs []pair
	for j, is := range im.streams {
		if used[is.id] || is.external {
			continue
		}
		for i := range out {
			if final[i] != 0 || natural[i] == 0 || out[i].StreamType != is.typ || isExternalStream(out[i]) {
				continue
			}
			score := 1
			if out[i].LanguageCode == is.lang {
				score += 8
			}
			if out[i].Codec == is.codec {
				score += 2
			}
			if out[i].Channels == is.channels {
				score++
			}
			if streamSelected(out[i]) == is.selected {
				score += 4
			}
			pairs = append(pairs, pair{score: score, issued: j, out: i})
		}
	}
	sort.SliceStable(pairs, func(a, b int) bool { return pairs[a].score > pairs[b].score })
	for _, pr := range pairs {
		id := im.streams[pr.issued].id
		if used[id] || final[pr.out] != 0 {
			continue
		}
		final[pr.out], used[id] = id, true
		im.alias[natural[pr.out]] = id
	}
	for i := range out {
		if final[i] == 0 {
			final[i] = natural[i]
		}
		if final[i] != natural[i] && src[i] >= 0 && src[i] < len(info.Streams) {
			im.remap[final[i]] = remapTarget{index: src[i], info: info.Streams[src[i]]}
		} else {
			delete(im.remap, final[i])
		}
		out[i].ID = final[i]
		rec := issuedStream{id: final[i], typ: out[i].StreamType, lang: out[i].LanguageCode, codec: out[i].Codec,
			channels: out[i].Channels, selected: streamSelected(out[i]), external: isExternalStream(out[i])}
		if j, ok := issued[final[i]]; ok {
			im.streams[j] = rec
		} else if final[i] != 0 {
			issued[final[i]] = len(im.streams)
			im.streams = append(im.streams, rec)
		}
	}
	if len(im.streams) > maxIssuedStreams {
		keep := make(map[int64]bool, n)
		for _, id := range final {
			keep[id] = true
		}
		trimmed := im.streams[:0]
		drop := len(im.streams) - maxIssuedStreams
		for _, is := range im.streams {
			if drop > 0 && !keep[is.id] {
				drop--
				delete(im.remap, is.id)
				for nat, a := range im.alias {
					if a == is.id {
						delete(im.alias, nat)
					}
				}
				continue
			}
			trimmed = append(trimmed, is)
		}
		im.streams = trimmed
	}
	im.rendered, im.subtitle = true, nil
	for i := range out {
		if out[i].StreamType == 3 && streamSelected(out[i]) && src[i] >= 0 && src[i] < len(info.Streams) {
			st := info.Streams[src[i]]
			im.subtitle = &st
			break
		}
	}
	e.at = now
}

// pickSubtitle records that this client set the title's subtitle itself
// (PUT /library/parts): it shows that track (nil = None) selected on every
// Media it holds.
func (p *playIdentities) pickSubtitle(k playIdentityKey, st *frontend.StreamInfo, now time.Time) {
	if p == nil || !k.valid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(k, now)
	if e == nil {
		return
	}
	var picked *frontend.StreamInfo
	if st != nil {
		cp := *st
		picked = &cp
	}
	e.mediaLocked(int64(k.key))
	for _, im := range e.media {
		im.rendered, im.subtitle = true, picked
	}
	e.at = now
}

// shownSubtitles returns the subtitle each Media's latest stream list for k
// marked selected. rendered is false when this client holds no current
// stream list for the title.
func (p *playIdentities) shownSubtitles(k playIdentityKey, now time.Time) (subs []frontend.StreamInfo, rendered bool) {
	if p == nil || !k.valid() {
		return nil, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.m[k]
	if e == nil || (!e.frozen && e.admitting == 0 && now.Sub(e.at) > playIdentityTTL) {
		return nil, false
	}
	for _, im := range e.media {
		if !im.rendered {
			continue
		}
		rendered = true
		if im.subtitle != nil {
			subs = append(subs, *im.subtitle)
		}
	}
	return subs, rendered
}

// resolve returns the track an issued id now names for this client, when
// a probe or failover moved it. The selected Media's mapping wins.
func (p *playIdentities) resolve(k playIdentityKey, wireID int64) (remapTarget, bool) {
	if p == nil || !k.valid() || wireID == 0 {
		return remapTarget{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.m[k]
	if e == nil {
		return remapTarget{}, false
	}
	if im := e.media[e.selected]; im != nil {
		if t, ok := im.remap[wireID]; ok {
			return t, true
		}
	}
	if im := e.media[int64(k.key)]; im != nil {
		if t, ok := im.remap[wireID]; ok {
			return t, true
		}
	}
	ids := make([]int64, 0, len(e.media))
	for id := range e.media {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	for _, id := range ids {
		if t, ok := e.media[id].remap[wireID]; ok {
			return t, true
		}
	}
	return remapTarget{}, false
}

// expire drops idle identities. A frozen one stays while alive reports its
// session; alive is called without the table lock held.
func (p *playIdentities) expire(now time.Time, alive func(playIdentityKey) bool) {
	if p == nil {
		return
	}
	var frozen []playIdentityKey
	p.mu.Lock()
	for k, e := range p.m {
		if now.Sub(e.at) <= playIdentityTTL || e.admitting > 0 {
			continue
		}
		if e.frozen {
			frozen = append(frozen, k)
			continue
		}
		delete(p.m, k)
	}
	p.mu.Unlock()
	if len(frozen) == 0 {
		return
	}
	var gone []playIdentityKey
	for _, k := range frozen {
		if alive == nil || !alive(k) {
			gone = append(gone, k)
		}
	}
	p.mu.Lock()
	for _, k := range gone {
		if e := p.m[k]; e != nil && e.admitting == 0 && now.Sub(e.at) > playIdentityTTL {
			delete(p.m, k)
		}
	}
	p.mu.Unlock()
}

func (s *Server) playIdentityAlive(k playIdentityKey) bool {
	if s.playback() == nil {
		return false
	}
	_, ok := s.playback().SessionFor(k.userID, k.clientID, k.key)
	return ok
}

func cloneVariants(vs []frontend.ReleaseVariant) []frontend.ReleaseVariant {
	if len(vs) < 2 {
		return nil
	}
	out := make([]frontend.ReleaseVariant, len(vs))
	copy(out, vs)
	for i := range out {
		out[i].Info.Streams = append([]frontend.StreamInfo(nil), vs[i].Info.Streams...)
	}
	return out
}

func streamSelected(st model.Stream) bool { return st.Selected != nil && *st.Selected }

func isExternalStream(st model.Stream) bool { return st.External != nil && *st.External }

// installVersionFlow adopts an authoritative flow snapshot without changing
// issued stream aliases. Pending pre-play expires promptly; live sessions keep
// the existing admission lifetime.
func (p *playIdentities) installVersionFlow(k playIdentityKey, vs []frontend.ReleaseVariant, until, now time.Time) {
	if !k.valid() {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entryLocked(k, now)
	if e == nil {
		return
	}
	// An empty or shorter snapshot never replaces a list this node already
	// issued: the shared flow row can be absent or expired (90s pre-play
	// TTL) while the client still holds the issued Media/Part ids, and
	// shrinking the list strands its mediaIndex and part lookups (#389).
	// The snapshot still freezes the flow either way.
	if len(vs) > 0 && !(len(e.variants) >= 2 && len(vs) < len(e.variants)) {
		e.variants = cloneVariants(vs)
		e.resolved = true
	}
	e.frozen = true
	e.flowUntil = until
	e.at = now
}
