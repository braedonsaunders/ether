package releasepolicy

import (
	"regexp"
	"strings"
	"time"

	"github.com/braedonsaunders/ether/internal/core"
	"github.com/braedonsaunders/ether/internal/releaseparse"
)

var channelDisc = regexp.MustCompile(`(?i)(?:^|[ ._/\[\]()-])(?:ISO|BDISO|DVDISO|BDMV|BD25|BD50|BD66|BD100|DVD5|DVD9|(?:COMPLETE|FULL)[ ._-]*(?:BLURAY|BLU-RAY|UHD)|DISC[ ._-]*IMAGE)(?:$|[ ._/\[\]()-])`)

// ChannelSourceAllowed is the owner's hard source envelope for virtual TV.
// Network headroom never admits 4K, remuxes or disc images. Ordinary encodes
// reserve 25% of the rendition budget for variable bitrate and cheap playout.
// Unknown individual file size is checked again after opening a pack/link.
func ChannelSourceAllowed(r core.Release, duration time.Duration, renditionKbps int) bool {
	if renditionKbps <= 0 {
		return true
	}
	parsed := releaseparse.Parse(r.Name)
	if r.Info.Height > 1080 || parsed.Height > 1080 || r.Info.Source == core.SourceRemux || parsed.Source == core.SourceRemux || channelDisc.MatchString(r.Name) || strings.EqualFold(r.Info.Container, "iso") || strings.EqualFold(r.Info.Container, "bdmv") {
		return false
	}
	if r.Size <= 0 {
		return true
	}
	return duration > 0 && float64(r.Size)*8/duration.Seconds() <= float64(renditionKbps)*1000*.75
}
