package playback

import "golang.org/x/sys/unix"

func payloadFilesystemReliable(dir string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, err
	}
	// Local APFS/HFS only. A remote/unknown filesystem cannot witness retirement.
	// Go 1.27 dropped syscall.MNT_LOCAL; x/sys still publishes the mount flag.
	return (st.Type == 0x1a || st.Type == 0x11) && st.Flags&unix.MNT_LOCAL != 0, nil
}
