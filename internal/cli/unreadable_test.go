package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite" // the raw handle that corrupts a row

	"github.com/marsadhq/tend/internal/cli"
)

// withUnreadableJob makes a database with a readable job "fine" and a job
// "broken" whose env cannot be decoded, and returns its path.
func withUnreadableJob(t *testing.T) string {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "tend.db")
	for _, args := range [][]string{
		{"-name", "fine", "-command", "true"},
		{"-name", "broken", "-command", "true", "-env", "A=b"},
	} {
		if _, err := addJob(t, dsn, args...); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE jobs SET env = '{not json' WHERE name = 'broken'`); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func TestJobListAndDoctorShowUnreadableJob(t *testing.T) {
	dsn := withUnreadableJob(t)
	for _, cmd := range [][]string{{"job", "list"}, {"doctor"}} {
		var stdout, stderr bytes.Buffer
		if err := cli.Run(context.Background(), tempConfigAt(dsn), cmd, nil, &stdout, &stderr); err != nil {
			t.Fatalf("%v: %v", cmd, err)
		}
		out := stdout.String()
		if !strings.Contains(out, `job "broken"`) || !strings.Contains(out, "cannot be read") {
			t.Errorf("%v does not report the unreadable job:\n%s", cmd, out)
		}
		if cmd[0] == "job" && !strings.Contains(out, "fine") {
			t.Errorf("job list lost the readable job:\n%s", out)
		}
	}
}

func TestJobRmRemovesUnreadableJob(t *testing.T) {
	dsn := withUnreadableJob(t)
	var stdout, stderr bytes.Buffer
	if err := cli.Run(context.Background(), tempConfigAt(dsn), []string{"job", "rm", "broken"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("job rm: %v", err)
	}
	stdout.Reset()
	if err := cli.Run(context.Background(), tempConfigAt(dsn), []string{"job", "list"}, nil, &stdout, &stderr); err != nil {
		t.Fatalf("job list: %v", err)
	}
	if strings.Contains(stdout.String(), "broken") {
		t.Fatalf("job rm left the unreadable job:\n%s", stdout.String())
	}
}
