package jobs

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// drillJob is a drill whose scratch directories are made in a fresh temp dir,
// returned so the test can check nothing is left in it.
func drillJob(t *testing.T, s Spec) (Job, string) {
	t.Helper()
	if s.ScratchDir == "" {
		s.ScratchDir = t.TempDir()
	}
	return Job{Name: "drill", Type: Drill, TimeoutSeconds: 10, Spec: &s}, s.ScratchDir
}

// assertEmpty fails unless dir holds nothing: every scratch directory made in
// it was removed.
func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		t.Errorf("left behind in %s: %s", dir, e.Name())
	}
}

func wantOutput(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestDrillSuccess(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen")
	j, parent := drillJob(t, Spec{
		Restore: `printf hello > "$TEND_SCRATCH/data.txt" && echo restored`,
		Checks: []string{
			`test "$(cat "$TEND_SCRATCH/data.txt")" = hello`,
			// Steps run in the scratch directory.
			`test -f data.txt && pwd > ` + seen,
		},
		MaxDurationSeconds: 60,
	})
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusSucceeded || res.ExitCode != 0 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	if !strings.HasPrefix(res.Output, "ok: restored in ") {
		t.Fatalf("output does not start with the verdict:\n%s", res.Output)
	}
	wantOutput(t, res.Output, "2 checks passed", "max_duration 1m",
		"restore:", "check 1:", "check 2:", "scratch:", "(removed)",
		"--- restore (exit 0", "$ printf hello", "restored", "--- check 2 (exit 0")
	dir, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("the second check did not run: %v", err)
	}
	if got := filepath.Dir(strings.TrimSpace(string(dir))); got != parent {
		t.Fatalf("the steps ran in %s, want a directory in %s", got, parent)
	}
	assertEmpty(t, parent)
}

func TestDrillRestoreFails(t *testing.T) {
	cleaned := filepath.Join(t.TempDir(), "cleaned")
	j, parent := drillJob(t, Spec{
		Restore: `echo "repository is locked" >&2; exit 4`,
		Checks:  []string{`touch never`},
		Cleanup: `touch ` + cleaned,
	})
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusFailed || res.ExitCode != 4 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	if !strings.HasPrefix(res.Output, "FAIL: the restore failed (exit 4)") {
		t.Fatalf("output:\n%s", res.Output)
	}
	wantOutput(t, res.Output, "repository is locked", "check 1:", "not run")
	if _, err := os.Stat(cleaned); err != nil {
		t.Fatalf("the cleanup did not run after a failed restore: %v", err)
	}
	assertEmpty(t, parent)
}

func TestDrillCheckFails(t *testing.T) {
	j, parent := drillJob(t, Spec{
		Restore: `true`,
		Checks:  []string{`echo "integrity: page 7 corrupt"; false`, `exit 7`, `true`},
	})
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusFailed || res.ExitCode != 1 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	wantOutput(t, res.Output,
		"FAIL: check 1 failed (exit 1): echo",
		"FAIL: check 2 failed (exit 7): exit 7",
		"check 3:", "page 7 corrupt", "--- check 3 (exit 0")
	if strings.Contains(res.Output, "FAIL: check 3") {
		t.Fatalf("a passing check was reported:\n%s", res.Output)
	}
	assertEmpty(t, parent)
}

func TestDrillTimeout(t *testing.T) {
	cleaned := filepath.Join(t.TempDir(), "cleaned")
	j, parent := drillJob(t, Spec{
		// A restore that also leaves a child behind holding the output pipe.
		Restore: `sleep 30 & sleep 30`,
		Checks:  []string{`true`},
		Cleanup: `touch ` + cleaned,
	})
	j.TimeoutSeconds = 1
	start := time.Now()
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusTimedOut {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("took %v to time out after 1s", took)
	}
	wantOutput(t, res.Output, "FAIL: the restore did not finish within the job's timeout", "not run")
	if _, err := os.Stat(cleaned); err != nil {
		t.Fatalf("the cleanup did not run after a timeout: %v", err)
	}
	assertEmpty(t, parent)
}

func TestDrillOverMaxDuration(t *testing.T) {
	j, parent := drillJob(t, Spec{Restore: `sleep 2`, Checks: []string{`true`}, MaxDurationSeconds: 1})
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusFailed || res.ExitCode != 1 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	wantOutput(t, res.Output, "FAIL: the restore took 2.", "over max_duration 1s", "check 1:")
	assertEmpty(t, parent)
}

func TestDrillCleanupFails(t *testing.T) {
	j, parent := drillJob(t, Spec{Restore: `true`, Checks: []string{`true`}, Cleanup: `echo "database in use" >&2; exit 5`})
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusFailed || res.ExitCode != 5 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	wantOutput(t, res.Output, "FAIL: the cleanup failed (exit 5)", "database in use")
	assertEmpty(t, parent)
}

func TestDrillCleanupIsBounded(t *testing.T) {
	old := drillCleanupTimeout
	drillCleanupTimeout = time.Second
	t.Cleanup(func() { drillCleanupTimeout = old })
	j, parent := drillJob(t, Spec{Restore: `true`, Cleanup: `sleep 30`})
	start := time.Now()
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusFailed || !strings.Contains(res.Output, "FAIL: the cleanup did not finish within 1s") {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Fatalf("a stuck cleanup held the run for %v", took)
	}
	assertEmpty(t, parent)
}

func TestDrillRemovesReadOnlyScratch(t *testing.T) {
	j, parent := drillJob(t, Spec{
		// Restores keep the modes they find, read-only directories included.
		Restore: `mkdir -p ro/sub && touch ro/sub/f && chmod 0500 ro/sub ro && chmod 0400 ro/sub/f`,
	})
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
	assertEmpty(t, parent)
}

func TestDrillEnvAndNoSplicing(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	// A scratch_dir whose name is shell (touching the file $M names): it
	// reaches the steps only as $TEND_SCRATCH, so quoting it is the steps'
	// business and nothing in it is ever run.
	parent := filepath.Join(dir, `with space $(touch "$M") `+"`touch \"$M\"`")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(dir, "id")
	j, _ := drillJob(t, Spec{
		ScratchDir: parent,
		Restore:    `touch "$TEND_SCRATCH/x" && test "$BACKUP_PASSWORD" = s3cret`,
		Checks:     []string{`test -f "$TEND_SCRATCH/x" && printf '%s' "$TEND_DRILL_ID" > ` + idFile},
	})
	res := NewExecutor().Run(context.Background(), j, map[string]string{"BACKUP_PASSWORD": "s3cret", "M": marker})
	if res.Status != StatusSucceeded {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the scratch_dir's name was run as shell")
	}
	id, err := os.ReadFile(idFile)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^tend_drill_[0-9]+$`).Match(id) {
		t.Fatalf("TEND_DRILL_ID = %q, want tend_drill_<digits> (safe as a database name)", id)
	}
	assertEmpty(t, parent)
}

func TestDrillScratchDirMissing(t *testing.T) {
	j, _ := drillJob(t, Spec{ScratchDir: filepath.Join(t.TempDir(), "missing"), Restore: `true`})
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusFailed || res.ExitCode != -1 || !strings.Contains(res.Output, "scratch directory") {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
}

func TestDrillOutputKeepsHeadAndTail(t *testing.T) {
	j, parent := drillJob(t, Spec{Restore: `echo FIRST LINE; head -c 300000 /dev/zero | tr '\0' x; echo; echo LAST LINE`})
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status %s, output:\n%.500s", res.Status, res.Output)
	}
	wantOutput(t, res.Output, "FIRST LINE", "LAST LINE", "bytes skipped")
	if len(res.Output) > 2*drillStepOutput+4096 {
		t.Fatalf("output is %d bytes for one step", len(res.Output))
	}
	assertEmpty(t, parent)
}

func TestDrillFreshScratchPerAttempt(t *testing.T) {
	dir := t.TempDir()
	// The first attempt leaves a file in its scratch and fails; the second
	// must start from an empty scratch directory.
	j, parent := drillJob(t, Spec{
		Restore: `if [ -f ` + dir + `/tried ]; then test ! -e left; else touch left ` + dir + `/tried; exit 1; fi`,
	})
	j.MaxRetries = 1
	ex := &Executor{Backoff: func(int) time.Duration { return 0 }}
	res := ex.Run(context.Background(), j, nil)
	if res.Status != StatusSucceeded || res.Attempt != 2 {
		t.Fatalf("status %s attempt %d, output:\n%s", res.Status, res.Attempt, res.Output)
	}
	assertEmpty(t, parent)
}

func TestDrillRunLimitCoversCleanup(t *testing.T) {
	e := NewExecutor()
	shell := Job{Type: Shell, TimeoutSeconds: 60, MaxRetries: 1}
	drill := Job{Type: Drill, TimeoutSeconds: 60, MaxRetries: 1}
	if got, want := e.maxRunDuration(drill)-e.maxRunDuration(shell), 2*(drillCleanupTimeout+killGraceDelay); got != want {
		t.Fatalf("a drill's limit exceeds a shell job's by %v, want %v (the cleanup of each attempt)", got, want)
	}
}

func TestValidateSpecDrill(t *testing.T) {
	ok := Spec{Restore: "restic restore latest --target \"$TEND_SCRATCH\"", Checks: []string{"true"}, ScratchDir: "/var/tmp", MaxDurationSeconds: 600}
	if err := ValidateSpec(Drill, &ok); err != nil {
		t.Errorf("ValidateSpec(%+v) = %v", ok, err)
	}
	bad := []struct {
		spec Spec
		want string
	}{
		{Spec{}, "restore is required"},
		{Spec{Restore: "  "}, "restore is required"},
		{Spec{Restore: "true", Checks: []string{"true", " "}}, "check 2 is empty"},
		{Spec{Restore: "true", ScratchDir: "tmp"}, "scratch_dir must be an absolute path"},
		{Spec{Restore: "true", MaxDurationSeconds: -1}, "max_duration_seconds"},
	}
	for _, c := range bad {
		err := ValidateSpec(Drill, &c.spec)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateSpec(%+v) = %v, want an error about %q", c.spec, err, c.want)
		}
	}
}

func TestHeadTail(t *testing.T) {
	h := &headTail{max: 4}
	for _, c := range "abcdefghijk" { // one byte at a time
		h.Write([]byte(string(c)))
	}
	if got := h.String(); got != "abcd\n... [3 bytes skipped] ...\nhijk" {
		t.Errorf("byte by byte: %q", got)
	}
	h = &headTail{max: 4}
	h.Write([]byte("abcdef"))
	if got := h.String(); got != "abcdef" {
		t.Errorf("within head and tail: %q", got)
	}
	h = &headTail{max: 4}
	h.Write([]byte("abcd0123456789"))
	if got := h.String(); got != "abcd\n... [6 bytes skipped] ...\n6789" {
		t.Errorf("one big write: %q", got)
	}
	h = &headTail{max: 4}
	h.Write([]byte("abc"))
	h.Write([]byte("d12"))
	h.Write([]byte("34567"))
	if got := h.String(); got != "abcd\n... [3 bytes skipped] ...\n4567" {
		t.Errorf("mixed writes: %q", got)
	}
}
