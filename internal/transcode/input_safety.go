package transcode

// These demuxers cover the self-contained video containers accepted by
// nzb.IsVideoExt. In particular, a downloaded file's extension is not proof
// of its contents: generic probing must not turn a VOD source into a playlist
// that can open other inputs. Keep this list explicit rather than blacklisting
// individual manifest formats.
const vodInputFormats = "matroska,webm,mov,mp4,m4a,3gp,3g2,mj2,avi,mpegts,asf,mpeg,mpegvideo,ogg,flv"

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
