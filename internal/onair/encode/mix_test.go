package encode

import (
	"strings"
	"testing"
)

func TestMixEnvelopes(t *testing.T) {
	end := 5.0
	got := MixFilter("1:a:0", []SoundInput{{At: 2, Start: 1, End: &end, Volume: .4, FadeIn: .2, FadeOut: .5}, {At: 0, Volume: 1}}, 2, 8)
	for _, want := range []string{"[1:a:0]apad=whole_dur=8,atrim=duration=8", "[2:a:0]atrim=start=1:end=5,asetpts=PTS-STARTPTS,atrim=duration=4", "afade=t=out:st=3.5:d=0.5", "volume=0.4,adelay=2000:all=1", "[3:a:0]", "amix=inputs=3:duration=first:normalize=0", "alimiter=limit=0.95:level=0"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}
