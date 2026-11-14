package transcode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"strconv"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
)

// ValidCopyTimeline checks shape, not authenticity. The private source-owner
// handshake must match the fact against the pinned source before worker start.
func ValidCopyTimeline(q core.HLSRequest) bool {
	p, d := q.Info.CopyTimeline, q.Decision
	if p == nil || p.Version != 1 || p.Origin < 0 || p.Origin > 90000*3600 || p.Step != 172800 || p.End <= p.Step || p.End > 90000*120 || p.End%p.Step != 0 || p.VideoAUs != p.End/3600 || p.AudioAUs != p.End/1920 || p.PacketSHA256 == ([32]byte{}) {
		return false
	}
	if q.Info.Container != "mpegts" || q.Info.Estimated || q.Info.Duration != time.Duration(p.End)*time.Second/90000 || q.SegmentDuration != 1920*time.Millisecond || q.Ramp.Segments != 0 || q.StartOffset != 0 || q.LiveInput || q.CopyTimestamps || q.RecoveryStart != nil || q.SoftSubtitleStream != nil || len(q.TextSubtitleStreams) > 0 || len(q.SlateText) != 0 || (q.Protocol != "" && q.Protocol != core.ProtocolHLS) {
		return false
	}
	if d.Kind != core.DecisionRemux || !d.VideoCopy || !d.AudioCopy || d.Container != ContainerMPEGTS || d.VideoCodec != "h264" || d.AudioCodec != "aac" || d.AudioStream != p.AudioStream || d.BurnSubtitles || d.SubtitleStream >= 0 || d.ToneMap || d.StripDolbyVision || enhancementSelected(d) || len(d.AudioFilters) != 0 {
		return false
	}
	v, a := q.Info.Video(), findStream(q.Info, p.AudioStream, "audio")
	if v == nil || v.Index != p.VideoStream || v.Codec != "h264" || v.FrameRate != 25 || v.VariableFrameRate || a == nil || a.Codec != "aac" || a.Profile != "LC" || a.SampleRate != 48000 || a.Channels != 2 || a.AACProgramConfig {
		return false
	}
	if len(q.Info.Keyframes) != int(p.End/p.Step) {
		return false
	}
	for i, k := range q.Info.Keyframes {
		if k.Time != time.Duration(int64(i)*p.Step)*time.Second/90000 {
			return false
		}
	}
	return true
}

// scanCopyTimeline runs only on a bounded TS source through the authenticated
// source owner's loopback input. A partial scan can never establish eligibility.
func scanCopyTimeline(ctx context.Context, r Runner, path, input string, info *core.MediaInfo) {
	if info.Container != "mpegts" || info.Duration <= 0 || info.Duration > 120*time.Second || len(info.Streams) != 2 {
		return
	}
	v, a := info.Video(), pickAudio(*info, -1)
	if v == nil || a == nil || v.TimeBase != "1/90000" || a.TimeBase != "1/90000" || v.Codec != "h264" || v.FrameRate != 25 || v.VariableFrameRate || a.Codec != "aac" || a.Profile != "LC" || a.SampleRate != 48000 || a.Channels != 2 || a.AACProgramConfig {
		return
	}
	var out, diagnostics bytes.Buffer
	args := []string{"-v", "error", "-show_packets", "-show_data", "-show_entries", "packet=stream_index,pts,dts,duration,pos,data", "-of", "json"}
	args = append(args, inputSafetyArgs(input, false)...)
	args = append(args, httpInputArgs(input, false)...)
	args = append(args, "-i", input)
	if r.Run(ctx, Command{Path: path, Args: args, Stdout: &out, Stderr: &diagnostics}) != nil || diagnostics.Len() != 0 {
		return
	}
	p, points := parseCopyPackets(out.Bytes(), v.Index, a.Index)
	if p == nil {
		return
	}
	duration := time.Duration(p.End) * time.Second / 90000
	// Probe duration must agree exactly with the complete packet span.
	if info.Duration != duration {
		return
	}
	// Decode the entire bounded video stream as a second independent check of
	// packet-derived IDRs and no-reorder clocks. Error diagnostics fail closed.
	out.Reset()
	args = []string{"-v", "error", "-select_streams", "v:0", "-show_frames", "-show_entries", "frame=pts,key_frame,pict_type", "-of", "json"}
	args = append(args, inputSafetyArgs(input, false)...)
	args = append(args, httpInputArgs(input, false)...)
	args = append(args, "-i", input)
	if r.Run(ctx, Command{Path: path, Args: args, Stdout: &out, Stderr: &diagnostics}) != nil || diagnostics.Len() != 0 {
		return
	}
	var decoded struct {
		Frames []struct {
			PTS     *int64 `json:"pts"`
			Key     int    `json:"key_frame"`
			Picture string `json:"pict_type"`
		} `json:"frames"`
	}
	if json.Unmarshal(out.Bytes(), &decoded) != nil || int64(len(decoded.Frames)) != p.VideoAUs {
		return
	}
	for i, f := range decoded.Frames {
		key, picture := 0, "P"
		if i%48 == 0 {
			key, picture = 1, "I"
		}
		if f.PTS == nil || *f.PTS != p.Origin+int64(i)*3600 || f.Key != key || f.Picture != picture {
			return
		}
	}
	info.CopyTimeline, info.Keyframes = p, points
}

type copyPacket struct {
	Stream   int    `json:"stream_index"`
	PTS      *int64 `json:"pts"`
	DTS      *int64 `json:"dts"`
	Duration int64  `json:"duration"`
	Pos      string `json:"pos"`
	Data     string `json:"data"`
}

func parseCopyPackets(raw []byte, video, audio int) (*core.CopyTimeline, []core.KeyframePoint) {
	var doc struct {
		Packets []copyPacket `json:"packets"`
	}
	if len(raw) > 32<<20 || json.Unmarshal(raw, &doc) != nil || len(doc.Packets) == 0 || len(doc.Packets) > 10000 {
		return nil, nil
	}
	p := &core.CopyTimeline{Version: 1, Step: 172800, VideoStream: video, AudioStream: audio, Origin: -1}
	var points []core.KeyframePoint
	var vo, ao int64 = -1, -1
	h := sha256.New()
	var word [8]byte
	for _, x := range doc.Packets {
		if x.PTS == nil || x.DTS == nil || *x.PTS != *x.DTS || *x.PTS < 0 {
			return nil, nil
		}
		data := probeExtradata(x.Data)
		if len(data) == 0 {
			return nil, nil
		}
		for _, n := range []int64{int64(x.Stream), *x.PTS, *x.DTS, int64(len(data))} {
			binary.BigEndian.PutUint64(word[:], uint64(n))
			h.Write(word[:])
		}
		h.Write(data)
		switch x.Stream {
		case video:
			if vo < 0 {
				vo = *x.PTS
			}
			if *x.PTS != vo+p.VideoAUs*3600 || x.Duration != 3600 {
				return nil, nil
			}
			idr, sps, pps, vcl, nonIDR := false, false, false, false, false
			for i := 0; i+4 < len(data); i++ {
				if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
					switch data[i+3] & 31 {
					case 5:
						idr = true
						vcl = true
					case 1:
						nonIDR = true
						vcl = true
					case 7:
						sps = true
					case 8:
						pps = true
					case 2, 3, 4:
						return nil, nil
					}
				}
			}
			cut := p.VideoAUs%48 == 0
			if !vcl || idr && nonIDR || idr != cut || cut && (!sps || !pps) || !cut && (sps || pps) {
				return nil, nil
			}
			if cut {
				offset, err := strconv.ParseInt(x.Pos, 10, 64)
				if err != nil || offset < 0 || len(points) > 0 && offset <= points[len(points)-1].Offset {
					return nil, nil
				}
				points = append(points, core.KeyframePoint{Offset: offset, Time: time.Duration(p.VideoAUs) * 40 * time.Millisecond})
			}
			p.VideoAUs++
		case audio:
			if ao < 0 {
				ao = *x.PTS
			}
			if *x.PTS != ao+p.AudioAUs*1920 || x.Duration != 1920 || len(data) < 7 || data[0] != 0xff || data[1]&0xf6 != 0xf0 || data[2]>>6 != 1 || (data[2]>>2)&15 != 3 || ((int(data[2]&1)<<2)|int(data[3]>>6)) != 2 || data[6]&3 != 0 || (int(data[3]&3)<<11|int(data[4])<<3|int(data[5]>>5)) != len(data) {
				return nil, nil
			}
			p.AudioAUs++
		default:
			return nil, nil
		}
	}
	p.Origin, p.End = vo, p.VideoAUs*3600
	if vo != ao || vo < 0 || p.End != p.AudioAUs*1920 || p.End%p.Step != 0 || p.End <= p.Step || p.End > 90000*120 {
		return nil, nil
	}
	copy(p.PacketSHA256[:], h.Sum(nil))
	return p, points
}

// AuthorCopyTimeline freezes the proven source grid before durable admission.
func AuthorCopyTimeline(q core.HLSRequest) core.HLSRequest {
	if q.Resume != nil || q.Info.CopyTimeline == nil || !q.Decision.VideoCopy {
		return q
	}
	original := q
	if q.SegmentDuration == 0 {
		q.SegmentDuration = 1920 * time.Millisecond
	}
	if q.Ramp == (core.HLSRamp{}) {
		q.Ramp = core.HLSRamp{Version: 1}
	}
	if !ValidCopyTimeline(q) {
		return original
	}
	return q
}
