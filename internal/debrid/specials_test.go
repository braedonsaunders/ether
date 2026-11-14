package debrid

import (
	"github.com/braedonsaunders/ether/internal/core"
	"testing"
)

func TestSpecialFileIdentityInvariant(t *testing.T) {
	target := &core.ReleaseTarget{Season: 0, Episode: 5, AddonSpecialEpisode: 9, EpisodeTitle: "The Path of the Ball", ShowTitles: []string{"Haikyu!!"}, SpecialTitles: []string{"Land vs Air", "The Path of the Ball"}}
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"Haikyu!!.S00E09.The.Path.of.the.Ball.1080p.mkv", true},
		{"Haikyu!!.S00E05.1080p.mkv", false},
		{"Haikyu!!.S00E09.Land.vs.Air.1080p.mkv", false},
		{"Haikyu!! Specials/Land vs Air/Haikyu!!.S00E09.mkv", false},
		{"Haikyu!! - 05.mkv", false},
		{"Haikyu!! OAD 02 [1080p].mkv", false},
		{"Haikyu!! Special The Path of the Ball [1080p].mkv", true},
	} {
		_, err := ChooseFile([]File{{Path: c.name, Size: 100}}, target)
		if (err == nil) != c.ok {
			t.Errorf("%q: %v", c.name, err)
		}
	}
}
