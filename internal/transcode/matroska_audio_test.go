package transcode

import "testing"

// Plex for Android TV streams HLS as Matroska segments and declares DTS and
// TrueHD in that target, so PMS copies them. TS/fMP4 HLS cannot carry them,
// and a target that omits the codec still converts.
func TestMatroskaHLSCopiesAudioTheTargetAccepts(t *testing.T) {
	for _, codec := range []string{"dts", "truehd"} {
		info := mustParse(t, "mkv_dv_p8.json")
		for i := range info.Streams {
			if info.Streams[i].Type == "audio" {
				info.Streams[i].Codec = codec
			}
		}
		opts := allow()
		opts.DirectPlay = false
		caps := appleTV
		caps.AudioCodecs = []string{"aac", "ac3", "eac3", "dts", "truehd"}
		caps.HLSContainers = []string{ContainerMatroska}
		if d := Decide(info, caps, opts, Policy{}); !d.AudioCopy || d.AudioCodec != codec {
			t.Fatalf("%s into Matroska HLS: %+v", codec, d)
		}
		caps.HLSContainers = []string{ContainerFMP4}
		if d := Decide(info, caps, opts, Policy{}); d.AudioCopy {
			t.Fatalf("%s copied into fMP4 HLS: %+v", codec, d)
		}
		caps.HLSContainers = []string{ContainerMatroska}
		caps.TranscodeAudioCodecs = []string{"aac", "ac3", "eac3"}
		if d := Decide(info, caps, opts, Policy{}); d.AudioCopy {
			t.Fatalf("%s copied although the target omits it: %+v", codec, d)
		}
	}
}
