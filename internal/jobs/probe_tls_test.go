package jobs

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"
)

// t0 is the fixed "now" of the certificate checks below.
var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// problemsOf runs checkCertificate on c as served for name, with the given
// thresholds and the chain verified against roots (nil: not verified).
func problemsOf(t *testing.T, c tls.Certificate, now time.Time, name string, warnDays int, fraction float64, roots *x509.CertPool) []string {
	t.Helper()
	check := certCheck{Now: now, Name: name, WarnDays: warnDays, RenewAfterFraction: fraction, Verify: roots != nil, Roots: roots}
	return checkCertificate(chainOf(t, c), check).problems
}

func wantProblem(t *testing.T, problems []string, substr string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, substr) {
			return
		}
	}
	t.Fatalf("problems %q do not mention %q", problems, substr)
}

func wantNoProblems(t *testing.T, problems []string) {
	t.Helper()
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %q", problems)
	}
}

func TestCertExpiryThreshold(t *testing.T) {
	ca := newTestCA(t)
	notAfter := t0.Add(14 * day)
	cert := ca.issue(t, notAfter.Add(-90*day), notAfter, "tend.test")
	roots := ca.pool()

	// Exactly warn_days left is within warn_days.
	p := problemsOf(t, cert, t0, "tend.test", 14, 0, roots)
	wantProblem(t, p, "expires in 14 days")
	if len(p) != 1 {
		t.Fatalf("want only the expiry problem, got %q", p)
	}
	// A second more than that is not.
	wantNoProblems(t, problemsOf(t, cert, t0.Add(-time.Second), "tend.test", 14, 0, roots))
	// warn_days 0 lets it run to the end...
	wantNoProblems(t, problemsOf(t, cert, notAfter.Add(-time.Hour), "tend.test", 0, 0, roots))
	// ...and not a second past it.
	wantProblem(t, problemsOf(t, cert, notAfter.Add(time.Second), "tend.test", 0, 0, roots), "expired")
}

func TestCertExpiredReportedOnce(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.issue(t, t0.Add(-100*day), t0.Add(-3*day), "tend.test")
	p := problemsOf(t, cert, t0, "tend.test", 14, 2.0/3, ca.pool())
	if len(p) != 1 {
		t.Fatalf("an expired certificate should be one problem (no chain or renewal echo), got %q", p)
	}
	wantProblem(t, p, "expired")
	wantProblem(t, p, "3 days ago")
}

func TestCertNotYetValid(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.issue(t, t0.Add(day), t0.Add(91*day), "tend.test")
	wantProblem(t, problemsOf(t, cert, t0, "tend.test", 14, 2.0/3, ca.pool()), "not valid until")
}

func TestCertRenewalMissed(t *testing.T) {
	ca := newTestCA(t)
	roots := ca.pool()

	// A 90-day certificate is due for renewal at day 60; the grace is the
	// smaller of 48 hours and a tenth of the lifetime, so 48 hours here.
	nb := t0
	cert := ca.issue(t, nb, nb.Add(90*day), "tend.test")
	wantNoProblems(t, problemsOf(t, cert, nb.Add(59*day), "tend.test", 14, 2.0/3, roots))
	wantNoProblems(t, problemsOf(t, cert, nb.Add(62*day), "tend.test", 14, 2.0/3, roots))
	p := problemsOf(t, cert, nb.Add(62*day+time.Second), "tend.test", 14, 2.0/3, roots)
	wantProblem(t, p, "renewal missed")
	wantProblem(t, p, "90-day lifetime")
	if len(p) != 1 {
		t.Fatalf("27 days before expiry only the renewal check should fail, got %q", p)
	}
	// Turned off with a fraction of 0.
	wantNoProblems(t, problemsOf(t, cert, nb.Add(75*day), "tend.test", 14, 0, roots))

	// A short-lived 6-day certificate renewed at half its life: the grace is
	// a tenth of the lifetime (14.4 hours), not 48 hours.
	short := ca.issue(t, nb, nb.Add(6*day), "tend.test")
	wantNoProblems(t, problemsOf(t, short, nb.Add(3*day+14*time.Hour), "tend.test", 1, 0.5, roots))
	wantProblem(t, problemsOf(t, short, nb.Add(3*day+15*time.Hour), "tend.test", 1, 0.5, roots), "renewal missed")
}

func TestRenewalDue(t *testing.T) {
	nb := t0
	cases := []struct {
		lifetime  time.Duration
		fraction  float64
		wantDue   time.Duration
		wantGrace time.Duration
	}{
		{90 * day, 2.0 / 3, 60 * day, 48 * time.Hour},
		{47 * day, 2.0 / 3, 47 * day * 2 / 3, 48 * time.Hour},
		{200 * day, 2.0 / 3, 200 * day * 2 / 3, 48 * time.Hour},
		{6 * day, 0.5, 3 * day, 6 * day / 10},
	}
	for _, c := range cases {
		due, grace := renewalDue(nb, nb.Add(c.lifetime), c.fraction)
		if d := due.Sub(nb) - c.wantDue; d < -time.Second || d > time.Second {
			t.Errorf("lifetime %v at %v: due after %v, want %v", c.lifetime, c.fraction, due.Sub(nb), c.wantDue)
		}
		if grace != c.wantGrace {
			t.Errorf("lifetime %v: grace %v, want %v", c.lifetime, grace, c.wantGrace)
		}
	}
}

func TestCertHostname(t *testing.T) {
	ca := newTestCA(t)
	roots := ca.pool()
	cert := ca.issue(t, t0.Add(-day), t0.Add(80*day), "tend.test", "*.apps.tend.test", "192.0.2.7")
	wantNoProblems(t, problemsOf(t, cert, t0, "tend.test", 14, 2.0/3, roots))
	wantNoProblems(t, problemsOf(t, cert, t0, "web.apps.tend.test", 14, 2.0/3, roots))
	wantNoProblems(t, problemsOf(t, cert, t0, "192.0.2.7", 14, 2.0/3, roots))
	p := problemsOf(t, cert, t0, "other.test", 14, 2.0/3, roots)
	wantProblem(t, p, "host name mismatch")
	wantProblem(t, p, "other.test")
	// The host name is checked when the chain is not.
	wantProblem(t, problemsOf(t, cert, t0, "other.test", 14, 2.0/3, nil), "host name mismatch")
}

func TestCertChain(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.issue(t, t0.Add(-day), t0.Add(80*day), "tend.test")

	// Signed by a CA the roots do not hold.
	p := problemsOf(t, cert, t0, "tend.test", 14, 2.0/3, x509.NewCertPool())
	wantProblem(t, p, "chain does not verify")
	wantProblem(t, p, "unknown authority")

	// A self-signed certificate fails the chain check, and passes without it.
	self := selfSigned(t, t0.Add(-day), t0.Add(80*day), "tend.test")
	wantProblem(t, problemsOf(t, self, t0, "tend.test", 14, 2.0/3, x509.NewCertPool()), "insecure_skip_verify")
	wantNoProblems(t, problemsOf(t, self, t0, "tend.test", 14, 2.0/3, nil))

	// A server that leaves out its intermediate.
	root := newTestCA(t)
	inter := root.issueCA(t)
	leaf := inter.issue(t, t0.Add(-day), t0.Add(80*day), "tend.test")
	full := tls.Certificate{Certificate: [][]byte{leaf.Certificate[0], inter.cert.Raw}}
	wantNoProblems(t, problemsOf(t, full, t0, "tend.test", 14, 2.0/3, root.pool()))
	bare := tls.Certificate{Certificate: [][]byte{leaf.Certificate[0]}}
	wantProblem(t, problemsOf(t, bare, t0, "tend.test", 14, 2.0/3, root.pool()), "chain does not verify")
}

func TestCertReportLines(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.issue(t, t0.Add(-30*day), t0.Add(60*day), "tend.test", "www.tend.test")
	r := checkCertificate(chainOf(t, cert), certCheck{Now: t0, Name: "tend.test", WarnDays: 14, RenewAfterFraction: 2.0 / 3, Verify: true, Roots: ca.pool()})
	out := strings.Join(r.info, "\n")
	for _, want := range []string{
		"subject:", "CN=tend.test",
		"issuer:", "CN=Tend Test CA",
		"not_before:", "2026-09-21T12:00:00Z",
		"not_after:", "2026-12-20T12:00:00Z",
		"days_left:", "60",
		"lifetime:", "90 days, 33% used",
		"renewal_due:", "2026-11-20T12:00:00Z",
		"sans:", "tend.test, www.tend.test",
		"chain:", "verified",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// --- the probe against local servers ---

func tlsJob(port int, s Spec) Job {
	s.Port = port
	if s.Host == "" {
		s.Host = "127.0.0.1"
	}
	return Job{Name: "cert", Type: TLS, TimeoutSeconds: 5, Spec: &s}
}

func TestTLSProbeOK(t *testing.T) {
	ca := newTestCA(t)
	port := tlsServer(t, ca.issue(t, time.Now().Add(-10*day), time.Now().Add(80*day), "tend.test"))
	res := NewExecutor().Run(context.Background(),
		tlsJob(port, Spec{ServerName: "tend.test", CAFile: ca.writePEM(t), WarnDays: 14, RenewAfterFraction: 2.0 / 3}), nil)
	if res.Status != StatusSucceeded || res.ExitCode != 0 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	for _, want := range []string{"ok:", "expires in 79 days", "subject:", "issuer:", "not_before:", "not_after:", "days_left:", "sans:", "tend.test", "chain:", "verified"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("output lacks %q:\n%s", want, res.Output)
		}
	}
}

func TestTLSProbeFailures(t *testing.T) {
	ca := newTestCA(t)
	good := ca.issue(t, time.Now().Add(-10*day), time.Now().Add(80*day), "tend.test")
	cases := []struct {
		name string
		cert tls.Certificate
		spec Spec
		want string
	}{
		{"unknown authority", good, Spec{ServerName: "tend.test", WarnDays: 14}, "chain does not verify"},
		{"host name", good, Spec{ServerName: "other.test", CAFile: ca.writePEM(t), WarnDays: 14}, "host name mismatch"},
		{"expiring", ca.issue(t, time.Now().Add(-80*day), time.Now().Add(5*day), "tend.test"),
			Spec{ServerName: "tend.test", CAFile: ca.writePEM(t), WarnDays: 14}, "expires in 4 days"},
		{"renewal missed", ca.issue(t, time.Now().Add(-70*day), time.Now().Add(20*day), "tend.test"),
			Spec{ServerName: "tend.test", CAFile: ca.writePEM(t), WarnDays: 14, RenewAfterFraction: 2.0 / 3}, "renewal missed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			port := tlsServer(t, c.cert)
			res := NewExecutor().Run(context.Background(), tlsJob(port, c.spec), nil)
			if res.Status != StatusFailed || res.ExitCode != 1 {
				t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
			}
			if !strings.HasPrefix(res.Output, "FAIL: ") || !strings.Contains(res.Output, c.want) {
				t.Fatalf("output does not start with FAIL or lacks %q:\n%s", c.want, res.Output)
			}
		})
	}
}

func TestTLSProbeSelfSigned(t *testing.T) {
	port := tlsServer(t, selfSigned(t, time.Now().Add(-day), time.Now().Add(300*day), "tend.test"))
	res := NewExecutor().Run(context.Background(),
		tlsJob(port, Spec{ServerName: "tend.test", InsecureSkipVerify: true, WarnDays: 14}), nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
	if !strings.Contains(res.Output, "not checked (insecure_skip_verify)") {
		t.Fatalf("output does not say the chain was not checked:\n%s", res.Output)
	}
}

func TestTLSProbeConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	res := NewExecutor().Run(context.Background(), tlsJob(port, Spec{WarnDays: 14}), nil)
	if res.Status != StatusFailed || res.ExitCode != -1 {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
	if !strings.Contains(res.Output, "127.0.0.1") {
		t.Fatalf("output does not name the address:\n%s", res.Output)
	}
}

func TestTLSProbeTimeout(t *testing.T) {
	// A server that accepts and never says a word.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := make(chan net.Conn, 4)
	t.Cleanup(func() {
		ln.Close()
		close(conns)
		for c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()
	j := tlsJob(ln.Addr().(*net.TCPAddr).Port, Spec{WarnDays: 14})
	j.TimeoutSeconds = 1
	start := time.Now()
	res := NewExecutor().Run(context.Background(), j, nil)
	if res.Status != StatusTimedOut {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Fatalf("the probe took %v to time out after 1s", took)
	}
}

func TestTLSProbeWithoutSpec(t *testing.T) {
	res := NewExecutor().Run(context.Background(), Job{Name: "cert", Type: TLS}, nil)
	if res.Status != StatusFailed || !strings.Contains(res.Output, "host") {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
}

// startTLSServer accepts one connection on 127.0.0.1, has talk speak the
// plain-text part of a STARTTLS exchange, and then (when talk returns true)
// completes a TLS handshake with cert. It returns the port.
func startTLSServer(t *testing.T, cert tls.Certificate, talk func(r *bufio.Reader, w net.Conn) bool) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		if !talk(bufio.NewReader(conn), conn) {
			return
		}
		tc := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}})
		_ = tc.Handshake()
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func readLine(r *bufio.Reader) string {
	line, _ := r.ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func smtpTalk(offer bool) func(r *bufio.Reader, w net.Conn) bool {
	return func(r *bufio.Reader, w net.Conn) bool {
		w.Write([]byte("220-mail.tend.test ESMTP\r\n220 ready\r\n"))
		if !strings.HasPrefix(strings.ToUpper(readLine(r)), "EHLO ") {
			return false
		}
		if offer {
			w.Write([]byte("250-mail.tend.test\r\n250-PIPELINING\r\n250 STARTTLS\r\n"))
		} else {
			w.Write([]byte("250-mail.tend.test\r\n250 PIPELINING\r\n"))
			readLine(r)
			return false
		}
		if strings.ToUpper(readLine(r)) != "STARTTLS" {
			return false
		}
		w.Write([]byte("220 go ahead\r\n"))
		return true
	}
}

func TestTLSProbeStartTLSSMTP(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.issue(t, time.Now().Add(-day), time.Now().Add(80*day), "mail.tend.test")
	port := startTLSServer(t, cert, smtpTalk(true))
	res := NewExecutor().Run(context.Background(),
		tlsJob(port, Spec{ServerName: "mail.tend.test", StartTLS: "smtp", CAFile: ca.writePEM(t), WarnDays: 14}), nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}

	port = startTLSServer(t, cert, smtpTalk(false))
	res = NewExecutor().Run(context.Background(),
		tlsJob(port, Spec{ServerName: "mail.tend.test", StartTLS: "smtp", CAFile: ca.writePEM(t), WarnDays: 14}), nil)
	if res.Status != StatusFailed || res.ExitCode != -1 || !strings.Contains(res.Output, "does not offer STARTTLS") {
		t.Fatalf("status %s exit %d, output:\n%s", res.Status, res.ExitCode, res.Output)
	}
}

func TestTLSProbeStartTLSIMAP(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.issue(t, time.Now().Add(-day), time.Now().Add(80*day), "imap.tend.test")
	talk := func(ok bool) func(r *bufio.Reader, w net.Conn) bool {
		return func(r *bufio.Reader, w net.Conn) bool {
			w.Write([]byte("* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n"))
			tag, cmd, _ := strings.Cut(readLine(r), " ")
			if strings.ToUpper(cmd) != "STARTTLS" {
				return false
			}
			if !ok {
				w.Write([]byte(tag + " BAD not now\r\n"))
				return false
			}
			w.Write([]byte("* a note first\r\n" + tag + " OK begin TLS\r\n"))
			return true
		}
	}
	port := startTLSServer(t, cert, talk(true))
	res := NewExecutor().Run(context.Background(),
		tlsJob(port, Spec{ServerName: "imap.tend.test", StartTLS: "imap", CAFile: ca.writePEM(t), WarnDays: 14}), nil)
	if res.Status != StatusSucceeded {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}

	port = startTLSServer(t, cert, talk(false))
	res = NewExecutor().Run(context.Background(),
		tlsJob(port, Spec{ServerName: "imap.tend.test", StartTLS: "imap", CAFile: ca.writePEM(t), WarnDays: 14}), nil)
	if res.Status != StatusFailed || !strings.Contains(res.Output, "BAD not now") {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
}

func TestTLSProbeRefusesDataBeforeHandshake(t *testing.T) {
	ca := newTestCA(t)
	cert := ca.issue(t, time.Now().Add(-day), time.Now().Add(80*day), "mail.tend.test")
	port := startTLSServer(t, cert, func(r *bufio.Reader, w net.Conn) bool {
		w.Write([]byte("220 ready\r\n"))
		readLine(r)
		w.Write([]byte("250 STARTTLS\r\n"))
		readLine(r)
		// The reply and something injected after it in one write.
		w.Write([]byte("220 go ahead\r\n250 injected\r\n"))
		return true
	})
	res := NewExecutor().Run(context.Background(),
		tlsJob(port, Spec{ServerName: "mail.tend.test", StartTLS: "smtp", CAFile: ca.writePEM(t), WarnDays: 14}), nil)
	if res.Status != StatusFailed || !strings.Contains(res.Output, "before the handshake") {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
}
