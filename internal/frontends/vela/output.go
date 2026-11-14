package vela

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
)

// refreshOutput runs only for an opened MSE session. Unknown codec tokens stay
// unknown until an actual init segment proves them; it never starts encoders.
// Caller holds s.mu, keeping these facts inside the active seek generation.
func (i *instance) refreshOutput(ctx context.Context, id string, s *playSession) bool {
	if !s.mse {
		return false
	}
	hs, ok := i.host.Sessions.TranscodeFor(ctx, id)
	if !ok || hs == nil {
		return false
	}
	paths, _ := s.plan["paths"].([]object)
	if len(paths) == 0 {
		return false
	}
	path := selectedPlaybackPath(s.plan, paths)
	if path["transport"] != "hls" {
		return false
	}
	before, _ := json.Marshal(object{"qualities": path["qualities"], "preparation": s.plan["preparation"]})
	if ladder, ok := hs.(interface{ Rungs() []frontend.HLSRung }); ok {
		qualities := []object{}
		delete(path, "initialRenditionId")
		base, _ := path["url"].(string)
		base = strings.TrimSuffix(base, "master.m3u8")
		for _, r := range ladder.Rungs() {
			if r.Width <= 0 || r.Height <= 0 || r.Bandwidth <= 0 {
				continue
			}
			// The IDs are host-produced URL components, still reject separators.
			if r.ID == "" || strings.ContainsAny(r.ID, "/\\?#%\r\n") {
				continue
			}
			q := object{"id": r.ID, "width": r.Width, "height": r.Height, "bitrate": r.Bandwidth, "url": base + r.ID + "/index.m3u8", "videoCodec": "", "codecStatus": "pending"}
			if r.OutputCodecs != "" {
				codecs := strings.Split(r.OutputCodecs, ",")
				q["videoCodec"] = codecs[0]
				q["codecStatus"] = "verified"
				if len(codecs) > 1 {
					q["audioCodec"] = codecs[1]
					tracks, _ := s.plan["audioTracks"].([]object)
					for _, track := range tracks {
						if track["id"] == s.plan["selectedAudioTrackId"] && r.OutputAudioChannels > 0 {
							track["channels"] = r.OutputAudioChannels
						}
					}
				}
			}
			if r.ID == "base" {
				path["initialRenditionId"] = "base"
			}
			qualities = append(qualities, q)
		}
		path["qualities"] = qualities
	}
	if diagnostics, ok := hs.(frontend.HLSDiagnosticSession); ok {
		d := diagnostics.PlaybackDiagnostics()
		s.diagnostics = d.Records
		events := append([]frontend.HLSPreparationEvent{}, s.preparation...)
		events = append(events, d.Events...)
		s.plan["preparation"] = object{"sourceGeneration": s.source, "seekGeneration": s.seek, "events": events}
	}
	after, _ := json.Marshal(object{"qualities": path["qualities"], "preparation": s.plan["preparation"]})
	return string(before) != string(after)
}
