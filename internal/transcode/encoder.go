package transcode

import (
	"os/exec"

	"github.com/braedonsaunders/ether/internal/mediaproc"
)

func prepareEncoder(cmd *exec.Cmd)             { mediaproc.PrepareGuardian(cmd) }
func ownEncoder(cmd *exec.Cmd) (func(), error) { return mediaproc.Own(cmd) }
func stopEncoder(cmd *exec.Cmd)                { mediaproc.Stop(cmd) }
