package vela

import (
	"net/http"
	"strconv"

	"github.com/braedonsaunders/ether/internal/frontend"
)

func (i *instance) audioFallbackAvailable() bool {
	audio, ok := i.host.Sessions.(frontend.AudioFallback)
	return ok && audio.AudioFallbackAvailable()
}
func (i *instance) audioFallback(w http.ResponseWriter, r *http.Request) {
	v, s := i.session(w, r)
	if s == nil {
		return
	}
	audio, ok := i.host.Sessions.(frontend.AudioFallback)
	if !ok || !audio.AudioFallbackAvailable() {
		i.unavailable(w, r)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ended.IsZero() || s.plan["channelId"] != nil || s.plan["purpose"] == "preview" {
		problem(w, 409, "conflict", "session_ended")
		return
	}
	id := r.PathValue("id")
	if r.Method == http.MethodDelete {
		if err := audio.StopAudioFallback(r.Context(), i.access(r, v), id, r.PathValue("audioId")); err != nil {
			fail(w, err)
			return
		}
		send(w, 200, object{"accepted": true})
		return
	}
	var b struct {
		TrackID, PreferredCodec          string
		PositionSeconds                  float64
		SourceGeneration, SeekGeneration uint64
	}
	if body(r, &b) != nil || b.PreferredCodec != "aac" {
		problem(w, 400, "request", "invalid_audio_fallback")
		return
	}
	if stale(w, s, b.SourceGeneration, b.SeekGeneration) {
		return
	}
	track, ok := offeredTrack(s.plan, "audioTracks", b.TrackID)
	pos, valid := seconds(b.PositionSeconds)
	if !ok || !valid {
		problem(w, 400, "request", "invalid_audio_track")
		return
	}
	result, err := audio.StartAudioFallback(r.Context(), i.access(r, v), id, track, pos)
	if err != nil {
		fail(w, err)
		return
	}
	if selected := strconv.Itoa(track); s.plan["selectedAudioTrackId"] != selected {
		s.plan["selectedAudioTrackId"] = selected
		s.publish("plan")
	}
	send(w, 200, object{"id": result.ID, "audioGeneration": result.Generation, "trackId": b.TrackID, "url": result.URL, "mimeType": "application/vnd.apple.mpegurl", "codec": "mp4a.40.2", "timelineOriginSeconds": float64(0)})
}
