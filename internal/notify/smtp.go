package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"strings"
	"time"
)

// smtpTimeout bounds one whole SMTP send: connecting, the conversation, and
// the message itself. The delivery worker sends everything on one goroutine
// and leases a claimed delivery for a minute, so a relay that accepts the
// connection and then goes quiet must not hold it longer than an HTTP send
// can (10s) by much, and never past the lease. A var so tests can shorten it.
var smtpTimeout = 30 * time.Second

// smtpSend is the package-level seam for sending mail. Tests replace it via
// SetSMTPSender so they never touch the network.
var smtpSend = sendMail

// SetSMTPSender overrides the mail-send function (test seam). f receives what
// net/smtp.SendMail would. Not safe for concurrent use; tests must restore it
// via ResetSMTPSender (t.Cleanup) and must not run in parallel with each other.
func SetSMTPSender(f func(addr string, a smtp.Auth, from string, to []string, msg []byte) error) {
	smtpSend = func(_ context.Context, addr, _ string, a smtp.Auth, from string, to []string, msg []byte) error {
		return f(addr, a, from, to, msg)
	}
}

// ResetSMTPSender restores the real sender.
func ResetSMTPSender() { smtpSend = sendMail }

// SMTPConfig holds the connection and authentication parameters for an SMTP
// server, as well as the envelope sender and recipient list.
//
// Authentication uses smtp.PlainAuth only when Username is set. With no Username
// the message is sent unauthenticated (nil Auth), which is required to reach a
// relay that does not advertise AUTH: a non-nil Auth against such a server fails
// the send with "smtp: server doesn't support AUTH". When authenticating,
// net/smtp refuses to transmit credentials unless the server advertises
// STARTTLS or the host is localhost, so a relay that requires auth but does not
// offer STARTTLS fails at send time with "unencrypted connection".
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
	To       []string
}

// SMTPProvider delivers notifications by sending an email via SMTP. It uses
// the package-level smtpSend seam so unit tests can intercept outgoing mail
// without touching the network.
type SMTPProvider struct{ c SMTPConfig }

// NewSMTP constructs an SMTPProvider from the given configuration.
func NewSMTP(c SMTPConfig) *SMTPProvider { return &SMTPProvider{c} }

// Send formats m as an RFC 5322 message and delivers it over SMTP. The send
// honours ctx and is bounded by smtpTimeout whatever ctx allows: a relay that
// stalls fails this attempt (to be retried) instead of holding the delivery
// worker.
func (p *SMTPProvider) Send(ctx context.Context, m Message) error {
	addr := net.JoinHostPort(p.c.Host, fmt.Sprint(p.c.Port))
	// Authenticate only when a username is configured. Passing a non-nil Auth to
	// a server that does not advertise AUTH fails the whole send, so an
	// unauthenticated relay must receive a nil Auth.
	var auth smtp.Auth
	if p.c.Username != "" {
		auth = smtp.PlainAuth("", p.c.Username, p.c.Password, p.c.Host)
	}
	// A Subject header is a single line: strip CR/LF so a subject can never
	// inject additional headers or prematurely terminate the header block.
	subject := strings.NewReplacer("\r", " ", "\n", " ").Replace(m.Subject)
	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s\r\n",
		p.c.From, strings.Join(p.c.To, ","), subject, m.Body))
	return smtpSend(ctx, addr, p.c.Host, auth, p.c.From, p.c.To, msg)
}

// sendMail is net/smtp.SendMail with a context and deadlines: the same
// conversation (EHLO, STARTTLS when offered, AUTH when a is set, MAIL, RCPT,
// DATA, QUIT), but the connection is dialled under ctx, every read and write
// shares one deadline (ctx's, capped at smtpTimeout), and cancelling ctx
// closes the connection, which ends any exchange still in progress.
// net/smtp.SendMail has none of this: a relay that accepts the connection and
// never answers blocks it forever.
func sendMail(ctx context.Context, addr, host string, a smtp.Auth, from string, to []string, msg []byte) error {
	for _, line := range append([]string{from}, to...) {
		if strings.ContainsAny(line, "\r\n") {
			return errors.New("smtp: a line must not contain CR or LF")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, smtpTimeout)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp: dial %s: %w", addr, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			conn.Close()
			return fmt.Errorf("smtp: set deadline: %w", err)
		}
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	err = converse(conn, host, a, from, to, msg)
	if err == nil {
		return nil
	}
	// A 5xx reply (unknown recipient, message refused, bad credentials) will
	// be the same on every retry.
	err = smtpReplyError(err)
	// The deadline or a cancel cut the exchange short; say so rather than
	// leaving only "i/o timeout" or "use of closed network connection". The
	// connection's deadline is ctx's, so it can fire a moment before ctx
	// itself reports it.
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("smtp: %w (%w)", cerr, err)
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return fmt.Errorf("smtp: %w (%w)", context.DeadlineExceeded, err)
	}
	return err
}

// converse runs the SMTP conversation on conn and always closes it.
func converse(conn net.Conn, host string, a smtp.Auth, from string, to []string, msg []byte) error {
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: host}); err != nil {
			return err
		}
	}
	if a != nil {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: server doesn't support AUTH")
		}
		if err := c.Auth(a); err != nil {
			return err
		}
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Compile-time assertion: SMTPProvider must satisfy the Provider interface.
var _ Provider = (*SMTPProvider)(nil)
