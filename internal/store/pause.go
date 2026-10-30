package store

// pauseAt, when a test sets it, is called at the named points below, inside
// a transaction and with its locks held, so a test can line up two
// transactions the way a race would and see that they do not deadlock. It
// is nil outside tests.
var pauseAt func(point string)

// Points pauseAt is called at.
const (
	pauseFinishRunLocked  = "finish: run row locked"  // FinishRunAndEmit, after the run is finished
	pauseReleaseJobLocked = "release: job row locked" // releaseOne, after the job's row is locked
)

func pause(point string) {
	if pauseAt != nil {
		pauseAt(point)
	}
}
