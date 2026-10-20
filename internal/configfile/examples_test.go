package configfile_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/configfile"
)

// TestExampleConfigsParse keeps the shipped examples valid: jobs.yaml,
// jobs.example.yaml, and every job in a YAML block of the probe and drill
// docs must pass Parse.
func TestExampleConfigsParse(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, name := range []string{"jobs.yaml", "jobs.example.yaml"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := configfile.Parse(data); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	block := regexp.MustCompile("(?s)```yaml\n(.*?)```")
	for _, doc := range []string{"docs/probes.md", "docs/drills.md", "README.md"} {
		data, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range block.FindAllSubmatch(data, -1) {
			text := string(m[1])
			switch {
			case strings.HasPrefix(text, "jobs:"):
			case strings.HasPrefix(text, "  - name:"):
				text = "jobs:\n" + text // a job entry shown on its own
			default:
				continue // a block about channels or heartbeats only
			}
			n++
			cfg, err := configfile.Parse([]byte(text))
			if err != nil {
				t.Errorf("%s, YAML block %d: %v\n%s", doc, n, err, text)
				continue
			}
			if len(cfg.Jobs) == 0 {
				t.Errorf("%s, YAML block %d has no jobs", doc, n)
			}
		}
		if n == 0 {
			t.Errorf("%s: no job blocks found", doc)
		}
	}
}
