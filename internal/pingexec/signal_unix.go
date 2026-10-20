//go:build unix

package pingexec

import (
	"os"
	"syscall"
)

// forwardedSignals are passed on to the command.
var forwardedSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT}

// killedBy reports the signal that killed the process, if one did.
func killedBy(state *os.ProcessState) (int, bool) {
	ws, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return int(ws.Signal()), true
}
