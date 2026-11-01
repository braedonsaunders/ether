package frontend

// KnownSDR is Emby's evidence rule for an SDR picture. An explicit SDR
// transfer curve is enough. With no transfer at all, 8-bit 4:2:0 is SDR
// once the caller knows Dolby Vision is absent and the transfer is not PQ.
// Other pixel formats, depths, and unrecognised curves stay unlabelled.
func KnownSDR(stream StreamInfo) bool {
	switch stream.ColorTransfer {
	case "bt709", "bt470m", "bt470bg", "smpte170m", "smpte240m", "iec61966-2-1", "bt2020-10", "bt2020-12":
		return true
	case "":
	default:
		return false
	}
	depth := stream.BitDepth
	if stream.BitsPerRawSample != nil && *stream.BitsPerRawSample > 0 {
		depth = *stream.BitsPerRawSample
	}
	if depth != 8 {
		return false
	}
	switch stream.PixelFormat {
	case "yuv420p", "yuvj420p":
		return true
	default:
		return false
	}
}
