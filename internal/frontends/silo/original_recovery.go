package silo

import (
	"encoding/json"
	"net/http"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// A client replanning a failed original gets a new attempt key and a
// revision-bound file URL, even when the delivery type stays the same.
func (i *instance) replanRecoveredOriginal(w http.ResponseWriter, r *http.Request, a frontend.SessionAccess, id, currentKey string, snap playbackPlan, document, plan map[string]any) bool {
	reader, ok := i.host.Sessions.(frontend.OriginalPlaybackReader)
	if !ok {
		return false
	}
	state, err := reader.OriginalPlaybackState(r.Context(), a, id)
	if err != nil {
		return false
	}
	if state.Unavailable {
		versionUnavailable(w)
		return true
	}
	if state.Revision == "" || currentKey == state.Revision {
		return false
	}
	var start startRequest
	if json.Unmarshal(snap.start, &start) != nil {
		return false
	}
	pos := state.Position.Seconds()
	stream, _ := plan["stream"].(map[string]any)
	if stream == nil {
		stream = map[string]any{}
	}
	stream["url"] = siloMediaURL(id, frontend.StreamDirect) + "?sourceRevision=" + state.Revision
	stream["protocol"], stream["container"], stream["mime_type"] = "http", state.Media.Container, "video/"+state.Media.Container
	plan["stream"] = stream
	plan["timeline"] = map[string]any{
		"source_start_seconds": pos, "player_start_seconds": pos,
		"stream_origin_seconds": 0, "timeline_offset_seconds": 0,
		"can_seek_anywhere": true, "seek_restoration": "player_position",
	}
	selected, subtitle, policy, authored, _ := i.siloTracks(siloTrackInputs{
		r: r, viewer: a.Viewer, access: a, sessionID: id, start: start,
		media: state.Media, format: frontend.StreamDirect, delivery: "original_http",
		audioStream: state.Decision.AudioStream, subtitleStream: state.Decision.SubtitleStream,
	})
	plan["selected_tracks"], plan["subtitle"], plan["subtitle_fidelity_policy"] = selected, subtitle, policy
	plan["plan_id"], plan["plan_attempt_key"], plan["delivery"] = state.Revision, state.Revision, "original_http"
	plan["decision_reason"] = state.Decision.Reason
	plan["source"] = map[string]any{
		"container": state.Media.Container, "duration_seconds": state.Media.Duration.Seconds(),
		"bitrate_kbps": state.Media.BitrateKbps, "source_origin": state.Origin,
	}
	plan["effective_recipe"] = map[string]any{"video_codec": state.Decision.VideoCodec, "audio_codec": state.Decision.AudioCodec, "bitrate_kbps": state.Decision.BitrateKbps}
	plan["transformations"] = []any{}
	plan["claims"] = map[string]any{}
	document["session_id"] = id
	snap.format, snap.delivery, snap.replacement = frontend.StreamDirect, "original_http", state.Revision
	snap.requireAuthored, snap.body, snap.media, snap.audioCopy = authored, document, state.Media, state.Decision.AudioCopy
	i.updatePlan(id, snap)
	reply(w, http.StatusOK, document)
	return true
}
