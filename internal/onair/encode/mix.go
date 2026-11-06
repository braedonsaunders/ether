package encode

import (
	"fmt"
	"strings"
)

// SoundInput is a prepared asset path plus its authored timeline envelope.
type SoundInput struct {
	Path                    string
	At, Start               float64
	End                     *float64
	Volume, FadeIn, FadeOut float64
}
type VideoInput struct {
	Path, Fit string
	Start     float64
}

// MixFilter builds a bounded, timestamp-reset mix alongside the generated bed.
// Sound input indexes begin at firstInput; base names the existing audio stream.
func MixFilter(base string, sounds []SoundInput, firstInput int, duration float64) string {
	filters := []string{fmt.Sprintf("[%s]apad=whole_dur=%g,atrim=duration=%g,asetpts=PTS-STARTPTS[bed]", base, duration, duration)}
	labels := "[bed]"
	for i, s := range sounds {
		trim := fmt.Sprintf("atrim=start=%g", s.Start)
		length := duration - s.At
		if s.End != nil {
			trim += fmt.Sprintf(":end=%g", *s.End)
			length = min(length, *s.End-s.Start)
		}
		chain := fmt.Sprintf("[%d:a:0]%s,asetpts=PTS-STARTPTS,atrim=duration=%g", firstInput+i, trim, length)
		if s.FadeIn > 0 {
			chain += fmt.Sprintf(",afade=t=in:st=0:d=%g", s.FadeIn)
		}
		if s.FadeOut > 0 {
			chain += fmt.Sprintf(",afade=t=out:st=%g:d=%g", max(0, length-s.FadeOut), s.FadeOut)
		}
		chain += fmt.Sprintf(",volume=%g,adelay=%g:all=1[s%d]", s.Volume, s.At*1000, i)
		filters = append(filters, chain)
		labels += fmt.Sprintf("[s%d]", i)
	}
	filters = append(filters, fmt.Sprintf("%samix=inputs=%d:duration=first:normalize=0,alimiter=limit=0.95:level=0[mixed]", labels, len(sounds)+1))
	return strings.Join(filters, ";")
}
