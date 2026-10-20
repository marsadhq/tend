package notify

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests talk to a fake SMTP server on a loopback port through the real
// sender (not the SetSMTPSender seam). They change smtpTimeout and must not
// run in parallel.

// stallingSMTPServer accepts connections and never says a word.
func stallingSMTPServer(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

// fakeSMTPServer speaks just enough SMTP for one unauthenticated delivery
// (no STARTTLS, no AUTH advertised) and reports what it received.
func fakeSMTPServer(t *testing.T) (host string, port int, received <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
		say("220 fake ESMTP")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"):
				say("250-fake")
				say("250 8BITMIME")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				say("250 ok")
			case cmd == "DATA":
				say("354 go ahead")
				for {
					l, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
				got <- data.String()
			case cmd == "QUIT":
				say("221 bye")
				return
			default:
				say("502 unknown")
			}
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port, got
}

// TestSMTPSendStopsAtContextDeadline: a relay that accepts the connection and
// never answers must not hold the send past the caller's deadline. The old
// net/smtp.SendMail path waited forever.
func TestSMTPSendStopsAtContextDeadline(t *testing.T) {
	host, port := stallingSMTPServer(t)
	p := NewSMTP(SMTPConfig{Host: host, Port: port, From: "a@x", To: []string{"b@y"}})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := p.Send(ctx, Message{Subject: "Down", Body: "api"})
	if err == nil {
		t.Fatal("Send to a silent relay: err = nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Send took %s against a 200ms deadline", took)
	}
}

// TestSMTPSendStopsOnCancel: cancelling the context ends a send in progress.
func TestSMTPSendStopsOnCancel(t *testing.T) {
	host, port := stallingSMTPServer(t)
	p := NewSMTP(SMTPConfig{Host: host, Port: port, From: "a@x", To: []string{"b@y"}})

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	err := p.Send(ctx, Message{Subject: "Down", Body: "api"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to wrap context.Canceled", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Send took %s after a cancel at 200ms", took)
	}
}

// TestSMTPSendHasItsOwnLimit: with no deadline from the caller (the delivery
// worker's context has none), a stalled relay still releases the send once
// smtpTimeout has passed.
func TestSMTPSendHasItsOwnLimit(t *testing.T) {
	old := smtpTimeout
	smtpTimeout = 200 * time.Millisecond
	t.Cleanup(func() { smtpTimeout = old })

	host, port := stallingSMTPServer(t)
	p := NewSMTP(SMTPConfig{Host: host, Port: port, From: "a@x", To: []string{"b@y"}})
	start := time.Now()
	if err := p.Send(context.Background(), Message{Subject: "Down", Body: "api"}); err == nil {
		t.Fatal("Send to a silent relay: err = nil")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("Send took %s with a 200ms limit", took)
	}
}

// TestSMTPSendDeliversUnauthenticated runs a whole conversation against a
// relay that offers neither STARTTLS nor AUTH: with no username the message
// goes out with no AUTH attempt.
func TestSMTPSendDeliversUnauthenticated(t *testing.T) {
	host, port, received := fakeSMTPServer(t)
	p := NewSMTP(SMTPConfig{Host: host, Port: port, From: "a@x", To: []string{"b@y"}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Send(ctx, Message{Subject: "Job failed: nightly", Body: "status=failed exit_code=1"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case msg := <-received:
		if !strings.Contains(msg, "Subject: Job failed: nightly") || !strings.Contains(msg, "status=failed") {
			t.Fatalf("relay received %q", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay received nothing")
	}
}

// TestSMTPSendRefusesAuthWithoutServerSupport keeps net/smtp's behaviour: a
// configured username against a relay that does not advertise AUTH fails
// rather than sending credentials nowhere.
func TestSMTPSendRefusesAuthWithoutServerSupport(t *testing.T) {
	host, port, _ := fakeSMTPServer(t)
	p := NewSMTP(SMTPConfig{Host: host, Port: port, Username: "u", Password: "p", From: "a@x", To: []string{"b@y"}})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.Send(ctx, Message{Subject: "Down", Body: "api"})
	if err == nil || !strings.Contains(err.Error(), "doesn't support AUTH") {
		t.Fatalf("err = %v, want the server-doesn't-support-AUTH refusal", err)
	}
}
