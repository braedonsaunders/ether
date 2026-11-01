package sessions

import "github.com/braedonsaunders/ether/internal/core"

// NotifyTranscodeActivity forwards timeline state to an active output. It is
// deliberately a no-op for transcoders and outputs without this capability.
func NotifyTranscodeActivity(transcoder core.Transcoder, id, state string) {
	if transcoder == nil {
		return
	}
	if output, ok := transcoder.Session(id); ok {
		if activity, ok := output.(core.SessionActivity); ok {
			activity.PlaybackState(state)
		}
	}
}
