package jobs

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// renewalGraceMax is the most a tls probe waits past a certificate's renewal
// point before it reports the renewal as missed. ACME clients look once or
// twice a day (certbot twice, Traefik once) and ACME Renewal Info spreads
// renewals over a window, so a certificate is routinely a few hours past the
// point when it is renewed; reporting that would raise an alert on every
// renewal. For a short-lived certificate the grace is a tenth of its lifetime
// when that is shorter.
const renewalGraceMax = 48 * time.Hour

// renewalDue returns when a certificate valid from notBefore to notAfter is
// due for renewal by an ACME client that renews after the given fraction of
// its lifetime, and the grace a probe allows past that before it reports the
// renewal as missed. due is rounded to the second.
func renewalDue(notBefore, notAfter time.Time, fraction float64) (due time.Time, grace time.Duration) {
	lifetime := notAfter.Sub(notBefore)
	due = notBefore.Add(time.Duration(float64(lifetime) * fraction)).Round(time.Second)
	return due, min(renewalGraceMax, lifetime/10)
}

// certCheck is what checkCertificate judges a certificate by.
type certCheck struct {
	Now                time.Time
	Name               string // the host name (or IP address) the certificate must be valid for
	WarnDays           int
	RenewAfterFraction float64 // 0: no renewal check
	Verify             bool    // check the chain
	Roots              *x509.CertPool
}

// checkCertificate judges the chain a server sent (leaf first) by c: whether
// the leaf is valid yet, has expired or expires within c.WarnDays, has gone
// past its renewal point, is valid for c.Name, and (when c.Verify) chains to
// one of c.Roots (nil: the system's). Every problem is reported, not just the
// first, and the info lines describe the leaf.
func checkCertificate(chain []*x509.Certificate, c certCheck) probeReport {
	var r probeReport
	leaf := chain[0]
	nb, na := leaf.NotBefore.UTC(), leaf.NotAfter.UTC()
	left := na.Sub(c.Now)
	lifetime := na.Sub(nb)
	used := 0
	if lifetime > 0 {
		used = int(float64(c.Now.Sub(nb)) / float64(lifetime) * 100)
	}

	r.add("subject", "%s", leaf.Subject)
	r.add("issuer", "%s", leaf.Issuer)
	r.add("not_before", "%s", dateText(nb))
	r.add("not_after", "%s", dateText(na))
	r.add("days_left", "%d", floorDays(left))
	if lifetime > 0 {
		r.add("lifetime", "%d days, %d%% used", (lifetime+day/2)/day, max(used, 0))
	}

	switch {
	case c.Now.Before(nb):
		r.fail("certificate is not valid until %s", dateText(nb))
	case left <= 0:
		r.fail("certificate expired %s ago (%s)", humanDays(left), dateText(na))
	case left <= time.Duration(c.WarnDays)*day:
		r.fail("certificate expires in %s (%s), within warn_days %d", humanDays(left), dateText(na), c.WarnDays)
	}

	if c.RenewAfterFraction > 0 && lifetime > 0 {
		due, grace := renewalDue(nb, na, c.RenewAfterFraction)
		r.add("renewal_due", "%s (after %.0f%% of the lifetime; missed once %s past that)",
			dateText(due), c.RenewAfterFraction*100, durationText(grace))
		// Only for a certificate in force: an expired one is reported as such.
		if left > 0 && !c.Now.Before(nb) && c.Now.After(due.Add(grace)) {
			r.fail("renewal missed: %d%% of the %d-day lifetime has passed and the certificate was due for renewal on %s; is the ACME client still renewing it?",
				used, (lifetime+day/2)/day, dateText(due))
		}
	}

	r.add("sans", "%s", sansOf(leaf))
	if err := leaf.VerifyHostname(c.Name); err != nil {
		r.fail("host name mismatch: %v", err)
	}

	if !c.Verify {
		r.add("chain", "not checked (insecure_skip_verify)")
		return r
	}
	// The chain is judged as of a moment inside the leaf's own validity: a
	// leaf that has expired or is not valid yet is reported above, and
	// verifying at the present would only say so again and hide whatever
	// else is wrong with the chain.
	at := c.Now
	if at.After(na) {
		at = na
	}
	if at.Before(nb) {
		at = nb
	}
	intermediates := x509.NewCertPool()
	for _, ic := range chain[1:] {
		intermediates.AddCert(ic)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: c.Roots, Intermediates: intermediates, CurrentTime: at}); err != nil {
		msg := "chain does not verify: " + err.Error()
		var unknown x509.UnknownAuthorityError
		if errors.As(err, &unknown) {
			msg += " (set ca_file for a private CA, or insecure_skip_verify for a self-signed certificate)"
		}
		r.fail("%s", msg)
		r.add("chain", "does not verify (%d sent)", len(chain))
	} else {
		r.add("chain", "verified (%d sent)", len(chain))
	}
	return r
}

// sansOf lists the names and addresses a certificate is valid for.
func sansOf(c *x509.Certificate) string {
	names := append([]string(nil), c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// runOnceTLS runs the tls branch of runOnce: fetch the certificate chain the
// server presents and judge it.
func (e *Executor) runOnceTLS(ctx context.Context, j Job, attempt int, started time.Time) RunResult {
	s := specOf(j)
	if err := ValidateSpec(TLS, &s); err != nil {
		return probeErrorResult(ctx, attempt, started, err)
	}
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.tlsPort()))
	name := s.tlsName()
	var roots *x509.CertPool
	if s.CAFile != "" {
		var err error
		if roots, err = loadRoots(s.CAFile); err != nil {
			return probeErrorResult(ctx, attempt, started, err)
		}
	}
	chain, err := fetchChain(ctx, addr, name, s.StartTLS)
	if err != nil {
		return probeErrorResult(ctx, attempt, started, fmt.Errorf("tls %s: %w", addr, err))
	}
	now := e.now()
	r := checkCertificate(chain, certCheck{
		Now:                now,
		Name:               name,
		WarnDays:           s.WarnDays,
		RenewAfterFraction: s.RenewAfterFraction,
		Verify:             !s.InsecureSkipVerify,
		Roots:              roots,
	})
	target := addr
	if name != s.Host {
		target += " (server name " + name + ")"
	}
	if s.StartTLS != "" {
		target += " via STARTTLS (" + s.StartTLS + ")"
	}
	r.info = append([]string{fmt.Sprintf("%-12s %s", "target:", target)}, r.info...)
	na := chain[0].NotAfter
	return r.result(attempt, started, fmt.Sprintf("certificate for %s expires in %s (%s; %s)",
		name, humanDays(na.Sub(now)), dateText(na), warnText(s.WarnDays)))
}

// tlsPort is the port a tls probe connects to.
func (s Spec) tlsPort() int {
	if s.Port > 0 {
		return s.Port
	}
	switch s.StartTLS {
	case "smtp":
		return 25
	case "imap":
		return 143
	}
	return 443
}

// tlsName is the name a tls probe sends as SNI and checks the certificate for.
func (s Spec) tlsName() string {
	if s.ServerName != "" {
		return s.ServerName
	}
	return s.Host
}

// loadRoots returns the system's roots plus the certificates in the PEM file
// at path.
func loadRoots(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ca_file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("ca_file %s: no PEM certificate in it", path)
	}
	return pool, nil
}

// fetchChain connects to addr, upgrades the connection with STARTTLS when
// starttls names a protocol, completes a TLS handshake sending name as SNI,
// and returns the certificates the server presented. Nothing is sent over the
// encrypted connection; it is closed straight after the handshake.
//
// The handshake itself does not verify the certificate (InsecureSkipVerify):
// checkCertificate does that afterwards, against the same roots a client
// uses, so that an expired certificate, a wrong name and a broken chain are
// each reported rather than the handshake stopping at the first of them.
func fetchChain(ctx context.Context, addr, name, starttls string) ([]*x509.Certificate, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// Reads and writes below block on the server; ending ctx (its deadline,
	// or shutdown) unblocks them at once.
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	switch starttls {
	case "smtp":
		err = startTLSSMTP(conn)
	case "imap":
		err = startTLSIMAP(conn)
	}
	if err != nil {
		return nil, fmt.Errorf("starttls (%s): %w", starttls, err)
	}

	tc := tls.Client(conn, &tls.Config{ServerName: name, InsecureSkipVerify: true}) // verified in checkCertificate
	if err := tc.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("handshake: %w", err)
	}
	chain := tc.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		return nil, errors.New("the server sent no certificate")
	}
	return chain, nil
}

// maxStartTLSLines bounds how many lines of a reply the STARTTLS exchanges
// read, so a server that never stops talking cannot keep a probe going.
const maxStartTLSLines = 100

// readStartTLSLine reads one CRLF- or LF-terminated line of at most the reader's
// buffer size, without its line ending.
func readStartTLSLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", errors.New("the server sent an overlong line")
		}
		if errors.Is(err, io.EOF) {
			return "", errors.New("the server closed the connection")
		}
		return "", err
	}
	return strings.TrimRight(string(line), "\r\n"), nil
}

// readSMTPReply reads a (possibly multi-line) SMTP reply and returns its
// lines without their codes, or an error when its code is not want.
func readSMTPReply(r *bufio.Reader, want int) ([]string, error) {
	var lines []string
	for range maxStartTLSLines {
		line, err := readStartTLSLine(r)
		if err != nil {
			return nil, err
		}
		if len(line) < 3 || (len(line) > 3 && line[3] != ' ' && line[3] != '-') {
			return nil, fmt.Errorf("not an SMTP reply: %q", line)
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return nil, fmt.Errorf("not an SMTP reply: %q", line)
		}
		lines = append(lines, strings.TrimSpace(line[min(len(line), 4):]))
		if len(line) == 3 || line[3] == ' ' {
			if code != want {
				return nil, fmt.Errorf("the server replied %q", line)
			}
			return lines, nil
		}
	}
	return nil, errors.New("the server's reply does not end")
}

// startTLSSMTP takes an SMTP connection from the greeting to the point where
// the TLS handshake starts (RFC 3207).
func startTLSSMTP(conn net.Conn) error {
	r := bufio.NewReader(conn)
	if _, err := readSMTPReply(r, 220); err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	if _, err := io.WriteString(conn, "EHLO localhost\r\n"); err != nil {
		return err
	}
	ext, err := readSMTPReply(r, 250)
	if err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	offered := false
	for _, e := range ext {
		if f := strings.Fields(e); len(f) > 0 && strings.EqualFold(f[0], "STARTTLS") {
			offered = true
		}
	}
	if !offered {
		_, _ = io.WriteString(conn, "QUIT\r\n")
		return errors.New("the server does not offer STARTTLS")
	}
	if _, err := io.WriteString(conn, "STARTTLS\r\n"); err != nil {
		return err
	}
	if _, err := readSMTPReply(r, 220); err != nil {
		return fmt.Errorf("STARTTLS: %w", err)
	}
	return nothingBuffered(r)
}

// startTLSIMAP takes an IMAP connection from the greeting to the point where
// the TLS handshake starts (RFC 3501 section 6.2.1).
func startTLSIMAP(conn net.Conn) error {
	r := bufio.NewReader(conn)
	greeting, err := readStartTLSLine(r)
	if err != nil {
		return fmt.Errorf("greeting: %w", err)
	}
	if !strings.HasPrefix(strings.ToUpper(greeting), "* OK") {
		return fmt.Errorf("greeting: the server said %q", greeting)
	}
	const tag = "tend1"
	if _, err := io.WriteString(conn, tag+" STARTTLS\r\n"); err != nil {
		return err
	}
	for range maxStartTLSLines {
		line, err := readStartTLSLine(r)
		if err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
		if strings.HasPrefix(line, "* ") {
			continue // untagged data before the answer
		}
		if strings.HasPrefix(strings.ToUpper(line), strings.ToUpper(tag)+" OK") {
			return nothingBuffered(r)
		}
		return fmt.Errorf("STARTTLS: the server replied %q", line)
	}
	return errors.New("STARTTLS: the server's reply does not end")
}

// nothingBuffered refuses a server that sent more after agreeing to STARTTLS:
// those bytes came before the encryption and would otherwise be taken as part
// of the handshake (or, in a client that went on to talk, as an encrypted
// reply: the STARTTLS command injection of CVE-2011-0411).
func nothingBuffered(r *bufio.Reader) error {
	if r.Buffered() > 0 {
		return errors.New("the server sent data before the handshake")
	}
	return nil
}
