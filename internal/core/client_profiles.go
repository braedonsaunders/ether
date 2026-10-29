package core

// ClientProfileCondition preserves a client-declared condition for the broker.
// Required conditions unknown to an evaluator must not be silently accepted.
type ClientProfileCondition struct {
	Operator string `json:"operator"`
	Property string `json:"property"`
	Value    string `json:"value"`
	Required bool   `json:"required"`
}

// ClientDirectPlayProfile keeps container and codec alternatives paired.
// Empty alternatives represent an unconstrained field, not another profile.
type ClientDirectPlayProfile struct {
	MediaType   string   `json:"mediaType"`
	Containers  []string `json:"containers"`
	VideoCodecs []string `json:"videoCodecs"`
	AudioCodecs []string `json:"audioCodecs"`
}

type ClientTranscodeProfile struct {
	MediaType, Delivery, Context                                           string
	Containers, VideoCodecs, AudioCodecs                                   []string
	EstimateContentLength, MPEGTSM2TS, CopyTimestamps, SubtitlesInManifest bool
	SeekInfo, MaxAudioChannels                                             string
	MinSegments, SegmentLength                                             int
	BreakOnNonKeyFrames, AudioVBR                                          *bool
	Conditions                                                             []ClientProfileCondition
}

type ClientSubtitleProfile struct{ Format, Method, DIDLMode, Language, Container string }
type ClientContainerProfile struct {
	MediaType                 string
	Containers, SubContainers []string
	Conditions                []ClientProfileCondition
}
type ClientCodecProfile struct {
	MediaType                         string
	Codecs, Containers, SubContainers []string
	Conditions, ApplyConditions       []ClientProfileCondition
}

// ClientCapabilityProfiles are declared together by a single client. Nil
// profile collections mean omitted; an explicit empty collection remains empty.
// A nil pointer on ClientCaps preserves legacy protocol behavior.
type ClientCapabilityProfiles struct {
	DirectPlay                                                                []ClientDirectPlayProfile
	Transcoding                                                               []ClientTranscodeProfile
	Subtitles                                                                 []ClientSubtitleProfile
	Containers                                                                []ClientContainerProfile
	Codecs                                                                    []ClientCodecProfile
	MaxStaticBitrate, MusicStreamingTranscodingBitrate, MaxStaticMusicBitrate int
}
