//go:build !windows

package playback

import (
	"os"
	"syscall"
)

func lockPayloadWitness(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
