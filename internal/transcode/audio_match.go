package transcode

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// Source matching is deliberately not programme loudness normalization. It
// compares the same passage in two releases, then holds ONE gain for the new
// source. No silence riding, AGC, or change to the viewer's processing rules.
const audioMatchDuration = 6 * time.Second

// defaultAudioMatchBudget keeps the loudness analysis off the swap's
// critical path; WithAudioMatchBudget overrides it.
const defaultAudioMatchBudget = 1500 * time.Millisecond
const audioMatchRate = 8000

type audioMatchKey struct {
	from, to           string
	oldTrack, newTrack int
	gain               float64
}
type audioMatchState struct {
	gain       float64
	forcedCopy bool // the matcher, rather than the decider, disabled audio copy
	prepared   map[audioMatchKey]float64
}

// prepareAudioMatch is best-effort and only spends time while media is already
// buffered. At most one pair runs per Manager, with two single-thread decoders,
// a shared deadline (1.5s by default), bounded PCM, and joined cancellation. A cold or broken
// input never delays an unbuffered rescue and never rejects a playable source.
func (s *Session) prepareAudioMatch(ctx context.Context, input string, info core.MediaInfo, at time.Duration) {
	s.mu.Lock()
	old := findStream(s.info, s.decision.AudioStream, "audio")
	next := matchStream(info, old, "audio")
	b := s.recoveryBufferLocked()
	eligible := !s.closed && !s.liveInput && s.container == ContainerMatroska && !s.rtxTimeline && len(s.rungs) <= 1 && len(s.inputHeaders) == 0 && len(s.decision.AudioFilters) == 0 && input != s.input && b.ReadyUntil-b.Position >= 12*time.Second && audioMatchTracks(old, next)
	key := audioMatchKey{from: s.input, to: input, gain: s.audioMatch.gain}
	if old != nil {
		key.oldTrack = old.Index
	}
	if next != nil {
		key.newTrack = next.Index
	}
	_, cached := s.audioMatch.prepared[key]
	// Only supported, already-negotiated output codecs; do not silently lose
	// lossless/object audio or change the client's decoder to apply a gain.
	codec := s.decision.AudioCodec
	if s.decision.AudioCopy || s.audioMatch.forcedCopy {
		if old != nil {
			codec = old.Codec
		}
	}
	// AAC HE has different decoder configuration; E-AC-3 copy may carry JOC
	// even if the probe did not identify Atmos. Never flatten either implicitly.
	if old != nil && (s.decision.AudioCopy || s.audioMatch.forcedCopy) {
		eligible = eligible && (codec == "ac3" || codec == "aac" && old.Profile == "LC")
	}
	eligible = eligible && (codec == "aac" || codec == "ac3" || codec == "eac3") && s.m.feat.hasEncoder(codec) && s.m.feat.hasFilter("alimiter")
	s.mu.Unlock()
	if !eligible || cached || !s.m.audioMatchBusy.CompareAndSwap(false, true) {
		return
	}
	defer s.m.audioMatchBusy.Store(false)
	budget := s.m.opts.audioMatchBudget
	if budget <= 0 {
		budget = defaultAudioMatchBudget
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// Use a passage behind the buffered boundary, normally already in the
	// source cache, rather than fetching further ahead of playback.
	at = max(0, min(at, b.ReadyUntil)-audioMatchDuration)
	var pcm [2][]byte
	var wg sync.WaitGroup
	for i, src := range []struct {
		url   string
		track int
	}{{key.from, key.oldTrack}, {key.to, key.newTrack}} {
		wg.Add(1)
		go func(i int, input string, track int) {
			defer wg.Done()
			pcm[i] = s.sampleAudio(ctx, input, track, at)
		}(i, src.url, src.track)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	delta, ok := matchingAudioGain(pcm[0], pcm[1])
	if !ok {
		return
	}
	gain := boundedAudioGain(key.gain + delta)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.input != key.from || s.decision.AudioStream != key.oldTrack || s.audioMatch.gain != key.gain || len(s.decision.AudioFilters) != 0 {
		return
	}
	if s.audioMatch.prepared == nil {
		s.audioMatch.prepared = make(map[audioMatchKey]float64)
	}
	if len(s.audioMatch.prepared) >= 16 {
		clear(s.audioMatch.prepared)
	}
	s.audioMatch.prepared[key] = gain
	s.log.Info("replacement audio level matched", "gainDB", gain, "differenceDB", math.Round(delta*10)/10)
}

func audioMatchTracks(a, b *core.StreamInfo) bool {
	if a == nil || b == nil || a.Channels <= 0 || a.Channels > 6 || a.Channels != b.Channels || a.Language != b.Language || a.ChannelLayout != b.ChannelLayout {
		return false
	}
	for _, v := range []*core.StreamInfo{a, b} {
		p := strings.ToLower(v.Profile + " " + v.Title)
		if strings.Contains(p, "atmos") || strings.Contains(p, "joc") || strings.Contains(p, "commentary") || strings.Contains(p, "description") {
			return false
		}
		switch v.Codec {
		case "aac", "ac3", "eac3", "flac", "pcm_s16le", "pcm_s24le":
		default:
			return false
		}
	}
	return true
}

func (s *Session) sampleAudio(ctx context.Context, input string, track int, at time.Duration) []byte {
	// HTTP samples must stay on Ether's loopback source boundary. No credentials
	// or arbitrary upstream URL are copied into a second process command line.
	u, err := url.Parse(input)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil {
		return nil
	}
	q := u.Query()
	q.Set("background", "1")
	u.RawQuery = q.Encode()
	args := []string{"-hide_banner", "-v", "error", "-nostdin", "-threads", "1", "-filter_threads", "1", "-probesize", "1048576", "-analyzeduration", "500000"}
	args = append(args, inputSafetyArgs(input, false)...)
	args = append(args, httpInputArgs(input, false)...)
	args = append(args, "-ss", secs(at), "-i", u.String(), "-map", fmt.Sprintf("0:%d", track), "-vn", "-sn", "-dn", "-t", secs(audioMatchDuration+100*time.Millisecond), "-ac", "1", "-ar", "8000", "-c:a", "pcm_f32le", "-f", "f32le", "-")
	// Allow encoder priming at the cut, but compare exactly six seconds.
	const sampleBytes = audioMatchRate * 6 * 4
	out := &pictureSample{limit: sampleBytes + audioMatchRate*4/10}
	if s.encoderRun(ctx, Command{Path: s.m.ffmpeg, Args: args, Stdout: out, Stderr: io.Discard}) != nil || out.Len() < sampleBytes {
		return nil
	}
	return out.Bytes()[:sampleBytes]
}

// Compare 100ms energy envelopes. Require at least three seconds of audible,
// varying, strongly correlated content, and a stable ratio in >=90% of bins.
// Permit small mux/encoder offsets, not different scenes or arbitrary edits.
// This is relative level matching, not an estimate of integrated LUFS.
func matchingAudioGain(a, b []byte) (float64, bool) {
	envelope := func(raw []byte) []float64 {
		if len(raw) != audioMatchRate*6*4 {
			return nil
		}
		out := make([]float64, 60)
		for i := range out {
			sum := 0.0
			for j := 0; j < 800; j++ {
				v := float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[(i*800+j)*4:])))
				if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 4 {
					return nil
				}
				sum += v * v
			}
			out[i] = 10 * math.Log10(max(sum/800, 1e-12))
		}
		return out
	}
	x, y := envelope(a), envelope(b)
	if x == nil || y == nil {
		return 0, false
	}
	best, gain := 0.0, 0.0
	bestLag := 0
	for lag := -3; lag <= 3; lag++ {
		var xs, ys, ds []float64
		for i, v := range x {
			j := i + lag
			if j < 0 || j >= len(y) || v < -45 || y[j] < -45 {
				continue
			}
			xs = append(xs, v)
			ys = append(ys, y[j])
			ds = append(ds, v-y[j])
		}
		if len(ds) < 30 {
			continue
		}
		mx, my := 0.0, 0.0
		for i := range xs {
			mx += xs[i]
			my += ys[i]
		}
		mx /= float64(len(xs))
		my /= float64(len(ys))
		xx, yy, xy := 0.0, 0.0, 0.0
		for i := range xs {
			dx, dy := xs[i]-mx, ys[i]-my
			xx += dx * dx
			yy += dy * dy
			xy += dx * dy
		}
		if xx/float64(len(xs)) < 4 || yy/float64(len(ys)) < 4 {
			continue
		} // no steady tone / silence
		corr := xy / math.Sqrt(xx*yy)
		if corr < 0.97 || corr <= best {
			continue
		}
		sort.Float64s(ds)
		med := ds[len(ds)/2]
		stable := 0
		for _, d := range ds {
			if math.Abs(d-med) <= 0.8 {
				stable++
			}
		}
		if stable*10 < len(ds)*9 || math.Abs(med) > 12 {
			continue
		}
		best, gain = corr, med
		bestLag = lag
	}
	if best == 0 {
		return 0, false
	}
	// An envelope alone could match unrelated music with a similar rhythm.
	// Require the actual waveform to agree too (allow polarity and up to half
	// a bin of extra encoder delay). Sparse sampling bounds this to ~5M ops.
	samples := func(raw []byte) []float64 {
		out := make([]float64, len(raw)/4)
		for i := range out {
			out[i] = float64(math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:])))
		}
		return out
	}
	ax, by := samples(a), samples(b)
	for lag := bestLag*800 - 400; lag <= bestLag*800+400; lag++ {
		xx, yy, xy := 0.0, 0.0, 0.0
		for i := 800; i < len(ax)-800; i += 8 {
			j := i + lag
			if j < 0 || j >= len(by) {
				continue
			}
			xx += ax[i] * ax[i]
			yy += by[j] * by[j]
			xy += ax[i] * by[j]
		}
		if xx > 0 && yy > 0 && math.Abs(xy)/math.Sqrt(xx*yy) >= 0.85 {
			return gain, true
		}
	}
	return 0, false
}

func boundedAudioGain(gain float64) float64 {
	if math.IsNaN(gain) || math.IsInf(gain, 0) || math.Abs(gain) < 1.5 {
		return 0
	}
	return math.Round(max(-6, min(3, gain))*10) / 10
}

// Caller holds mu. Remove our old copy override before adapting the next
// source; the gain is absolute, never a growing stack of volume filters.
func (s *Session) audioMatchBaseDecision() core.Decision {
	d := s.decision
	if s.audioMatch.forcedCopy {
		d.AudioCopy = true
	}
	return d
}
func (s *Session) applyAudioMatchLocked(input string, next core.Decision) core.Decision {
	if input == s.input {
		next.AudioCopy = s.decision.AudioCopy
		next.AudioCodec = s.decision.AudioCodec
		next.AudioChannels = s.decision.AudioChannels
		next.AudioSampleRate = s.decision.AudioSampleRate
		return next
	}
	key := audioMatchKey{from: s.input, to: input, oldTrack: s.decision.AudioStream, newTrack: next.AudioStream, gain: s.audioMatch.gain}
	gain := s.audioMatch.prepared[key]
	s.audioMatch.gain = 0
	s.audioMatch.forcedCopy = false
	if gain == 0 || len(next.AudioFilters) != 0 {
		return next
	}
	if a := findStream(s.info, s.decision.AudioStream, "audio"); a != nil {
		if next.AudioCopy {
			next.AudioCodec = a.Codec
			next.AudioChannels = a.Channels
			next.AudioSampleRate = a.SampleRate
			next.AudioCopy = false
			s.audioMatch.forcedCopy = true
		}
		s.audioMatch.gain = gain
		next.Reason += "; source audio level matched"
	}
	return next
}
