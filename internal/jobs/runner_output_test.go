package jobs_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/marsadhq/tend/internal/clock"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/secrets"
	"github.com/marsadhq/tend/internal/store"
)

// The documented output cap and the marker that ends truncated output. Written
// as literals so a changed constant cannot hide here.
const (
	outputCap       = 1 << 20
	truncatedMarker = "\n... [output truncated at 1MiB]"
)

// outputJob is one job whose stored output a test wants to look at.
type outputJob struct {
	name string
	// raw is what the job prints, byte for byte (served from a file by cat).
	raw string
	// secret, when not empty, is stored encrypted and injected as $API_KEY, so
	// the runner redacts it from the output.
	secret string
}

// runOutputJobs runs every job through a real runner against s and returns
// each job's stored output by name. Every run must succeed.
func runOutputJobs(t *testing.T, ctx context.Context, s store.Store, cases []outputJob) map[string]string {
	t.Helper()
	orgID := seedOrg(t, ctx, s)
	box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	dir := t.TempDir()

	jobIDs := map[string]int64{}
	for i, c := range cases {
		file := filepath.Join(dir, fmt.Sprintf("out-%d", i))
		if err := os.WriteFile(file, []byte(c.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		job := jobs.Job{OrgID: orgID, Name: c.name, Type: jobs.Shell, Command: "cat '" + file + "'", Enabled: true}
		if c.secret != "" {
			ct, err := box.Encrypt([]byte(c.secret))
			if err != nil {
				t.Fatalf("Encrypt: %v", err)
			}
			if err := s.PutSecret(ctx, orgID, "key_"+c.name, ct); err != nil {
				t.Fatalf("PutSecret: %v", err)
			}
			job.Env = map[string]string{"API_KEY": "{{ secret.key_" + c.name + " }}"}
		}
		id, err := s.CreateJob(ctx, job)
		if err != nil {
			t.Fatalf("CreateJob %s: %v", c.name, err)
		}
		if _, err := s.EnqueueRun(ctx, orgID, id); err != nil {
			t.Fatalf("EnqueueRun %s: %v", c.name, err)
		}
		jobIDs[c.name] = id
	}

	r := jobs.NewRunner(s, jobs.NewExecutor(), box, clock.RealClock{})
	if err := r.DrainOnce(ctx); err != nil {
		t.Fatalf("DrainOnce: %v", err)
	}

	out := map[string]string{}
	for name, id := range jobIDs {
		runs, err := s.ListRuns(ctx, orgID, id, 1)
		if err != nil || len(runs) != 1 {
			t.Fatalf("ListRuns %s: %v (%d runs)", name, err, len(runs))
		}
		// GetRun is what the dashboard and the API read the output through.
		run, err := s.GetRun(ctx, orgID, runs[0].ID)
		if err != nil {
			t.Fatalf("GetRun %s: %v", name, err)
		}
		if run.Status != jobs.StatusSucceeded {
			t.Fatalf("%s: run status = %s, want succeeded (a run must finish whatever its job printed)", name, run.Status)
		}
		out[name] = run.Output
	}
	return out
}

// backendOf names the backend a forEachStore subtest is running against.
func backendOf(t *testing.T) string { return path.Base(t.Name()) }

// assertSameOnEveryBackend fails when the backends that ran stored different
// bytes for the same job.
func assertSameOnEveryBackend(t *testing.T, stored map[string]map[string]string) {
	t.Helper()
	ref, ok := stored["sqlite"]
	if !ok {
		t.Fatal("the sqlite backend did not run")
	}
	for backend, outs := range stored {
		for name, out := range outs {
			if out != ref[name] {
				t.Errorf("%s: %s stored %d bytes %q, sqlite stored %d bytes %q",
					name, backend, len(out), clip(out), len(ref[name]), clip(ref[name]))
			}
		}
	}
}

// clip shortens s for an error message.
func clip(s string) string {
	if len(s) > 120 {
		return s[:60] + "..." + s[len(s)-60:]
	}
	return s
}

// between returns s with sep between each of its bytes.
func between(s, sep string) string {
	parts := make([]string, len(s))
	for i := 0; i < len(s); i++ {
		parts[i] = s[i : i+1]
	}
	return strings.Join(parts, sep)
}

// TestRunnerRedactsSecretPrintedWithNULs proves, through the real runner on
// every backend, that a secret printed with NUL bytes between its characters is
// not stored in plain text. NULs are dropped from stored output, so redaction
// has to match the text as it is after that: matched against the raw bytes, the
// spaced-out secret slips past and is then put back together.
func TestRunnerRedactsSecretPrintedWithNULs(t *testing.T) {
	const secret = "s3cr3t-value"
	const wide = "päss-€-wörd" // multi-byte characters
	cases := []struct {
		outputJob
		want string
	}{
		{outputJob{"plain", "key=" + secret + "\n", secret}, "key=***\n"},
		{outputJob{"nul-between-every-character", "key=" + between(secret, "\x00") + "\n", secret}, "key=***\n"},
		{outputJob{"one-nul-in-the-middle", "key=s3cr3t\x00-value\n", secret}, "key=***\n"},
		{outputJob{"nuls-around-and-inside", "\x00\x00s3cr\x003t-value\x00 done\n", secret}, "*** done\n"},
		{outputJob{"plain-and-spaced", secret + " " + between(secret, "\x00") + "\n", secret}, "*** ***\n"},
		{outputJob{"multi-byte-split-between-bytes", "k=" + between(wide, "\x00") + "\n", wide}, "k=***\n"},
		// A secret that is not valid UTF-8 (a Latin-1 password) is still redacted,
		// printed as it is or spaced out.
		{outputJob{"latin-1", "pw=p\xe4ssw\xf6rd\n", "p\xe4ssw\xf6rd"}, "pw=***\n"},
		{outputJob{"latin-1-spaced", "pw=" + between("p\xe4ssw\xf6rd", "\x00") + "\n", "p\xe4ssw\xf6rd"}, "pw=***\n"},
		// One that ends in half a character, printed with a NUL inside and
		// followed by the byte that completes the character.
		{outputJob{"half-character-completed-by-its-neighbour", "k=a\x00bc\xc3\xa9\n", "abc\xc3"}, "k=***�\n"},
	}
	jobList := make([]outputJob, len(cases))
	for i, c := range cases {
		jobList[i] = c.outputJob
	}

	stored := map[string]map[string]string{}
	forEachStore(t, func(t *testing.T, s store.Store) {
		got := runOutputJobs(t, context.Background(), s, jobList)
		stored[backendOf(t)] = got
		for _, c := range cases {
			out := got[c.name]
			if strings.Contains(out, c.secret) {
				t.Errorf("%s: the secret is stored in plain text: %q", c.name, out)
			}
			if out != c.want {
				t.Errorf("%s: stored %q, want %q", c.name, out, c.want)
			}
			if !utf8.ValidString(out) || strings.Contains(out, "\x00") {
				t.Errorf("%s: stored output is not clean text: %q", c.name, out)
			}
		}
	})
	assertSameOnEveryBackend(t, stored)
}

// TestRunnerStoresTheSameCleanTextOnEveryBackend proves both backends store
// identical bytes for output that is not text: NUL bytes are dropped and
// invalid UTF-8 is replaced in the runner, before any store sees it, so the
// dashboard is never served invalid UTF-8 and the two never differ.
func TestRunnerStoresTheSameCleanTextOnEveryBackend(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"empty", "", ""},
		{"plain", "plain text\n", "plain text\n"},
		{"valid-multi-byte", "héllo € \U0001F600\n", "héllo € \U0001F600\n"},
		{"literal-replacement-character", "a�b\n", "a�b\n"},
		{"nul-in-the-middle", "a\x00b\n", "ab\n"},
		{"only-nuls", "\x00\x00\x00", ""},
		{"invalid-byte", "a\xffb\n", "a�b\n"},
		{"run-of-invalid-bytes", "a\xff\xfe\xfdb\n", "a�b\n"},
		{"separate-invalid-bytes", "a\xffb\xfec\n", "a�b�c\n"},
		{"sequence-cut-short-at-the-end", "caf\xc3", "caf�"},
		{"latin-1-text", "na\xefve caf\xe9\n", "na�ve caf�\n"},
		{"overlong-encoding", "a\xc0\xafb\n", "a�b\n"},
		{"surrogate-half", "a\xed\xa0\x80b\n", "a�b\n"},
		{"nuls-splitting-a-character", "\xe2\x00\x82\x00\xac\n", "€\n"},
		{"nuls-between-invalid-bytes", "a\xff\x00\xfeb\n", "a�b\n"},
		{"utf-16-text", "\xff\xfeh\x00i\x00\n\x00", "�hi\n"},
		{"binary", "\x7fELF\x02\x01\x01\x00\x00\x00\x80\x90\xa0\xff\x00end\n", "\x7fELF\x02\x01\x01�end\n"},
		{"the-original-mixture", "before \xff\xfe middle \x00 caf\xc3 after", "before � middle  caf� after"},
	}
	jobList := make([]outputJob, len(cases))
	for i, c := range cases {
		jobList[i] = outputJob{name: c.name, raw: c.raw}
	}

	stored := map[string]map[string]string{}
	forEachStore(t, func(t *testing.T, s store.Store) {
		got := runOutputJobs(t, context.Background(), s, jobList)
		stored[backendOf(t)] = got
		for _, c := range cases {
			out := got[c.name]
			if out != c.want {
				t.Errorf("%s: stored %q, want %q", c.name, out, c.want)
			}
			if !utf8.ValidString(out) || strings.Contains(out, "\x00") {
				t.Errorf("%s: stored output is not clean text: %q", c.name, out)
			}
		}
	})
	assertSameOnEveryBackend(t, stored)
}

// TestRunnerKeepsStoredOutputWithinTheCap proves the 1 MiB cap holds for what
// is stored, not only for what was captured. Replacing invalid bytes and
// redacting a short secret both make the text longer than the bytes the
// executor capped, so the runner trims again: the stored value never exceeds
// the cap plus the marker, is cut on a character boundary, and is the same on
// every backend.
func TestRunnerKeepsStoredOutputWithinTheCap(t *testing.T) {
	const kib = 1 << 10
	cases := []struct {
		outputJob
		// truncated: the stored output must be cut at the cap and end with the
		// marker. Otherwise it must be exactly wantLen bytes with no marker.
		truncated bool
		wantLen   int
		// markedShort: the capture was cut but what is left fits, so the output
		// keeps the marker and is wantLen bytes without it.
		markedShort bool
	}{
		// Each "a\xff" pair is 2 bytes captured and 4 bytes stored.
		{outputJob: outputJob{name: "grows-past-the-cap-from-under-it", raw: strings.Repeat("a\xff", 350*kib)}, truncated: true},
		{outputJob: outputJob{name: "grows-after-the-capture-was-cut", raw: strings.Repeat("a\xff", 800*kib)}, truncated: true},
		// 3 bytes of euro sign plus 1 invalid byte become 6 bytes; the cut must
		// not land inside either character.
		{outputJob: outputJob{name: "multi-byte-around-the-cut", raw: "x" + strings.Repeat("€\xff", 300*kib)}, truncated: true},
		{outputJob: outputJob{name: "two-byte-around-the-cut", raw: strings.Repeat("é\xff", 400*kib)}, truncated: true},
		// A 2 byte secret becomes the 3 bytes of "***".
		{outputJob: outputJob{name: "redaction-grows-it", raw: strings.Repeat("ab", 400*kib), secret: "ab"}, truncated: true},
		// Exactly at the cap, and just under it after growing: untouched.
		{outputJob: outputJob{name: "exactly-the-cap", raw: strings.Repeat("x", outputCap)}, wantLen: outputCap},
		{outputJob: outputJob{name: "grows-to-exactly-the-cap", raw: strings.Repeat("a\xff", outputCap/4)}, wantLen: outputCap},
		// NULs only shrink it.
		{outputJob: outputJob{name: "nuls-shrink-it", raw: strings.Repeat("x\x00", outputCap/2)}, wantLen: outputCap / 2},
		{outputJob: outputJob{name: "nuls-shrink-a-cut-capture", raw: strings.Repeat("x\x00", outputCap)}, markedShort: true, wantLen: outputCap / 2},
		// The capture is cut two bytes short of the cap, because the next euro
		// sign would straddle it. With the marker that is more than the cap, and
		// it must be stored as captured: nothing cut again, the marker once.
		{outputJob: outputJob{name: "capture-cut-short-of-the-cap", raw: "xx" + strings.Repeat("€", 400*kib)}, markedShort: true, wantLen: outputCap - 2},
	}
	jobList := make([]outputJob, len(cases))
	for i, c := range cases {
		jobList[i] = c.outputJob
	}

	stored := map[string]map[string]string{}
	forEachStore(t, func(t *testing.T, s store.Store) {
		got := runOutputJobs(t, context.Background(), s, jobList)
		stored[backendOf(t)] = got
		for _, c := range cases {
			out := got[c.name]
			if len(out) > outputCap+len(truncatedMarker) {
				t.Errorf("%s: stored %d bytes, more than the cap plus the marker (%d)", c.name, len(out), outputCap+len(truncatedMarker))
			}
			if !utf8.ValidString(out) || strings.Contains(out, "\x00") {
				t.Errorf("%s: stored output is not clean text (was a character cut in half?)", c.name)
			}
			if c.secret != "" && strings.Contains(out, c.secret) {
				t.Errorf("%s: the secret is stored in plain text", c.name)
			}
			if n := strings.Count(out, "[output truncated"); n > 1 {
				t.Errorf("%s: the stored output is marked as truncated %d times", c.name, n)
			}
			kept, marked := strings.CutSuffix(out, truncatedMarker)
			switch {
			case c.truncated:
				// A cut gives up at most one character, and none is longer than 4 bytes.
				if !marked || len(kept) > outputCap || len(kept) <= outputCap-utf8.UTFMax {
					t.Errorf("%s: marked=%v, kept %d bytes; want the marker and a cut within one character of the %d byte cap",
						c.name, marked, len(kept), outputCap)
				}
			case c.markedShort:
				if !marked || len(kept) != c.wantLen {
					t.Errorf("%s: marked=%v, kept %d bytes; want the marker after %d bytes", c.name, marked, len(kept), c.wantLen)
				}
			default:
				if marked || len(out) != c.wantLen {
					t.Errorf("%s: marked=%v, %d bytes; want %d bytes and no marker", c.name, marked, len(out), c.wantLen)
				}
			}
		}
	})
	assertSameOnEveryBackend(t, stored)
}

// TestRunnerKeepsStoredHTTPOutputWithinTheCap proves the same for an HTTP job,
// whose stored output starts with a status line in front of the body: status
// line included, the stored value stays within the cap plus the marker.
func TestRunnerKeepsStoredHTTPOutputWithinTheCap(t *testing.T) {
	bodies := map[string]string{
		"plain-over-the-cap":   strings.Repeat("b", outputCap+4096),
		"plain-exactly-at-cap": strings.Repeat("b", outputCap),
		"grows-under-the-cap":  strings.Repeat("a\xff", 350<<10),
		"small":                "pong\x00\xff\n",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, bodies[strings.TrimPrefix(r.URL.Path, "/")])
	}))
	defer srv.Close()

	stored := map[string]map[string]string{}
	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		runIDs := map[string]int64{}
		for name := range bodies {
			jobID, err := s.CreateJob(ctx, jobs.Job{
				OrgID: orgID, Name: name, Type: jobs.HTTP, HTTPURL: srv.URL + "/" + name, Enabled: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			runID, err := s.EnqueueRun(ctx, orgID, jobID)
			if err != nil {
				t.Fatal(err)
			}
			runIDs[name] = runID
		}
		r := jobs.NewRunner(s, jobs.NewExecutor(), nil, clock.RealClock{})
		if err := r.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}

		got := map[string]string{}
		for name, runID := range runIDs {
			run := mustGetRun(t, ctx, s, orgID, runID)
			if run.Status != jobs.StatusSucceeded {
				t.Fatalf("%s: run status = %s, want succeeded", name, run.Status)
			}
			out := run.Output
			got[name] = out
			if len(out) > outputCap+len(truncatedMarker) {
				t.Errorf("%s: stored %d bytes, more than the cap plus the marker (%d)", name, len(out), outputCap+len(truncatedMarker))
			}
			if !strings.HasPrefix(out, "HTTP 200\n") {
				t.Errorf("%s: stored output does not start with the status line: %q", name, clip(out))
			}
			if !utf8.ValidString(out) || strings.Contains(out, "\x00") {
				t.Errorf("%s: stored output is not clean text", name)
			}
			if _, marked := strings.CutSuffix(out, truncatedMarker); marked != (name != "small") {
				t.Errorf("%s: truncation marker present = %v", name, marked)
			}
		}
		if want := "HTTP 200\npong�\n"; got["small"] != want {
			t.Errorf("small: stored %q, want %q", got["small"], want)
		}
		stored[backendOf(t)] = got
	})
	assertSameOnEveryBackend(t, stored)
}
