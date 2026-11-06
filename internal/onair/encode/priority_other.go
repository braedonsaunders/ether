//go:build !windows

package encode

import "os/exec"

// Unix renders already execute through nice -n 10.
func lowerRenderPriority(*exec.Cmd) {}
