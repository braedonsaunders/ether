package hlsplay

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/splicecheck"
)

type Config struct {
	Protocol, Prefix, CredentialsFile, DeviceID                     string
	DirectPlay, DirectStream                                        bool
	SkipDecision                                                    bool
	MaxVideoBitrate                                                 int
	VideoResolution                                                 string
	ProducerEvidence                                                string
	BaseURL, Token, RatingKey, Output, Session, Host                string
	Window, ForwardBuffer, RequestTimeout, Backoff, SegmentDuration time.Duration
	Tries                                                           int
	FFmpeg, FFprobe                                                 string
	Reference                                                       []splicecheck.Segment
}
type Attempt struct {
	Kind                           string
	Index                          int
	RequestedAt                    time.Time
	ElapsedSeconds                 float64
	Status, Bytes                  int
	SHA256                         string
	FirstByteSeconds, TotalSeconds float64
	Try                            int
	Error                          string `json:",omitempty"`
}
type Segment struct {
	Index                  int
	Path                   string
	DurationSeconds        float64
	ReceivedAt             time.Time
	Status, Bytes, Retries int
	SHA256                 string
	FirstByteSeconds       float64
	// Generation is unknown unless a trusted server observer supplies it.
	Generation uint64
}
type Continuity struct {
	Verified bool
	Error    string
	Report   splicecheck.Report
}
type Report struct {
	Protocol                                     string
	DecisionSkipped                              bool
	Session                                      string
	StartedAt                                    time.Time
	StartupSeconds, ElapsedSeconds               float64
	Outcome, Error                               string
	WindowComplete, IndicesExact, PlaybackPassed bool
	Segments                                     []Segment
	Attempts                                     []Attempt
	Buffer                                       []Sample
	Stalls                                       []Stall
	Retries                                      int
	Continuity                                   Continuity
}
type player struct {
	playSession, itemID, mediaSource string
	c                                Config
	client                           *http.Client
	start                            time.Time
	mu                               sync.Mutex
	attempts                         []Attempt
	events                           *os.File
}

func (p *player) elapsed() float64 { return time.Since(p.start).Seconds() }
func (p *player) event(v any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = json.NewEncoder(p.events).Encode(v)
}
func (p *player) fetch(ctx context.Context, raw, kind string, index int, retry bool) ([]byte, error) {
	return p.request(ctx, raw, kind, index, retry, "GET", nil)
}
func (p *player) request(ctx context.Context, raw, kind string, index int, retry bool, method string, body []byte) ([]byte, error) {
	u, e := resolve(p.c.BaseURL, raw)
	if e != nil {
		return nil, e
	}
	tries := 1
	if retry {
		tries = p.c.Tries
	}
	for n := 1; n <= tries; n++ {
		a := Attempt{Kind: kind, Index: index, RequestedAt: time.Now().UTC(), ElapsedSeconds: p.elapsed(), Try: n, FirstByteSeconds: -1}
		begin := time.Now()
		req, e := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		if e != nil {
			return nil, fmt.Errorf("request construction failed")
		}
		if p.isMediaBrowser() {
			req.Header.Set("X-Emby-Token", p.c.Token)
			req.Header.Set("X-Emby-Device-Id", p.c.DeviceID)
			scheme := "MediaBrowser"
			if p.c.Protocol == "emby" {
				scheme = "Emby"
			}
			req.Header.Set("Authorization", scheme+` Client="`+p.mediaBrowserProduct()+`", Device="Ether lab", DeviceId="`+p.c.DeviceID+`", Version="10.10.0"`)
		} else {
			req.Header.Set("X-Plex-Token", p.c.Token)
			req.Header.Set("X-Plex-Client-Identifier", p.c.Session)
			req.Header.Set("X-Plex-Session-Identifier", p.c.Session)
			req.Header.Set("X-Plex-Product", "Plex Web")
			req.Header.Set("X-Plex-Platform", "Chrome")
		}
		if method == "POST" {
			req.Header.Set("Content-Type", "application/json")
		}

		req.Header.Set("Accept", "application/json")
		if p.c.Host != "" {
			req.Host = p.c.Host
		}
		var first time.Time
		trace := &httptrace.ClientTrace{GotFirstResponseByte: func() { first = time.Now() }}
		resp, err := p.client.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), trace)))
		var data []byte
		if err == nil {
			a.Status = resp.StatusCode
			data, err = io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
			resp.Body.Close()
			if len(data) > 32<<20 {
				err = fmt.Errorf("response bound")
			}
			if len(data) == 0 && kind == "segment" {
				err = fmt.Errorf("empty segment")
			}
		}
		if !first.IsZero() {
			a.FirstByteSeconds = first.Sub(begin).Seconds()
		}
		a.TotalSeconds = time.Since(begin).Seconds()
		a.Bytes = len(data)
		if err != nil {
			a.Error = "transport or body failure"
		}
		sum := sha256.Sum256(data)
		a.SHA256 = hex.EncodeToString(sum[:])
		p.mu.Lock()
		p.attempts = append(p.attempts, a)
		p.mu.Unlock()
		p.event(a)
		if err == nil && (a.Status == 200 || method == "POST" && a.Status == 204) {
			return data, nil
		}
		retriable := err != nil || a.Status == 404 || a.Status >= 500
		if !retriable || n == tries {
			return nil, fmt.Errorf("%s request failed (HTTP %d)", kind, a.Status)
		}
		delay := p.c.Backoff * time.Duration(1<<min(n-1, 4))
		if !sleep(ctx, delay) {
			return nil, ctx.Err()
		}
	}
	return nil, fmt.Errorf("request exhausted")
}
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// Run writes durable, credential-free events and JSON/Markdown outcomes, including
// failed sessions. The output directory must be new. Callers supply a lab token.
func Run(ctx context.Context, c Config) (r Report, err error) {
	if c.Window <= 0 || c.ForwardBuffer <= 0 || c.RequestTimeout <= 0 || c.Tries < 1 || c.Backoff <= 0 || c.SegmentDuration <= 0 {
		return r, fmt.Errorf("positive timing and retry settings required")
	}
	base, e := url.Parse(c.BaseURL)
	if e != nil || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return r, fmt.Errorf("invalid origin")
	}
	if (c.Token == "" && c.CredentialsFile == "") || c.RatingKey == "" {
		return r, fmt.Errorf("viewer token and rating key required")
	}
	if c.Protocol == "" {
		c.Protocol = "plex"
	}
	if c.Protocol != "plex" && c.Protocol != "jellyfin" && c.Protocol != "emby" {
		return r, fmt.Errorf("protocol must be plex, jellyfin or emby")
	}
	if c.Prefix == "" {
		c.Prefix = "/" + c.Protocol
	}
	if c.Session == "" {
		var nonce [12]byte
		if _, e = rand.Read(nonce[:]); e != nil {
			return r, e
		}
		c.Session = "hlsplay-" + hex.EncodeToString(nonce[:])
	}
	if c.DeviceID == "" {
		c.DeviceID = c.Session
	}
	if e = os.Mkdir(c.Output, 0700); e != nil {
		return r, e
	}
	events, e := os.OpenFile(filepath.Join(c.Output, "events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return r, e
	}
	defer events.Close()
	p := &player{c: c, start: time.Now(), events: events, client: &http.Client{Timeout: c.RequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	r = Report{Protocol: c.Protocol, Session: c.Session, StartedAt: p.start.UTC(), DecisionSkipped: c.SkipDecision, IndicesExact: true, Outcome: "failed"}
	b := Buffer{}
	defer func() {
		if p.playSession != "" {
			r.Session = p.playSession
		}
		b.Advance(min(p.elapsed(), c.Window.Seconds()))
		r.Stalls = b.Stalls
		r.ElapsedSeconds = min(p.elapsed(), c.Window.Seconds())
		p.mu.Lock()
		r.Attempts = append([]Attempt(nil), p.attempts...)
		p.mu.Unlock()
		for _, a := range r.Attempts {
			if a.Try > 1 {
				r.Retries++
			}
		}
		if err != nil {
			r.Error = err.Error()
		}
		if c.ProducerEvidence != "" {
			rowsData, readErr := os.ReadFile(c.ProducerEvidence)
			var rows []struct {
				Session    string `json:"session"`
				Generation uint64 `json:"generation"`
				Index      int    `json:"segmentIndex"`
			}
			if readErr == nil {
				readErr = json.Unmarshal(rowsData, &rows)
			}
			if readErr != nil {
				r.Error = "producer evidence unavailable"
				r.IndicesExact = false
			} else {
				for i := range r.Segments {
					for _, row := range rows {
						if row.Session == r.Session && row.Index == r.Segments[i].Index {
							r.Segments[i].Generation = row.Generation
						}
					}
				}
			}
		}
		segs := make([]splicecheck.Segment, 0, len(r.Segments))
		for _, s := range r.Segments {
			segs = append(segs, splicecheck.Segment{Index: s.Index, Path: filepath.Join(c.Output, s.Path), Generation: s.Generation})
		}
		if len(segs) > 0 {
			checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			defer cancel()
			cr, ce := splicecheck.Check(checkCtx, segs, splicecheck.Options{SegmentDuration: c.SegmentDuration, Reference: matchingReference(c.Reference, segs), FFprobe: c.FFprobe, FFmpeg: c.FFmpeg})
			r.Continuity.Report = cr
			if ce != nil {
				r.Continuity.Error = ce.Error()
			} else {
				r.Continuity.Verified = cr.FrameCountVsReference.Pass && cr.IDRStart.Pass && cr.ClosedGOP.Pass && cr.PTSContinuity.Pass && cr.CleanDecode.Pass && cr.AudioAUExact.Pass && cr.AudioContinuity.Pass && cr.GridDurations.Pass
			}
		}
		r.PlaybackPassed = r.WindowComplete && r.IndicesExact && len(r.Stalls) == 0 && len(r.Segments) > 0
		if c.SkipDecision && r.PlaybackPassed {
			r.Outcome = "diagnostic-only"
		} else if r.PlaybackPassed && r.Continuity.Verified {
			r.Outcome = "passed"
		} else if r.PlaybackPassed {
			r.Outcome = "continuity-unverified"
		}
		data, je := json.MarshalIndent(r, "", "  ")
		if je == nil {
			je = os.WriteFile(filepath.Join(c.Output, "report.json"), append(data, '\n'), 0600)
		}
		if je != nil && err == nil {
			err = je
		}
		md := fmt.Sprintf("# HLS playback measurement\n\nOutcome: **%s**\n\nSegments: %d; retries: %d; stalls: %d; startup: %.3fs.\n\nPlayback window complete: %t. Indices exact: %t. Continuity verified: %t.\n\n%s\n", r.Outcome, len(r.Segments), r.Retries, len(r.Stalls), r.StartupSeconds, r.WindowComplete, r.IndicesExact, r.Continuity.Verified, r.Error)
		if me := os.WriteFile(filepath.Join(c.Output, "report.md"), []byte(md), 0600); me != nil && err == nil {
			err = me
		}
	}()
	runCtx, cancel := context.WithTimeout(ctx, c.Window)
	defer cancel()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		p.finish(cleanup, b.Played)
	}()
	absolute, data, openErr := p.open(runCtx)
	if openErr != nil {
		err = openErr
		return
	}
	pl, e := parsePlaylist(absolute, data)
	if e != nil {
		err = e
		return
	}
	variant := absolute
	if pl.variant != "" {
		variant = pl.variant
		pl = playlist{}
	}
	var next *int
	seen := map[int]bool{}
	nextTimeline := 0.0
	ended := false
	for runCtx.Err() == nil {
		now := p.elapsed()
		b.Advance(now)
		sample := Sample{now, b.Level, b.Played}
		r.Buffer = append(r.Buffer, sample)
		p.event(sample)
		if b.Started && now >= nextTimeline {
			nextTimeline = now + 5
			state := "playing"
			if b.Level == 0 {
				state = "buffering"
			}
			_ = p.progress(runCtx, state, b.Played, false)

		}
		if b.Started && b.Level >= c.ForwardBuffer.Seconds() {
			sleep(runCtx, 100*time.Millisecond)
			continue
		}
		if ended {
			if b.Level <= 0 {
				err = fmt.Errorf("title ended before bounded window")
				return
			}
			sleep(runCtx, 100*time.Millisecond)
			continue
		}
		if len(pl.entries) == 0 {
			data, err = p.fetch(runCtx, variant, "variant", -1, true)
			if err != nil {
				break
			}
			pl, err = parsePlaylist(variant, data)
			if err != nil {
				break
			}
			if pl.variant != "" {
				err = fmt.Errorf("nested master unsupported")
				break
			}
		}
		progressed := false
		for _, en := range pl.entries {
			if seen[en.index] {
				continue
			}
			if b.Started && b.Level >= c.ForwardBuffer.Seconds() {
				break
			}
			if next != nil && en.index != *next {
				r.IndicesExact = false
				err = fmt.Errorf("missing segment index %d", *next)
				return
			}
			data, err = p.fetch(runCtx, en.url, "segment", en.index, true)
			if err != nil {
				break
			}
			path := fmt.Sprintf("segment-%06d.ts", en.index)
			if err = os.WriteFile(filepath.Join(c.Output, path), data, 0600); err != nil {
				return
			}
			sum := sha256.Sum256(data)
			s := Segment{Index: en.index, Path: path, DurationSeconds: en.duration, ReceivedAt: time.Now().UTC(), Status: 200, Bytes: len(data), SHA256: hex.EncodeToString(sum[:])}
			p.mu.Lock()
			for _, a := range p.attempts {
				if a.Kind == "segment" && a.Index == en.index {
					s.FirstByteSeconds = a.FirstByteSeconds
					s.Retries = a.Try - 1
				}
			}
			p.mu.Unlock()
			r.Segments = append(r.Segments, s)
			p.event(s)
			seen[en.index] = true
			n := en.index + 1
			next = &n
			b.Add(p.elapsed(), en.duration)
			progressed = true
			if !b.Started && b.Level >= c.ForwardBuffer.Seconds() {
				b.Started = true
				if c.Protocol != "plex" {
					_ = p.progress(runCtx, "playing", b.Played, true)
				}
				r.StartupSeconds = p.elapsed()
				p.event(map[string]any{"kind": "playing", "elapsedSeconds": r.StartupSeconds, "wallTime": time.Now().UTC(), "session": p.playSession})
			}
		}
		if err != nil {
			break
		}
		all := true
		for _, en := range pl.entries {
			if !seen[en.index] {
				all = false
				break
			}
		}
		if all && pl.end {
			ended = true
			if !b.Started {
				b.Started = true
				if c.Protocol != "plex" {
					_ = p.progress(runCtx, "playing", b.Played, true)
				}
				r.StartupSeconds = p.elapsed()
			}
		}
		if all {
			pl.entries = nil
		}
		if !progressed {
			sleep(runCtx, 100*time.Millisecond)
		}
	}
	// Continue consuming the forward buffer after terminal fetch exhaustion so the
	// report measures whether an outage actually reaches the client's playhead.
	for err != nil && runCtx.Err() == nil {
		b.Advance(min(p.elapsed(), c.Window.Seconds()))
		r.Buffer = append(r.Buffer, Sample{p.elapsed(), b.Level, b.Played})
		sleep(runCtx, 100*time.Millisecond)
	}
	b.Advance(min(p.elapsed(), c.Window.Seconds()))
	r.WindowComplete = ctx.Err() == nil && b.Started && p.elapsed() >= c.Window.Seconds()
	if r.WindowComplete {
		err = nil
	}
	return
}

// LoadReference reads a prior hlsplay report and resolves its artifact paths.
func LoadReference(path string) ([]splicecheck.Segment, error) {
	data, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	var r Report
	if e = json.Unmarshal(data, &r); e != nil {
		return nil, e
	}
	if !r.PlaybackPassed {
		return nil, fmt.Errorf("reference capture did not pass playback")
	}
	var out []splicecheck.Segment
	for _, s := range r.Segments {
		out = append(out, splicecheck.Segment{Index: s.Index, Path: filepath.Join(filepath.Dir(path), s.Path), Generation: s.Generation})
	}
	return out, nil
}

// Match the reference to the fetched bounded window; never compare a shorter
// client observation against an unrelated tail from a longer reference run.
func matchingReference(ref, segs []splicecheck.Segment) []splicecheck.Segment {
	if len(segs) == 0 {
		return nil
	}
	var out []splicecheck.Segment
	for _, s := range ref {
		if s.Index >= segs[0].Index && s.Index <= segs[len(segs)-1].Index {
			out = append(out, s)
		}
	}
	return out
}
