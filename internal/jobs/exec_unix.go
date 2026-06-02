//go:build unix

package jobs

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup makes cmd the leader of a new process group and arranges
// for context cancellation to SIGKILL the entire group, so child processes the
// job spawns are killed too (not just the top-level shell).
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid targets the whole process group (pgid == leader pid).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
