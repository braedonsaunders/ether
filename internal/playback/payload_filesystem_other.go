//go:build !linux && !darwin && !windows

package playback

func payloadFilesystemReliable(string) (bool, error) { return false, nil }
