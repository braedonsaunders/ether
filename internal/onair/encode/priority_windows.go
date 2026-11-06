//go:build windows

package encode

import (
	"os/exec"

	"golang.org/x/sys/windows"
)

func lowerRenderPriority(cmd *exec.Cmd) {
	cmd.SysProcAttr.CreationFlags |= windows.BELOW_NORMAL_PRIORITY_CLASS
}
