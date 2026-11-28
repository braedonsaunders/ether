package livetv

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// An upstream tap is the one provider connection behind a live input.
//
// A tune reads its input several times: the sniff that tells HLS from a
// transport stream, ffprobe's codec analysis, then ffmpeg for as long as
// the viewer watches. Each read used to be its own provider connection, and
// so was every other viewer of the same channel. IPTV accounts are sold by
// connection (one, two, five) and strict panels refuse the overlap or ban
// the account for it. Every relay read of a media input now joins a tap
// keyed by the input URL and provider headers: one upstream GET whose bytes
// are kept in a ring and fanned out to every reader.
//
// A reader that joins while the tap is young (the start of the stream is
// still in the ring) reads it from the first byte, so ffmpeg receives
// exactly what the probe saw. A later reader joins at a keyframe: near the
// live edge for a relay, at the reserve depth for Steady, and where its
// grant's previous reader stopped for a restarted decoder. A reader that
// falls a whole ring behind skips forward instead of stalling everyone else.
//
// A raw transport stream is the stability reserve (reserve_ts.go): the ring
// holds packet-aligned bytes with a media clock beside them, sized from the
// measured bitrate and the preset. When the provider stalls on an open
// connection, the tap keeps the connection open and, if the source's
// connection limit has room, races a parallel replacement connection under
// the same provider-safety rules (pacing, refusal ceiling, unified account
// limit), splicing whichever delivers first onto one continuous timeline.
// Readers keep playing their reserve meanwhile and never see the reconnect.
//
// A tap lives as long as a grant (a tune or a probe) references it, and
// closes synchronously when the last one is revoked, so a viewer changing
// channels on a one-connection account never overlaps the old and the new
// stream. It also closes after tapIdle without any reader, so an abandoned
// grant cannot hold the provider connection.
//
// Manifests (HLS, DASH) are not tapped: their reads are short requests.

var (
	// tapRing is the base per-connection buffer: about 30 s at 4K bitrates,
	// a minute or more at HD.
	tapRing = 24 << 20
	// tapRingMax bounds one tap's ring: Rock solid's 30 s plus margin at
	// up to ~28 Mb/s, above any 4K IPTV channel seen in practice.
	tapRingMax = 128 << 20
	// tapRingBudget bounds every tap together: four Rock-solid 4K channels,
	// or a dozen HD ones, at once. Beyond it rings stay at their base size
	// and the reserve target shrinks to what fits.
	tapRingBudget int64 = 512 << 20
	// tapJoinBacklog is how far behind the live edge a late reader starts
	// when the stream has no usable index, enough for ffmpeg to find a
	// keyframe without waiting for one.
	tapJoinBacklog = 2 << 20
	tapIdle        = 5 * time.Second
	// tapStallAfter is the floor data age that counts as a stalled provider:
	// continuous raw TS arrives without silent gaps, so two seconds of silence
	// is not jitter. Bursty providers (HLS segments restreamed as TS) raise a
	// tap's threshold toward tapStallMax as healthy inter-burst gaps are seen.
	tapStallAfter = 2 * time.Second
	// tapStallMax caps a tap's learned stall threshold and burst runway so a
	// source that goes silent for good still fails over quickly.
	tapStallMax = 8 * time.Second
	// tapQuietCutAfter bounds how long a quiet open connection is held when
	// the account's connection limit prevents racing a parallel replacement:
	// transient pauses resume without a teardown, while dead half-open sockets
	// still reconnect inside the 10 s picture hold.
	tapQuietCutAfter = 6 * time.Second
	// tapYoung is how much media a tap may hold and still serve new readers
	// from its first byte (the probe-then-ffmpeg sequence of a tune).
	tapYoung = 20 * time.Second
	// tapPaceLead is how far ahead of real time a paced reader may run:
	// enough for Steady to encode its first segments at once, so a reserve
	// never slows a tune.
	tapPaceLead = 4 * time.Second
	// tapPaceStarved is how long a paced reader must wait at the live edge
	// before its schedule re-anchors.
	tapPaceStarved = time.Second
	// tapReconnectTimeout bounds one reconnect's headers and first bytes.
	tapReconnectTimeout = 4 * time.Second
	// tapReconnectDelays space same-source reconnects after consecutive
	// failures: immediately, then 0.5 s, 1.5 s, then up to 4 s.
	tapReconnectDelays = []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond, 3 * time.Second, 4 * time.Second}
)

const (
	// tapFailAfter consecutive failed reconnects raise InputFailed so the
	// owner of the input can fail over while the reserve still plays.
	tapFailAfter = 3
	// tapGiveUpAfter consecutive failed reconnects end the tap, the same
	// ceiling the live restart path uses.
	tapGiveUpAfter = 7
	// A reconnect that delivered tapHealthy of media replenishes the
	// allowance; tapBurstLimit reconnects per tapBurstWindow end the tap.
	tapHealthy     = 4 * time.Second
	tapBurstLimit  = 28
	tapBurstWindow = 30 * time.Second
	// tapBurstGapFloor separates inter-burst silences from ordinary packet
	// spacing inside a burst or on a continuous stream.
	tapBurstGapFloor = 250 * time.Millisecond
	// tapBurstGrace covers tsSplicer's one-frame hold and small segment
	// delivery jitter when a burst's own media covers the following silence.
	tapBurstGrace = 500 * time.Millisecond
	// tapRhythmWindow and tapRhythmHistory bound the sliding window of
	// healthy inter-burst gaps so old gaps age out on continuous delivery.
	tapRhythmWindow  = 8 * time.Second
	tapRhythmHistory = 6
	// tapMarkLimit bounds the index (marks are at most 10 a second plus
	// keyframes; this is several minutes).
	tapMarkLimit = 8192
)

// tapRingInUse is the bytes held by every tap's ring.
var tapRingInUse atomic.Int64

// tsPacket is the MPEG-TS packet size; join points stay packet aligned.
const tsPacket = 188

// tapBusyHeader marks a relay refusal because the provider's connections
// are all in use, so the tune can say so.
const tapBusyHeader = "X-Ether-Provider-Busy"

// ErrProviderBusy reports that every connection the provider allows is in
// use by other channels.
var ErrProviderBusy = fmt.Errorf("every connection this IPTV provider allows is in use: %w", core.ErrStreamLimit)

// ErrProviderPaced reports that the source's hourly allowance of new
// provider streams is spent.
var ErrProviderPaced = fmt.Errorf("this IPTV source has started as many streams this hour as its owner allows: %w", core.ErrStreamLimit)

var (
	errTapReconnectLimit = errors.New("live provider reconnect limit reached")
	errTapSilent         = errors.New("live provider answered but sent no media")
	errTapNotTS          = errors.New("live provider reconnect returned something other than the transport stream")
)

type tapRegistry struct {
	external map[string]int
	inputs   map[string]map[string]int
	mu       sync.Mutex
	byKey    map[string]*upstreamTap
	bySource map[string]map[*upstreamTap]struct{}
	pace     map[string]*sourcePace
}

// sourcePace spaces a source's new provider streams.
type sourcePace struct {
	next  time.Time   // earliest start of the next stream
	opens []time.Time // starts in the last hour
}

// reserveOpen books the source's next stream start and returns how long the
// caller waits for it. A spent hourly allowance refuses the stream.
func (r *tapRegistry) reserveOpen(source string, gap time.Duration, perHour int, now time.Time) (time.Duration, error) {
	if source == "" {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pace == nil {
		r.pace = map[string]*sourcePace{}
	}
	p := r.pace[source]
	if p == nil {
		p = &sourcePace{}
		r.pace[source] = p
	}
	kept := p.opens[:0]
	for _, at := range p.opens {
		if now.Sub(at) < time.Hour {
			kept = append(kept, at)
		}
	}
	p.opens = kept
	if perHour > 0 && len(p.opens) >= perHour {
		return 0, ErrProviderPaced
	}
	start := now
	if p.next.After(start) {
		start = p.next
	}
	p.next = start.Add(gap)
	p.opens = append(p.opens, start)
	return start.Sub(now), nil
}

// tapReader is one reader's position, for health and for resuming a
// restarted decoder where its grant's previous reader stopped.
type tapReader struct {
	g          *inputGrant
	start, off int64
	// A paced reader (a real-time consumer's grant) shares its grant's
	// schedule; see tapPace.
	pace *tapPace
}

// tapPace is a real-time grant's release schedule: media up to anchor clock
// plus elapsed play time plus tapPaceLead. Without it the reserve would
// drain into socket buffers the tap cannot see, into Steady's startup
// burst, or into a copy relay's segments. It is per grant, so the readers
// of one tune (probe, then ffmpeg) share one allowance rather than each
// spending a fresh one. It re-anchors whenever a reader catches the live
// edge, so time spent starved in a stall never becomes an allowance that
// would swallow the reconnect's burst instead of keeping it as reserve.
type tapPace struct {
	anchored bool
	clock    int64
	wall     time.Time
	rate     float64 // playout rate relative to real time (0 means 1.0)
}

func (p *tapPace) effectiveRate() float64 {
	if p.rate <= 0 {
		return 1.0
	}
	return min(p.rate, 1.0)
}

func (p *tapPace) dueClock(now time.Time) int64 {
	elapsed := now.Sub(p.wall)
	if elapsed <= 0 {
		return p.clock
	}
	scaledMS := float64(elapsed) * p.effectiveRate() / float64(time.Millisecond)
	return p.clock + int64(math.Round(scaledMS*float64(tsClockHz)/1000.0))
}

func (p *tapPace) setRate(rate float64, now time.Time) {
	if rate <= 0 || rate > 1.0 {
		rate = 1.0
	}
	if p.anchored {
		p.clock = p.dueClock(now)
		p.wall = now
	}
	p.rate = rate
}

// tapResume is where a grant's last reader started and stopped.
type tapResume struct{ start, last int64 }

type upstreamTap struct {
	managed     bool // its grant already holds a provider input slot
	key, source string
	reg         *tapRegistry
	svc         *Service
	rawURL      string

	ready      chan struct{} // closed once the first response is known
	closedDone chan struct{} // closed after the upstream body and registry entry are gone
	opening    chan struct{} // set under mu while opening the first response
	// Set before ready closes and immutable after.
	status     int
	header     http.Header
	base       *url.URL
	isManifest bool
	manifest   []byte // a manifest body, handed to the first reader only
	hls        bool
	ctype      string
	openErr    error

	mu             sync.Mutex
	cond           *sync.Cond
	ring           []byte
	written        int64 // bytes received from upstream
	err            error // set when upstream ends
	closed         bool
	finite         bool
	finitePlayback bool
	refs           map[*inputGrant]struct{}
	readers        int
	idle           *time.Timer
	idleGeneration uint64
	ctx            context.Context
	cancel         context.CancelFunc
	body           io.Closer

	// Parallel replacement race state (TS only, guarded by mu).
	raceActive bool
	raceCancel context.CancelFunc
	raceWinner io.Reader
	raceDone   chan struct{}
	lastRaceAt time.Time

	// Reserve state; ts is nil for an input that is not a transport stream.
	ts        *tsSplicer
	marks     []tsMark
	active    map[*tapReader]struct{}
	resume    map[*inputGrant]tapResume // where each grant's last reader started and stopped
	paces     map[*inputGrant]*tapPace
	lastData  time.Time
	dataAt    atomic.Int64 // lastData for lock-free health (unix nanoseconds)
	published int          // format generation last published
	// Source-history reporting: what was last reported, the stall count and
	// the connect burst of the current connection.
	rep        tapReport
	stallCount int64
	burstDone  bool
	burst      time.Duration
	burstNew   bool
	epochStart time.Time
	epochClock int64 // splicer clock when the current connection started
	stalled    bool
	failed     bool
	failures   int // consecutive failed reconnects
	refusals   int // consecutive refused reconnects (connection limit, auth, rate)
	reconnects int
	attempts   []time.Time // reconnect attempts in the burst window
	bitrate    int64       // bits per second
	ringBytes  int64       // bytes charged to tapRingInUse
	// Adaptive stall detection (TS only, guarded by mu): tracks healthy
	// inter-burst gaps (~1x media progress over a window) and the latest
	// burst's media lead so HLS-as-TS restreamers are not cut between bursts.
	stallAfter     time.Duration
	stallLogged    bool
	recentGaps     []time.Duration
	rhythmAt       time.Time
	rhythmClock    int64
	rhythmGap      time.Duration
	pendingGap     time.Duration
	gapClock       int64
	burstAt        time.Time
	burstClock     int64
	paceAt         time.Time
	paceClock      int64
	haveBurst      bool
	epochReads     int
	epochBytes     int64
	burstProbeUsed bool
}

func (g *inputGrant) rootTapKey(raw string, headers, mediaHeaders http.Header) string {
	key := tapKey(g.source+"\x00"+raw+"\x00"+tapKey("", mediaHeaders), headers)
	if g.login != nil {
		key += "\x00" + g.login.login
	}
	if g.finite {
		key += "\x00" + g.token
	}
	return key
}

func tapKey(raw string, h http.Header) string {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	sum := sha256.New()
	io.WriteString(sum, raw)
	for _, k := range names {
		fmt.Fprintf(sum, "\x00%s\x00%s", k, strings.Join(h[k], "\x01"))
	}
	return hex.EncodeToString(sum.Sum(nil))
}

// acquire joins the tap for key, creating it when none is open. created
// reports that the caller must open it. A capped source refuses a new tap
// once its limit of open taps is reached.
func (r *tapRegistry) acquire(key, source string, limit int, g *inputGrant) (t *upstreamTap, created bool, err error) {
	t, _, created, err = r.acquireOrManifest(key, source, limit, g, nil)
	return t, created, err
}

// acquireOrManifest atomically joins an in-flight tap, returns a cached
// manifest published by a tap that just closed, or registers a new tap for
// key. Re-checking cached under r.mu closes the race where a concurrent
// manifest tap caches its master playlist and closes itself between a second
// reader's pre-acquire cache miss and its call to acquire.
func (r *tapRegistry) acquireOrManifest(key, source string, limit int, g *inputGrant, cached func() *cachedManifest) (t *upstreamTap, cm *cachedManifest, created bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byKey == nil {
		r.byKey, r.bySource = map[string]*upstreamTap{}, map[string]map[*upstreamTap]struct{}{}
	}
	if g.ctx != nil && g.ctx.Err() != nil {
		return nil, nil, false, g.ctx.Err()
	}
	if t = r.byKey[key]; t != nil {
		t.mu.Lock()
		available := !t.closed && len(t.refs) != 0
		if available {
			t.refs[g] = struct{}{}
		}
		t.mu.Unlock()
		if available {
			return t, nil, false, nil
		}
	}
	if cached != nil {
		if cm = cached(); cm != nil {
			return nil, cm, false, nil
		}
	}
	// A retiring tap occupies its key until the old body closes. A
	// cached manifest above can still be served without opening a body.
	if t != nil {
		return nil, nil, false, ErrProviderBusy
	}
	if g.releaseSlot == nil && source != "" && limit > 0 && r.inUseLocked(source) >= limit {
		return nil, nil, false, ErrProviderBusy
	}
	t = &upstreamTap{finite: g.finite, managed: g.releaseSlot != nil, key: key, source: source, reg: r, ready: make(chan struct{}), closedDone: make(chan struct{}), refs: map[*inputGrant]struct{}{g: {}}}
	t.cond = sync.NewCond(&t.mu)
	r.byKey[key] = t
	if source != "" {
		if r.bySource[source] == nil {
			r.bySource[source] = map[*upstreamTap]struct{}{}
		}
		r.bySource[source][t] = struct{}{}
	}
	return t, nil, true, nil
}

// canJoinOrOpen reports whether key already has an open tap that a new grant
// will join without starting a provider stream, or ErrProviderBusy when a
// capped source has no free connection for a new tap.
func (r *tapRegistry) canJoinOrOpen(key, source string, limit int) (joined bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.byKey[key]; t != nil {
		t.mu.Lock()
		closed := t.closed
		t.mu.Unlock()
		if !closed {
			return true, nil
		}
	}
	if source != "" && limit > 0 && r.inUseLocked(source) >= limit {
		return false, ErrProviderBusy
	}
	return false, nil
}

// release drops grant g from every tap; a tap without grants closes now.
func (r *tapRegistry) release(g *inputGrant) {
	r.mu.Lock()
	var done []*upstreamTap
	for _, t := range r.byKey {
		t.mu.Lock()
		if _, ok := t.refs[g]; ok {
			// The session ends: its last evidence goes to history first.
			if ev := t.evidenceLocked(); !ev.empty() {
				g.noteEvidence(ev)
			}
			delete(t.refs, g)
			delete(t.resume, g)
			delete(t.paces, g)
			if len(t.refs) == 0 {
				done = append(done, t)
			}
		}
		t.mu.Unlock()
	}
	r.mu.Unlock()
	for _, t := range done {
		t.close(nil)
		if t.closedDone != nil {
			<-t.closedDone
		}
		t.mu.Lock()
		opening := t.opening
		t.mu.Unlock()
		if opening != nil {
			<-opening
		}
	}
}

// open reports how many taps a source has open (tests and status).
func (r *tapRegistry) open(source string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bySource[source])
}

func (r *tapRegistry) forget(t *upstreamTap) {
	r.mu.Lock()
	if r.byKey[t.key] == t {
		delete(r.byKey, t.key)
	}
	if set := r.bySource[t.source]; set != nil {
		delete(set, t)
		if len(set) == 0 {
			delete(r.bySource, t.source)
		}
	}
	r.mu.Unlock()
}

// close ends the upstream connection and wakes every reader. It runs
// synchronously: when it returns the provider connection is closed. Bytes
// already in the ring stay readable: a reader drains its reserve before it
// sees the error.
func (t *upstreamTap) close(err error) {
	t.closeWithIdleCheck(err, 0)
}

func (t *upstreamTap) closeWithIdleCheck(err error, idleGeneration uint64) {
	t.mu.Lock()
	// Test for readers and claim the close under the same lock as attach.
	if t.closed || (idleGeneration != 0 && (t.readers != 0 || t.idleGeneration != idleGeneration)) {
		t.mu.Unlock()
		return
	}
	t.closed = true
	if t.closedDone != nil {
		defer close(t.closedDone)
	}
	if t.err == nil {
		t.err = err
		if t.err == nil {
			t.err = io.EOF
		}
	}
	body, cancel, idle := t.body, t.cancel, t.idle
	raceCancel, raceDone := t.raceCancel, t.raceDone
	t.body = nil
	t.raceCancel, t.raceDone, t.raceActive, t.raceWinner = nil, nil, false, nil
	t.freeRingLocked()
	t.publishLocked()
	t.cond.Broadcast()
	t.mu.Unlock()
	if raceDone != nil {
		close(raceDone)
	}
	if raceCancel != nil {
		raceCancel()
	}
	if idle != nil {
		idle.Stop()
	}
	if cancel != nil {
		cancel()
	}
	if body != nil {
		body.Close()
	}
	if t.reg != nil {
		t.reg.forget(t)
	}
}

// freeRingLocked returns the ring's bytes to the shared budget. Readers may
// still drain it; the memory itself goes when they let go.
func (t *upstreamTap) freeRingLocked() {
	if t.ringBytes != 0 {
		tapRingInUse.Add(-t.ringBytes)
		t.ringBytes = 0
	}
}

// fill copies a non-TS upstream body into the ring until it ends.
func (t *upstreamTap) fill(body io.Reader) {
	buf := make([]byte, 64<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			t.mu.Lock()
			if t.closed {
				t.mu.Unlock()
				return
			}
			t.lastData = time.Now()
			t.dataAt.Store(t.lastData.UnixNano())
			t.writeLocked(buf[:n])
			t.cond.Broadcast()
			t.mu.Unlock()
		}
		if err != nil {
			t.close(err)
			return
		}
	}
}

func (t *upstreamTap) fillArchive(body io.Reader, idle *time.Timer) {
	buf := make([]byte, 64<<10)
	for {
		t.mu.Lock()
		idle.Stop()
		for {
			if t.closed {
				t.mu.Unlock()
				return
			}
			var consumed int64
			if t.finitePlayback && len(t.active) > 0 {
				consumed = t.written
				for rd := range t.active {
					consumed = min(consumed, rd.off)
				}
			}
			capacity := int64(len(t.ring)) - (t.written - consumed)
			if capacity > 0 {
				t.mu.Unlock()
				idle.Reset(relayIOTimeout)
				n, err := body.Read(buf[:min(len(buf), int(capacity))])
				idle.Stop()
				t.mu.Lock()
				if !t.closed && n > 0 {
					t.writeLocked(buf[:n])
					t.cond.Broadcast()
				}
				t.mu.Unlock()
				if err != nil {
					t.close(err)
					return
				}
				break
			}
			t.cond.Wait()
		}
	}
}

func (t *upstreamTap) allocRingLocked(n int) {
	t.ring = make([]byte, n)
	t.ringBytes = int64(n)
	tapRingInUse.Add(t.ringBytes)
}

func (t *upstreamTap) writeLocked(p []byte) {
	if t.ring == nil {
		t.allocRingLocked(tapRing)
	}
	for len(p) > 0 {
		at := int(t.written % int64(len(t.ring)))
		n := copy(t.ring[at:], p)
		t.written += int64(n)
		p = p[n:]
	}
}

// ringStartLocked is the oldest offset still in the ring.
func (t *upstreamTap) ringStartLocked() int64 {
	return max(0, t.written-int64(len(t.ring)))
}

// joinOffset is where a reader without an index starts: the first byte
// while it is still buffered, otherwise a packet-aligned point
// tapJoinBacklog behind the edge.
func (t *upstreamTap) joinOffsetLocked() int64 {
	if t.written <= int64(len(t.ring)) {
		return 0
	}
	at := t.written - int64(tapJoinBacklog)
	return at - at%tsPacket
}

// joinLocked picks a new reader's start for the relay URL's join query:
// "live" is the live edge; "format" is the first keyframe of the current
// program format (after the latest PMT change), so a decoder reopened for a
// codec change reads the new format's backlog at once.
func (t *upstreamTap) joinLocked(g *inputGrant, join string) int64 {
	if join == "format" && t.ts != nil {
		if off, ok := t.formatEntryLocked(); ok {
			return off
		}
		join = "live"
	}
	live := join == "live"
	if t.ts == nil || len(t.marks) == 0 {
		if live && t.written > 0 {
			at := max(0, t.written-int64(tapJoinBacklog))
			return at - at%tsPacket
		}
		return t.joinOffsetLocked()
	}
	if live {
		return t.keyBeforeLocked(t.written, 0)
	}
	// A Steady decoder restarted on its grant resumes where the previous one
	// stopped. A relay grant's readers (sniff, probe, ffmpeg) instead all
	// start at the first byte while the tap is young, so ffmpeg sees what
	// the probe saw.
	if r, ok := t.resume[g]; ok && g != nil && g.steady {
		return t.resumeLocked(r)
	}
	if t.youngLocked() {
		return 0
	}
	if g != nil {
		if r, ok := t.resume[g]; ok {
			return t.resumeLocked(r)
		}
		if g.reserve > 0 {
			return t.keyAtDepthLocked(g.joinDepth())
		}
	}
	return t.keyBeforeLocked(t.written, 2*time.Second)
}

func (t *upstreamTap) youngLocked() bool {
	if t.written > int64(len(t.ring)) {
		return false
	}
	return time.Duration(t.headClockLocked()-t.marks[0].clock)*time.Second/tsClockHz <= tapYoung
}

func (t *upstreamTap) headClockLocked() int64 {
	if t.ts != nil {
		return t.ts.clock
	}
	return 0
}

// resumeLocked is where a grant's next reader starts. After a short reader
// (a probe, which read the start of the tune and left) it is where that
// reader started, so ffmpeg sees what the probe saw and the reserve the
// probe ran through is not lost. After a long one (a decoder that ran and
// died) it is the keyframe before where it stopped.
func (t *upstreamTap) resumeLocked(r tapResume) int64 {
	if r.start >= t.ringStartLocked() && time.Duration(t.clockAtLocked(r.last)-t.clockAtLocked(r.start))*time.Second/tsClockHz <= tapYoung {
		return r.start
	}
	return t.keyBeforeLocked(r.last, 0)
}

// formatEntryLocked is the entry of the oldest buffered keyframe in the
// current program format.
func (t *upstreamTap) formatEntryLocked() (int64, bool) {
	if len(t.marks) == 0 {
		return 0, false
	}
	start, cur := t.ringStartLocked(), t.marks[len(t.marks)-1].format
	off, ok := int64(0), false
	for i := len(t.marks) - 1; i >= 0; i-- {
		m := t.marks[i]
		if m.entry < start || m.format != cur {
			break
		}
		if m.key {
			off, ok = m.entry, true
		}
	}
	return off, ok
}

// keyBeforeLocked is the entry of the last keyframe at or before off whose
// media time is at least behind the head; the oldest buffered keyframe when
// none is that far back; the legacy backlog join without keyframes.
func (t *upstreamTap) keyBeforeLocked(off int64, behind time.Duration) int64 {
	start := t.ringStartLocked()
	limit := t.headClockLocked() - int64(behind/time.Millisecond)*tsClockHz/1000
	oldest := int64(-1)
	for i := len(t.marks) - 1; i >= 0; i-- {
		m := t.marks[i]
		if m.entry < start {
			break
		}
		if !m.key {
			continue
		}
		oldest = m.entry
		if m.off <= off && m.clock <= limit {
			return m.entry
		}
	}
	if oldest >= 0 {
		return oldest
	}
	at := max(start, t.written-int64(tapJoinBacklog))
	if r := at % tsPacket; r != 0 {
		at += tsPacket - r
	}
	return min(at, t.written)
}

// keyAtDepthLocked starts a reader d of media behind the head, at a keyframe,
// or as far back as the ring holds.
func (t *upstreamTap) keyAtDepthLocked(d time.Duration) int64 {
	return t.keyBeforeLocked(t.written, d)
}

// clockAtLocked is the media clock at off, from the index.
func (t *upstreamTap) clockAtLocked(off int64) int64 {
	i := sort.Search(len(t.marks), func(i int) bool { return t.marks[i].off > off })
	if i == 0 {
		if len(t.marks) == 0 {
			return t.headClockLocked()
		}
		return t.marks[0].clock
	}
	return t.marks[i-1].clock
}

// depthLocked is the media time between off and the live edge.
func (t *upstreamTap) depthLocked(off int64) time.Duration {
	if t.ts == nil || len(t.marks) == 0 || off >= t.written {
		return 0
	}
	d := t.headClockLocked() - t.clockAtLocked(off)
	if d <= 0 {
		return 0
	}
	return time.Duration(d) * time.Second / tsClockHz
}

// read copies bytes at offset into p, waiting for data. It returns the new
// offset. A reader a full ring behind skips forward.
func (t *upstreamTap) read(ctx context.Context, off int64, p []byte) (int, int64, error) {
	return t.readAs(ctx, nil, off, p)
}

func (t *upstreamTap) readAs(ctx context.Context, rd *tapReader, off int64, p []byte) (int, int64, error) {
	stop := context.AfterFunc(ctx, func() { t.mu.Lock(); t.cond.Broadcast(); t.mu.Unlock() })
	defer stop()
	t.mu.Lock()
	defer t.mu.Unlock()
	waited := time.Time{}
	if off >= t.written {
		waited = time.Now()
	}
	for off >= t.written && !t.closed && ctx.Err() == nil {
		t.cond.Wait()
	}
	if !waited.IsZero() && rd != nil && rd.g != nil && rd.g.steady && rd.g.log != nil && time.Since(waited) >= tapPaceStarved {
		rd.g.log.Info("live tap reader starved", "waited", time.Since(waited).Round(time.Millisecond), "paced", rd.pace != nil, "reserveTarget", rd.g.reserve, "stalled", t.stalled, "reconnects", t.reconnects)
	}
	if !waited.IsZero() && rd != nil && rd.pace != nil && time.Since(waited) >= tapPaceStarved {
		// Starved at the live edge (a stall): the schedule restarts here,
		// so the stall's time is not an allowance that swallows the
		// reconnect's burst. Short waits for the next live chunk do not
		// count, or an arriving burst would pull the anchor along with it.
		rd.pace.anchored = false
	}
	if ctx.Err() != nil {
		return 0, off, ctx.Err()
	}
	if off >= t.written {
		return 0, off, t.err
	}
	var g *inputGrant
	if rd != nil {
		g = rd.g
	}
	if t.written-off > int64(len(t.ring)) {
		if t.ts != nil && g != nil && g.reserve > 0 {
			off = t.keyAtDepthLocked(g.joinDepth())
			if rd.pace != nil {
				rd.pace.anchored = false
			}
		} else if t.ts != nil {
			off = t.keyBeforeLocked(t.written, 2*time.Second)
		} else {
			off = t.joinOffsetLocked()
		}
	} else if t.ts != nil && g != nil && g.steady && t.depthLocked(off) > max(60*time.Second, g.joinDepth()+10*time.Second) {
		// Safety valve: if accumulated hold delay ever exceeds 60 s, make one
		// single clean cut at a keyframe rather than dropping frames per tick.
		off = t.keyAtDepthLocked(max(g.joinDepth(), 2*time.Second))
		if rd.pace != nil {
			rd.pace.anchored = false
		}
	}
	end := t.written
	if rd != nil && rd.pace != nil && t.ts != nil && len(t.marks) > 0 {
		end = t.paceLocked(rd.pace, off)
		if end <= off {
			return 0, off, nil
		}
	}
	at := int(off % int64(len(t.ring)))
	n := min(int(end-off), len(p), len(t.ring)-at)
	copy(p, t.ring[at:at+n])
	if rd != nil {
		rd.off = off + int64(n)
		if t.finite {
			t.cond.Broadcast()
		}
	}
	return n, off + int64(n), nil
}

// paceLocked is the end of what a paced reader at off may have now: media
// up to its start plus elapsed play time plus tapPaceLead.
func (t *upstreamTap) paceLocked(p *tapPace, off int64) int64 {
	now := time.Now()
	if !p.anchored {
		p.anchored, p.clock, p.wall = true, t.clockAtLocked(off), now
	}
	limit := p.dueClock(now) + int64(tapPaceLead/time.Millisecond)*tsClockHz/1000
	i := sort.Search(len(t.marks), func(i int) bool { return t.marks[i].clock > limit })
	if i == len(t.marks) {
		return t.written
	}
	return t.marks[i].off
}

// refillSteadyInput re-anchors ss's paced release at its reader, so the
// reader may take up to tapPaceLead beyond what it already has. The frame
// clock calls it when its decoded pictures run low: a pace whose clock fell
// behind real playout must never starve a decoder while the ring holds media.
func (s *Service) refillSteadyInput(ss *session) {
	g := s.healthGrant(ss)
	if g == nil {
		return
	}
	g.mu.Lock()
	t := g.tap
	g.mu.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.paces[g]
	if p == nil || !p.anchored || t.ts == nil || len(t.marks) == 0 {
		return
	}
	// Only a reader the pace is actually holding back, with media waiting
	// in the ring, is released; otherwise the reserve stays where it is.
	due := p.dueClock(time.Now()) + int64(tapPaceLead/time.Millisecond)*tsClockHz/1000
	for rd := range t.active {
		if rd.g == g && rd.pace == p && rd.off < t.written && t.clockAtLocked(rd.off) >= due-tsClockHz/5 {
			p.anchored = false
			t.cond.Broadcast()
			return
		}
	}
}

// setSteadyInputRate adjusts the paced release rate of ss's grant on its
// shared tap so slow-playout reserve accrual stays in the tap's ring.
func (s *Service) setSteadyInputRate(ss *session, rate float64) {
	rate = min(rate, 1.0)
	if s == nil || ss == nil {
		return
	}
	g := s.healthGrant(ss)
	if g == nil {
		return
	}
	g.mu.Lock()
	t := g.tap
	g.mu.Unlock()
	if t == nil {
		return
	}
	t.mu.Lock()
	if !t.closed && g.reserve > 0 {
		if t.paces == nil {
			t.paces = map[*inputGrant]*tapPace{}
		}
		p := t.paces[g]
		if p == nil {
			p = &tapPace{}
			t.paces[g] = p
		}
		prev := p.effectiveRate()
		p.setRate(rate, time.Now())
		t.publishLocked()
		if p.effectiveRate() > prev {
			t.cond.Broadcast()
		}
	}
	t.mu.Unlock()
}

func (t *upstreamTap) attach() {
	t.mu.Lock()
	t.readers++
	if t.idle != nil {
		t.idle.Stop()
		t.idle = nil
	}
	t.mu.Unlock()
}

func (t *upstreamTap) detach() {
	t.mu.Lock()
	t.readers--
	if t.readers == 0 && !t.closed && !t.finite {
		t.idleGeneration++
		generation := t.idleGeneration
		t.idle = time.AfterFunc(tapIdle, func() {
			t.closeWithIdleCheck(nil, generation)
		})
	}
	t.mu.Unlock()
}

// addReader registers a positioned reader for g; dropReader records where
// it stopped so a restarted decoder on the same grant resumes there.
func (t *upstreamTap) addReaderLocked(g *inputGrant, off int64) *tapReader {
	rd := &tapReader{g: g, start: off, off: off}
	if g != nil && g.reserve > 0 {
		if t.paces == nil {
			t.paces = map[*inputGrant]*tapPace{}
		}
		if t.paces[g] == nil {
			t.paces[g] = &tapPace{}
		}
		rd.pace = t.paces[g]
	}
	defer t.publishLocked()
	if t.active == nil {
		t.active = map[*tapReader]struct{}{}
	}
	t.active[rd] = struct{}{}
	return rd
}

func (t *upstreamTap) dropReader(rd *tapReader) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.active, rd)
	if t.finite {
		t.cond.Broadcast()
	}
	if _, ok := t.refs[rd.g]; ok && rd.g != nil {
		if t.resume == nil {
			t.resume = map[*inputGrant]tapResume{}
		}
		r := tapResume{start: rd.start, last: rd.off}
		if prev, ok := t.resume[rd.g]; ok && prev.start < r.start {
			r.start = prev.start
		}
		t.resume[rd.g] = r
	}
	t.publishLocked()
}

// tapSource names the provider account behind a grant and its connection
// limit; "" when the input does not belong to a capped source.
func (s *Service) tapSource(g *inputGrant) (string, int) {
	if g.login != nil {
		return g.source + "/login/" + g.login.login, g.login.limit
	}
	if g.source == "" {
		return "", 0
	}
	return g.source, s.sourceConnectionLimit(g.source)
}

func (g *inputGrant) setTap(t *upstreamTap) {
	g.mu.Lock()
	g.tap = t
	g.mu.Unlock()
	t.mu.Lock()
	t.publishLocked()
	t.mu.Unlock()
}

func (g *inputGrant) setManifest(cm *cachedManifest) {
	if g == nil || cm == nil || !cm.isMaster && !g.finite {
		return
	}
	g.mu.Lock()
	g.manifest = cm
	direct := g.direct
	g.mu.Unlock()
	if direct != nil {
		direct.setManifest(cm)
	}
}

func (g *inputGrant) peekManifest(raw string) *cachedManifest {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	cm := g.manifest
	direct := g.direct
	g.mu.Unlock()
	if direct != nil {
		if dm := direct.manifestFor(raw); dm != nil {
			return dm
		}
		if _, _, _, _, _, invalidated, _ := direct.current(); invalidated {
			return nil
		}
	}
	if cm != nil && cm.raw == raw {
		return cm
	}
	return nil
}

// consumeManifest returns a cached root manifest for raw. Master playlists stay
// cached for the grant's lifetime (they are static variant lists, often behind
// single-use tokens); media playlists and MPDs are consumed once so live
// reloads continue fetching fresh segments.
func (g *inputGrant) consumeManifest(raw string) *cachedManifest {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	direct := g.direct
	g.mu.Unlock()
	if direct != nil {
		if dm := direct.manifestFor(raw); dm != nil {
			g.mu.Lock()
			g.manifest = dm
			g.mu.Unlock()
			return dm
		}
		if _, _, _, _, _, invalidated, _ := direct.current(); invalidated {
			g.mu.Lock()
			g.manifest = nil
			g.mu.Unlock()
			return nil
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.manifest == nil || g.manifest.raw != raw {
		return nil
	}
	cm := g.manifest
	if !cm.isMaster && !g.finite {
		g.manifest = nil
	}
	return cm
}

// serveTapped serves a media input through its tap. It reports false when
// the input turned out to be a manifest this reader must fetch itself.
func (s *Service) serveTapped(w http.ResponseWriter, r *http.Request, g *inputGrant, raw string) bool {
	// A byte range is a seek into a file input, not a live read; an active
	// Steady transport-stream tap ignores ffmpeg reconnect Range headers so
	// the decoder rejoins at resumeLocked instead of opening a raw upstream GET.
	g.mu.Lock()
	tExisting := g.tap
	g.mu.Unlock()
	steadyTS := g.steady && tExisting != nil
	if steadyTS {
		tExisting.mu.Lock()
		steadyTS = !tExisting.closed && tExisting.ts != nil
		tExisting.mu.Unlock()
	}
	var t *upstreamTap
	refreshed := false
	if g.finite && tExisting != nil {
		t = tExisting
	}
	for t == nil {
		var directGen uint64
		if g.direct != nil {
			var invalidated bool
			var expStatus int
			raw, _, directGen, invalidated, expStatus = g.syncDirect()
			if invalidated {
				if refreshed {
					code := expStatus
					if code < 400 || code > 599 {
						code = http.StatusForbidden
					}
					http.Error(w, "upstream unavailable", code)
					return true
				}
				nextRaw, rerr := g.refreshDirect(r.Context(), directGen)
				if rerr != nil {
					code := http.StatusBadGateway
					if st, ok := expiredProviderStatus(rerr); ok {
						code = st
					} else if errors.Is(rerr, context.DeadlineExceeded) {
						code = http.StatusGatewayTimeout
					} else if expStatus >= 400 && expStatus <= 599 && !g.direct.canResolve() {
						code = expStatus
					}
					http.Error(w, "upstream unavailable", code)
					return true
				}
				raw = nextRaw
				refreshed = true
				continue
			}
		}
		if cm := g.consumeManifest(raw); cm != nil {
			s.serveRelayManifest(w, g, raw, cm.base, bytes.NewReader(cm.body), cm.hls)
			return true
		}
		if rng := r.Header.Get("Range"); rng != "" && rng != "bytes=0-" && !steadyTS && !g.finite {
			return false
		}
		source, limit := s.tapSource(g)
		_, headers, mediaHeaders := g.requestScope()
		var cm *cachedManifest
		var created bool
		var err error
		t, cm, created, err = s.inputRelay.taps.acquireOrManifest(g.rootTapKey(raw, headers, mediaHeaders), source, limit, g, func() *cachedManifest {
			return g.consumeManifest(raw)
		})
		if err != nil {
			if errors.Is(err, ErrProviderBusy) && limit > 0 {
				s.emitProviderLimitReached(source, limit, limit, g.histChannel)
			}
			w.Header().Set(tapBusyHeader, "1")
			http.Error(w, "provider connections in use", http.StatusServiceUnavailable)
			return true
		}
		if cm != nil {
			s.serveRelayManifest(w, g, raw, cm.base, bytes.NewReader(cm.body), cm.hls)
			return true
		}
		if created {
			hdr := r.Header.Clone()
			hdr.Del("Range")
			hdr.Del("If-Range")
			s.openTap(t, g, raw, hdr, r.Context())
		}
		select {
		case <-t.ready:
		case <-r.Context().Done():
			return true
		}
		if t.openErr != nil || t.status != http.StatusOK && t.status != http.StatusPartialContent {
			if t.openErr == nil && isExpiredStatus(t.status) {
				g.noteExpiredManifest(t.status)
				if !refreshed && g.direct != nil && g.direct.canResolve() {
					if nextRaw, rerr := g.refreshDirect(r.Context(), directGen); rerr == nil {
						raw = nextRaw
						refreshed = true
						t = nil
						continue
					}
				}
			}
			code := t.status
			if code < 400 || code > 599 {
				code = http.StatusBadGateway
			}
			if errors.Is(t.openErr, context.DeadlineExceeded) {
				code = http.StatusGatewayTimeout
			}
			http.Error(w, "upstream unavailable", code)
			return true
		}
		if t.isManifest {
			if cm := g.consumeManifest(raw); cm != nil {
				s.serveRelayManifest(w, g, raw, cm.base, bytes.NewReader(cm.body), cm.hls)
				return true
			}
			if body := t.manifestCopy(); body != nil {
				if t.hls && isMasterPlaylist(body) {
					cm := newCachedManifest(raw, body, t.base, true)
					g.setManifest(cm)
					s.serveRelayManifest(w, g, raw, cm.base, bytes.NewReader(cm.body), true)
					return true
				}
				s.serveRelayManifest(w, g, raw, t.base, bytes.NewReader(body), t.hls)
				return true
			}
			return false
		}
		break
	}
	g.setTap(t)
	if mt, params, err := mime.ParseMediaType(t.header.Get("Content-Type")); err == nil {
		w.Header().Set("Content-Type", mime.FormatMediaType(mt, params))
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	t.attach()
	defer t.detach()
	t.mu.Lock()
	off := t.joinLocked(g, r.URL.Query().Get(relayJoinQuery))
	if g.finite && off > 0 {
		t.mu.Unlock()
		return true
	}
	rd := t.addReaderLocked(g, off)
	t.mu.Unlock()
	defer t.dropReader(rd)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	defer context.AfterFunc(g.ctx, cancel)()
	// Wake this reader's wait when its request ends.
	defer context.AfterFunc(ctx, func() {
		t.mu.Lock()
		t.cond.Broadcast()
		t.mu.Unlock()
	})()
	buf := make([]byte, 64<<10)
	for {
		n, next, err := t.readAs(ctx, rd, off, buf)
		off = next
		if n > 0 {
			_ = rc.SetWriteDeadline(time.Now().Add(relayIOTimeout))
			if _, werr := w.Write(buf[:n]); werr != nil {
				return true
			}
			if rc.Flush() != nil {
				return true
			}
			_ = rc.SetWriteDeadline(time.Time{})
		}
		if err != nil {
			return true
		}
		if n == 0 {
			// Paced: the next 100 ms of media is due shortly.
			select {
			case <-ctx.Done():
				return true
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
}

func (t *upstreamTap) manifestCopy() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return bytes.Clone(t.manifest)
}

// openContext is the tap's own context with the opening reader's deadline:
// dialling and the wait for response headers honour the tune's startup
// budget, while the stream that follows outlives it.
type openContext struct {
	context.Context
	deadline time.Time
}

func (c openContext) Deadline() (time.Time, bool) { return c.deadline, true }

// openTap makes the tap's one upstream request and starts filling the ring.
// opener is the first reader's context: its deadline bounds the connect,
// and the open is abandoned if it ends first.
func (s *Service) openTap(t *upstreamTap, g *inputGrant, raw string, hdr http.Header, opener context.Context) {
	started := time.Now()
	var openingBody io.Closer
	var opening chan struct{}
	published := false
	defer func() {
		if !published {
			close(t.ready)
		}
		if opening != nil {
			close(opening)
		}
	}()
	defer func() {
		if recover() != nil {
			if openingBody != nil {
				_ = openingBody.Close()
			}
			t.openErr = core.ErrNotAvailable
			t.close(t.openErr)
		}
	}()
	ctx, cancel := context.WithCancel(s.serviceContext())
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		cancel()
		t.openErr = context.Canceled
		return
	}
	opening = make(chan struct{})
	t.opening = opening
	t.ctx, t.cancel = ctx, cancel
	t.svc, t.rawURL = s, raw
	t.mu.Unlock()
	stop := context.AfterFunc(opener, func() {
		t.mu.Lock()
		other := false
		for ref := range t.refs {
			if ref != g && (ref.ctx == nil || ref.ctx.Err() == nil) {
				other = true
				break
			}
		}
		t.mu.Unlock()
		if !other {
			cancel()
		}
	})
	defer stop() // initial body inspection is part of opening the stream
	wait, err := s.paceGrant(ctx, g)
	connCtx, connCancel := context.WithCancel(ctx)
	var reqCtx context.Context = connCtx
	if dl, ok := opener.Deadline(); ok {
		reqCtx = openContext{Context: connCtx, deadline: dl.Add(wait)}
	}
	var res *http.Response
	if err == nil {
		res, err = s.relayFetch(reqCtx, connCancel, g, raw, &http.Request{Header: hdr})
	}
	if ctx.Err() != nil && err == nil {
		// The opener left while headers arrived; nobody asked for this stream.
		connCancel()
		res.Body.Close()
		err = ctx.Err()
	}
	if err != nil {
		connCancel()
		if g.log != nil {
			g.log.Debug("live upstream open failed", append(Diagnose("upstream_headers", err).LogAttrs(), "elapsed", time.Since(started))...)
		}
		t.openErr = err
		t.close(err)
		return
	}
	closer := closeFunc(func() error { connCancel(); return res.Body.Close() })
	openingBody = closer
	t.status, t.header, t.base = res.StatusCode, res.Header.Clone(), res.Request.URL
	t.ctype = res.Header.Get("Content-Type")
	if g.log != nil {
		g.log.Debug("live upstream headers", "status", res.StatusCode, "elapsed", time.Since(started))
	}
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		closer.Close()
		if res.StatusCode == 401 || res.StatusCode == 403 {
			s.failedLogin(g.login)
			providerID := g.source
			accountID := ""
			if g.login != nil {
				if providerID == "" {
					providerID = g.login.source
				}
				accountID = g.login.accountID
			}
			if providerID == "" {
				providerID = g.histSrc.AuthSourceID
			}
			s.emitProviderAuthFailed(providerID, "", accountID, res.StatusCode, providerResponseError(res.StatusCode))
		}
		t.close(nil)
		return
	}
	idle := time.AfterFunc(relayIOTimeout, connCancel)
	body := bufio.NewReaderSize(&idleReader{r: res.Body, t: idle}, 32<<10)
	if magic, _ := body.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		packed, _ := io.ReadAll(io.LimitReader(body, relayManifestLimit+1))
		if len(packed) <= relayManifestLimit {
			if zr, err := gzip.NewReader(bytes.NewReader(packed)); err == nil {
				plain, err := io.ReadAll(io.LimitReader(zr, relayManifestLimit+1))
				zr.Close()
				if err == nil && len(plain) <= relayManifestLimit {
					if trimmed := bytes.TrimLeft(bytes.TrimPrefix(plain, []byte("\xef\xbb\xbf")), " \t\r\n"); bytes.HasPrefix(trimmed, []byte("#EXTM3U")) {
						idle.Stop()
						closer.Close()
						t.hls = true
						t.isManifest = true
						cm := newCachedManifest(raw, plain, t.base, true)
						t.manifest = bytes.Clone(cm.body)
						g.setManifest(cm)
						t.header = nil
						t.close(nil)
						return
					}
				}
			}
		}
		body = bufio.NewReaderSize(io.MultiReader(bytes.NewReader(packed), body), 32<<10)
	}
	head := sniffHead(body)
	ctype := strings.ToLower(res.Header.Get("Content-Type"))
	t.hls = bytes.HasPrefix(head, []byte("#EXTM3U"))
	mpd := bytes.HasPrefix(head, []byte("<?xml")) || bytes.HasPrefix(head, []byte("<MPD")) ||
		(bytes.HasPrefix(head, []byte("<")) && strings.Contains(ctype, "xml"))
	if t.hls || mpd {
		t.isManifest = true
		b, err := io.ReadAll(io.LimitReader(body, relayManifestLimit+1))
		idle.Stop()
		closer.Close()
		if err != nil || len(b) > relayManifestLimit {
			t.openErr = errRelayUnsupported
		} else {
			cm := newCachedManifest(raw, b, t.base, t.hls)
			t.manifest = bytes.Clone(cm.body)
			g.setManifest(cm)
		}
		t.header = nil
		t.close(nil)
		return
	}
	// Three packets (or two sync bytes when HTTP/2 delivers one TS packet in
	// its initial DATA frame) decide whether this is a transport stream the
	// reserve can index and splice; anything else is relayed as before.
	ts := !g.finite && tsPrefixWithType(body, ctype)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		idle.Stop()
		closer.Close()
		t.openErr = context.Canceled
		return
	}
	now := time.Now()
	t.body = closer
	t.lastData, t.epochStart = now, now
	t.dataAt.Store(now.UnixNano())
	if ts {
		t.ts = newTSSplicer()
		t.allocRingLocked(tapRingSize(tapRing))
	} else {
		t.allocRingLocked(tapRing)
	}
	t.mu.Unlock()
	close(t.ready)
	published = true
	if g.finite {
		go t.fillArchive(body, idle)
		return
	}
	if !ts {
		go func() { defer idle.Stop(); t.fill(body) }()
		return
	}
	// The stall watchdog replaces the idle timeout for a transport stream.
	idle.Stop()
	go s.pumpTap(t, raw, body)
	go t.watch()
}

// tsPrefix decides from the bytes already buffered, never waiting for
// more: a slow provider's first packet must not hold up the tune. It needs
// two sync bytes a packet apart; a body that cannot show them is relayed
// as plain bytes, without a reserve, exactly as before.
func tsPrefix(br *bufio.Reader) bool {
	b, _ := br.Peek(br.Buffered())
	if len(b) >= 3*tsPacket {
		return looksTS(b)
	}
	return len(b) > tsPacket && b[0] == 0x47 && b[tsPacket] == 0x47
}

// tsPrefixWithType handles HTTP/2 providers that flush a single 188-byte TS
// packet in their initial DATA frame: when Content-Type declares MPEG-TS (or
// the first packet is a PAT with PUSI set), peek one more packet to confirm.
func tsPrefixWithType(br *bufio.Reader, ctype string) bool {
	if tsPrefix(br) {
		return true
	}
	b, _ := br.Peek(br.Buffered())
	if len(b) == 0 || b[0] != 0x47 {
		return false
	}
	isMP2T := strings.Contains(ctype, "mp2t") || strings.Contains(ctype, "mpegts")
	isPAT := len(b) >= tsPacket && pid(b[:tsPacket]) == 0 && b[1]&0x40 != 0
	if !isMP2T && !isPAT {
		return false
	}
	p, _ := br.Peek(2 * tsPacket)
	return len(p) >= 2*tsPacket && p[0] == 0x47 && p[tsPacket] == 0x47
}

// tapRingSize rounds a ring to whole packets so every offset in it stays
// packet aligned.
func tapRingSize(n int) int { return n - n%tsPacket }

// pumpTap fills a transport-stream tap and reconnects it when the provider
// stalls or ends, until the tap closes or reconnecting gives up.
func (s *Service) pumpTap(t *upstreamTap, raw string, body io.Reader) {
	defer func() {
		if recover() != nil {
			err := errors.New("live transport stream pump stopped unexpectedly")
			s.logger().Error(err.Error())
			t.close(err)
		}
	}()

	for {
		err := t.fillTS(body)
		t.mu.Lock()
		if t.closed {
			old := t.body
			t.body = nil
			t.mu.Unlock()
			if old != nil {
				old.Close()
			}
			return
		}
		if t.raceWinner != nil {
			next := t.raceWinner
			t.raceWinner = nil
			t.activateSplicedLocked(true)
			t.mu.Unlock()
			body = next
			continue
		}
		old := t.body
		t.body = nil
		waitRace := t.raceDone
		t.mu.Unlock()
		if old != nil {
			// The old connection closes before a serial reopen: an account
			// sold by the connection never sees two.
			old.Close()
		}
		if waitRace != nil {
			select {
			case <-waitRace:
			case <-t.ctx.Done():
				return
			}
			t.mu.Lock()
			if t.closed {
				t.mu.Unlock()
				return
			}
			if t.raceWinner != nil {
				next := t.raceWinner
				t.raceWinner = nil
				t.activateSplicedLocked(true)
				t.mu.Unlock()
				body = next
				continue
			}
			t.mu.Unlock()
		}
		next, rerr := s.reconnectTap(t, raw, err)
		if rerr != nil {
			t.close(rerr)
			return
		}
		body = next
	}
}

func (t *upstreamTap) anySteadyLocked() bool {
	for g := range t.refs {
		if g != nil && g.steady {
			return true
		}
	}
	return false
}

func (t *upstreamTap) activateSplicedLocked(fromRace bool) {
	now := time.Now()
	t.lastData, t.epochStart = now, now
	t.dataAt.Store(now.UnixNano())
	t.epochClock = t.ts.clock
	t.reconnects++
	t.refusals = 0
	t.failed = false
	t.burstDone = false
	t.haveBurst = false
	t.rhythmGap = 0
	t.pendingGap = 0
	t.epochReads = 0
	t.epochBytes = 0
	t.ts.reconnect(fromRace || t.anySteadyLocked() || t.minDepthLocked() >= tapDedupeDepth)
}

// fillTS copies the provider body through the splicer into the ring.
func (t *upstreamTap) fillTS(body io.Reader) error {
	buf := make([]byte, 64<<10)
	var out []byte
	for {
		n, err := body.Read(buf)
		if n > 0 {
			var abortRace context.CancelFunc
			var doneRace chan struct{}
			var adaptLog *slog.Logger
			var learnedGap, stallAfter time.Duration
			var recovered bool
			if stopped := func() bool {
				t.mu.Lock()
				defer t.mu.Unlock()
				if t.closed || t.raceWinner != nil {
					return true
				}
				now := time.Now()
				prev := t.lastData
				t.lastData = now
				t.dataAt.Store(now.UnixNano())
				t.epochReads++
				t.epochBytes += int64(n)
				var gap time.Duration
				if t.epochReads > 1 && !prev.IsZero() && now.After(prev) {
					gap = now.Sub(prev)
				}
				recovered = t.stalled && t.body != nil
				if recovered {
					t.stalled = false
					t.failed = false
				}
				clockBefore := t.ts.clock
				var marks []tsMark
				out, marks = t.ts.feed(buf[:n], out[:0], t.written)
				if len(out) > 0 {
					t.writeLocked(out)
					t.indexLocked(marks)
					t.cond.Broadcast()
				}
				adaptLog, learnedGap, stallAfter = t.observeRhythmLocked(now, gap, clockBefore, marks)
				if t.ts.format != t.published {
					// A program change: tell consumers now, not at the next tick.
					t.published = t.ts.format
					t.publishLocked()
				}
				if time.Duration(t.ts.clock-t.epochClock)*time.Second/tsClockHz >= tapHealthy {
					t.burstProbeUsed = false
					t.failures, t.refusals, t.failed = 0, 0, false
				}
				if t.raceCancel != nil {
					abortRace, doneRace = t.raceCancel, t.raceDone
					t.raceCancel, t.raceDone, t.raceActive = nil, nil, false
				}
				return false
			}(); stopped {
				return io.EOF
			}
			if doneRace != nil {
				close(doneRace)
			}
			if abortRace != nil {
				abortRace()
			}
			if adaptLog != nil {
				adaptLog.Info("live provider stall threshold adapted", "gap", learnedGap, "stall_after", stallAfter)
			}
			if recovered {
				t.emit(InputRecovered, nil)
			}
		}
		if err != nil {
			return err
		}
	}
}

// observeRhythmLocked tracks inter-burst gaps and burst lead while the stream
// keeps up with real time. It returns a logger and the newly learned gap and
// threshold the first time this tap raises its threshold above tapStallAfter.
func (t *upstreamTap) observeRhythmLocked(now time.Time, gap time.Duration, clockBefore int64, marks []tsMark) (*slog.Logger, time.Duration, time.Duration) {
	if gap >= tapBurstGapFloor {
		if gap > t.pendingGap {
			t.pendingGap, t.gapClock = gap, clockBefore
		}
		if t.haveBurst {
			t.burstAt, t.burstClock = now, clockBefore
			t.paceAt, t.paceClock = now, clockBefore
		}
	}
	if t.ts == nil || !t.ts.haveClock || t.ts.seeking {
		return nil, 0, 0
	}
	if !t.haveBurst {
		startClock := clockBefore
		if len(marks) > 0 {
			startClock = marks[0].clock
		}
		t.haveBurst = true
		t.burstAt, t.burstClock = now, startClock
		t.paceAt, t.paceClock = now, startClock
		t.rhythmAt, t.rhythmClock = now, startClock
		if t.pendingGap > 0 {
			// A gap already elapsed before the splicer entered (for example
			// after deduplicating a reconnect's initial burst): measure the
			// window across that gap so this burst can confirm it.
			t.rhythmAt = now.Add(-t.pendingGap)
			t.gapClock = startClock
		}
	} else if gap < tapBurstGapFloor && now.Sub(t.paceAt) >= 1500*time.Millisecond {
		// Continuous ~1x delivery retires any earlier connect backlog so a
		// later stall on a continuous source is still detected at 2 s.
		stepWall := now.Sub(t.paceAt)
		stepMedia := time.Duration(t.ts.clock-t.paceClock) * time.Second / tsClockHz
		if stepMedia <= stepWall*5/4 {
			t.burstAt, t.burstClock = t.paceAt, t.paceClock
		}
		t.paceAt, t.paceClock = now, t.ts.clock
	}
	if t.pendingGap > 0 {
		postMedia := time.Duration(t.ts.clock-t.gapClock) * time.Second / tsClockHz
		if postMedia >= min(time.Second, t.pendingGap/2) {
			t.rhythmGap = max(t.rhythmGap, t.pendingGap)
			t.pendingGap = 0
		}
	}
	wall := now.Sub(t.rhythmAt)
	media := time.Duration(t.ts.clock-t.rhythmClock) * time.Second / tsClockHz
	healthy := wall >= 2*time.Second && media >= wall*4/5
	if wall >= tapRhythmWindow && healthy {
		t.recentGaps = append(t.recentGaps, t.rhythmGap)
		if len(t.recentGaps) > tapRhythmHistory {
			copy(t.recentGaps, t.recentGaps[len(t.recentGaps)-tapRhythmHistory:])
			t.recentGaps = t.recentGaps[:tapRhythmHistory]
		}
		t.rhythmAt, t.rhythmClock, t.rhythmGap = now, t.ts.clock, 0
	} else if wall >= 2*tapRhythmWindow {
		// Unhealthy trickle: discard this window without learning its gaps.
		t.rhythmAt, t.rhythmClock, t.rhythmGap, t.pendingGap = now, t.ts.clock, 0, 0
	}
	var maxGap time.Duration
	for _, g := range t.recentGaps {
		maxGap = max(maxGap, g)
	}
	if healthy {
		maxGap = max(maxGap, t.rhythmGap)
	}
	if len(t.recentGaps) == 0 && (!healthy || t.rhythmGap == 0) {
		return nil, 0, 0
	}
	next := min(max(tapStallAfter, 3*maxGap/2), max(tapStallAfter, tapStallMax))
	t.stallAfter = next
	if next > tapStallAfter && !t.stallLogged {
		t.stallLogged = true
		for ref := range t.refs {
			if ref.log != nil {
				return ref.log, maxGap, next
			}
		}
	}
	return nil, 0, 0
}

// stallThresholdLocked is how long t.lastData may sit idle at now before the
// connection counts as stalled.
func (t *upstreamTap) stallThresholdLocked(now time.Time) time.Duration {
	limit := max(tapStallAfter, t.stallAfter)
	// Don't cut while the media delivered by the latest burst still covers
	// the silence ahead of the readers' live edge.
	if t.haveBurst && t.ts != nil && !t.lastData.Before(t.burstAt) {
		burstDur := t.lastData.Sub(t.burstAt)
		burstMedia := time.Duration(t.ts.clock-t.burstClock) * time.Second / tsClockHz
		if lead := burstMedia - burstDur; lead > 0 {
			if lead >= time.Second {
				lead += tapBurstGrace
			}
			limit = max(limit, lead)
		}
	}
	// A reconnect followed by a short burst and then silence is the signature
	// of a bursty restreamer (mid-segment or deduplicated by tsSplicer), not a
	// dead source. Hold one such silence up to tapStallMax so the next burst
	// can arrive and teach the tap its rhythm instead of burning quota in a
	// tight cut-and-reconnect loop.
	if t.reconnects > 0 && !t.burstProbeUsed && t.epochBytes >= 4*tsPacket && t.ts != nil &&
		time.Duration(t.ts.clock-t.epochClock)*time.Second/tsClockHz < tapHealthy {
		limit = max(limit, tapStallMax)
	}
	return min(limit, max(tapStallAfter, tapStallMax))
}

// indexLocked appends marks, drops those that left the ring, measures the
// bitrate and grows the ring toward what the reserve needs.
func (t *upstreamTap) indexLocked(marks []tsMark) {
	if len(marks) == 0 {
		return
	}
	t.marks = append(t.marks, marks...)
	start := t.ringStartLocked()
	drop := 0
	for drop < len(t.marks) && (t.marks[drop].entry < start || len(t.marks)-drop > tapMarkLimit) {
		drop++
	}
	if drop > 0 {
		t.marks = append(t.marks[:0], t.marks[drop:]...)
	}
	last := t.marks[len(t.marks)-1]
	i := sort.Search(len(t.marks), func(i int) bool { return t.marks[i].clock >= last.clock-5*tsClockHz })
	if first := t.marks[i]; last.clock-first.clock >= 3*tsClockHz {
		t.bitrate = (last.off - first.off) * 8 * tsClockHz / (last.clock - first.clock)
	}
	t.growLocked()
}

// targetLocked is the deepest reserve any grant on this tap wants.
func (t *upstreamTap) targetLocked() time.Duration {
	var d time.Duration
	for g := range t.refs {
		d = max(d, g.joinDepth())
	}
	return d
}

// growLocked sizes the ring to bitrate × (target + margin), within
// tapRingMax and the shared budget. The margin covers the join backlog and
// the free accrual a burst can add on top of the target.
func (t *upstreamTap) growLocked() {
	target := t.targetLocked()
	if target <= 0 || t.bitrate <= 0 {
		return
	}
	want := int64(float64(t.bitrate) / 8 * (target + 10*time.Second).Seconds())
	want = min(want, int64(tapRingMax))
	cur := int64(len(t.ring))
	if want < cur+cur/4 {
		return
	}
	for {
		used := tapRingInUse.Load()
		allowed := min(want, cur+max(0, tapRingBudget-used))
		allowed = int64(tapRingSize(int(allowed)))
		if allowed < cur+cur/4 {
			return
		}
		if tapRingInUse.CompareAndSwap(used, used+allowed-cur) {
			t.resizeLocked(int(allowed))
			t.ringBytes += allowed - cur
			return
		}
	}
}

// resizeLocked moves the buffered bytes into a ring of n bytes, keeping
// every absolute offset where it was.
func (t *upstreamTap) resizeLocked(n int) {
	next := make([]byte, n)
	old := int64(len(t.ring))
	for off := t.ringStartLocked(); off < t.written; {
		src := off % old
		dst := off % int64(n)
		c := min(t.written-off, old-src, int64(n)-dst)
		copy(next[dst:dst+c], t.ring[src:src+c])
		off += c
	}
	t.ring = next
}

// readerDepthLocked is a reader's whole reserve: media in the tap ahead of
// it, plus, for a paced reader, what it has been handed beyond play time
// (up to tapPaceLead), which its consumer holds and plays through a stall.
func (t *upstreamTap) readerDepthLocked(rd *tapReader) time.Duration {
	d := t.depthLocked(rd.off)
	if p := rd.pace; p != nil && p.anchored && t.ts != nil && len(t.marks) > 0 {
		due := p.dueClock(time.Now())
		if held := t.clockAtLocked(rd.off) - due; held > 0 {
			d += min(time.Duration(held)*time.Second/tsClockHz, tapPaceLead)
		}
	}
	return d
}

// tapDedupeDepth is the reserve readers need for a reconnect to wait out
// content the provider re-sends rather than show it twice.
const tapDedupeDepth = 2 * time.Second

// minDepthLocked is the smallest reserve among active readers (zero with
// none).
func (t *upstreamTap) minDepthLocked() time.Duration {
	first, low := true, time.Duration(0)
	for rd := range t.active {
		if d := t.readerDepthLocked(rd); first || d < low {
			first, low = false, d
		}
	}
	return low
}

// capacityLocked is how much media the ring can hold at the current bitrate.
func (t *upstreamTap) capacityLocked() time.Duration {
	if t.bitrate <= 0 {
		return 0
	}
	return time.Duration(float64(len(t.ring)) * 8 / float64(t.bitrate) * float64(time.Second))
}

// watch raises a stall when the provider stops delivering, keeps the open
// connection alive while racing a parallel replacement when connection limits
// allow, and only cuts a quiet connection on a single-connection account after
// tapQuietCutAfter so transient silences resume without a teardown.
func (t *upstreamTap) watch() {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
		case <-t.ctx.Done():
			return
		}
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return
		}
		t.publishLocked()
		now := time.Now()
		silence := now.Sub(t.lastData)
		thresh := t.stallThresholdLocked(now)
		picStall := t.body != nil && len(t.active) > 0 && t.minDepthLocked() == 0 && silence >= broadcastStall && (thresh <= tapStallAfter || silence >= thresh)
		stall := !t.stalled && t.body != nil && silence >= thresh
		if stall {
			if t.reconnects > 0 && silence >= tapStallMax && t.ts != nil &&
				time.Duration(t.ts.clock-t.epochClock)*time.Second/tsClockHz < tapHealthy {
				t.burstProbeUsed = true
			}
			t.stalled = true
			t.stallCount++
		}
		wantRace := (picStall || t.stalled) && t.body != nil && !t.raceActive && t.raceWinner == nil && t.svc != nil
		if wantRace && !t.lastRaceAt.IsZero() {
			backoff := tapReconnectDelays[min(t.failures, len(tapReconnectDelays)-1)]
			if t.refusals > 0 {
				backoff = time.Second
			}
			if now.Sub(t.lastRaceAt) < backoff {
				wantRace = false
			}
		}
		t.measureBurstLocked()
		due := now.Sub(t.rep.at) >= historyReportEvery
		svc := t.svc
		t.mu.Unlock()

		if stall {
			t.emit(InputStalled, errors.New("provider stopped delivering"))
		}
		if wantRace {
			raced := svc.startTapRace(t)
			if !raced {
				// Account cannot open a parallel replacement (e.g. limit == 1)
				// or parallel races failed: hold quiet transport up to
				// tapQuietCutAfter before cutting for serial reconnect.
				t.mu.Lock()
				var cut io.Closer
				cutAfter := max(tapQuietCutAfter, thresh)
				if !t.closed && t.body != nil && !t.raceActive && t.raceWinner == nil && now.Sub(t.lastData) >= cutAfter {
					cut, t.body = t.body, nil
				}
				t.mu.Unlock()
				if cut != nil {
					cut.Close()
				}
			}
		} else {
			// Even if parallel races backed off after repeated failures on a
			// dead socket, cut the silent socket once cutAfter has elapsed and
			// at least two parallel attempts failed.
			t.mu.Lock()
			var cut io.Closer
			cutAfter := max(tapQuietCutAfter, thresh)
			if !t.closed && t.stalled && t.body != nil && !t.raceActive && t.raceWinner == nil && t.failures >= 2 && now.Sub(t.lastData) >= cutAfter {
				cut, t.body = t.body, nil
			}
			t.mu.Unlock()
			if cut != nil {
				cut.Close()
			}
		}
		if stall || due {
			t.report()
		}
	}
}

// reserveParallelTapSlot takes one extra provider connection slot for racing a
// replacement while the current tap stays open, honouring unified account
// limits across direct sessions, Steady outputs, hot standbys, and taps.
func (s *Service) reserveParallelTapSlot(account string, limit int) (func(), error) {
	if account == "" {
		return func() {}, nil
	}
	return s.inputRelay.taps.reserve(account, limit, false)
}

// startTapRace launches a parallel replacement connection on t if the source's
// connection limit and burst ceilings allow one more stream right now.
func (s *Service) startTapRace(t *upstreamTap) bool {
	t.mu.Lock()
	if t.closed || t.body == nil || t.raceActive || t.raceWinner != nil {
		t.mu.Unlock()
		return false
	}
	now := time.Now()
	kept := t.attempts[:0]
	for _, at := range t.attempts {
		if now.Sub(at) < tapBurstWindow {
			kept = append(kept, at)
		}
	}
	t.attempts = kept
	if t.failures >= tapGiveUpAfter || len(t.attempts) >= tapBurstLimit {
		t.mu.Unlock()
		return false
	}
	var g *inputGrant
	for ref := range t.refs {
		if ref.ctx.Err() == nil {
			g = ref
			break
		}
	}
	raw := t.rawURL
	t.mu.Unlock()
	if g == nil || g.login != nil {
		// A pooled login owns one durable slot for the shared input.
		// Reconnect serially rather than open a second stream on it.
		return false
	}

	source, limit := s.tapSource(g)
	release, err := s.reserveParallelTapSlot(source, limit)
	if err != nil {
		return false
	}

	t.mu.Lock()
	if t.closed || t.body == nil || t.raceActive || t.raceWinner != nil {
		t.mu.Unlock()
		release()
		return false
	}
	candCtx, candCancel := context.WithCancel(t.ctx)
	done := make(chan struct{})
	t.raceActive = true
	t.raceCancel = candCancel
	t.raceDone = done
	t.lastRaceAt = time.Now()
	t.attempts = append(t.attempts, t.lastRaceAt)
	t.mu.Unlock()

	go s.runTapRace(candCtx, candCancel, done, t, g, raw, release)
	return true
}

func (s *Service) runTapRace(candCtx context.Context, candCancel context.CancelFunc, done chan struct{}, t *upstreamTap, g *inputGrant, raw string, release func()) {
	defer release()
	finishLose := func(err error) {
		candCancel()
		var raise bool
		t.mu.Lock()
		sameRace := t.raceDone == done
		if sameRace {
			t.raceCancel, t.raceDone, t.raceActive = nil, nil, false
			t.lastRaceAt = time.Now()
			if err != nil && t.ctx.Err() == nil {
				if tapRefusal(err) {
					t.refusals++
				} else {
					t.failures++
				}
				// Failed means the transport is actually gone; a refused or
				// failed speculative parallel open while t.body is still alive
				// must not declare the primary connection dead.
				if t.body == nil && t.failures+t.refusals >= tapFailAfter && !t.failed {
					t.failed = true
					raise = true
				}
			}
		}
		t.mu.Unlock()
		if sameRace {
			close(done)
		}
		if raise {
			t.emit(InputFailed, err)
		}
	}

	if g.source != "" {
		e, _ := s.sourceEntry(candCtx, g.source)
		wait, err := t.reg.reserveOpen(g.source, e.gap, e.perHour, time.Now())
		if err != nil {
			finishLose(err)
			return
		}
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-candCtx.Done():
				finishLose(nil)
				return
			}
		}
	}

	deadline := time.Now().Add(tapReconnectTimeout)
	res, err := s.relayFetch(openContext{Context: candCtx, deadline: deadline}, candCancel, g, raw, &http.Request{Header: http.Header{}})
	if err != nil {
		if candCtx.Err() != nil {
			finishLose(nil)
		} else {
			finishLose(err)
		}
		return
	}
	closer := closeFunc(func() error { candCancel(); return res.Body.Close() })
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		closer.Close()
		finishLose(providerResponseError(res.StatusCode))
		return
	}
	var silent atomic.Bool
	timer := time.AfterFunc(time.Until(deadline), func() { silent.Store(true); candCancel() })
	body := bufio.NewReaderSize(res.Body, 32<<10)
	first, perr := body.Peek(1)
	timer.Stop()
	if perr != nil {
		closer.Close()
		if silent.Load() {
			finishLose(errTapSilent)
		} else if candCtx.Err() != nil {
			finishLose(nil)
		} else {
			finishLose(perr)
		}
		return
	}
	if first[0] != 0x47 && !tsPrefix(body) {
		closer.Close()
		finishLose(errTapNotTS)
		return
	}

	t.mu.Lock()
	if t.closed || t.raceDone != done {
		t.mu.Unlock()
		closer.Close()
		return
	}
	old := t.body
	t.body = closer
	t.raceWinner = body
	t.raceCancel, t.raceDone, t.raceActive = nil, nil, false
	t.mu.Unlock()

	if old != nil {
		old.Close()
	}
	close(done)
	if g.log != nil {
		g.log.Info("live provider parallel replacement spliced", "reconnects", t.reconnectCount()+1)
	}
}

// reconnectTap reopens the same URL after the provider stalled or ended,
// within the provider-safety rules every open follows: the source's open
// pacing, the grant's refusal accounting in relayFetch, and the live
// restart ceilings (seven consecutive failures, 28 attempts in 30 s).
func (s *Service) reconnectTap(t *upstreamTap, raw string, cause error) (io.Reader, error) {
	t.mu.Lock()
	raiseStall := !t.stalled
	if raiseStall {
		t.stallCount++
	}
	t.stalled = true
	// A connection that ended before delivering tapHealthy of media is a
	// failed attempt too: a source that accepts and drops at once must not
	// reconnect in a tight loop.
	if time.Duration(t.ts.clock-t.epochClock)*time.Second/tsClockHz < tapHealthy {
		t.failures++
	}
	t.mu.Unlock()
	if raiseStall {
		t.emit(InputStalled, cause)
	}
	for {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return nil, io.EOF
		}
		now := time.Now()
		kept := t.attempts[:0]
		for _, at := range t.attempts {
			if now.Sub(at) < tapBurstWindow {
				kept = append(kept, at)
			}
		}
		t.attempts = kept
		if t.failures >= tapGiveUpAfter || len(t.attempts) >= tapBurstLimit {
			t.mu.Unlock()
			return nil, errTapReconnectLimit
		}
		raise := t.failures+t.refusals >= tapFailAfter && !t.failed
		if raise {
			t.failed = true
		}
		delay := tapReconnectDelays[min(t.failures, len(tapReconnectDelays)-1)]
		if t.refusals > 0 {
			// A refused open is retried at the grant's own refusal spacing
			// (1 s, 28 attempts): the panel frees its slot on its own clock,
			// and the stream should resume the moment it does.
			delay = 0
		}
		var g *inputGrant
		for ref := range t.refs {
			if ref.ctx.Err() == nil {
				g = ref
				break
			}
		}
		t.attempts = append(t.attempts, now)
		ctx := t.ctx
		t.mu.Unlock()
		if raise {
			t.emit(InputFailed, cause)
		}
		if g == nil {
			return nil, io.EOF
		}
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, io.EOF
			}
		}
		body, err := s.reopenTap(ctx, t, g, raw)
		if err == nil {
			if g.log != nil {
				g.log.Info("live provider reconnected", "reconnects", t.reconnectCount(), "cause", cause)
			}
			return body, nil
		}
		if ctx.Err() != nil {
			return nil, io.EOF
		}
		if g.log != nil {
			g.log.Debug("live provider reconnect failed", Diagnose("upstream_headers", err).LogAttrs()...)
		}
		cause = err
		t.mu.Lock()
		if tapRefusal(err) {
			t.refusals++
		} else {
			t.failures++
		}
		t.mu.Unlock()
	}
}

func (t *upstreamTap) reconnectCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reconnects
}

// reopenTap makes one reconnect attempt and readies its body for splicing.
func (s *Service) reopenTap(ctx context.Context, t *upstreamTap, g *inputGrant, raw string) (io.Reader, error) {
	if g.source != "" {
		e, _ := s.sourceEntry(ctx, g.source)
		wait, err := t.reg.reserveOpen(g.source, e.gap, e.perHour, time.Now())
		if err != nil {
			return nil, err
		}
		if wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
	}
	actx, cancel := context.WithCancel(ctx)
	deadline := time.Now().Add(tapReconnectTimeout)
	res, err := s.relayFetch(openContext{Context: actx, deadline: deadline}, cancel, g, raw, &http.Request{Header: http.Header{}})
	if err != nil {
		cancel()
		return nil, err
	}
	closer := closeFunc(func() error { cancel(); return res.Body.Close() })
	if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
		closer.Close()
		return nil, providerResponseError(res.StatusCode)
	}
	// The first packets must arrive within the same budget. A provider-wide
	// stall answers and then sends nothing; holding this connection open
	// until the budget ends resumes the moment the provider does, with
	// fewer opens than cutting it and dialling again.
	var silent atomic.Bool
	timer := time.AfterFunc(time.Until(deadline), func() { silent.Store(true); cancel() })
	body := bufio.NewReaderSize(res.Body, 32<<10)
	// The tap is already a transport stream; the splicer resyncs on its own.
	first, perr := body.Peek(1)
	timer.Stop()
	if perr != nil {
		closer.Close()
		if silent.Load() {
			return nil, errTapSilent
		}
		return nil, perr
	}
	if first[0] != 0x47 && !tsPrefix(body) {
		closer.Close()
		return nil, errTapNotTS
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		closer.Close()
		return nil, io.EOF
	}
	t.body = closer
	t.activateSplicedLocked(false)
	t.mu.Unlock()
	return body, nil
}

// tapRefusal reports a provider refusing the open (connection limit,
// authentication, rate) rather than failing: v0.3.5 paces those at one
// second within the grant's 28-attempt ceiling.
func tapRefusal(err error) bool {
	var te *TuneError
	if !errors.As(err, &te) {
		return false
	}
	switch te.UpstreamStatus {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, 458, 509:
		return true
	}
	return false
}

type closeFunc func() error

func (f closeFunc) Close() error { return f() }

// emit tells every grant on the tap about a health change.
func (t *upstreamTap) emit(kind UpstreamEventKind, err error) {
	t.mu.Lock()
	t.publishLocked()
	grants := make([]*inputGrant, 0, len(t.refs))
	for g := range t.refs {
		grants = append(grants, g)
	}
	t.mu.Unlock()
	now := time.Now()
	for _, g := range grants {
		h, _ := g.snapHealth()
		g.notify(UpstreamEvent{Kind: kind, At: now, Health: h, Err: err})
	}
}

// publishLocked refreshes the health snapshot of every grant on the tap.
func (t *upstreamTap) publishLocked() {
	for g := range t.refs {
		g.snap.Store(&healthSnap{h: t.healthForLocked(g), since: &t.dataAt})
	}
}

func (g *inputGrant) snapHealth() (UpstreamHealth, bool) {
	if snap := g.snap.Load(); snap != nil {
		return snap.read(), true
	}
	return UpstreamHealth{}, false
}

// healthForLocked reports the tap's health as seen by grant g's reader.
func (t *upstreamTap) healthForLocked(g *inputGrant) UpstreamHealth {
	now := time.Now()
	liveSocket := !t.closed && t.body != nil
	h := UpstreamHealth{
		Stalled:    t.stalled,
		Failed:     t.failed,
		Reconnects: t.reconnects,
		Bitrate:    t.bitrate,
		LiveSocket: liveSocket,
	}
	if t.ts != nil && t.ts.pmtSig != "" {
		h.FormatGeneration = uint64(t.ts.format) + 1
	}
	h.EdgePTS = -1
	if t.ts != nil {
		h.Kind = "ts"
		h.StallAfter = t.stallThresholdLocked(now)
		h.EdgePTS = t.ts.maxPTS
	}
	h.Connected = liveSocket && !t.stalled
	if !t.lastData.IsZero() {
		h.DataAge = now.Sub(t.lastData)
	}
	var reader, oldest *tapReader
	for rd := range t.active {
		if rd.g != g {
			continue
		}
		h.Readers++
		if reader == nil || rd.off > reader.off {
			reader = rd
		}
		if oldest == nil || rd.off < oldest.off {
			oldest = rd
		}
	}
	if reader != nil && oldest != reader && t.ts != nil && len(t.marks) > 0 {
		h.Spread = time.Duration(t.clockAtLocked(reader.off)-t.clockAtLocked(oldest.off)) * time.Second / tsClockHz
	}
	if reader != nil {
		h.ReserveDepth = t.readerDepthLocked(reader)
		if p := reader.pace; p != nil && p.anchored && t.ts != nil && len(t.marks) > 0 {
			h.PaceAhead = time.Duration(t.clockAtLocked(reader.off)-p.dueClock(now)) * time.Second / tsClockHz
		}
	} else if r, ok := t.resume[g]; ok {
		h.ReserveDepth = t.depthLocked(r.last)
	}
	if g != nil && g.reserve > 0 && t.ts != nil {
		h.Target = g.reserve
		if c := t.capacityLocked() - 4*time.Second; c > 0 && c < h.Target {
			h.Target = c
		}
	}
	return h
}

// sniffInput reads the first bytes of the session's input through its tap,
// inside the tune's budget, and reports the response's content type.
// It joins (or opens) the same provider connection the probe and ffmpeg
// read from.
func (s *Service) sniffInput(ctx context.Context, ss *session, n int) ([]byte, string, error) {
	return s.sniffInputAttempt(ctx, ss, n, false)
}

func (s *Service) sniffInputAttempt(ctx context.Context, ss *session, n int, refreshed bool) ([]byte, string, error) {
	r := &s.inputRelay
	r.mu.Lock()
	g := r.bySess[ss]
	r.mu.Unlock()
	if g == nil || g.ctx.Err() != nil {
		return nil, "", core.ErrNotAvailable
	}
	var raw string
	var directGen uint64
	if g.direct != nil {
		var invalidated bool
		var expStatus int
		raw, _, directGen, invalidated, expStatus = g.syncDirect()
		if invalidated {
			if refreshed {
				return nil, "", providerResponseError(expStatus)
			}
			nextRaw, rerr := g.refreshDirect(ctx, directGen)
			if rerr != nil {
				return nil, "", rerr
			}
			_, dRawHdrs, dHdrs, _, _, _, _ := g.direct.current()
			ss.mu.Lock()
			ss.inputURL = nextRaw
			ss.inputHeaders = dHdrs
			ss.inputSource.URL = nextRaw
			ss.inputSource.Headers = dRawHdrs
			ss.mu.Unlock()
			return s.sniffInputAttempt(ctx, ss, n, true)
		}
	} else {
		raw = g.rootURL()
	}
	if cm := g.peekManifest(raw); cm != nil {
		ss.setSniffedManifest(cm)
		if cm.hls {
			return []byte("#EXTM3U"), "application/vnd.apple.mpegurl", nil
		}
		return []byte("<?xml"), "application/dash+xml", nil
	}
	source, limit := s.tapSource(g)
	_, headers, mediaHeaders := g.requestScope()
	t, cm, created, err := r.taps.acquireOrManifest(g.rootTapKey(raw, headers, mediaHeaders), source, limit, g, func() *cachedManifest {
		return g.peekManifest(raw)
	})
	if err != nil {
		if errors.Is(err, ErrProviderBusy) && limit > 0 {
			s.emitProviderLimitReached(source, limit, limit, g.histChannel)
		}
		return nil, "", err
	}
	if cm != nil {
		ss.setSniffedManifest(cm)
		if cm.hls {
			return []byte("#EXTM3U"), "application/vnd.apple.mpegurl", nil
		}
		return []byte("<?xml"), "application/dash+xml", nil
	}
	if created {
		s.openTap(t, g, raw, http.Header{"User-Agent": {defaultUserAgent()}}, ctx)
	}
	select {
	case <-t.ready:
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if t.openErr != nil || t.status != http.StatusOK && t.status != http.StatusPartialContent {
		if !g.finite && g.login != nil && (t.status == 401 || t.status == 403) {
			src := ss.inputSource
			src.URL = ss.inputURL
			s.revokeInput(ss)
			if err := s.grantInputForContext(ctx, ss, src, ss.inputHeaders, g.steady, g.lead); err != nil {
				return nil, "", err
			}
			return s.sniffInputAttempt(ctx, ss, n, refreshed)
		}
		if !refreshed && g.direct.canResolve() && !errors.Is(t.openErr, ErrProviderPaced) {
			g.direct.invalidate(directGen, t.status)
			return s.sniffInputAttempt(ctx, ss, n, false)
		}
		if t.openErr != nil {
			return nil, "", Diagnose("upstream_headers", t.openErr)
		}
		return nil, "", providerResponseError(t.status)
	}
	if t.isManifest {
		if body := t.manifestCopy(); body != nil {
			cm := newCachedManifest(raw, body, t.base, t.hls)
			g.setManifest(cm)
			ss.setSniffedManifest(cm)
		}
		if t.hls {
			return []byte("#EXTM3U"), t.ctype, nil
		}
		return []byte("<?xml"), t.ctype, nil
	}
	g.setTap(t)
	t.attach()
	defer t.detach()
	out := make([]byte, 0, n)
	var off int64
	for len(out) < n {
		buf := make([]byte, n-len(out))
		got, next, err := t.read(ctx, off, buf)
		out, off = append(out, buf[:got]...), next
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			break
		}
	}
	return out, t.ctype, nil
}

// reserve accounts for VOD and metadata reads in the same mutex as live taps.
func (r *tapRegistry) reserve(source string, limit int, background bool) (func(), error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.inUseLocked(source)
	if (background && n > 0) || (limit > 0 && n >= limit) {
		return nil, ErrProviderBusy
	}
	if r.external == nil {
		r.external = map[string]int{}
	}
	r.external[source]++
	var once sync.Once
	return func() { once.Do(func() { r.mu.Lock(); r.external[source]--; r.mu.Unlock() }) }, nil
}

// tapReport is what a tap last reported to the source history.
type tapReport struct {
	at         time.Time
	clock      int64
	damage     int64
	format     int
	reconnects int
	stalls     int64
}

// tapBurstProbe is how long into a connection the connect burst is read.
const tapBurstProbe = 1500 * time.Millisecond

// measureBurstLocked records how much media the current connection
// delivered beyond real time in its first tapBurstProbe.
func (t *upstreamTap) measureBurstLocked() {
	if t.ts == nil || t.burstDone || t.epochStart.IsZero() || time.Since(t.epochStart) < tapBurstProbe {
		return
	}
	t.burstDone = true
	got := time.Duration(t.ts.clock-t.epochClock) * time.Second / tsClockHz
	t.burst, t.burstNew = max(0, got-tapBurstProbe), true
}

// evidenceLocked is what the tap observed since its last report.
func (t *upstreamTap) evidenceLocked() SourceEvidence {
	ev := SourceEvidence{
		Stalls:     t.stallCount - t.rep.stalls,
		Reconnects: int64(t.reconnects - t.rep.reconnects),
	}
	t.rep.stalls, t.rep.reconnects, t.rep.at = t.stallCount, t.reconnects, time.Now()
	if t.ts == nil {
		return ev
	}
	ev.Media = time.Duration(t.ts.clock-t.rep.clock) * time.Second / tsClockHz
	ev.DamageEvents = t.ts.damage - t.rep.damage
	ev.FormatChanges = int64(t.ts.format - t.rep.format)
	ev.KeyframeGap, t.ts.maxGap = t.ts.maxGap, 0
	if t.burstNew {
		ev.ConnectBurst, ev.burstMeasured, t.burstNew = t.burst, true, false
	}
	t.rep.clock, t.rep.damage, t.rep.format = t.ts.clock, t.ts.damage, t.ts.format
	return ev
}

// report sends the tap's new evidence to the history of each distinct
// source reading it (several grants of one source count once).
func (t *upstreamTap) report() {
	t.mu.Lock()
	ev := t.evidenceLocked()
	seen := map[string]bool{}
	var grants []*inputGrant
	for g := range t.refs {
		if key := sourceHealthKey(g.histChannel, g.histSrc); !seen[key] {
			seen[key] = true
			grants = append(grants, g)
		}
	}
	t.mu.Unlock()
	if ev.empty() {
		return
	}
	for _, g := range grants {
		g.noteEvidence(ev)
	}
}
