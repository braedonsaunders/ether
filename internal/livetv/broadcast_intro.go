package livetv

import (
	"encoding/binary"
	"time"

	"github.com/braedonsaunders/ether/internal/transcode"
)

// Only the unscheduled cold-tune ident is fill. Scheduled moments retain their
// complete saved duration. Keep decoding its next frames during the 300ms cut.
func (b *broadcast) finishIntro(now time.Time) {
	b.introUntil = now
	if b.introFade == nil && b.introInput != nil {
		b.introFade = b.introInput
		b.introInput = nil
		b.introFadeFrame = b.lastFrame
		b.introFadeStart = now
	}
}
func (b *broadcast) blendIntro(now time.Time, target *transcode.RawFrame, fresh bool) {
	if b.introFade == nil {
		return
	}
	const duration = 300 * time.Millisecond
	age := now.Sub(b.introFadeStart)
	if age >= duration {
		b.closeInput(b.introFade)
		b.introFade = nil
		b.introFadeFrame = transcode.RawFrame{}
		return
	}
	select {
	case frame, ok := <-b.introFade.frames:
		if ok && validRawFrame(b.format, frame) {
			b.introFadeFrame = frame
		}
	default:
	}
	old := b.introFadeFrame
	if len(old.Video) != len(target.Video) {
		return
	}
	if !fresh {
		target.Video = append([]byte(nil), target.Video...)
	}
	gain := max(0, min(int64(65536), int64(age)*65536/int64(duration)))
	for n, v := range target.Video {
		target.Video[n] = byte((int64(old.Video[n])*(65536-gain) + int64(v)*gain + 32768) / 65536)
	}
	for n := 0; n+2 <= len(target.PCM) && n+2 <= len(old.PCM); n += 2 {
		at := min(duration, age+time.Duration(n/(b.format.Channels*2))*time.Second/time.Duration(b.format.SampleRate))
		weight := max(0, min(int64(65536), int64(at)*65536/int64(duration)))
		a := int64(int16(binary.LittleEndian.Uint16(old.PCM[n:])))
		z := int64(int16(binary.LittleEndian.Uint16(target.PCM[n:])))
		binary.LittleEndian.PutUint16(target.PCM[n:], uint16(int16((a*(65536-weight)+z*weight)/65536)))
	}
}
