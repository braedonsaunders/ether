package playback

import "syscall"

func payloadFilesystemReliable(dir string) (bool, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return false, err
	}
	// Local ext, XFS, Btrfs, tmpfs, overlay and ZFS. Unknown/NFS/SMB fails closed.
	switch uint64(st.Type) {
	case 0xef53, 0x58465342, 0x9123683e, 0x01021994, 0x794c7630, 0x2fc12fc1:
		return true, nil
	}
	return false, nil
}
