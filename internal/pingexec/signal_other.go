//go:build !unix

package pingexec

import "os"

// forwardedSignals are passed on to the command.
var forwardedSignals = []os.Signal{os.Interrupt}

// killedBy reports the signal that killed the process; outside Unix the exit
// code says it all.
func killedBy(*os.ProcessState) (int, bool) { return 0, false }

// signalExitCode is the exit status for a run the signal sig stopped.
func signalExitCode(os.Signal) int { return 1 }
