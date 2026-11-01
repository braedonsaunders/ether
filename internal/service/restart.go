package service

import (
	"errors"
	"os"
	"runtime"
	"strings"

	ksvc "github.com/kardianos/service"
)

// ErrRestartRequested is returned by a RunFunc that wants a fresh process
// (after a self-update, a restore, or the owner's restart). Under a service
// manager the process exits with ExitCodeRestart and the manager starts a
// new one; in a container or a terminal it re-executes itself in place
// (RestartProcess). Interactively Run returns it to the caller.
var ErrRestartRequested = errors.New("service: restart requested")

// ExitCodeRestart is the process exit code used for ErrRestartRequested.
const ExitCodeRestart = 3

// RestartMethod is how this process restarts itself.
type RestartMethod string

const (
	// RestartSupervisor exits and lets the Windows Service Control
	// Manager start a new process (its recovery actions: after 5s, then
	// 15s and 60s for repeated restarts within a day).
	RestartSupervisor RestartMethod = "supervisor"
	// RestartInPlace replaces this process with a fresh copy of itself
	// (exec, same PID) on every Unix: systemd and launchd keep tracking
	// the same process (no RestartSec wait), a container keeps running
	// whatever its restart policy, and a terminal keeps its session.
	RestartInPlace RestartMethod = "in-place"
	// RestartSpawn starts a new process and exits: Windows has no exec.
	RestartSpawn RestartMethod = "spawn"
)

// Restart describes how a restart would happen here.
type Restart struct {
	Method RestartMethod `json:"method"`
	// Manager is what runs Ether: "systemd", "launchd", "windows-service",
	// "service" (another service manager), "container" or "terminal".
	Manager string `json:"manager"`
}

// DetectRestart reports how this process restarts itself.
func DetectRestart() Restart {
	return detectRestart(Interactive(), InContainer(), runtime.GOOS, ksvc.Platform())
}

func detectRestart(interactive, container bool, goos, platform string) Restart {
	switch {
	case container:
		// Ether is PID 1 (or tini's child): exiting would stop the
		// container unless a restart policy happens to be set.
		return Restart{Method: RestartInPlace, Manager: "container"}
	case !interactive && goos == "windows":
		return Restart{Method: RestartSupervisor, Manager: "windows-service"}
	case !interactive:
		manager := "service"
		switch {
		case strings.Contains(platform, "systemd"):
			manager = "systemd"
		case strings.Contains(platform, "launchd"):
			manager = "launchd"
		}
		return Restart{Method: RestartInPlace, Manager: manager}
	case goos == "windows":
		return Restart{Method: RestartSpawn, Manager: "terminal"}
	default:
		return Restart{Method: RestartInPlace, Manager: "terminal"}
	}
}

// startExecutable is this program's path as it started: after a
// self-update replaced the file, /proc/self/exe names the deleted old one,
// while this path holds the new binary.
var startExecutable, _ = os.Executable()

// InContainer reports whether we are running inside a container image
// (which is also updated by pulling a new image, never in place).
func InContainer() bool {
	if v := strings.TrimSpace(os.Getenv("ETHER_IN_CONTAINER")); v != "" && v != "0" && !strings.EqualFold(v, "false") {
		return true
	}
	for _, p := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// RestartProcess starts a fresh copy of this program with the same
// arguments and environment, for a process no service manager restarts. On
// Unix it replaces the current process and only returns on failure; on
// Windows it starts the new process and returns, and the caller exits. The
// caller must have stopped serving and released the data directory first.
func RestartProcess() error {
	exe := startExecutable
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	return restartProcess(exe, os.Args, os.Environ())
}
