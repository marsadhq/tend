package httpserver_test

import (
	"io"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// v03PingHeaders are the response headers v0.3.0 sent with a ping reply,
// besides Date and Content-Length.
var v03PingHeaders = []string{"Content-Security-Policy", "Content-Type", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"}

// TestPlainPingRepliesAsBefore: scripts written against Tend may match its
// reply, so a ping answers exactly as v0.3.0 did: "ok" in lower case (not
// Healthchecks' "OK") with the same headers, plus only Ping-Body-Limit, and
// the same "404 page not found" for an unknown token. The status codes are
// what clients normally check.
func TestPlainPingRepliesAsBefore(t *testing.T) {
	ts := newStore(t)
	ts.seedHB(t, "x", "tok-compat", "up")
	srv := startServer(t, ts.store, nil)
	check := func(method, path string, code int, body string, extra ...string) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if method == http.MethodHead {
			body = ""
		}
		if resp.StatusCode != code || string(b) != body {
			t.Errorf("%s %s: %d %q, want %d %q", method, path, resp.StatusCode, b, code, body)
		}
		var got []string
		for k := range resp.Header {
			if k != "Date" && k != "Content-Length" {
				got = append(got, k)
			}
		}
		want := append(append([]string(nil), v03PingHeaders...), extra...)
		sort.Strings(got)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s %s: headers %v, want %v", method, path, got, want)
		}
		if ct := resp.Header.Get("Content-Type"); ct != "text/plain; charset=utf-8" {
			t.Errorf("%s %s: Content-Type %q", method, path, ct)
		}
	}
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodHead} {
		check(m, "/ping/tok-compat", http.StatusOK, "ok", "Ping-Body-Limit")
		check(m, "/ping/tok-compat/start", http.StatusOK, "ok", "Ping-Body-Limit")
		check(m, "/ping/nope", http.StatusNotFound, "404 page not found\n")
		check(m, "/ping/nope/fail", http.StatusNotFound, "404 page not found\n")
	}
}
