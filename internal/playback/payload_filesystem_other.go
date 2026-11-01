//go:build !linux && !darwin && !windows && !freebsd

package playback

func payloadFilesystemReliable(string) (bool, error) { return false, nil }
