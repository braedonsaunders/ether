package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/mediaproc"
)

type packetTiming struct {
	Stream int    `json:"stream_index"`
	PTS    string `json:"pts_time"`
	DTS    string `json:"dts_time"`
}
type mediaTiming struct {
	Packets []packetTiming `json:"packets"`
	Streams []struct {
		Index int    `json:"index"`
		Type  string `json:"codec_type"`
	} `json:"streams"`
}
type timeSpan struct {
	first, last float64
	valid       bool
}
type timelineCheck struct {
	previous        int
	spans           map[string]timeSpan
	offset          float64
	hasOffset       bool
	discontinuities map[int]bool
}

func discontinuities(playlist string) map[int]bool {
	result := map[int]bool{}
	pending := false
	for _, line := range strings.Split(playlist, "\n") {
		line = strings.TrimSpace(line)
		if line == "#EXT-X-DISCONTINUITY" {
			pending = true
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if pending {
			base := line[strings.LastIndex(line, "/")+1:]
			base = strings.SplitN(base, ".", 2)[0]
			if n, err := strconv.Atoi(base); err == nil {
				result[n] = true
			}
			pending = false
		}
	}
	return result
}

func (c *timelineCheck) check(ctx context.Context, fp, path string, n int) error {
	out, err := mediaproc.CommandContext(ctx, mediaproc.ProbeTimeout, fp, "-v", "error", "-show_entries", "stream=index,codec_type:packet=stream_index,pts_time,dts_time", "-show_streams", "-show_packets", "-of", "json", path).Output()
	if err != nil {
		return fmt.Errorf("packet probe: %w", err)
	}
	var m mediaTiming
	if err = json.Unmarshal(out, &m); err != nil {
		return err
	}
	kinds := map[int]string{}
	selected := map[string]bool{}
	for _, s := range m.Streams {
		if (s.Type == "video" || s.Type == "audio") && !selected[s.Type] {
			kinds[s.Index] = s.Type
			selected[s.Type] = true
		}
	}
	spans := map[string]timeSpan{}
	for _, p := range m.Packets {
		kind := kinds[p.Stream]
		if kind == "" {
			continue
		}
		v, e := strconv.ParseFloat(p.DTS, 64)
		if e != nil {
			v, e = strconv.ParseFloat(p.PTS, 64)
		}
		if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		s := spans[kind]
		if !s.valid {
			s.first = v
			s.valid = true
		}
		s.last = v
		spans[kind] = s
	}
	if !spans["video"].valid {
		return fmt.Errorf("segment %d has no timestamped video", n)
	}
	if c.spans != nil && n == c.previous+1 && !c.discontinuities[n] {
		for _, kind := range []string{"video", "audio"} {
			a, b := c.spans[kind], spans[kind]
			if a.valid && b.valid && b.first < a.last-0.100 {
				return fmt.Errorf("%s DTS went backwards across %d->%d: %.3f -> %.3f", kind, c.previous, n, a.last, b.first)
			}
		}
	}
	v, a := spans["video"], spans["audio"]
	if v.valid && a.valid && math.Abs(a.first-v.first) < 1 {
		offset := a.first - v.first
		if c.hasOffset && n == c.previous+1 && math.Abs(offset-c.offset) > 0.35 {
			return fmt.Errorf("A/V start offset changed by %.3fs across %d->%d", math.Abs(offset-c.offset), c.previous, n)
		}
		c.offset = offset
		c.hasOffset = true
	} else {
		// A track can intentionally begin late or end early. Such a
		// partial segment is not evidence of accumulated playback drift.
		c.hasOffset = false
	}
	c.previous = n
	c.spans = spans
	return nil
}
