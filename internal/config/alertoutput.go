package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/marsadhq/tend/internal/core"
)

// EnvAlertOutputLines sets how many lines of output (a failed run's, a
// failure ping's) a chat alert shows: 0 leaves the output out.
const EnvAlertOutputLines = "TEND_ALERT_OUTPUT_LINES"

// LoadAlertOutputLines reads TEND_ALERT_OUTPUT_LINES, defaulting to
// core.OutputTailLines (10). A value that is not a whole number from 0 to
// core.MaxOutputTailLines (50, what an event keeps) is an error naming the
// variable. Whatever the lines, the output an alert shows is at most
// core.OutputTailBytes (1000 bytes).
func LoadAlertOutputLines() (int, error) {
	v := strings.TrimSpace(os.Getenv(EnvAlertOutputLines))
	if v == "" {
		return core.OutputTailLines, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 || n > core.MaxOutputTailLines {
		return 0, fmt.Errorf("%s=%q: want a whole number of lines from 0 (no output in chat alerts) to %d", EnvAlertOutputLines, v, core.MaxOutputTailLines)
	}
	return n, nil
}
