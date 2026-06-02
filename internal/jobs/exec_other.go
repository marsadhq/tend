//go:build !unix

package jobs

import "os/exec"

// configureProcessGroup is a no-op on non-Unix platforms; the default
// exec.CommandContext behavior (kill the direct child) applies.
func configureProcessGroup(cmd *exec.Cmd) {}
