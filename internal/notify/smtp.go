package notify

import (
	"context"
	"fmt"
	"net/smtp"
	"strings"
)

// smtpSend is the package-level seam for sending mail. Tests replace it via
// SetSMTPSender so they never touch the network.
var smtpSend = smtp.SendMail

// SetSMTPSender overrides the mail-send function (test seam). Not safe for
// concurrent use; tests must restore it via ResetSMTPSender (t.Cleanup) and
// must not run in parallel with each other.
func SetSMTPSender(f func(addr string, a smtp.Auth, from string, to []string, msg []byte) error) {
	smtpSend = f
}

// ResetSMTPSender restores the real net/smtp.SendMail.
func ResetSMTPSender() { smtpSend = smtp.SendMail }

// SMTPConfig holds the connection and authentication parameters for an SMTP
// server, as well as the envelope sender and recipient list.
//
// Authentication uses smtp.PlainAuth only when Username is set. With no Username
// the message is sent unauthenticated (nil Auth), which is required to reach a
// relay that does not advertise AUTH: net/smtp.SendMail rejects a non-nil Auth
// against such a server with "smtp: server doesn't support AUTH". When
// authenticating, net/smtp refuses to transmit credentials unless the server
// advertises STARTTLS or the host is localhost, so a relay that requires auth
// but does not offer STARTTLS fails at send time with "unencrypted connection".
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

// Send formats m as an RFC 5322 message and delivers it over SMTP. The ctx
// parameter is required by the Provider interface; net/smtp.SendMail does not
// yet accept a context, so cancellation is not propagated - callers that need
// hard deadlines should wrap this provider.
func (p *SMTPProvider) Send(_ context.Context, m Message) error {
	addr := fmt.Sprintf("%s:%d", p.c.Host, p.c.Port)
	// Authenticate only when a username is configured. Passing a non-nil Auth to
	// a server that does not advertise AUTH makes net/smtp fail the whole send, so
	// an unauthenticated relay must receive a nil Auth.
	var auth smtp.Auth
	if p.c.Username != "" {
		auth = smtp.PlainAuth("", p.c.Username, p.c.Password, p.c.Host)
	}
	// A Subject header is a single line: strip CR/LF so a subject can never
	// inject additional headers or prematurely terminate the header block.
	subject := strings.NewReplacer("\r", " ", "\n", " ").Replace(m.Subject)
	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\n%s\r\n",
		p.c.From, strings.Join(p.c.To, ","), subject, m.Body))
	return smtpSend(addr, auth, p.c.From, p.c.To, msg)
}

// Compile-time assertion: SMTPProvider must satisfy the Provider interface.
var _ Provider = (*SMTPProvider)(nil)
