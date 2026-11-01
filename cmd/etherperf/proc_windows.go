package main

import (
	"os"
	"os/exec"
)

func setProcessGroup(*exec.Cmd) {}

func interruptGroup(p *os.Process) { _ = p.Kill() }

func killGroup(p *os.Process) { _ = p.Kill() }
