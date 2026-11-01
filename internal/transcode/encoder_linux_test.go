//go:build linux

package transcode

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestEncoderDiesWithGuardian(t *testing.T) {
	cmd := exec.Command("ffmpeg")
	prepareEncoder(cmd)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pdeathsig != syscall.SIGKILL {
		t.Fatal("ffmpeg must die with the guardian")
	}
}
