package transcode

import (
	"fmt"
	"time"
)

// continuousLadder is the forced-keyframe schedule, the single source of
// segment cuts for the encoder and the segment index. Short opening segments
// reduce cold tune time without disabling mbtree or lookahead: a quarter, a
// quarter and a half of a segment, then whole segments (1, 1, 2, then 4 s at
// 4 s segments). A live provider output has no backlog to start from, so a
// client joins one cushion behind the first picture: two more one-half
// segments let it start 1.5 segments behind with no cut longer than its
// lead (0.5, 0.5, 1, 1, 1, then 2 s at 2 s segments). All cuts are integer
// frames.
func continuousLadder(f RawFormat, segment time.Duration, live bool) (g int64, startup []int64, steady int64) {
	if segment <= 0 {
		segment = 4 * time.Second
	}
	g = (int64(segment)*int64(f.FPSNum) + int64(time.Second)*int64(f.FPSDen)/2) / (int64(time.Second) * int64(f.FPSDen))
	startup = []int64{0, g / 4, g / 2}
	steady = g
	if live {
		startup = append(startup, g, g+g/2)
		steady = 2 * g
	}
	return g, startup, steady
}

func continuousSegmentFrames(f RawFormat, segment time.Duration, live bool, seq int64) (start, frames int64) {
	g, startup, steady := continuousLadder(f, segment, live)
	if seq < int64(len(startup)) {
		end := steady
		if seq+1 < int64(len(startup)) {
			end = startup[seq+1]
		}
		return startup[seq], end - startup[seq]
	}
	return steady + (seq-int64(len(startup)))*g, g
}

// continuousKeyframeExpr forces the ladder's keyframes in ffmpeg.
func continuousKeyframeExpr(f RawFormat, segment time.Duration, live bool) string {
	return continuousKeyframeExprAt(f, segment, live, 0)
}

// Restart at an unpublished segment without skipping the short opening cuts.
func continuousKeyframeExprAt(f RawFormat, segment time.Duration, live bool, seq int64) string {
	g, startup, steady := continuousLadder(f, segment, live)
	offset, _ := continuousSegmentFrames(f, segment, live, seq)
	if seq >= int64(len(startup)) {
		return fmt.Sprintf("expr:gte(n,n_forced*%d)", g)
	}
	opening := ""
	for _, n := range startup[seq:] {
		if opening != "" {
			opening += "+"
		}
		opening += fmt.Sprintf("eq(n,%d)", n-offset)
	}
	if seq == 0 {
		return fmt.Sprintf("expr:if(lt(n,%d),%s,gte(n,(n_forced-%d)*%d))", steady, opening, int64(len(startup))-steady/g, g)
	}
	// n_forced counts only the replacement's pictures; seq accounts for
	// cuts already published by its predecessor.
	return fmt.Sprintf("expr:if(lt(n,%d),%s,gte(n,(n_forced+%d-%d)*%d-%d))", steady-offset, opening, seq, int64(len(startup))-steady/g, g, offset)
}

func continuousFrameTime(f RawFormat, frames int64) time.Duration {
	return time.Duration(frames*int64(f.FPSDen)) * time.Second / time.Duration(f.FPSNum)
}
func (s *continuousSegmentIndex) segment(seq int64) Segment {
	start, frames := continuousSegmentFrames(s.enc.req.Source, s.enc.req.SegmentDuration, s.enc.req.LiveSource, seq)
	return Segment{Seq: seq, Start: s.start.Add(continuousFrameTime(s.enc.req.Source, start)), Duration: continuousFrameTime(s.enc.req.Source, frames)}
}
