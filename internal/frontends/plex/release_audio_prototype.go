package plex

// Throwaway prototype: can unmodified Plex clients use an audio menu as a
// release picker? Keep synthetic tracks out of probe facts and encoder options.
// See release_audio_prototype.txt for the experiment and client validation steps.

import (
	"context"
	"strconv"
	"strings"

	"github.com/braedonsaunders/ether/internal/frontend"
	"github.com/braedonsaunders/ether/internal/frontends/plex/model"
)

const releaseAudioPrefix = "ether-release-prototype:"
const releaseAudioAuto = "@auto"

func isReleaseAudio(info frontend.StreamInfo) bool {
	return strings.HasPrefix(info.StreamIdentifier, releaseAudioPrefix)
}

func (s *Server) releaseAudioBinding(b frontend.StreamBinding) (string, bool) {
	if !s.cfg().Plex.ReleaseAudioSelector || !isReleaseAudio(b.Info) {
		return "", false
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(b.Info.StreamIdentifier, releaseAudioPrefix), 10, 64)
	if err != nil {
		return "", false
	}
	if id == int64(b.Key) {
		return releaseAudioAuto, true
	}
	key, release, ok := s.partTitle(id)
	return release, ok && key == b.Key && release != ""
}

func (s *Server) appendReleaseAudio(pr *Request, key frontend.RatingKey, m *model.Media, override ...string) {
	if !s.cfg().Plex.ReleaseAudioSelector || pr == nil || pr.User == nil || len(m.Part) == 0 || s.variantIDs == nil {
		return
	}
	vs := s.playIDs.pinned(s.playIdentityFor(pr, key), true, s.now()).variants
	if len(vs) < 2 {
		return
	}
	stored := s.selections.get(pr.User.ID, int64(key))
	picked := stored.releaseAudio
	if len(override) > 0 {
		picked = override[0]
	} else if stored.audioSet {
		picked = "" // a subsequent real audio choice keeps its own radio
	}
	var choices []model.Stream
	seen := map[string]bool{}
	for _, v := range vs {
		release, id, label := v.Release.ID, s.variantIDs.idFor(key, v.Release.ID), v.Label
		if v.Auto {
			release, id, label = releaseAudioAuto, int64(key), "Auto"
		} else if release == "" || seen[release] {
			continue
		}
		seen[release] = true
		if label == "" {
			label = v.Release.Name
		}
		label = "Release: " + label
		info := frontend.StreamInfo{
			Type: "audio", Codec: "aac", Title: label,
			StreamIdentifier: releaseAudioPrefix + strconv.FormatInt(id, 10),
		}
		// A negative, release-derived position keeps otherwise identical
		// labels distinct in the broker's stream identity. It is never sent
		// to an encoder or included in the session's actual MediaInfo.
		wire := s.assignStream(context.Background(), pr.User, key, -int(id), info, "")
		if wire == 0 {
			continue
		}
		choices = append(choices, model.Stream{
			ID: wire, StreamType: 2, Codec: "aac", Title: label,
			DisplayTitle: label, ExtendedDisplayTitle: label,
			Selected: boolPtrIf(picked == release),
		})
	}
	if picked != "" {
		for i := range m.Part[0].Stream {
			if m.Part[0].Stream[i].StreamType == 2 {
				m.Part[0].Stream[i].Selected = nil
			}
		}
	}
	m.Part[0].Stream = append(m.Part[0].Stream, choices...)
}
