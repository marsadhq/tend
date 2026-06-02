package notify_test

import (
	"bytes"
	"context"
	"errors"
	"net/smtp"
	"testing"

	"github.com/marsadhq/tend/internal/notify"
)

// These tests mutate package-global state via SetSMTPSender; they must not
// run in parallel with each other.

// TestSMTPProviderSanitizesSubject guards against header injection: a CR/LF in
// the subject must be flattened so it cannot inject an extra header line.
func TestSMTPProviderSanitizesSubject(t *testing.T) {
	var gotMsg []byte
	notify.SetSMTPSender(func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		gotMsg = msg
		return nil
	})
	t.Cleanup(notify.ResetSMTPSender)

	p := notify.NewSMTP(notify.SMTPConfig{Host: "mail", Port: 587, From: "a@x", To: []string{"b@y"}})
	if err := p.Send(context.Background(), notify.Message{Subject: "Down\r\nBcc: evil@x", Body: "api"}); err != nil {
		t.Fatal(err)
	}
	// The injected "Bcc:" header must NOT appear as its own header line.
	if bytes.Contains(gotMsg, []byte("\r\nBcc: evil@x")) {
		t.Fatalf("subject CRLF injection not sanitized: %q", gotMsg)
	}
	// The header block (everything before the first blank line) must contain
	// exactly the three intended headers and nothing injected.
	header, _, _ := bytes.Cut(gotMsg, []byte("\r\n\r\n"))
	if bytes.Count(header, []byte("\r\n")) != 2 { // From, To, Subject => 2 CRLF separators
		t.Fatalf("unexpected header lines: %q", header)
	}
}

func TestSMTPProviderSends(t *testing.T) {
	var gotAddr, gotFrom string
	var gotTo []string
	var gotMsg []byte
	notify.SetSMTPSender(func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		gotAddr, gotFrom, gotTo, gotMsg = addr, from, to, msg
		return nil
	})
	t.Cleanup(notify.ResetSMTPSender)

	p := notify.NewSMTP(notify.SMTPConfig{
		Host:     "mail",
		Port:     587,
		Username: "u",
		Password: "p",
		From:     "a@x",
		To:       []string{"b@y"},
	})
	if err := p.Send(context.Background(), notify.Message{Subject: "Down", Body: "api"}); err != nil {
		t.Fatal(err)
	}
	if gotAddr != "mail:587" {
		t.Fatalf("expected addr=mail:587, got %s", gotAddr)
	}
	if gotFrom != "a@x" {
		t.Fatalf("expected from=a@x, got %s", gotFrom)
	}
	if len(gotTo) != 1 || gotTo[0] != "b@y" {
		t.Fatalf("expected to=[b@y], got %v", gotTo)
	}
	if !bytes.Contains(gotMsg, []byte("Down")) {
		t.Fatalf("expected msg to contain subject 'Down', got %q", gotMsg)
	}
	if !bytes.Contains(gotMsg, []byte("api")) {
		t.Fatalf("expected msg to contain body 'api', got %q", gotMsg)
	}
	if !bytes.Contains(gotMsg, []byte("Subject: Down")) {
		t.Fatalf("expected msg to contain 'Subject: Down', got %q", gotMsg)
	}
	if !bytes.Contains(gotMsg, []byte("From: a@x")) {
		t.Fatalf("expected msg to contain 'From: a@x', got %q", gotMsg)
	}
	if !bytes.Contains(gotMsg, []byte("To: b@y")) {
		t.Fatalf("expected msg to contain 'To: b@y', got %q", gotMsg)
	}
}

func TestSMTPProviderPropagatesError(t *testing.T) {
	sentinelErr := errors.New("smtp dial failed")
	notify.SetSMTPSender(func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		return sentinelErr
	})
	t.Cleanup(notify.ResetSMTPSender)

	p := notify.NewSMTP(notify.SMTPConfig{
		Host: "mail", Port: 587, Username: "u", Password: "p",
		From: "a@x", To: []string{"b@y"},
	})
	err := p.Send(context.Background(), notify.Message{Subject: "Down", Body: "api"})
	if !errors.Is(err, sentinelErr) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestSMTPProviderMultipleRecipients(t *testing.T) {
	var gotTo []string
	var gotMsg []byte
	notify.SetSMTPSender(func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		gotTo, gotMsg = to, msg
		return nil
	})
	t.Cleanup(notify.ResetSMTPSender)

	p := notify.NewSMTP(notify.SMTPConfig{
		Host:     "mail",
		Port:     587,
		Username: "u",
		Password: "p",
		From:     "a@x",
		To:       []string{"b@y", "c@z"},
	})
	if err := p.Send(context.Background(), notify.Message{Subject: "Down", Body: "api"}); err != nil {
		t.Fatal(err)
	}
	if len(gotTo) != 2 || gotTo[0] != "b@y" || gotTo[1] != "c@z" {
		t.Fatalf("expected to=[b@y c@z], got %v", gotTo)
	}
	if !bytes.Contains(gotMsg, []byte("b@y,c@z")) {
		t.Fatalf("expected To header to contain 'b@y,c@z', got %q", gotMsg)
	}
}

// TestSMTPProviderOmitsAuthWithoutUsername verifies that with no Username the
// provider sends a nil Auth. net/smtp.SendMail rejects a non-nil Auth against a
// server that does not advertise AUTH, so an unauthenticated relay requires nil.
func TestSMTPProviderOmitsAuthWithoutUsername(t *testing.T) {
	var gotAuth smtp.Auth
	sent := false
	notify.SetSMTPSender(func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		gotAuth, sent = a, true
		return nil
	})
	t.Cleanup(notify.ResetSMTPSender)

	// No Username/Password: an unauthenticated relay.
	p := notify.NewSMTP(notify.SMTPConfig{Host: "mail", Port: 25, From: "a@x", To: []string{"b@y"}})
	if err := p.Send(context.Background(), notify.Message{Subject: "Down", Body: "api"}); err != nil {
		t.Fatal(err)
	}
	if !sent {
		t.Fatal("expected the message to be sent")
	}
	if gotAuth != nil {
		t.Fatalf("expected nil Auth when no username is configured, got %T", gotAuth)
	}
}

// TestSMTPProviderUsesAuthWithUsername verifies a configured username still
// produces a non-nil PlainAuth.
func TestSMTPProviderUsesAuthWithUsername(t *testing.T) {
	var gotAuth smtp.Auth
	notify.SetSMTPSender(func(addr string, a smtp.Auth, from string, to []string, msg []byte) error {
		gotAuth = a
		return nil
	})
	t.Cleanup(notify.ResetSMTPSender)

	p := notify.NewSMTP(notify.SMTPConfig{Host: "mail", Port: 587, Username: "u", Password: "p", From: "a@x", To: []string{"b@y"}})
	if err := p.Send(context.Background(), notify.Message{Subject: "Down", Body: "api"}); err != nil {
		t.Fatal(err)
	}
	if gotAuth == nil {
		t.Fatal("expected non-nil Auth when username is configured")
	}
}
