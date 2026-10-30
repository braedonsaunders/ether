package playback

import "syscall"

func payloadFilesystemReliable(dir string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false, err
	}
	// Local APFS/HFS only. A remote/unknown filesystem cannot witness retirement.
	return (st.Type == 0x1a || st.Type == 0x11) && st.Flags&syscall.MNT_LOCAL != 0, nil
}
