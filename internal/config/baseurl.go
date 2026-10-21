package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// EnvBaseURL is Tend's externally reachable URL: the base of the ping URLs
// the CLI prints and of the links in alerts.
const EnvBaseURL = "TEND_BASE_URL"

// LoadBaseURL reads TEND_BASE_URL for the links in alerts, without a trailing
// slash. Unset or blank it is "" and alerts carry no links (the CLI prints
// ping URLs against http://localhost:8080 then, as it always has). A value
// that is not an absolute http or https URL, or that has a query or a
// fragment, is an error; the caller then leaves the links out.
func LoadBaseURL() (string, error) {
	v := strings.TrimSpace(os.Getenv(EnvBaseURL))
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("%s=%q is not an http or https URL like https://tend.example", EnvBaseURL, v)
	}
	return strings.TrimRight(v, "/"), nil
}
