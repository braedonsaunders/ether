package playback

import (
	"testing"

	"github.com/braedonsaunders/ether/internal/core"
)

type originSrc struct{ fakeOriginless }

func (originSrc) Origin() string { return "TorBox" }

type fakeOriginless struct{ core.MediaSource }

func TestSourceOrigin(t *testing.T) {
	cases := []struct {
		src      core.MediaSource
		protocol string
		want     string
	}{
		{originSrc{}, core.ProtocolTorrent, "TorBox"},
		{fakeOriginless{}, core.ProtocolUsenet, "Usenet"},
		{fakeOriginless{}, core.ProtocolTorrent, "Debrid"},
		{fakeOriginless{}, core.ProtocolHTTP, "HTTP"},
	}
	for _, c := range cases {
		if got := sourceOrigin(c.src, c.protocol); got != c.want {
			t.Errorf("sourceOrigin(%T, %q) = %q, want %q", c.src, c.protocol, got, c.want)
		}
	}
}
