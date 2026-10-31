package diagnostics

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrNotFound     = errors.New("diagnostics: not found")
	ErrStaleDevice  = errors.New("diagnostics: stale device")
	ErrTooMany      = errors.New("diagnostics: too many captures")
	ErrNotRecording = errors.New("diagnostics: not recording")
)

type Recorder struct {
	mu       sync.Mutex
	deps     Deps
	ring     []Event
	head     int
	n        int
	coalesce map[string]coalSlot
	devices  map[string]*device
	devOrder []string
	sessions map[string]string
	sesOrder []string
	captures map[string]*capture
	// testArchiveLimit, when set, replaces the archive byte caps. Production
	// leaves it zero. Tests use it to force a budget drop.
	testArchiveLimit int
}

type coalSlot struct {
	idx   int
	first time.Time
	last  time.Time
}

type device struct {
	info     DeviceInfo
	lastFail Kind
}

type capture struct {
	id             string
	owner          [32]byte
	deviceKey      string
	confirmed      bool
	category       string
	note           string
	titles         bool
	users          bool
	addrs          bool
	started        time.Time
	deadline       time.Time
	finished       time.Time
	state          string
	discover       []DeviceInfo
	gaps           []Gap
	uncorrelated   int
	zip            []byte
	gen            uint64
	building       bool
	deviceSnap     DeviceInfo
	ev             []Event
	evHead         int
	evN            int
	coal           map[string]coalSlot
	dropped        int
	droppedN       int
	boundaryN      int
	frozen         bool
	sealedFiles    []string
	sealedIdentity *IdentityPreview
}

// New records observations in memory. A nil clock uses time.Now.
func New(deps Deps) *Recorder {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Recorder{
		deps:     deps,
		ring:     make([]Event, maxEvents),
		coalesce: map[string]coalSlot{},
		devices:  map[string]*device{},
		sessions: map[string]string{},
		captures: map[string]*capture{},
	}
}

func (r *Recorder) now() time.Time { return r.deps.Now().UTC() }

func (r *Recorder) secretUses() []secretUse {
	if r.deps.Secrets == nil {
		return nil
	}
	uses, _ := prepareSecrets(r.deps.Secrets())
	return uses
}

// Observe retains one bounded event. Ordinary device sightings update the
// device table and do not take a ring slot.
func (r *Recorder) Observe(e Event) {
	e = boundEvent(e, r.secretUses())
	if e.Time.IsZero() {
		e.Time = r.now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneDevicesLocked(r.now())
	if e.DeviceKey != "" {
		r.touchDeviceLocked(e)
	}
	if e.SessionKey != "" && e.DeviceKey != "" {
		r.rememberSessionLocked(e.SessionKey, e.DeviceKey)
	}
	if (e.Kind == KindEncoderStarted || e.Kind == KindEncoderFailed) && e.DeviceKey == "" && e.SessionKey != "" {
		if dev, ok := r.sessions[e.SessionKey]; ok {
			e.DeviceKey = dev
			e.Uncorrelated = false
		}
	}
	r.noteDiscoveryLocked(e)
	if e.Kind == KindDevice {
		return
	}
	r.fanoutLocked(e)
	if r.coalesceLocked(&e) {
		return
	}
	r.appendLocked(e)
}

func (r *Recorder) touchDeviceLocked(e Event) {
	d := r.devices[e.DeviceKey]
	if d == nil {
		if len(r.devices) >= maxDevices && len(r.devOrder) > 0 {
			old := r.devOrder[0]
			r.devOrder = r.devOrder[1:]
			delete(r.devices, old)
		}
		d = &device{info: DeviceInfo{ID: devicePublicID(e.DeviceKey)}}
		r.devices[e.DeviceKey] = d
		r.devOrder = append(r.devOrder, e.DeviceKey)
	}
	if e.DeviceName != "" {
		d.info.Name = e.DeviceName
	}
	if e.Model != "" {
		d.info.Model = e.Model
	}
	if e.Product != "" {
		d.info.Product = e.Product
	}
	if e.AppVersion != "" {
		d.info.Version = e.AppVersion
	}
	if e.Platform != "" {
		d.info.Platform = e.Platform
	}
	if e.Time.After(d.info.LastSeen) {
		d.info.LastSeen = e.Time
	}
	switch e.Kind {
	case KindEncoderFailed, KindSourceRejected:
		d.lastFail = e.Kind
		d.info.SuggestedKind = string(e.Kind)
	case KindMediaRequest:
		if e.Status >= 400 {
			d.lastFail = e.Kind
			d.info.SuggestedKind = string(e.Kind)
		}
	}
}

func (r *Recorder) rememberSessionLocked(session, device string) {
	if _, ok := r.sessions[session]; ok {
		r.sessions[session] = device
		return
	}
	if len(r.sessions) >= maxSessions && len(r.sesOrder) > 0 {
		old := r.sesOrder[0]
		r.sesOrder = r.sesOrder[1:]
		delete(r.sessions, old)
	}
	r.sessions[session] = device
	r.sesOrder = append(r.sesOrder, session)
}

func (r *Recorder) noteDiscoveryLocked(e Event) {
	if e.DeviceKey == "" {
		return
	}
	info := r.devices[e.DeviceKey].info
	for _, c := range r.captures {
		if c.state != "recording" || c.confirmed || e.Time.Before(c.started) {
			continue
		}
		found := false
		for i := range c.discover {
			if c.discover[i].ID == info.ID {
				c.discover[i] = info
				found = true
				break
			}
		}
		if !found && len(c.discover) < maxDiscovery {
			c.discover = append(c.discover, info)
		}
	}
}

func (r *Recorder) coalesceLocked(e *Event) bool {
	key := coalesceKey(*e)
	if key == "" || r.n == 0 {
		return false
	}
	slot, ok := r.coalesce[key]
	if !ok {
		return false
	}
	cur := r.ring[slot.idx]
	if cur.Kind != e.Kind || cur.DeviceKey != e.DeviceKey || coarseRoute(cur.Route) != coarseRoute(e.Route) || cur.Status != e.Status {
		delete(r.coalesce, key)
		return false
	}
	// The span is measured from the first arrival. Comparing with the
	// previous arrival would let a 1s poll merge for the whole session
	// and hide every sample behind that first timestamp.
	if slot.first.IsZero() || e.Time.Sub(slot.first) > coalesceWindow {
		delete(r.coalesce, key)
		return false
	}
	cur.Repeat++
	cur.Bytes += e.Bytes
	if e.Time.After(cur.Until) {
		cur.Until = e.Time
	}
	r.ring[slot.idx] = cur
	slot.last = e.Time
	r.coalesce[key] = slot
	return true
}

func (r *Recorder) appendLocked(e Event) {
	if e.Until.IsZero() {
		e.Until = e.Time
	}
	if r.n == maxEvents {
		for k, slot := range r.coalesce {
			if slot.idx == r.head {
				delete(r.coalesce, k)
			}
		}
	}
	r.ring[r.head] = e
	if key := coalesceKey(e); key != "" {
		r.coalesce[key] = coalSlot{idx: r.head, first: e.Time, last: e.Until}
	}
	r.head = (r.head + 1) % maxEvents
	if r.n < maxEvents {
		r.n++
	}
}

func (r *Recorder) eachLocked(fn func(Event)) {
	if r.n == 0 {
		return
	}
	start := r.head - r.n
	if start < 0 {
		start += maxEvents
	}
	for i := 0; i < r.n; i++ {
		fn(r.ring[(start+i)%maxEvents])
	}
}

// Devices returns device cards seen within deviceRecent. Ids are opaque.
func (r *Recorder) Devices() []DeviceInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneDevicesLocked(r.now())
	out := make([]DeviceInfo, 0, len(r.devOrder))
	for _, k := range r.devOrder {
		if d := r.devices[k]; d != nil {
			out = append(out, d.info)
		}
	}
	return out
}

func (r *Recorder) Start(token string, in StartIn) (CaptureView, error) {
	if token == "" {
		return CaptureView{}, ErrNotFound
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked()
	active := 0
	for _, c := range r.captures {
		if c.state == "recording" || c.state == "finished" {
			active++
		}
	}
	if active >= maxCaptures {
		return CaptureView{}, ErrTooMany
	}
	var deviceKey string
	confirmed := false
	if in.DeviceID != "" {
		key, ok := r.deviceKeyLocked(in.DeviceID)
		if !ok {
			return CaptureView{}, ErrStaleDevice
		}
		deviceKey = key
		confirmed = true
	}
	id, err := newID()
	if err != nil {
		return CaptureView{}, err
	}
	now := r.now()
	c := &capture{
		id: id, owner: ownerHash(token), deviceKey: deviceKey, confirmed: confirmed,
		category: boundCategory(in.Category),
		note:     boundNote(in.Note, r.secretUses()),
		titles:   in.IncludeTitles, users: in.IncludeUsernames, addrs: in.IncludeAddresses,
		started: now, deadline: now.Add(maxDuration), state: "recording",
	}
	r.captures[id] = c
	if confirmed {
		r.rememberDeviceSnap(c)
		r.seedLocked(c, now)
	}
	return r.viewLocked(c), nil
}

// SelectDevice confirms a discovered or recent device. A stale id leaves the
// capture unconfirmed.
func (r *Recorder) SelectDevice(token, id, deviceID string) (CaptureView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.ownedLocked(token, id)
	if err != nil {
		return CaptureView{}, err
	}
	if c.state != "recording" {
		return CaptureView{}, ErrNotRecording
	}
	if c.confirmed {
		return CaptureView{}, ErrNotRecording
	}
	key, ok := r.deviceKeyLocked(deviceID)
	if !ok {
		return CaptureView{}, ErrStaleDevice
	}
	c.deviceKey = key
	c.confirmed = true
	c.discover = nil
	r.rememberDeviceSnap(c)
	r.seedLocked(c, r.now())
	return r.viewLocked(c), nil
}

func (r *Recorder) Get(token, id string) (CaptureView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.ownedLocked(token, id)
	if err != nil {
		return CaptureView{}, err
	}
	return r.viewLocked(c), nil
}

func (r *Recorder) Discard(token, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, err := r.ownedLocked(token, id)
	if err != nil {
		return err
	}
	delete(r.captures, c.id)
	return nil
}

// Expire drops finished captures past their retention and finishes recordings
// past the deadline so a disconnected browser cannot leave one open.
func (r *Recorder) Expire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expireLocked()
}

func (r *Recorder) expireLocked() {
	now := r.now()
	r.pruneDevicesLocked(now)
	for id, c := range r.captures {
		if c.state == "recording" && !now.Before(c.deadline) {
			r.finishLocked(c, c.deadline)
		}
		if c.state != "finished" {
			continue
		}
		end := c.finished
		if end.IsZero() {
			end = c.deadline
		}
		// One pass finishes a recording that outlived its deadline and
		// deletes it when retention has also elapsed, so it does not keep
		// a maxCaptures slot until a later call.
		if !now.Before(end.Add(retainFor)) {
			delete(r.captures, id)
		}
	}
}

func (r *Recorder) finishLocked(c *capture, at time.Time) {
	if c.state != "recording" || c.frozen {
		return
	}
	if at.After(c.deadline) {
		at = c.deadline
	}
	c.state = "finished"
	c.finished = at
	c.frozen = true
	c.coal = nil
	r.rememberDeviceSnap(c)
}

func (r *Recorder) ownedLocked(token, id string) (*capture, error) {
	r.expireLocked()
	c := r.captures[id]
	sum := ownerHash(token)
	if c == nil || subtle.ConstantTimeCompare(c.owner[:], sum[:]) != 1 {
		return nil, ErrNotFound
	}
	return c, nil
}

func (r *Recorder) deviceKeyLocked(id string) (string, bool) {
	now := r.now()
	for k, d := range r.devices {
		if d.info.ID == id && d.info.LastSeen.Add(deviceRecent).After(now) {
			return k, true
		}
	}
	return "", false
}

func (r *Recorder) pruneDevicesLocked(now time.Time) {
	for i := 0; i < len(r.devOrder); {
		k := r.devOrder[i]
		d := r.devices[k]
		if d == nil || !d.info.LastSeen.Add(deviceRecent).After(now) {
			delete(r.devices, k)
			r.devOrder = append(r.devOrder[:i], r.devOrder[i+1:]...)
			continue
		}
		i++
	}
}

func (r *Recorder) rememberDeviceSnap(c *capture) {
	if c == nil || c.deviceKey == "" {
		return
	}
	if d := r.devices[c.deviceKey]; d != nil {
		c.deviceSnap = d.info
	}
}

func (r *Recorder) viewLocked(c *capture) CaptureView {
	end := r.now()
	if c.state == "finished" && !c.finished.IsZero() {
		end = c.finished
	}
	events := c.evidenceCopy(c.started.Add(-preRoll), end)
	v := CaptureView{
		ID: c.id, State: c.state, DeviceConfirmed: c.confirmed,
		StartedAt: c.started, Deadline: c.deadline, Category: c.category, Note: c.note,
		IncludeTitles: c.titles, IncludeUsernames: c.users, IncludeAddresses: c.addrs,
		Observations: observations(events), Hypotheses: hypotheses(events, c.confirmed),
		Coverage: r.coverageLocked(c),
	}
	if c.state == "finished" {
		fin := c.finished
		v.FinishedAt = &fin
	}
	if c.confirmed {
		if !c.frozen {
			r.rememberDeviceSnap(c)
		}
		if c.deviceSnap.ID != "" {
			info := c.deviceSnap
			v.Device = &info
		}
	} else {
		v.Discovery = append([]DeviceInfo(nil), c.discover...)
	}
	ident := identityPreview(events, c.titles, c.users, c.addrs)
	var files []string
	if c.sealedIdentity != nil {
		ident = *c.sealedIdentity
		files = append([]string(nil), c.sealedFiles...)
	}
	v.Preview = &Preview{
		Files:     files,
		Identity:  ident,
		Note:      c.note,
		Redaction: "Known credential values are removed. Free-text log lines are omitted. This is not a guarantee that every unrecognized secret is gone.",
	}
	return v
}

func (r *Recorder) coverageLocked(c *capture) []Gap {
	gaps := []Gap{
		{Collector: "client", Status: "unavailable", Detail: "External client internals are not visible from this server."},
		{Collector: "compose", Status: "unavailable", Detail: "VPS host Compose mappings are not visible from this process."},
		{Collector: "dns", Status: "unavailable", Detail: "Remote DNS vantage is not observed."},
		{Collector: "cluster", Status: "unavailable", Detail: "Other cluster nodes' events are not included."},
	}
	if !c.confirmed {
		gaps = append(gaps, Gap{Collector: "device", Status: "unconfirmed", Detail: "No device is confirmed. Playback from other devices is not retained."})
	}
	if c.uncorrelated > 0 {
		gaps = append(gaps, Gap{Collector: "encoder", Status: "omitted", Detail: "Encoder events without a device or a known session were omitted."})
	}
	if c.dropped > 0 {
		gaps = append(gaps, Gap{Collector: "events", Status: "truncated", Detail: fmt.Sprintf("Dropped %d retained events (%d samples) because the capture buffer was full.", c.dropped, c.droppedN)})
	}
	if c.boundaryN > 0 {
		gaps = append(gaps, Gap{Collector: "events", Status: "omitted", Detail: fmt.Sprintf("Left out %d samples because their coalesced span crossed the capture window.", c.boundaryN)})
	}
	return append(gaps, c.gaps...)
}

func (r *Recorder) fanoutLocked(e Event) {
	for _, c := range r.captures {
		if c.state != "recording" || c.frozen || !c.confirmed || c.deviceKey == "" {
			continue
		}
		r.acceptLocked(c, e)
	}
}

func (r *Recorder) acceptLocked(c *capture, e Event) {
	from := c.started.Add(-preRoll)
	in := !e.Time.Before(from) && e.Time.Before(c.deadline)
	if r.subjectLocked(c, e) {
		if e.Time.Before(from) {
			return
		}
		if !e.Time.Before(c.deadline) {
			if c.extendsOpen(e) {
				c.boundaryN++
			}
			return
		}
		if c.coalesceEv(e) {
			return
		}
		c.appendEv(e)
		return
	}
	if in && e.DeviceKey == "" && (e.Kind == KindEncoderStarted || e.Kind == KindEncoderFailed) {
		c.uncorrelated++
	}
}

func (r *Recorder) subjectLocked(c *capture, e Event) bool {
	if c.deviceKey == "" {
		return false
	}
	if e.DeviceKey == c.deviceKey {
		return true
	}
	if e.Kind != KindEncoderStarted && e.Kind != KindEncoderFailed {
		return false
	}
	if e.DeviceKey != "" || e.Uncorrelated || e.SessionKey == "" {
		return false
	}
	return r.sessions[e.SessionKey] == c.deviceKey
}

func (r *Recorder) seedLocked(c *capture, end time.Time) {
	if !c.confirmed || c.deviceKey == "" {
		return
	}
	from := c.started.Add(-preRoll)
	if end.After(c.deadline) {
		end = c.deadline
	}
	r.eachLocked(func(e Event) {
		until := e.Until
		if until.IsZero() {
			until = e.Time
		}
		if until.Before(from) || e.Time.After(end) {
			return
		}
		if e.Time.Before(from) || until.After(end) {
			if r.subjectLocked(c, e) || (e.DeviceKey == "" && (e.Kind == KindEncoderStarted || e.Kind == KindEncoderFailed)) {
				c.boundaryN += e.Repeat + 1
			}
			return
		}
		if r.subjectLocked(c, e) {
			c.appendEv(e)
			return
		}
		if e.DeviceKey == "" && (e.Kind == KindEncoderStarted || e.Kind == KindEncoderFailed) {
			c.uncorrelated++
		}
	})
}

func (c *capture) extendsOpen(e Event) bool {
	key := coalesceKey(e)
	if key == "" || c.coal == nil {
		return false
	}
	slot, ok := c.coal[key]
	if !ok || slot.first.IsZero() || e.Time.Sub(slot.first) > coalesceWindow {
		return false
	}
	cur := c.ev[slot.idx]
	return cur.Kind == e.Kind && cur.DeviceKey == e.DeviceKey && coarseRoute(cur.Route) == coarseRoute(e.Route) && cur.Status == e.Status
}

func (c *capture) coalesceEv(e Event) bool {
	key := coalesceKey(e)
	if key == "" || c.evN == 0 || c.coal == nil {
		return false
	}
	slot, ok := c.coal[key]
	if !ok {
		return false
	}
	cur := c.ev[slot.idx]
	if cur.Kind != e.Kind || cur.DeviceKey != e.DeviceKey || coarseRoute(cur.Route) != coarseRoute(e.Route) || cur.Status != e.Status {
		delete(c.coal, key)
		return false
	}
	if slot.first.IsZero() || e.Time.Sub(slot.first) > coalesceWindow {
		delete(c.coal, key)
		return false
	}
	cur.Repeat++
	cur.Bytes += e.Bytes
	if e.Time.After(cur.Until) {
		cur.Until = e.Time
	}
	c.ev[slot.idx] = cur
	slot.last = e.Time
	c.coal[key] = slot
	return true
}

func (c *capture) appendEv(e Event) {
	e = cloneEvent(e)
	if e.Until.IsZero() {
		e.Until = e.Time
	}
	if c.ev == nil {
		c.ev = make([]Event, maxCaptureEvents)
		c.coal = map[string]coalSlot{}
	}
	if c.evN == maxCaptureEvents {
		old := c.ev[c.evHead]
		c.dropped++
		c.droppedN += old.Repeat + 1
		for k, slot := range c.coal {
			if slot.idx == c.evHead {
				delete(c.coal, k)
			}
		}
	}
	c.ev[c.evHead] = e
	if key := coalesceKey(e); key != "" {
		c.coal[key] = coalSlot{idx: c.evHead, first: e.Time, last: e.Until}
	}
	c.evHead = (c.evHead + 1) % maxCaptureEvents
	if c.evN < maxCaptureEvents {
		c.evN++
	}
}

func (c *capture) eachEv(fn func(Event)) {
	if c == nil || c.evN == 0 {
		return
	}
	start := c.evHead - c.evN
	if start < 0 {
		start += len(c.ev)
	}
	for i := 0; i < c.evN; i++ {
		fn(c.ev[(start+i)%len(c.ev)])
	}
}

func (c *capture) evidenceCopy(from, end time.Time) []Event {
	if c == nil || !c.confirmed {
		return nil
	}
	var out []Event
	c.eachEv(func(e Event) {
		until := e.Until
		if until.IsZero() {
			until = e.Time
		}
		if e.Time.Before(from) || e.Time.After(end) || until.After(end) {
			return
		}
		out = append(out, e)
	})
	return out
}

func cloneEvent(e Event) Event {
	if len(e.Candidates) > 0 {
		e.Candidates = append([]Candidate(nil), e.Candidates...)
	}
	if e.Media != nil {
		m := *e.Media
		e.Media = &m
	}
	return e
}

func devicePublicID(key string) string {
	sum := sha256.Sum256([]byte("ether-device:" + key))
	return hex.EncodeToString(sum[:16])
}

func ownerHash(token string) [32]byte {
	return sha256.Sum256([]byte("ether-support-owner:" + token))
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func observations(events []Event) []Observation {
	out := make([]Observation, 0, len(events))
	for _, e := range events {
		out = append(out, Observation{
			At: e.Time, Kind: e.Kind, Count: e.Repeat + 1, Status: e.Status,
			Route: e.Route, Bytes: e.Bytes,
		})
	}
	return out
}

func hypotheses(events []Event, confirmed bool) []Hypothesis {
	var hasPlay, hasPoll, hasEncoderFail, hasBytes, hasEncoder, hasStream bool
	for _, e := range events {
		switch e.Kind {
		case KindPlaybackRequested:
			hasPlay = true
		case KindPoll:
			hasPoll = true
		case KindEncoderFailed:
			hasEncoderFail = true
		case KindMediaBytes:
			if e.Bytes > 0 {
				hasBytes = true
			}
		case KindEncoderStarted:
			hasEncoder = true
		case KindStreamRequested:
			hasStream = true
		}
	}
	var out []Hypothesis
	if !confirmed {
		out = append(out, Hypothesis{Code: "device_unconfirmed", Text: "No device is confirmed. Choose one before playback evidence is included."})
	}
	if hasPoll && !hasPlay {
		out = append(out, Hypothesis{Code: "companion_poll_only", Text: "No playback request was observed in this capture. Poll responses, including HTTP 404, are not a playback failure."})
	}
	if hasEncoderFail {
		out = append(out, Hypothesis{Code: "encoder_failed", Text: "The encoder reported a startup failure."})
	}
	if hasStream && !hasEncoder && !hasBytes {
		out = append(out, Hypothesis{Code: "playlist_without_delivery", Text: "A playlist was requested. No encoder start and no media bytes were observed, so this is not recorded as playback success."})
	}
	return out
}

func identityPreview(events []Event, titles, users, addrs bool) IdentityPreview {
	p := IdentityPreview{
		UsernamesIncluded: users,
		AddressesIncluded: addrs,
		TitlesIncluded:    titles,
	}
	if users {
		p.Usernames = uniqueField(events, func(e Event) string { return e.Username })
	}
	if addrs {
		p.Addresses = uniqueField(events, func(e Event) string { return e.Peer })
	}
	if titles {
		p.Titles = mergeStrings(uniqueField(events, func(e Event) string { return e.Title }), uniqueField(events, func(e Event) string { return e.SourceName }))
	}
	switch {
	case users && addrs:
		p.Explanation = "The ZIP will include the usernames and addresses listed here."
	case users:
		p.Explanation = "The ZIP will include the usernames listed here and will not include addresses."
	case addrs:
		p.Explanation = "The ZIP will include the addresses listed here and will not include usernames."
	default:
		p.Explanation = "Usernames are not in the ZIP. Addresses are not in the ZIP."
	}
	if !titles {
		p.Explanation += " Titles and source names are not in the ZIP."
	} else {
		p.Explanation += " Titles and source names are in the ZIP."
	}
	return p
}

func mergeStrings(parts ...[]string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, part := range parts {
		for _, v := range part {
			if v == "" {
				continue
			}
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

func uniqueField(events []Event, get func(Event) string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, e := range events {
		v := get(e)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
