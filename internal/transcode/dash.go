package transcode

import (
	"bytes"
	"fmt"
	"html"
	"math"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// DefaultDASHSegmentDuration is the DASH segment length when a request
// leaves it unset; PMS uses 5 s.
const DefaultDASHSegmentDuration = 5 * time.Second

// Encoded video can force short GOPs. Shorter segments also lower the MPD's
// minimum buffer, reducing bytes needed before startup and a seek on weak links.
// Adaptive copy sessions use this grid too, so their encoded lower rungs
// get the same benefit. Standalone copy retains the PMS default above.
const encodedDASHSegmentDuration = 2 * time.Second

// dashChunkDuration is the length ffmpeg's dash muxer cuts chunks at. Chunks
// are cut at keyframes, and Ether regroups them into fixed-duration
// segments by their timestamps, so finer chunks place segment boundaries
// closer to where the MPD says they are.
const dashChunkDuration = time.Second

// DASHURLs returns the initialization and media SegmentTemplate URLs of a
// session, relative to the MPD, with $RepresentationID$ and $Number$.
type DASHURLs func(sessionID string) (initialization, media string)

// PlexDASHURLs is the URL layout real PMS uses (captured from a PMS
// start.mpd): segments live next to start.mpd under session/{id}/.
func PlexDASHURLs(sessionID string) (string, string) {
	base := "session/" + sessionID + "/$RepresentationID$/"
	return base + "header", base + "$Number$.m4s"
}

// dashRep is one Representation of a DASH session's MPD.
type dashRep struct {
	ID         int
	Kind       string // "video" or "audio"
	Codecs     string
	Bandwidth  int // bits per second
	Width      int
	Height     int
	SampleRate int
	Channels   int
}

func (r dashRep) mimeType() string {
	if r.Kind == "audio" {
		return "audio/mp4"
	}
	return "video/mp4"
}

// mpdDuration formats an xs:duration the way PMS does: "PT0H3M37.866S".
func mpdDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	ms := d.Milliseconds()
	h := ms / 3600000
	ms -= h * 3600000
	m := ms / 60000
	ms -= m * 60000
	s := ms / 1000
	ms -= s * 1000
	if ms == 0 {
		return fmt.Sprintf("PT%dH%dM%dS", h, m, s)
	}
	return fmt.Sprintf("PT%dH%dM%d.%03dS", h, m, s, ms)
}

// mpdSeconds formats a short duration: "PT10S", "PT2.5S".
func mpdSeconds(d time.Duration) string {
	return "PT" + strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "S"
}

// renderMPD writes the static, full-length MPD of a session: one
// AdaptationSet per representation, segments numbered from 0 with a fixed
// duration, so players can seek anywhere before anything is transcoded.
// The layout follows real PMS output byte for byte where it can.
func renderMPD(duration, segDur time.Duration, initURL, mediaURL string, reps []dashRep) []byte {
	if segDur <= 0 {
		segDur = DefaultDASHSegmentDuration
	}
	timescale, segTicks := int64(1), int64(segDur/time.Second)
	if segDur%time.Second != 0 {
		timescale, segTicks = 1000, segDur.Milliseconds()
	}
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>` + "\n")
	b.WriteString(`<MPD xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"` + "\n")
	b.WriteString("\t" + `xmlns="urn:mpeg:dash:schema:mpd:2011"` + "\n")
	b.WriteString("\t" + `xmlns:xlink="http://www.w3.org/1999/xlink"` + "\n")
	b.WriteString("\t" + `xsi:schemaLocation="urn:mpeg:dash:schema:mpd:2011 http://standards.iso.org/ittf/PubliclyAvailableStandards/MPEG-DASH_schema_files/DASH-MPD.xsd"` + "\n")
	b.WriteString("\t" + `profiles="urn:mpeg:dash:profile:isoff-live:2011"` + "\n")
	b.WriteString("\t" + `type="static"` + "\n")
	fmt.Fprintf(&b, "\tmediaPresentationDuration=%q\n", mpdDuration(duration))
	fmt.Fprintf(&b, "\tmaxSegmentDuration=%q\n", mpdSeconds(2*segDur))
	// One segment: Shaka (Plex Web) waits for max(minBufferTime, 2s)
	// before starting. Two segments held every start for a second GOP.
	fmt.Fprintf(&b, "\tminBufferTime=%q>\n", mpdSeconds(segDur))
	fmt.Fprintf(&b, "\t<Period start=\"PT0S\" id=\"0\" duration=%q>\n", mpdDuration(duration))
	// Several video representations form one AdaptationSet (an ABR
	// ladder the player may switch within); a single video keeps the
	// historic one-set-per-representation layout byte for byte.
	videoReps := 0
	for _, r := range reps {
		if r.Kind != "audio" {
			videoReps++
		}
	}
	if videoReps > 1 {
		var rest []dashRep
		b.WriteString("\t\t<AdaptationSet segmentAlignment=\"true\" contentType=\"video\">\n")
		fmt.Fprintf(&b, "\t\t\t<SegmentTemplate timescale=\"%d\" duration=\"%d\" initialization=\"%s\" media=\"%s\" startNumber=\"0\">\n",
			timescale, segTicks, html.EscapeString(initURL), html.EscapeString(mediaURL))
		b.WriteString("\t\t\t</SegmentTemplate>\n")
		for _, r := range reps {
			if r.Kind == "audio" {
				rest = append(rest, r)
				continue
			}
			fmt.Fprintf(&b, "\t\t\t<Representation id=\"%d\" mimeType=\"%s\" codecs=\"%s\" bandwidth=\"%d\"",
				r.ID, r.mimeType(), html.EscapeString(r.Codecs), r.Bandwidth)
			if r.Width > 0 && r.Height > 0 {
				fmt.Fprintf(&b, " width=\"%d\" height=\"%d\"", r.Width, r.Height)
			}
			b.WriteString(">\n\t\t\t</Representation>\n")
		}
		b.WriteString("\t\t</AdaptationSet>\n")
		reps = rest
	}
	for _, r := range reps {
		b.WriteString("\t\t<AdaptationSet segmentAlignment=\"true\">\n")
		fmt.Fprintf(&b, "\t\t\t<SegmentTemplate timescale=\"%d\" duration=\"%d\" initialization=\"%s\" media=\"%s\" startNumber=\"0\">\n",
			timescale, segTicks, html.EscapeString(initURL), html.EscapeString(mediaURL))
		b.WriteString("\t\t\t</SegmentTemplate>\n")
		fmt.Fprintf(&b, "\t\t\t<Representation id=\"%d\" mimeType=\"%s\" codecs=\"%s\" bandwidth=\"%d\"",
			r.ID, r.mimeType(), html.EscapeString(r.Codecs), r.Bandwidth)
		if r.Kind == "audio" {
			if r.SampleRate > 0 {
				fmt.Fprintf(&b, " audioSamplingRate=\"%d\"", r.SampleRate)
			}
			b.WriteString(">\n")
			if r.Channels > 0 {
				fmt.Fprintf(&b, "\t\t\t\t<AudioChannelConfiguration schemeIdUri=\"urn:mpeg:dash:23003:3:audio_channel_configuration:2011\" value=\"%d\"/>\n", r.Channels)
			}
		} else {
			if r.Width > 0 && r.Height > 0 {
				fmt.Fprintf(&b, " width=\"%d\" height=\"%d\"", r.Width, r.Height)
			}
			b.WriteString(">\n")
		}
		b.WriteString("\t\t\t</Representation>\n")
		b.WriteString("\t\t</AdaptationSet>\n")
	}
	b.WriteString("\t</Period>\n")
	b.WriteString("</MPD>\n")
	return b.Bytes()
}

// dashSegmentCount is how many $Number$ segments a player derives from the
// MPD: ceil(duration / segment duration).
func dashSegmentCount(d, segDur time.Duration) int {
	if d <= 0 || segDur <= 0 {
		return 0
	}
	return int((d + segDur - 1) / segDur)
}

// dashReps plans the representations of a decision: video first, then the
// selected audio stream. Codecs are estimates until ffmpeg's init segments
// replace them.
func dashReps(info core.MediaInfo, d core.Decision) []dashRep {
	var reps []dashRep
	if v := info.Video(); v != nil {
		r := dashRep{ID: len(reps), Kind: "video"}
		codec := v.Codec
		w, h := v.Width, v.Height
		if d.VideoCopy {
			r.Bandwidth = v.BitrateKbps * 1000
			if r.Bandwidth <= 0 {
				kbps := info.BitrateKbps
				if a := findStream(info, d.AudioStream, "audio"); a != nil {
					kbps -= copiedAudioKbps(a)
				}
				r.Bandwidth = max(kbps, 0) * 1000
			}
		} else {
			codec = d.VideoCodec
			if codec == "" {
				codec = "h264"
			}
			w, h = decisionOutputSize(v, d)
			r.Bandwidth = videoBitrate(info, d) * 1000
		}
		if r.Bandwidth <= 0 {
			r.Bandwidth = bitrateCeiling(h, codec) * 1000
		}
		r.Width, r.Height = w, h
		profile, depth := v.Profile, v.BitDepth
		if !d.VideoCopy {
			profile, depth = "", 8 // libx264 high / x265 main
			if codec == "hevc" && keepsHDR(v) && !d.ToneMap {
				depth = 10
			}
		}
		r.Codecs = estimateCodecs(codec, profile, h, depth)
		reps = append(reps, r)
	}
	if a := findStream(info, d.AudioStream, "audio"); a != nil {
		r := dashRep{ID: len(reps), Kind: "audio"}
		codec := a.Codec
		if d.AudioCopy {
			r.Bandwidth = copiedAudioKbps(a) * 1000
			r.SampleRate = a.SampleRate
			r.Channels = a.Channels
		} else {
			codec = d.AudioCodec
			if codec == "" {
				codec = "aac"
			}
			r.Channels = decisionAudioChannels(d, a)
			r.SampleRate = decisionAudioRate(d)
			r.Bandwidth = audioBitrateKbps(codec, r.Channels) * 1000
		}
		profile := a.Profile
		if !d.AudioCopy {
			profile = ""
		}
		r.Codecs = estimateCodecs(codec, profile, 0, 0)
		reps = append(reps, r)
	}
	return reps
}

// estimateCodecs guesses an RFC 6381 codecs string from probe data, for
// the MPD when no init segment exists yet.
func estimateCodecs(codec, profile string, height, bitDepth int) string {
	switch codec {
	case "h264":
		p := "64" // High
		switch profile {
		case "Baseline", "Constrained Baseline":
			p = "42"
		case "Main":
			p = "4d"
		case "High 10":
			p = "6e"
		}
		level := "28" // 4.0
		switch {
		case height > 1080:
			level = "33" // 5.1
		case height > 0 && height <= 576:
			level = "1e" // 3.0
		case height > 0 && height <= 720:
			level = "1f" // 3.1
		}
		return "avc1." + p + "00" + level
	case "hevc":
		if bitDepth > 8 || profile == "Main 10" {
			return "hvc1.2.4.L150.90"
		}
		return "hvc1.1.6.L120.90"
	case "av1":
		if bitDepth > 8 {
			return "av01.0.08M.10"
		}
		return "av01.0.08M.08"
	case "vp9":
		return "vp09.00.40.08"
	case "aac":
		if profile == "HE-AAC" {
			return "mp4a.40.5"
		}
		return "mp4a.40.2"
	case "mp3":
		return "mp4a.6b"
	case "ac3":
		return "ac-3"
	case "eac3":
		return "ec-3"
	}
	return codec // opus, flac, alac
}

// dashVideoOK and dashAudioOK are the codecs a DASH session can carry in
// fMP4 for browsers.
func dashVideoOK(codec string) bool { return hlsVideoOK(codec) }

func dashAudioOK(codec string) bool {
	switch codec {
	case "aac", "ac3", "eac3", "mp3", "flac", "opus":
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Regrouping ffmpeg's chunks into fixed-duration segments.

// slotOut is one finished segment of a track: its chunks, or (alias >= 0)
// a copy of an earlier segment whose content spans this one too (a GOP
// longer than a segment).
type slotOut struct {
	n      int
	chunks []fmp4Chunk
	alias  int
}

// slotter assigns one track's chunks, in order, to segments: segment n
// holds the chunks starting in [n*segDur, (n+1)*segDur) (with a little
// tolerance for encoder rounding), except that a run's first chunk always
// belongs to the segment the run started at (a remux starts at the
// keyframe before the seek point).
type slotter struct {
	segDur   time.Duration
	base     time.Duration
	total    int // segments in the MPD; 0 = unknown
	tol      time.Duration
	startSeg int
	// index, when set, maps a media time to its segment for plans whose
	// boundaries are not uniform (keyframe-planned HLS sessions) instead
	// of base+n*segDur.
	index func(t time.Duration) int

	started  bool
	slot     int // segment being filled; -1 none
	pending  []fmp4Chunk
	floor    int // earliest segment the next chunk may go to
	last     int // last finished segment (data or alias); -1 none
	lastData int // last segment with its own chunks; -1 none
	count    int // segments finished
}

func newSlotter(base, segDur time.Duration, total, startSeg int) *slotter {
	tol := 100 * time.Millisecond
	if tol > segDur/4 {
		tol = segDur / 4
	}
	return &slotter{segDur: segDur, base: base, total: total, tol: tol, startSeg: startSeg,
		slot: -1, floor: startSeg, last: -1, lastData: -1}
}

func (sl *slotter) slotOf(t time.Duration) int {
	var n int
	if sl.index != nil {
		n = sl.index(t + sl.tol)
	} else {
		n = int(math.Floor(float64(t-sl.base+sl.tol) / float64(sl.segDur)))
	}
	if n < 0 {
		n = 0
	}
	if sl.total > 0 && n > sl.total-1 {
		n = sl.total - 1
	}
	return n
}

// add places a chunk and returns the segments it completed.
func (sl *slotter) add(c fmp4Chunk) []slotOut {
	var out []slotOut
	n := sl.startSeg
	if sl.started {
		n = sl.slotOf(c.start)
	}
	sl.started = true
	if n < sl.floor {
		n = sl.floor
	}
	if sl.slot >= 0 && n > sl.slot {
		out = append(out, sl.finish(n)...)
	} else if sl.slot < 0 && sl.lastData >= 0 && n > sl.last+1 {
		out = append(out, sl.aliases(sl.last+1, n)...)
	}
	sl.slot = n
	sl.pending = append(sl.pending, c)
	// The chunk runs into a later segment, so nothing more starts in this
	// one: it is complete now rather than when the next chunk shows up.
	if e := sl.slotOf(c.end); e > sl.slot {
		out = append(out, sl.finish(e)...)
	}
	return out
}

// finish closes the pending segment; segments up to (not including) next
// that no chunk starts in become aliases of it.
func (sl *slotter) finish(next int) []slotOut {
	if sl.slot < 0 {
		return nil
	}
	out := []slotOut{{n: sl.slot, chunks: sl.pending, alias: -1}}
	sl.lastData = sl.slot
	sl.last = sl.slot
	sl.count++
	sl.pending = nil
	sl.slot = -1
	out = append(out, sl.aliases(sl.last+1, next)...)
	if next > sl.floor {
		sl.floor = next
	}
	return out
}

func (sl *slotter) aliases(from, to int) []slotOut {
	if sl.total > 0 && to > sl.total {
		to = sl.total
	}
	var out []slotOut
	for k := from; k < to; k++ {
		out = append(out, slotOut{n: k, alias: sl.lastData})
		sl.last = k
		sl.count++
	}
	return out
}

// flush ends the track: at the end of the input the pending segment is
// complete (and a final segment the MPD rounds up to becomes an alias);
// otherwise (ffmpeg stopped or failed) it is dropped.
func (sl *slotter) flush(eof bool) []slotOut {
	if !eof {
		sl.pending, sl.slot = nil, -1
		return nil
	}
	out := sl.finish(sl.slot + 1)
	if sl.total > 0 && sl.lastData >= 0 && sl.last < sl.total-1 && sl.total-1-sl.last <= 1 {
		out = append(out, sl.aliases(sl.last+1, sl.total)...)
	}
	return out
}

// Live manifests advertise only completed segments, with a bounded rewind window.
func renderLiveMPD(start, now time.Time, segDur time.Duration, last int, initURL, mediaURL string, reps []dashRep) []byte {
	if segDur <= 0 {
		segDur = DefaultDASHSegmentDuration
	}
	first := max(0, last-int(120*time.Second/segDur)+1)
	var b bytes.Buffer
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="utf-8"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic" profiles="urn:mpeg:dash:profile:isoff-live:2011" availabilityStartTime="%s" publishTime="%s" minimumUpdatePeriod="PT2S" timeShiftBufferDepth="PT120S" suggestedPresentationDelay="PT8S" minBufferTime="%s"><Period id="0" start="PT0S">`, start.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), mpdSeconds(segDur))
	for _, r := range reps {
		fmt.Fprintf(&b, `<AdaptationSet segmentAlignment="true" contentType="%s"><SegmentTemplate timescale="1000" initialization="%s" media="%s" startNumber="%d"><SegmentTimeline>`, r.Kind, html.EscapeString(initURL), html.EscapeString(mediaURL), first)
		if last >= 0 {
			fmt.Fprintf(&b, `<S t="%d" d="%d" r="%d"/>`, int64(first)*segDur.Milliseconds(), segDur.Milliseconds(), last-first)
		}
		fmt.Fprintf(&b, `</SegmentTimeline></SegmentTemplate><Representation id="%d" mimeType="%s" codecs="%s" bandwidth="%d"`, r.ID, r.mimeType(), html.EscapeString(r.Codecs), r.Bandwidth)
		if r.Width > 0 {
			fmt.Fprintf(&b, ` width="%d" height="%d"`, r.Width, r.Height)
		}
		if r.SampleRate > 0 {
			fmt.Fprintf(&b, ` audioSamplingRate="%d"`, r.SampleRate)
		}
		b.WriteString(`/></AdaptationSet>`)
	}
	b.WriteString(`</Period></MPD>`)
	return b.Bytes()
}
