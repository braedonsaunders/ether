package transcode

import (
	"fmt"
	"strings"
)

// A live demuxer can briefly omit an audio stream that the session already
// announced. Retry that run with encoded silence without changing the decoder's
// codec/layout or permanently discarding the source's returning audio.
func (s *Session) retryMissingLiveAudioLocked(r *run) bool {
	if r.spec.SilenceAudio || r.produced != 0 {
		return false
	}
	if !missingLiveAudio(r.spec, r.stderr.String()) {
		return false
	}
	s.silenceAudio = true
	s.log.Warn("live audio absent; retrying with encoded silence", "segment", r.startSeg, "audioStream", r.spec.Decision.AudioStream)
	if err := s.continueAfterExitLocked(r, "missing live audio"); err != nil {
		s.failErr = err
	}
	return true
}

func missingLiveAudio(spec runSpec, stderrText string) bool {
	audio := findStream(spec.Info, spec.Decision.AudioStream, "audio")
	if audio == nil {
		return false
	}
	stderr := strings.ToLower(stderrText)
	if !strings.Contains(stderr, "matches no streams") {
		return false
	}
	ordinal := 0
	for _, st := range spec.Info.Streams {
		if st.Type == "audio" && st.Index < audio.Index {
			ordinal++
		}
	}
	exact := fmt.Sprintf("'0:%d'", audio.Index)
	byKind := fmt.Sprintf("'0:a:%d'", ordinal)
	if !strings.Contains(stderr, exact) && !strings.Contains(stderr, byKind) {
		return false
	}
	return true
}
