package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The token buckets themselves are kit/httpx's and are tested there; these
// tests pin how Tend keys them: by the TCP peer, or by X-Forwarded-For only
// when TEND_TRUST_PROXY says so.

func TestClientIP_IgnoresXFFByDefault(t *testing.T) {
	t.Setenv("TEND_TRUST_PROXY", "")

	r1 := httptest.NewRequest(http.MethodGet, "/", nil)
	r1.RemoteAddr = "10.0.0.1:5555"
	r1.Header.Set("X-Forwarded-For", "1.2.3.4")

	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.RemoteAddr = "10.0.0.1:6666"
	r2.Header.Set("X-Forwarded-For", "9.9.9.9")

	ip1 := clientIP(r1)
	ip2 := clientIP(r2)
	if ip1 != ip2 {
		t.Fatalf("forged XFF changed the key: %q vs %q, want equal (both keyed on RemoteAddr)", ip1, ip2)
	}
	if ip1 != "10.0.0.1" {
		t.Fatalf("clientIP = %q, want RemoteAddr host 10.0.0.1", ip1)
	}
}

func TestClientIP_HonorsXFFWhenTrustProxySet(t *testing.T) {
	t.Setenv("TEND_TRUST_PROXY", "1")

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")

	if got := clientIP(r); got != "203.0.113.7" {
		t.Fatalf("clientIP = %q, want left-most XFF entry 203.0.113.7", got)
	}
}

// TestClientIP_XFFMustBeAnAddress pins what a forwarded entry may turn into a
// bucket key. Only an IP address counts, in one canonical spelling; any other
// header value (a name, junk, a huge string) falls back to the TCP peer, so the
// header cannot be used to mint arbitrary or arbitrarily large keys.
func TestClientIP_XFFMustBeAnAddress(t *testing.T) {
	t.Setenv("TEND_TRUST_PROXY", "1")
	const peer = "10.0.0.1"

	cases := []struct {
		name, xff, want string
	}{
		{"ipv4", "203.0.113.7", "203.0.113.7"},
		{"ipv4 with port", "203.0.113.7:4711", "203.0.113.7"},
		{"ipv4-mapped ipv6 is unmapped", "::ffff:203.0.113.7", "203.0.113.7"},
		{"ipv6 is canonicalised", "2001:DB8:0:0::1", "2001:db8::1"},
		{"ipv6 with port", "[2001:db8::1]:443", "2001:db8::1"},
		{"ipv6 zone is dropped", "fe80::1%eth0", "fe80::1"},
		{"surrounding space", "  203.0.113.7 , 10.0.0.1", "203.0.113.7"},
		{"empty header", "", peer},
		{"empty first entry", ", 203.0.113.7", peer},
		{"hostname", "client.example", peer},
		{"junk", "not an address", peer},
		{"huge value", strings.Repeat("a", 64<<10), peer},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = peer + ":5555"
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := clientIP(r); got != c.want {
				t.Errorf("clientIP with X-Forwarded-For %.40q = %q, want %q", c.xff, got, c.want)
			}
		})
	}
}
