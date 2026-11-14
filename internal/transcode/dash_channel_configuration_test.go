package transcode

import (
	"bytes"
	"strings"
	"testing"
)

// Speaker masks distinguish layouts with the same channel count. Keep Dolby
// positions separate from the MPEG descriptor used for AAC and other codecs.
func TestDASHChannelConfiguration(t *testing.T) {
	for _, tc := range []struct {
		layout   string
		channels int
		mask     string
	}{
		{"mono", 1, "4000"}, {"stereo", 2, "A000"}, {"2.1", 3, "A001"}, {"3.0", 3, "E000"}, {"3.0(back)", 3, "A100"}, {"4.0", 4, "E100"}, {"quad", 4, "B800"}, {"5.0(side)", 5, "F800"}, {"5.1(side)", 6, "F801"}, {"6.1", 7, "F901"}, {"7.1", 8, "FA01"}, {"", 6, "F801"}, {"unknown", 6, ""},
	} {
		for _, codec := range []string{"ec-3", "ac-3"} {
			var b bytes.Buffer
			writeDASHChannelConfiguration(&b, dashRep{Codecs: codec, ChannelLayout: tc.layout, Channels: tc.channels})
			if tc.mask == "" {
				if b.Len() != 0 {
					t.Fatalf("invented unknown Dolby layout: %s", b.String())
				}
				continue
			}
			if !strings.Contains(b.String(), `schemeIdUri="tag:dolby.com,2014:dash:audio_channel_configuration:2011"`) || !strings.Contains(b.String(), `value="`+tc.mask+`"`) {
				t.Fatalf("%s %s: %s", codec, tc.layout, b.String())
			}
		}
	}
	var b bytes.Buffer
	writeDASHChannelConfiguration(&b, dashRep{Codecs: "mp4a.40.2", Channels: 6})
	if !strings.Contains(b.String(), `schemeIdUri="urn:mpeg:dash:23003:3:audio_channel_configuration:2011" value="6"`) {
		t.Fatalf("changed AAC descriptor: %s", b.String())
	}
}
