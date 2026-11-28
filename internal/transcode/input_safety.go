package transcode

// These demuxers cover the self-contained video containers accepted by
// nzb.IsVideoExt. In particular, a downloaded file's extension is not proof
// of its contents: generic probing must not turn a VOD source into a playlist
// that can open other inputs. Keep this list explicit rather than blacklisting
// individual manifest formats.
//
// The audio demuxers below are self-contained single-file formats too (music,
// audiobooks: MP3, FLAC, AAC/ADTS, WAV, AIFF, CAF, WavPack, APE and raw
// Dolby/DTS); none of them can reference another input. M4A/M4B use mov.
const vodInputFormats = "matroska,webm,mov,mp4,m4a,3gp,3g2,mj2,avi,mpegts,asf,mpeg,mpegvideo,ogg,flv," +
	"mp3,flac,aac,wav,aiff,caf,wv,ape,ac3,eac3,dts,truehd"

// inputSafetyArgs applies to the next -i only. Live playlist support is an
// explicit trusted call-site choice, never inferred from media metadata.
// Network inputs cannot access files, pipes, or FFmpeg's other pseudo protocols.
func inputSafetyArgs(input string, live bool) []string {
	formats := vodInputFormats
	if live {
		// HLS masters often carry a WebVTT subtitle rendition. ffprobe opens
		// that demuxer while probing the playlist; leaving it off the list
		// rejects the whole channel.
		formats += ",hls,dash,aac,mp3,ac3,eac3,webvtt"
	}
	protocols := "http,https,tcp,tls,crypto"
	if !isNetworkURL(input) {
		// Local files are used by the local-file spike and offline fixtures.
		protocols = "file"
	}
	return []string{"-format_whitelist", formats, "-protocol_whitelist", protocols}
}

// subtitleInputFormats lists self-contained subtitle demuxers (plus container
// formats that may carry a single subtitle stream) for sidecar -i inputs.
const subtitleInputFormats = "srt,subrip,ass,ssa,webvtt,vtt,microdvd,mpl2,pjs,realtext,sami,stl,subviewer,subviewer1,jacosub,vplayer,matroska,webm,mov,mp4"

// subtitleInputSafetyArgs bounds a sidecar subtitle -i to subtitle demuxers
// and its transport protocol so a provider-supplied file cannot act as a
// playlist or open other inputs.
func subtitleInputSafetyArgs(input string) []string {
	protocols := "http,https,tcp,tls,crypto"
	if !isNetworkURL(input) {
		protocols = "file"
	}
	return []string{"-format_whitelist", subtitleInputFormats, "-protocol_whitelist", protocols}
}
