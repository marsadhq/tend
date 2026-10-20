package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/marsadhq/tend/internal/hcimport"
	"github.com/marsadhq/tend/internal/heartbeat"
	"github.com/marsadhq/tend/internal/store"
)

// cmdImport dispatches `tend import <source>`.
func cmdImport(ctx context.Context, st store.Store, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "healthchecks" {
		return errors.New("import: usage: tend import healthchecks --api-key-file <file> [--url https://healthchecks.io] [--dry-run] [--include-paused]")
	}
	return cmdImportHealthchecks(ctx, st, orgID, args[1:], stdin, stdout, stderr)
}

// cmdImportHealthchecks reads a Healthchecks project's checks through its
// Management API and creates or updates a heartbeat for each, keeping the
// check's UUID as the ping token so clients move over by changing only the
// base of their ping URL (see package hcimport). The API key is read from a
// file (or stdin, with "-"), never from the command line, and never printed.
func cmdImportHealthchecks(ctx context.Context, st store.Store, orgID int64, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("import healthchecks", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyFile := fs.String("api-key-file", "", "file holding the project's read-write API key (\"-\" reads it from stdin)")
	hcURL := fs.String("url", "https://healthchecks.io", "the Healthchecks instance (a self-hosted one's base URL)")
	dryRun := fs.Bool("dry-run", false, "print what would be imported and write nothing")
	includePaused := fs.Bool("include-paused", false, "import paused checks too (Tend has no pause: they alert once their period passes)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("import healthchecks: unexpected argument %q", fs.Arg(0))
	}
	if *keyFile == "" {
		return errors.New("import healthchecks: --api-key-file is required (the project's read-write API key, from its Settings page)")
	}
	key, err := readAPIKey(*keyFile, stdin)
	if err != nil {
		return fmt.Errorf("import healthchecks: %w", err)
	}

	client := &http.Client{Timeout: time.Minute}
	checks, err := hcimport.Fetch(ctx, client, *hcURL, key)
	if err != nil {
		return fmt.Errorf("import healthchecks: %w", err)
	}
	existing, err := st.ListHeartbeats(ctx, orgID)
	if err != nil {
		return fmt.Errorf("import healthchecks: %w", err)
	}
	plan := hcimport.Plan(checks, existing, hcimport.Options{Now: time.Now(), IncludePaused: *includePaused})

	if *dryRun {
		fmt.Fprintf(stdout, "dry run: %d checks read, nothing is written\n", len(checks))
	} else {
		fmt.Fprintf(stdout, "%d checks read\n", len(checks))
	}
	counts := map[string]int{}
	var failed int
	for _, a := range plan {
		line := fmt.Sprintf("%-10s %-24s", a.Op, a.Name)
		switch a.Op {
		case hcimport.OpCreate:
			line += fmt.Sprintf("  period %s, grace %s", seconds(a.PeriodSeconds), seconds(a.GraceSeconds))
		case hcimport.OpUpdate:
			line += fmt.Sprintf("  period %s -> %s, grace %s -> %s",
				seconds(a.Was.PeriodSeconds), seconds(a.PeriodSeconds), seconds(a.Was.GraceSeconds), seconds(a.GraceSeconds))
		case hcimport.OpSkip:
			line += "  " + a.Reason
		}
		if a.Check.Name != "" && a.Check.Name != a.Name {
			line += fmt.Sprintf("  (%q)", a.Check.Name)
		}
		if !*dryRun && (a.Op == hcimport.OpCreate || a.Op == hcimport.OpUpdate) {
			if _, _, err := st.CreateHeartbeat(ctx, heartbeat.Heartbeat{
				OrgID: orgID, Name: a.Name, Token: a.Token, PeriodSeconds: a.PeriodSeconds, GraceSeconds: a.GraceSeconds,
			}); err != nil {
				failed++
				line = fmt.Sprintf("%-10s %-24s  %v", "failed", a.Name, err)
				fmt.Fprintln(stdout, strings.TrimRight(line, " "))
				continue
			}
		}
		counts[a.Op]++
		fmt.Fprintln(stdout, strings.TrimRight(line, " "))
		for _, w := range a.Warnings {
			fmt.Fprintf(stdout, "  warning: %s\n", w)
		}
	}
	fmt.Fprintf(stdout, "created %d, updated %d, unchanged %d, skipped %d", counts[hcimport.OpCreate], counts[hcimport.OpUpdate], counts[hcimport.OpUnchanged], counts[hcimport.OpSkip])
	if failed > 0 {
		fmt.Fprintf(stdout, ", failed %d", failed)
	}
	fmt.Fprintln(stdout)
	if *dryRun {
		fmt.Fprintln(stdout, "dry run: nothing was written; run again without --dry-run to import")
	} else {
		fmt.Fprintf(stdout, "Ping URLs keep working with only their base changed: https://hc-ping.com/<uuid> -> %s/ping/<uuid>\n", baseURL())
	}
	if failed > 0 {
		return fmt.Errorf("import healthchecks: %d heartbeats could not be written", failed)
	}
	return nil
}

// readAPIKey reads an API key from path, or from stdin for "-", trimmed of
// surrounding space.
func readAPIKey(path string, stdin io.Reader) (string, error) {
	var (
		raw []byte
		err error
	)
	if path == "-" {
		if stdin == nil {
			return "", errors.New("no API key on stdin")
		}
		raw, err = io.ReadAll(io.LimitReader(stdin, 4096))
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		return "", fmt.Errorf("read the API key: %w", err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("the API key file %s is empty", path)
	}
	return key, nil
}

// seconds renders a count of seconds as a duration, e.g. "1h0m0s".
func seconds(n int) string { return (time.Duration(n) * time.Second).String() }
