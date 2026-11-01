package mediabrowser

import (
	"github.com/braedonsaunders/ether/internal/frontend"
	"testing"
)

func TestJellyfinStreamDTOUsesOptionalProbeFacts(t *testing.T) {
	fps := 23.976
	refs, nal, bits, level := 4, 4, 10, 153
	progressive := false
	i := &instance{protocol: "jellyfin"}
	data := i.mediaStreams([]frontend.StreamInfo{{Type: "video", AverageFrameRate: &fps, ReferenceFrameRate: &fps, DisplayAspectRatio: "16:9", PixelFormat: "yuv420p10le", CodecTag: "hvc1", TimeBase: "1/90000", NALLengthSize: &nal, ReferenceFrames: &refs, BitsPerRawSample: &bits, ProbedLevel: &level, ProbedInterlaced: &progressive, ColorPrimaries: "bt2020"}})[0].(map[string]any)
	for key, want := range map[string]any{"AverageFrameRate": fps, "ReferenceFrameRate": fps, "RealFrameRate": fps, "AspectRatio": "16:9", "PixelFormat": "yuv420p10le", "CodecTag": "hvc1", "TimeBase": "1/90000", "NalLengthSize": "4", "RefFrames": 4, "BitDepth": 10, "Level": 153, "IsInterlaced": false, "ColorPrimaries": "bt2020", "AudioSpatialFormat": "None"} {
		if data[key] != want {
			t.Errorf("%s: got %v want %v", key, data[key], want)
		}
	}
	if _, ok := data["VideoRange"]; ok {
		t.Fatal("unclassified video range", data["VideoRange"])
	}
	unknown := i.mediaStreams([]frontend.StreamInfo{{Type: "video"}})[0].(map[string]any)
	for _, key := range []string{"AverageFrameRate", "ReferenceFrameRate", "AspectRatio", "PixelFormat", "CodecTag", "TimeBase", "NalLengthSize", "RefFrames", "BitDepth", "Level", "ColorPrimaries", "VideoRange", "IsAVC", "IsAnamorphic"} {
		if _, ok := unknown[key]; ok {
			t.Errorf("unknown %s exposed", key)
		}
	}
	if unknown["AudioSpatialFormat"] != "None" {
		t.Fatal(unknown["AudioSpatialFormat"])
	}
}

func TestJellyfinStreamRangeAndAudioLabels(t *testing.T) {
	no := false
	i := &instance{protocol: "jellyfin"}
	sdr := i.mediaStreams([]frontend.StreamInfo{{Type: "video", Codec: "h264", SampleAspectRatio: "1:1", DolbyVisionConfigurationPresent: &no, HDR10Plus: &no}})[0].(map[string]any)
	if sdr["VideoRange"] != "SDR" || sdr["VideoRangeType"] != "SDR" || sdr["IsAVC"] != true || sdr["IsAnamorphic"] != false {
		t.Fatal(sdr)
	}
	bits := 8
	untagged := i.mediaStreams([]frontend.StreamInfo{{Type: "video", Codec: "h264", BitDepth: bits, PixelFormat: "yuv420p", DolbyVisionConfigurationPresent: &no}})[0].(map[string]any)
	if untagged["VideoRange"] != "SDR" || untagged["VideoRangeType"] != "SDR" {
		t.Fatal(untagged)
	}
	guessed := i.mediaStreams([]frontend.StreamInfo{{Type: "video", Codec: "h264", BitDepth: bits, PixelFormat: "yuv420p"}})[0].(map[string]any)
	if _, ok := guessed["VideoRange"]; ok {
		t.Fatal("unprobed 8-bit video claimed SDR", guessed["VideoRange"])
	}
	wide := i.mediaStreams([]frontend.StreamInfo{{Type: "video", Codec: "hevc", SampleAspectRatio: "8:9", ColorTransfer: "smpte2084"}})[0].(map[string]any)
	if wide["VideoRange"] != "HDR" || wide["VideoRangeType"] != "HDR10" || wide["IsAnamorphic"] != true || wide["IsAVC"] != false {
		t.Fatal(wide)
	}
	audio := i.mediaStreams([]frontend.StreamInfo{{Type: "audio", Language: "und", ChannelLayout: "5.1(side)", Title: "Atmos"}})[0].(map[string]any)
	for key, want := range map[string]any{"AudioSpatialFormat": "DolbyAtmos", "VideoRange": "Unknown", "VideoRangeType": "Unknown", "LocalizedLanguage": "Undetermined", "LocalizedDefault": "Default", "LocalizedExternal": "External", "LocalizedOriginal": "Original"} {
		if audio[key] != want {
			t.Errorf("%s: got %v", key, audio[key])
		}
	}
	named := i.mediaStreams([]frontend.StreamInfo{{Type: "audio", Language: "eng"}})[0].(map[string]any)
	if named["LocalizedLanguage"] != "English" {
		t.Fatal(named["LocalizedLanguage"])
	}
	text := i.mediaStreams([]frontend.StreamInfo{{Type: "subtitle", Codec: "subrip", Language: "eng"}})[0].(map[string]any)
	image := i.mediaStreams([]frontend.StreamInfo{{Type: "subtitle", Codec: "hdmv_pgs_subtitle"}})[0].(map[string]any)
	if text["IsTextSubtitleStream"] != true || image["IsTextSubtitleStream"] != false {
		t.Fatal(text["IsTextSubtitleStream"], image["IsTextSubtitleStream"])
	}
	for key, want := range map[string]any{"LocalizedDefault": "Default", "LocalizedExternal": "External", "LocalizedForced": "Forced", "LocalizedHearingImpaired": "Hearing Impaired", "LocalizedUndefined": "Undefined", "LocalizedLanguage": "English"} {
		if text[key] != want {
			t.Errorf("subtitle %s: got %v", key, text[key])
		}
	}
	if _, ok := audio["LocalizedForced"]; ok {
		t.Fatal("audio gained a subtitle label")
	}
	limited := i.mediaStreams([]frontend.StreamInfo{{Type: "video", Codec: "h264", ColorRange: "tv"}})[0].(map[string]any)
	full := i.mediaStreams([]frontend.StreamInfo{{Type: "video", Codec: "h264", ColorRange: "pc"}})[0].(map[string]any)
	if _, ok := limited["ColorRange"]; ok || full["ColorRange"] != "pc" {
		t.Fatal(limited["ColorRange"], full["ColorRange"])
	}
}

func TestJellyfinDefaultStreamIndexesUseProbeEvidence(t *testing.T) {
	i := &instance{protocol: "jellyfin"}
	source := map[string]any{}
	i.jellyfinMediaSourceDTO(source, frontend.MediaInfo{Container: "mp4", Streams: []frontend.StreamInfo{
		{Type: "video", Index: 0, Codec: "h264"},
		{Type: "audio", Index: 1, Codec: "aac"},
		{Type: "subtitle", Index: 2, Codec: "subrip"},
	}}, false)
	if source["DefaultAudioStreamIndex"] != 1 || source["DefaultSubtitleStreamIndex"] != -1 || source["VideoType"] != "VideoFile" {
		t.Fatal(source)
	}
	marked := map[string]any{}
	yes := true
	i.jellyfinMediaSourceDTO(marked, frontend.MediaInfo{Container: "mp4", Streams: []frontend.StreamInfo{
		{Type: "audio", Index: 0},
		{Type: "audio", Index: 1, DefaultDisposition: &yes},
		{Type: "subtitle", Index: 2, Default: true},
		{Type: "subtitle", Index: 3},
	}}, false)
	if marked["DefaultAudioStreamIndex"] != 1 || marked["DefaultSubtitleStreamIndex"] != 2 {
		t.Fatal(marked)
	}
	ambiguous := map[string]any{}
	i.jellyfinMediaSourceDTO(ambiguous, frontend.MediaInfo{Container: "mkv", Streams: []frontend.StreamInfo{
		{Type: "video", Index: 0},
		{Type: "audio", Index: 1},
		{Type: "audio", Index: 2},
	}}, false)
	if ambiguous["DefaultAudioStreamIndex"] != 1 {
		t.Fatal(ambiguous)
	}
	estimated := map[string]any{}
	i.jellyfinMediaSourceDTO(estimated, frontend.MediaInfo{Estimated: true, Streams: []frontend.StreamInfo{{Type: "audio", Index: 1}}}, false)
	if _, ok := estimated["DefaultAudioStreamIndex"]; ok || estimated["VideoType"] != nil {
		t.Fatal(estimated)
	}
}

func TestPlannedAudioReplacesTheFileDefault(t *testing.T) {
	source := map[string]any{"DefaultAudioStreamIndex": 1, "DefaultSubtitleStreamIndex": 3}
	media := frontend.MediaInfo{Streams: []frontend.StreamInfo{
		{Type: "audio", Index: 1, Language: "tur", Default: true},
		{Type: "audio", Index: 2, Language: "eng"},
		{Type: "subtitle", Index: 3, Language: "tur", Default: true},
		{Type: "subtitle", Index: 4, Language: "eng", Forced: true},
	}}
	applyPlannedStreamIndexes(source, media, frontend.Decision{AudioStream: 2, SubtitleStream: 4})
	if source["DefaultAudioStreamIndex"] != 2 || source["DefaultSubtitleStreamIndex"] != 4 {
		t.Fatal(source)
	}
	applyPlannedStreamIndexes(source, media, frontend.Decision{AudioStream: 2, SubtitleStream: -1})
	if source["DefaultAudioStreamIndex"] != 2 || source["DefaultSubtitleStreamIndex"] != -1 {
		t.Fatal(source)
	}
}

func TestJellyfinResumeStoresNoSubtitle(t *testing.T) {
	source := map[string]any{}
	item := map[string]any{
		"UserData":     map[string]any{"PlaybackPositionTicks": int64(50), "Played": false},
		"MediaSources": []any{source},
	}
	jellyfinResumeSubtitle(item)
	if source["DefaultSubtitleStreamIndex"] != -1 {
		t.Fatal(source)
	}
	finished := map[string]any{}
	jellyfinResumeSubtitle(map[string]any{
		"UserData":     map[string]any{"PlaybackPositionTicks": int64(50), "Played": true},
		"MediaSources": []any{finished},
	})
	if _, ok := finished["DefaultSubtitleStreamIndex"]; ok {
		t.Fatal(finished)
	}
}
