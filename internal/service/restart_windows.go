//go:build windows

package service

import (
	"os"
	"os/exec"
)

// restartProcess starts exe in this console and returns; the caller exits.
func restartProcess(exe string, args, env []string) error {
	cmd := exec.Command(exe, args[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Start()
}
