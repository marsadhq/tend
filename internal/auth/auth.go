// Package auth provides the cryptographic primitives and domain identity types
// for tend's web/API surface: argon2id password hashing, high-entropy API
// tokens, signed (HMAC) session cookies, and per-session CSRF tokens.
//
// This is a pure package: it imports nothing from internal/store or
// internal/httpserver. The store imports these row/identity types; never the
// reverse.
//
// Security invariants enforced here:
//   - All randomness comes from crypto/rand (never math/rand).
//   - All secret comparisons are constant-time (crypto/subtle / hmac.Equal).
//   - No secret material (password plaintext, password hash, token plaintext,
//     token hash, or signing key) is ever logged or printed.
//   - Parsing of attacker-controlled input (PHC strings, cookies, CSRF tokens)
//     is defensive: malformed input yields false/error, never a panic.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// --- Domain types ------------------------------------------------------------

// User is an authenticatable account scoped to an org.
type User struct {
	ID           int64
	OrgID        int64
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

// APIToken is a named bearer credential scoped to an org. Only the hash of the
// token is persisted; the plaintext is shown to the user exactly once at
// creation time.
type APIToken struct {
	ID        int64
	OrgID     int64
	Name      string
	TokenHash string
	CreatedAt time.Time
}

// Membership records a user's role within an org.
type Membership struct {
	ID        int64
	OrgID     int64
	UserID    int64
	Role      string
	CreatedAt time.Time
}

// Principal is the request-scoped identity resolved from a session cookie or an
// API token. UserID is 0 for token-only (non-interactive) authentication.
type Principal struct {
	OrgID  int64
	UserID int64
	Role   string
}

// --- Passwords (argon2id) ----------------------------------------------------

// Argon2id parameters. These travel inside the PHC string so stored hashes
// remain verifiable even if these defaults change later (upgrade-on-login).
const (
	argonTime    uint32 = 2
	argonMemory  uint32 = 19456 // KiB = 19 MiB
	argonThreads uint8  = 1
	argonKeyLen  uint32 = 32
	argonSaltLen        = 16
)

// HashPassword hashes pw with argon2id and a fresh 16-byte random salt,
// returning a standard PHC-format string of the form
// "$argon2id$v=19$m=19456,t=2,p=1$<b64salt>$<b64hash>".
func HashPassword(pw string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := func(b []byte) string { return base64.RawStdEncoding.EncodeToString(b) }
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		enc(salt), enc(key),
	), nil
}

// VerifyPassword reports whether pw matches the argon2id PHC string phc. It
// parses the embedded parameters, recomputes the hash, and compares in constant
// time. Malformed input returns false (never panics).
func VerifyPassword(phc, pw string) bool {
	// Expected layout: ["", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash]
	parts := strings.Split(phc, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}

	var memory, timeCost uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &timeCost, &threads); err != nil {
		return false
	}
	if memory == 0 || timeCost == 0 || threads == 0 {
		return false
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}

	got := argon2.IDKey([]byte(pw), salt, timeCost, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// --- API tokens --------------------------------------------------------------

// tokenPrefix marks tend-issued API tokens.
const tokenPrefix = "tend_"

// tokenEntropyBytes is the number of random bytes behind each API token.
const tokenEntropyBytes = 32

// GenerateToken returns a new high-entropy bearer token of the form
// "tend_" + base64url(32 random bytes). The plaintext is returned to the caller
// once; only HashToken(token) should ever be persisted.
func GenerateToken() (string, error) {
	b := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken returns hex(sha256(token)). It is deterministic so the store can
// look up a token by its hash. A plain SHA-256 (not a slow KDF) is sufficient
// because tokens carry full random entropy and are not user-chosen.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// --- Sessions ----------------------------------------------------------------

// DefaultSessionTTL is the lifetime applied to sessions by callers that don't
// set an explicit expiry policy.
const DefaultSessionTTL = 7 * 24 * time.Hour

// sessionPayloadLen is the fixed binary payload size: user_id(8) + org_id(8) +
// exp_unix(8), all big-endian int64.
const sessionPayloadLen = 24

// Session is the identity carried by a signed session cookie.
type Session struct {
	UserID int64
	OrgID  int64
	Expiry time.Time
}

// SessionCodec signs and verifies session cookies and CSRF tokens with an
// HMAC-SHA256 key supplied by the caller. The key is never logged.
type SessionCodec struct {
	key []byte
}

// NewSessionCodec returns a codec keyed by the given HMAC secret. The caller
// owns key derivation; key should be at least 32 bytes of secret material.
func NewSessionCodec(key []byte) *SessionCodec {
	return &SessionCodec{key: key}
}

// mac returns HMAC-SHA256(key, msg).
func (c *SessionCodec) mac(msg []byte) []byte {
	h := hmac.New(sha256.New, c.key)
	h.Write(msg)
	return h.Sum(nil)
}

// Encode serializes s and returns "base64url(payload).base64url(hmac(payload))".
func (c *SessionCodec) Encode(s Session) (string, error) {
	payload := make([]byte, sessionPayloadLen)
	binary.BigEndian.PutUint64(payload[0:8], uint64(s.UserID))
	binary.BigEndian.PutUint64(payload[8:16], uint64(s.OrgID))
	binary.BigEndian.PutUint64(payload[16:24], uint64(s.Expiry.Unix()))
	tag := c.mac(payload)
	b64 := base64.RawURLEncoding
	return b64.EncodeToString(payload) + "." + b64.EncodeToString(tag), nil
}

// Decode parses and verifies a token produced by Encode. It returns an error if
// the token is malformed, the MAC does not match (tamper or wrong key), or the
// session has expired.
func (c *SessionCodec) Decode(token string) (Session, error) {
	b64 := base64.RawURLEncoding
	pStr, tStr, ok := strings.Cut(token, ".")
	if !ok {
		return Session{}, errors.New("auth: malformed session token")
	}
	payload, err := b64.DecodeString(pStr)
	if err != nil || len(payload) != sessionPayloadLen {
		return Session{}, errors.New("auth: malformed session payload")
	}
	tag, err := b64.DecodeString(tStr)
	if err != nil {
		return Session{}, errors.New("auth: malformed session mac")
	}
	if !hmac.Equal(tag, c.mac(payload)) {
		return Session{}, errors.New("auth: session signature mismatch")
	}
	s := Session{
		UserID: int64(binary.BigEndian.Uint64(payload[0:8])),
		OrgID:  int64(binary.BigEndian.Uint64(payload[8:16])),
		Expiry: time.Unix(int64(binary.BigEndian.Uint64(payload[16:24])), 0),
	}
	if time.Now().After(s.Expiry) {
		return Session{}, errors.New("auth: session expired")
	}
	return s, nil
}

// --- CSRF --------------------------------------------------------------------

// csrfMessage binds a CSRF token to a specific session (user, org, expiry) so a
// token minted for one session cannot be replayed against another.
func csrfMessage(s Session) []byte {
	return []byte("csrf:" +
		strconv.FormatInt(s.UserID, 10) + ":" +
		strconv.FormatInt(s.OrgID, 10) + ":" +
		strconv.FormatInt(s.Expiry.Unix(), 10))
}

// IssueCSRF returns a CSRF token bound to s. Bearer-token (API) requests carry
// no ambient cookie and are exempt from CSRF entirely.
func (c *SessionCodec) IssueCSRF(s Session) string {
	return base64.RawURLEncoding.EncodeToString(c.mac(csrfMessage(s)))
}

// CheckCSRF reports, in constant time, whether token is a valid CSRF token for
// s. Malformed tokens return false.
func (c *SessionCodec) CheckCSRF(s Session, token string) bool {
	got, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return false
	}
	return hmac.Equal(got, c.mac(csrfMessage(s)))
}
