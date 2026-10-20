// Package auth holds Tend's identity types: the user, membership and API token
// rows the store persists, and the Principal a request is authenticated as.
// The cryptography behind them (argon2id passwords, API tokens, signed session
// cookies, CSRF tokens) is github.com/marsadhq/kit/auth; this package only
// adds the prefix Tend's tokens carry.
//
// This is a pure package: it imports nothing from internal/store or
// internal/httpserver. The store imports these row/identity types; never the
// reverse.
package auth

import (
	"time"

	kitauth "github.com/marsadhq/kit/auth"
)

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

// TokenPrefix marks Tend-issued API tokens.
const TokenPrefix = "tend_"

// GenerateToken returns a new high-entropy bearer token of the form
// "tend_" + base64url(32 random bytes). The plaintext is returned to the caller
// once; only kitauth.HashToken(token) should ever be persisted.
func GenerateToken() (string, error) {
	return kitauth.GenerateToken(TokenPrefix)
}
