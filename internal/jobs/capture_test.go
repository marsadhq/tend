package jobs

import (
	"math/rand"
	"strings"
	"testing"
)

// leaked returns the longest piece (5 bytes or more) of secret found in s,
// or "".
func leaked(s, secret string) string {
	for n := len(secret); n >= 5; n-- {
		for i := 0; i+n <= len(secret); i++ {
			if strings.Contains(s, secret[i:i+n]) {
				return secret[i : i+n]
			}
		}
	}
	return ""
}

const capTok = "tok_ACDEFGHIJKLMNOPQRSTUVWXYZ0123456789" // 38 bytes

// The head a drill step keeps: a token at the start and again 27 bytes past
// 32 KiB. The capture keeps the token's length past 32 KiB, so it holds 11
// bytes of the second copy; redacting the first copy shortens the text, and
// those 11 bytes must not slide back under the cut.
func TestDrillHeadKeepsNoPieceOfACutSecret(t *testing.T) {
	h := &headTail{max: drillStepOutput, margin: len(capTok)}
	h.Write([]byte(capTok + strings.Repeat("x", drillStepOutput+27-len(capTok)) + capTok + strings.Repeat("y", 100000)))
	if got := h.text([]string{capTok}); leaked(got, capTok) != "" {
		t.Errorf("kept %q of the secret: ...%q", leaked(got, capTok), got[drillStepOutput-60:drillStepOutput+80])
	}
}

// The tail a drill step keeps: the ring of its last bytes starts inside a
// copy of the token, 15 bytes before its end, and a later copy in the ring
// shortens the text when redacted; the 15 bytes must not slide into the last
// 32 KiB.
func TestDrillTailKeepsNoPieceOfACutSecret(t *testing.T) {
	L := len(capTok)
	h := &headTail{max: drillStepOutput, margin: L}
	h.Write([]byte(strings.Repeat("y", 50000) + capTok + strings.Repeat("x", drillStepOutput-35) + capTok + strings.Repeat("z", 20)))
	if got := h.text([]string{capTok}); leaked(got, capTok) != "" {
		t.Errorf("kept %q of the secret", leaked(got, capTok))
	}
}

// An expiry_command's stderr (curl -v, say) shows the token early and again
// just past the 16 KiB kept: the same slide must not happen there.
func TestExpiryStderrKeepsNoPieceOfACutSecret(t *testing.T) {
	line := "> Authorization: Bearer " + capTok + "\n"
	c := &cappedBuffer{max: maxExpiryStderr + len(capTok)}
	c.Write([]byte(line + strings.Repeat("x", maxExpiryStderr+5-len(line)) + capTok + strings.Repeat("y", 5000)))
	got := cutRedacted(c.buf.Bytes(), c.truncated, maxExpiryStderr, []string{capTok}, "\n... [stderr truncated]")
	if leaked(got, capTok) != "" {
		t.Errorf("kept %q of the secret", leaked(got, capTok))
	}
}

// TestCapturesKeepNoPieceOfACutSecret throws random outputs, full of copies
// of random secrets, at every capture that keeps only part of an output: a
// shell job's head (as the runner then redacts and caps it), a drill step's
// head and tail, and an expiry command's stderr, written in random pieces and
// at sizes small enough to cut often. Nothing of a secret may be kept.
func TestCapturesKeepNoPieceOfACutSecret(t *testing.T) {
	const upper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	for seed := int64(0); seed < 3000; seed++ {
		r := rand.New(rand.NewSource(seed))
		var secrets []string
		for i := 0; i < 1+r.Intn(3); i++ {
			b := make([]byte, 6+r.Intn(30))
			for j := range b {
				b[j] = upper[r.Intn(len(upper))]
			}
			secrets = append(secrets, string(b))
		}
		n := 32 + r.Intn(400)
		var out strings.Builder
		for out.Len() < n*(1+r.Intn(4)) {
			switch r.Intn(5) {
			case 0, 1:
				out.WriteString(secrets[r.Intn(len(secrets))])
			case 2:
				out.WriteString(strings.Repeat("x", 1+r.Intn(60)))
			case 3:
				out.WriteString(strings.Repeat("é", 1+r.Intn(5)))
			default:
				out.WriteString("\n")
			}
		}
		raw := []byte(out.String())
		margin := secretMargin(secrets)
		write := func(w interface{ Write([]byte) (int, error) }) {
			for b := raw; len(b) > 0; {
				k := min(len(b), 1+r.Intn(97))
				_, _ = w.Write(b[:k])
				b = b[k:]
			}
		}

		shell := &cappedBuffer{max: n + margin}
		write(shell)
		var shellText string
		if !shell.truncated {
			shellText = firstBytes(redact(shell.buf.String(), secrets), n)
		} else {
			head, _ := headOf(shell.buf.Bytes(), n, secrets, true)
			shellText = firstBytes(redact(string(head), secrets), n)
		}
		drill := &headTail{max: n, margin: margin}
		write(drill)
		stderr := &cappedBuffer{max: n + margin}
		write(stderr)
		for name, got := range map[string]string{
			"shell":  shellText,
			"drill":  drill.text(secrets),
			"stderr": cutRedacted(stderr.buf.Bytes(), stderr.truncated, n, secrets, "\n... [stderr truncated]"),
		} {
			for _, s := range secrets {
				if piece := leaked(got, s); piece != "" {
					t.Fatalf("seed %d, %s capture of %d bytes: kept %q of secret %q", seed, name, n, piece, s)
				}
			}
		}
	}
}
