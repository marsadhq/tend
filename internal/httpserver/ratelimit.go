package httpserver

import (
	"net/http"

	"github.com/marsadhq/kit/httpx"
)

// envTrustProxy opts in to keying rate limits on X-Forwarded-For; see clientIP.
const envTrustProxy = "TEND_TRUST_PROXY"

// clientIP returns the request's source IP for rate-limit keying: the TCP peer
// by default, and the left-most X-Forwarded-For address only when
// TEND_TRUST_PROXY is truthy (set by a trusted proxy). See httpx.ClientIP. The
// variable is read per request, so a change takes effect without a restart.
func clientIP(r *http.Request) string {
	return httpx.ClientIP(r, httpx.EnvFlag(envTrustProxy))
}
