package jobs

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// drillCleanupTimeout bounds a drill's cleanup command. The cleanup runs after
// the restore and the checks however they ended, a timeout included, so it
// cannot take its time from the job's timeout; it gets this much of its own.
// A variable so tests can shorten it.
var drillCleanupTimeout = time.Minute

// drillStepOutput is how much of each drill step's output is kept: this many
// bytes from its start and this many from its end, with the number of bytes
// skipped in between. A restore can print a great deal, and how it ended is
// at the end.
const drillStepOutput = 32 << 10

// drillStep is one command a drill ran.
type drillStep struct {
	label   string // "restore", "check 2", "cleanup"
	command string
	status  RunStatus
	code    int
	took    time.Duration
	output  string
}

// runDrillStep runs one of a drill's commands exactly as a shell job's command
// runs (shellCommand), in the scratch directory.
func runDrillStep(ctx context.Context, label, command string, env map[string]string, secrets []string, dir string) drillStep {
	out := &headTail{max: drillStepOutput, margin: secretMargin(secrets)}
	cmd := shellCommand(ctx, command, env)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = out, out
	start := time.Now()
	err := cmd.Run()
	took := time.Since(start)
	status, code := shellOutcome(ctx, err)
	return drillStep{label: label, command: command, status: status, code: code, took: took, output: out.text(secrets)}
}

// runOnceDrill runs the drill branch of runOnce.
//
// It makes a scratch directory (in scratch_dir, or the system's temporary
// directory), runs the restore in it, and, when the restore succeeded, every
// check in order. Then, however that went, it runs the cleanup command and
// removes the scratch directory. The steps learn where the scratch directory
// is from $TEND_SCRATCH (also their working directory) and get a name safe to
// use for a temporary database in $TEND_DRILL_ID; Tend never splices either
// into a command.
//
// The run fails when the restore or a check fails, when the restore took
// longer than max_duration, when the cleanup fails, or when the scratch
// directory cannot be removed; it times out when the restore or a check runs
// into the job's timeout. Its exit code is that of the first step that
// failed (1 for max_duration, -1 for the scratch directory).
func (e *Executor) runOnceDrill(ctx context.Context, j Job, env map[string]string, secrets []string, attempt int, started time.Time) RunResult {
	s := specOf(j)
	if err := ValidateSpec(Drill, &s); err != nil {
		return probeErrorResult(ctx, attempt, started, err)
	}
	scratch, err := os.MkdirTemp(s.ScratchDir, "tend_drill_*")
	if err != nil {
		return probeErrorResult(ctx, attempt, started, fmt.Errorf("make the scratch directory: %w", err))
	}
	removed := false
	defer func() {
		if !removed { // only when something above panicked
			_ = removeScratch(scratch)
		}
	}()

	stepEnv := make(map[string]string, len(env)+2)
	for k, v := range env {
		stepEnv[k] = v
	}
	stepEnv["TEND_SCRATCH"] = scratch
	stepEnv["TEND_DRILL_ID"] = filepath.Base(scratch)

	var r probeReport
	status, exit := StatusSucceeded, 0
	failed := func(st RunStatus, code int) {
		if status == StatusSucceeded { // the first failure decides
			status, exit = st, code
		}
	}
	var steps []drillStep

	restore := runDrillStep(ctx, "restore", s.Restore, stepEnv, secrets, scratch)
	steps = append(steps, restore)
	switch restore.status {
	case StatusSucceeded:
		r.add("restore", "ok, took %s", durationText(restore.took))
	case StatusTimedOut:
		r.fail("the restore did not finish within the job's timeout (%s)", durationText(restore.took))
		r.add("restore", "timed out after %s", durationText(restore.took))
		failed(StatusTimedOut, 0)
	default:
		r.fail("the restore failed (exit %d)", restore.code)
		r.add("restore", "failed (exit %d) after %s", restore.code, durationText(restore.took))
		failed(StatusFailed, restore.code)
	}

	passed := 0
	for i, command := range s.Checks {
		label := fmt.Sprintf("check %d", i+1)
		switch {
		case restore.status != StatusSucceeded:
			r.add(label, "not run (the restore did not succeed)")
			continue
		case ctx.Err() != nil:
			r.add(label, "not run (the job's timeout was reached)")
			continue
		}
		c := runDrillStep(ctx, label, command, stepEnv, secrets, scratch)
		steps = append(steps, c)
		switch c.status {
		case StatusSucceeded:
			passed++
			r.add(label, "ok")
		case StatusTimedOut:
			r.fail("%s did not finish within the job's timeout: %s", label, firstLine(command))
			r.add(label, "timed out")
			failed(StatusTimedOut, 0)
		default:
			r.fail("%s failed (exit %d): %s", label, c.code, firstLine(command))
			r.add(label, "failed (exit %d)", c.code)
			failed(StatusFailed, c.code)
		}
	}

	limit := seconds(s.MaxDurationSeconds)
	if restore.status == StatusSucceeded && limit > 0 && restore.took > limit {
		r.fail("the restore took %s, over max_duration %s", durationText(restore.took), durationText(limit))
		failed(StatusFailed, probeExitFailed)
	}

	if strings.TrimSpace(s.Cleanup) != "" {
		// Detached from ctx, whose deadline may well have passed already: the
		// cleanup runs however the restore ended, within its own bound.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drillCleanupTimeout)
		c := runDrillStep(cctx, "cleanup", s.Cleanup, stepEnv, secrets, scratch)
		cancel()
		steps = append(steps, c)
		switch c.status {
		case StatusSucceeded:
			r.add("cleanup", "ok")
		case StatusTimedOut:
			r.fail("the cleanup did not finish within %s", durationText(drillCleanupTimeout))
			r.add("cleanup", "timed out")
			failed(StatusFailed, probeExitError)
		default:
			r.fail("the cleanup failed (exit %d)", c.code)
			r.add("cleanup", "failed (exit %d)", c.code)
			failed(StatusFailed, c.code)
		}
	}

	removed = true
	if err := removeScratch(scratch); err != nil {
		r.fail("the scratch directory could not be removed: %v", err)
		r.add("scratch", "%s (NOT removed)", scratch)
		failed(StatusFailed, probeExitError)
	} else {
		r.add("scratch", "%s (removed)", scratch)
	}

	var b strings.Builder
	if len(r.problems) > 0 {
		for _, p := range r.problems {
			b.WriteString("FAIL: " + p + "\n")
		}
	} else {
		fmt.Fprintf(&b, "ok: restored in %s", durationText(restore.took))
		switch len(s.Checks) {
		case 0:
			b.WriteString(", no checks")
		case 1:
			b.WriteString(", 1 check passed")
		default:
			fmt.Fprintf(&b, ", %d checks passed", passed)
		}
		if limit > 0 {
			fmt.Fprintf(&b, " (max_duration %s)", durationText(limit))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n" + strings.Join(r.info, "\n") + "\n")
	for _, st := range steps {
		fmt.Fprintf(&b, "\n--- %s (exit %d, %s)\n$ %s\n", st.label, st.code, durationText(st.took), st.command)
		if st.status == StatusTimedOut {
			fmt.Fprintf(&b, "(timed out)\n")
		}
		if out := strings.TrimRight(st.output, "\n"); out != "" {
			b.WriteString(out + "\n")
		}
	}
	return RunResult{
		Status:   status,
		ExitCode: exit,
		Output:   strings.TrimRight(b.String(), "\n"),
		Attempt:  attempt,
		Started:  started,
		Ended:    time.Now(),
	}
}

// firstLine is a command as a FAIL line names it: its first line, cut short.
func firstLine(command string) string {
	line, _, more := strings.Cut(strings.TrimSpace(command), "\n")
	if len(line) > 80 {
		line, more = line[:80], true
	}
	if more {
		line += " ..."
	}
	return line
}

// removeScratch removes a drill's scratch directory and everything in it. A
// restore keeps the modes it finds, so a directory in it may be read-only and
// os.RemoveAll unable to empty it; then every directory in the tree is made
// writable by its owner and the removal tried again. Symbolic links are
// removed, never followed.
func removeScratch(dir string) error {
	if err := os.RemoveAll(dir); err == nil {
		return nil
	}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(dir)
}

// headTail keeps the first and the last max bytes written to it, and counts
// the bytes skipped in between. Write never fails, so a chatty step is never
// stopped by a full pipe, and costs the same however small the writes are.
//
// Each end is captured margin bytes longer than it is shown (margin is the
// length of the longest secret), so text redacts the secrets before it makes
// the cuts: a secret that straddled one is then redacted whole instead of
// being kept in part.
type headTail struct {
	max    int
	margin int
	head   []byte
	ring   []byte // the last bytes written after head filled up, wrapping at pos
	pos    int
	rest   int64 // bytes written after head filled up
}

func (h *headTail) Write(p []byte) (int, error) {
	n := len(p)
	keep := h.max + h.margin
	if room := keep - len(h.head); room > 0 {
		k := min(room, len(p))
		h.head = append(h.head, p[:k]...)
		p = p[k:]
	}
	if len(p) == 0 {
		return n, nil
	}
	if h.ring == nil {
		h.ring = make([]byte, keep)
	}
	h.rest += int64(len(p))
	if len(p) >= keep {
		copy(h.ring, p[len(p)-keep:])
		h.pos = 0
		return n, nil
	}
	k := copy(h.ring[h.pos:], p)
	copy(h.ring, p[k:])
	h.pos = (h.pos + len(p)) % keep
	return n, nil
}

// String is text with no secrets to redact.
func (h *headTail) String() string { return h.text(nil) }

// text returns what was kept with secrets redacted, then cut to its first and
// last max bytes with a marker where bytes were skipped. Cuts fall on
// character boundaries.
func (h *headTail) text(secrets []string) string {
	keep := int64(h.max + h.margin)
	var tail []byte
	if h.rest <= keep {
		tail = h.ring[:h.rest]
	} else {
		tail = append(append([]byte(nil), h.ring[h.pos:]...), h.ring[:h.pos]...)
	}
	if h.rest <= keep {
		// Nothing was lost: redact the whole and cut it once, if at all.
		whole := redact(string(h.head)+string(tail), secrets)
		if len(whole) <= 2*h.max {
			return whole
		}
		return cutMiddle(whole[:h.max], whole[len(whole)-h.max:], int64(len(whole)-2*h.max))
	}
	head := redact(string(h.head), secrets)
	end := redact(string(tail), secrets)
	head = head[:min(len(head), h.max)]
	end = end[max(0, len(end)-h.max):]
	skipped := h.rest + int64(len(h.head)) - int64(len(head)) - int64(len(end))
	return cutMiddle(head, end, skipped)
}

// cutMiddle joins the kept start and end of an output around the marker for
// the bytes skipped in between, trimming each to whole characters.
func cutMiddle(head, tail string, skipped int64) string {
	t := []byte(tail)
	for len(t) > 0 && !utf8.RuneStart(t[0]) {
		t = t[1:]
	}
	return string(trimPartialRune([]byte(head))) + fmt.Sprintf("\n... [%d bytes skipped] ...\n", skipped) + string(t)
}
