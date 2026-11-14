package playback

import (
	"encoding/json"
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestSpecialPlaybackIdentityInvariant(t *testing.T) {
	title := &core.Title{Type: core.TypeEpisode, Season: 0, Episode: 5, Title: "The Path of the Ball", ShowTitle: "Haikyu!!"}
	target := &core.ReleaseTarget{Season: 0, Episode: 5, AddonSpecialEpisode: 9, EpisodeTitle: title.Title, ShowTitles: []string{title.ShowTitle}}
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Haikyu!!.S00E09.The.Path.of.the.Ball.1080p.mkv", true},
		{"Haikyu!!.S00E05.1080p.mkv", false},
		{"Haikyu!!.S00E09.Land.vs.Air.1080p.mkv", false},
		{"Haikyu!! - 590.mkv", false},
	} {
		data, _ := json.Marshal(struct {
			Path   string              `json:"path"`
			Target *core.ReleaseTarget `json:"target"`
		}{c.name, target})
		if got := payloadMatchesTitle(append([]byte(debridPayloadPrefix), data...), title); got != c.ok {
			t.Errorf("%q: %v", c.name, got)
		}
	}
	rel := core.Release{Name: "Haikyu!!.S00E05.Path.of.Ball.1080p", Target: target}
	if !releaseMatchesTitle(rel, title) {
		t.Error("canonical abbreviated special rejected")
	}
	rel.Name = "Haikyu!!.S00E05.Land.vs.Air.1080p"
	if releaseMatchesTitle(rel, title) {
		t.Error("different special accepted")
	}
}
