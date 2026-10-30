package playback

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func lockPayloadWitness(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{Offset: 1024})
}
func payloadFilesystemReliable(dir string) (bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var final [4096]uint16
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &final[0], uint32(len(final)), 0)
	if err != nil {
		return false, err
	}
	if n >= uint32(len(final)) {
		return false, nil
	}
	resolved := windows.UTF16ToString(final[:])
	if strings.HasPrefix(strings.ToUpper(resolved), `\\?\UNC\`) {
		return false, nil
	}
	resolved = strings.TrimPrefix(resolved, `\\?\`)
	root := filepath.VolumeName(resolved) + `\`
	if strings.HasPrefix(root, `\\`) {
		return false, nil
	}
	ptr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return false, err
	}
	kind := windows.GetDriveType(ptr)
	if kind != windows.DRIVE_FIXED && kind != windows.DRIVE_REMOVABLE {
		return false, nil
	}
	var name [64]uint16
	if err := windows.GetVolumeInformationByHandle(windows.Handle(f.Fd()), nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err != nil {
		return false, err
	}
	fs := windows.UTF16ToString(name[:])
	return fs == "NTFS" || fs == "ReFS", nil
}
