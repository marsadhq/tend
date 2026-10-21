package jobs_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/marsadhq/kit/clock"
	"github.com/marsadhq/kit/secrets"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// TestSecretsRedactedBeforeProbeAndDrillCuts: an expiry command's output and
// a drill step's output are cut to a size before they are stored. A secret
// that straddles a cut used to be cut in two first, and the half that was
// kept no longer matched the secret when the runner redacted it. Each case
// here puts a 124-character secret across one of those cuts; none of it may
// be stored.
func TestSecretsRedactedBeforeProbeAndDrillCuts(t *testing.T) {
	secret := "s3cr3t-" + strings.Repeat("Zq7xKw9", 17)[:117]
	if len(secret) != 124 {
		t.Fatalf("secret is %d characters", len(secret))
	}
	fill := func(n int, c byte, stderr bool) string {
		cmd := fmt.Sprintf("head -c %d /dev/zero | tr '\\0' %c", n, c)
		if stderr {
			cmd += " >&2"
		}
		return cmd
	}
	cases := []struct {
		name string
		job  jobs.Job
	}{
		{"expiry stdout", jobs.Job{Type: jobs.Expiry, Spec: &jobs.Spec{WarnDays: 14,
			ExpiryCommand: `printf '%s\n' "$API_KEY"`}}},
		{"expiry stderr", jobs.Job{Type: jobs.Expiry, Spec: &jobs.Spec{WarnDays: 14,
			ExpiryCommand: fill(16340, 'e', true) + `; printf '%s' "$API_KEY" >&2; echo 2030-01-01T00:00:00Z`}}},
		{"drill head", jobs.Job{Type: jobs.Drill, Spec: &jobs.Spec{
			Restore: fill(32700, 'h', false) + `; printf '%s' "$API_KEY"; ` + fill(100000, 'm', false)}}},
		{"drill tail", jobs.Job{Type: jobs.Drill, Spec: &jobs.Spec{
			Restore: fill(100000, 'm', false) + `; printf '%s' "$API_KEY"; ` + fill(32700, 't', false)}}},
	}

	forEachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID := seedOrg(t, ctx, s)
		box, err := secrets.NewBox(base64.StdEncoding.EncodeToString(make([]byte, 32)))
		if err != nil {
			t.Fatal(err)
		}
		ct, err := box.Encrypt([]byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PutSecret(ctx, orgID, "api_key", ct); err != nil {
			t.Fatal(err)
		}
		ids := map[string]int64{}
		for i, c := range cases {
			j := c.job
			j.OrgID, j.Name, j.Enabled = orgID, fmt.Sprintf("job-%d", i), true
			j.Env = map[string]string{"API_KEY": "{{ secret.api_key }}"}
			if j.Type == jobs.Drill {
				j.Spec.ScratchDir = t.TempDir()
			}
			id, err := s.CreateJob(ctx, j)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.EnqueueRun(ctx, orgID, id); err != nil {
				t.Fatal(err)
			}
			ids[c.name] = id
		}
		r := jobs.NewRunner(s, jobs.NewExecutor(), box, clock.RealClock{})
		if err := r.DrainOnce(ctx); err != nil {
			t.Fatalf("DrainOnce: %v", err)
		}
		for _, c := range cases {
			runs, err := s.ListRuns(ctx, orgID, ids[c.name], 1)
			if err != nil || len(runs) != 1 {
				t.Fatalf("%s: ListRuns: %v", c.name, err)
			}
			out := runs[0].Output
			for i := 0; i+16 <= len(secret); i++ {
				if strings.Contains(out, secret[i:i+16]) {
					t.Errorf("%s: stored output holds %q, part of the secret (from character %d)", c.name, secret[i:i+16], i)
					break
				}
			}
			if !strings.Contains(out, "***") {
				t.Errorf("%s: the secret was not redacted where it was printed", c.name)
			}
		}
	})
}
