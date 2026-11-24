//go:build windows

package route

import (
	"os/exec"
	"syscall"
)

// spawnPoolRebuild starts the fleet rebuild detached: the router must not hold
// handles to it, and a restarted daemon must not kill the rebuild mid-flight.
// DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP.
func spawnPoolRebuild(script string) error {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script, "-Force")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
