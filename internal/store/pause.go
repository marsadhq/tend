package store

// pauseAt, when a test sets it, is called at the named points below, inside
// a transaction and with its locks held, so a test can line up two
// transactions the way a race would and see that they do not deadlock. It
// is nil outside tests.
var pauseAt func(point string)

// Points pauseAt is called at: in FinishRunAndEmit once the run is
// finished (its row locked), and in releaseOne once the job's row is locked.
const (
	pauseFinishRunLocked  = "finish: run row locked"
	pauseReleaseJobLocked = "release: job row locked"
)

func pause(point string) {
	if pauseAt != nil {
		pauseAt(point)
	}
}
