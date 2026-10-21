package jobs_test

import (
	"context"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/store"
)

// TestRunnerRedactsASecretAcrossTheOutputCap: a shell job that prints just
// under 1 MiB and then a secret has the secret cut by the 1 MiB cap. The
// executor keeps a secret's length past the cap, so the secret is captured
// whole and redacted before the stored output is cut; none of it is stored.
// Once redacting has shortened the text, what came after the cap is not let
// in to fill the space either: that is where a secret cut by the capture
// itself would be.
func TestRunnerRedactsASecretAcrossTheOutputCap(t *testing.T) {
	const secret = "tok_0123456789abcdefghijklmnopqrstuvw" // 37 bytes
	const mib = 1 << 20
	filler := strings.Repeat("x", mib-20)
	cases := []outputJob{
		// The reported case: 20 bytes of the secret fit under the cap.
		{name: "across-the-cap", raw: filler + secret + strings.Repeat("y", 4096), secret: secret},
		// Secrets early on shrink the text when redacted; the capture still
		// ends inside the secret printed after them.
		{name: "after-redactions-shrink-the-text",
			raw: strings.Repeat(secret+"\n", 2000) + strings.Repeat("x", mib-2000*38-20) + secret + strings.Repeat("y", 4096), secret: secret},
		// A secret that starts just past the cap is cut by the capture; the
		// text before it shrank, so it would fit under the cap.
		{name: "just-past-the-cap-after-redactions",
			raw: strings.Repeat(secret+"\n", 2000) + strings.Repeat("x", mib-2000*38+10) + secret + strings.Repeat("y", 4096), secret: secret},
	}
	forEachStore(t, func(t *testing.T, s store.Store) {
		got := runOutputJobs(t, context.Background(), s, cases)
		for _, c := range cases {
			out := got[c.name]
			for n := 6; n <= len(secret); n++ {
				if strings.Contains(out, secret[:n]) {
					t.Errorf("%s: %d bytes of the secret are stored: ...%q", c.name, n, clip(out[max(0, len(out)-200):]))
					break
				}
			}
			if !strings.HasSuffix(out, "\n... [output truncated at 1MiB]") {
				t.Errorf("%s: stored output does not end with the truncation marker: %q", c.name, clip(out))
			}
		}
	})
}
