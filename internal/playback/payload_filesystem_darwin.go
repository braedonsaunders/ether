package playback

import "syscall"

func payloadFilesystemReliable(dir string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false, err
	}
	// Local APFS/HFS only. A remote/unknown filesystem cannot witness retirement.
	// MNT_LOCAL is 0x1000. Newer Go syscall packages omit the name.
	const mntLocal uint32 = 0x1000
	return (st.Type == 0x1a || st.Type == 0x11) && st.Flags&mntLocal != 0, nil
}
