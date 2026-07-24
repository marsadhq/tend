package httpserver

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marsadhq/tend/internal/clock"
)

// rateLimiterIdleTTL bounds how long an idle bucket is kept before gc drops
// it, so the bucket map does not keep growing as distinct IPs churn through
// the limiter. Between two sweeps the map still holds one small entry per
// address seen.
const rateLimiterIdleTTL = 10 * time.Minute

// rateLimiter is a fixed-rate token bucket keyed by (ip, endpoint). Refill is
// lazy (computed from elapsed time on each Allow), so there is no background
// goroutine. Idle buckets are evicted opportunistically, at most once per
// rateLimiterIdleTTL.
type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens added per second
	burst   float64 // bucket capacity
	clk     clock.Clock
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter returns a rateLimiter allowing burst requests immediately and
// refilling at rate tokens/second thereafter, using clk as its time source.
func newRateLimiter(clk clock.Clock, rate, burst float64) *rateLimiter {
	return &rateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		clk:     clk,
		lastGC:  clk.Now(),
	}
}

// Allow reports whether one token is available for key, consuming it if so.
func (rl *rateLimiter) Allow(key string, now time.Time) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	b := rl.buckets[key]
	if b == nil {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	// lazy refill
	b.tokens = min(rl.burst, b.tokens+now.Sub(b.last).Seconds()*rl.rate)
	b.last = now
	allowed := false
	if b.tokens >= 1 {
		b.tokens -= 1
		allowed = true
	}
	rl.gc(now) // opportunistic eviction of stale buckets
	return allowed
}

// gc drops buckets untouched for longer than rateLimiterIdleTTL. It runs at
// most once per rateLimiterIdleTTL interval (tracked via lastGC) so it does
// not add per-request O(n) cost. Caller must hold rl.mu.
func (rl *rateLimiter) gc(now time.Time) {
	if now.Sub(rl.lastGC) < rateLimiterIdleTTL {
		return
	}
	rl.lastGC = now
	for k, b := range rl.buckets {
		if now.Sub(b.last) >= rateLimiterIdleTTL {
			delete(rl.buckets, k)
		}
	}
}

// retryAfter is the Retry-After value, in whole seconds, for a request the
// limiter rejected: how long until an empty bucket holds a token again, and at
// least one second.
func (rl *rateLimiter) retryAfter() string {
	secs := 1
	if rl.rate > 0 && rl.rate < 1 {
		secs = int(math.Ceil(1 / rl.rate))
	}
	return strconv.Itoa(secs)
}

// rateLimit wraps next so requests exceeding rl's per-(IP,endpoint) budget get
// a 429 instead of reaching the handler.
func (s *Server) rateLimit(rl *rateLimiter, endpoint string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := clientIP(r) + "|" + endpoint
		if !rl.Allow(key, s.clk.Now()) {
			w.Header().Set("Retry-After", rl.retryAfter())
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}

// clientIP returns the request's source IP for rate-limit keying. It uses the
// TCP peer (RemoteAddr) by default; only when TEND_TRUST_PROXY is truthy does
// it honor the left-most X-Forwarded-For entry (set by a trusted proxy).
// Trusting XFF unconditionally would let a client forge the key and evade the
// limit.
//
// The entry is used only when it is an IP address (optionally with a port),
// and in its canonical form. A header is text of any length: keying on it
// verbatim would let whatever reaches the header mint a new, arbitrarily large
// bucket key per request. Anything else falls back to the TCP peer.
func clientIP(r *http.Request) string {
	if trustProxy() {
		first, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ",")
		if addr, ok := forwardedAddr(first); ok {
			return addr.String()
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// forwardedAddr parses one X-Forwarded-For entry, "ip" or "ip:port", into a
// canonical address: an IPv4-mapped IPv6 address is unmapped and a zone is
// dropped, so one client cannot spell itself several ways.
func forwardedAddr(entry string) (netip.Addr, bool) {
	entry = strings.TrimSpace(entry)
	addr, err := netip.ParseAddr(entry)
	if err != nil {
		ap, perr := netip.ParseAddrPort(entry)
		if perr != nil {
			return netip.Addr{}, false
		}
		addr = ap.Addr()
	}
	return addr.Unmap().WithZone(""), true
}

// trustProxy reports whether X-Forwarded-For should be honored for client-IP
// extraction, mirroring cookieSecure()'s TEND_* opt-in pattern.
func trustProxy() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TEND_TRUST_PROXY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
