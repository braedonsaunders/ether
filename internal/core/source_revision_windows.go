package core

import (
	"os"
	"syscall"
)

func platformFileIdentity(f *os.File, _ os.FileInfo) (uint64, uint64, bool) {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return 0, 0, false
	}
	id := uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow)
	return uint64(info.VolumeSerialNumber), id, true
}
