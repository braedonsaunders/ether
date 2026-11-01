package playback

import "golang.org/x/sys/unix"

func payloadFilesystemReliable(dir string) (bool, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return false, err
	}
	// Local ZFS/UFS/tmpfs only; network and stacked filesystems fail closed.
	switch unix.ByteSliceToString(st.Fstypename[:]) {
	case "zfs", "ufs", "tmpfs":
		return st.Flags&unix.MNT_LOCAL != 0 && st.Flags&unix.MNT_UNION == 0, nil
	}
	return false, nil
}
