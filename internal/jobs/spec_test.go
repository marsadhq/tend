package jobs

import (
	"context"
	"strings"
	"testing"
)

func TestValidateSpecTLS(t *testing.T) {
	ok := []Spec{
		{Host: "example.com"},
		{Host: "192.0.2.1", Port: 8443, ServerName: "example.com"},
		{Host: "::1", StartTLS: "smtp", RenewAfterFraction: 0.5, CAFile: "/etc/ssl/ca.pem"},
		{Host: "mail.example.com", StartTLS: "imap", WarnDays: 0},
	}
	for _, s := range ok {
		if err := ValidateSpec(TLS, &s); err != nil {
			t.Errorf("ValidateSpec(%+v) = %v, want nil", s, err)
		}
	}
	bad := []struct {
		spec Spec
		want string
	}{
		{Spec{}, "host is required"},
		{Spec{Host: "example.com:443"}, "port has its own setting"},
		{Spec{Host: "https://example.com"}, "without a scheme"},
		{Spec{Host: "exa mple.com"}, "not a host name"},
		{Spec{Host: "example.com", Port: 70000}, "port must be"},
		{Spec{Host: "example.com", StartTLS: "pop3"}, "starttls must be"},
		{Spec{Host: "example.com", RenewAfterFraction: 1}, "renew_after_fraction"},
		{Spec{Host: "example.com", RenewAfterFraction: -0.1}, "renew_after_fraction"},
		{Spec{Host: "example.com", WarnDays: -1}, "warn_days"},
		{Spec{Host: "example.com", CAFile: "ca.pem"}, "absolute path"},
		{Spec{Host: "example.com", ServerName: "a b"}, "server_name"},
	}
	for _, c := range bad {
		err := ValidateSpec(TLS, &c.spec)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("ValidateSpec(%+v) = %v, want an error about %q", c.spec, err, c.want)
		}
	}
	if err := ValidateSpec(TLS, nil); err == nil {
		t.Error("ValidateSpec(tls, nil) = nil, want an error")
	}
	// Shell and http jobs have no settings to check.
	if err := ValidateSpec(Shell, nil); err != nil {
		t.Errorf("ValidateSpec(shell, nil) = %v", err)
	}
}

// TestValidateSpecWarnDaysCap: warn_days is at most ten years. A larger one
// overflows the time.Duration it becomes and would make a failing probe pass.
func TestValidateSpecWarnDaysCap(t *testing.T) {
	specs := map[JobType]Spec{
		TLS:    {Host: "example.com"},
		Domain: {Domain: "example.com"},
		Expiry: {ExpiresAt: t0},
	}
	for typ, s := range specs {
		for _, ok := range []int{0, 1, MaxWarnDays} {
			s.WarnDays = ok
			if err := ValidateSpec(typ, &s); err != nil {
				t.Errorf("%s warn_days %d: %v", typ, ok, err)
			}
		}
		for _, bad := range []int{MaxWarnDays + 1, 106752, 1 << 40} {
			s.WarnDays = bad
			if err := ValidateSpec(typ, &s); err == nil || !strings.Contains(err.Error(), "warn_days") {
				t.Errorf("%s warn_days %d: err = %v, want a refusal", typ, bad, err)
			}
		}
	}
	if MaxWarnDays != 3650 {
		t.Errorf("MaxWarnDays = %d, want 3650", MaxWarnDays)
	}
}

// TestHugeWarnDaysDoesNotPass shows what the cap prevents: a probe whose
// warn_days overflowed passed a date that is about to expire. A row stored by
// other means is refused at run time.
func TestHugeWarnDaysDoesNotPass(t *testing.T) {
	res := fixedNow(t0).Run(context.Background(), expiryJob(Spec{ExpiresAt: t0.Add(day), WarnDays: 106752}), nil)
	if res.Status != StatusFailed || !strings.Contains(res.Output, "warn_days") {
		t.Fatalf("status %s, output:\n%s", res.Status, res.Output)
	}
}
