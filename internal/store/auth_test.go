package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/marsadhq/tend/internal/auth"
	"github.com/marsadhq/tend/internal/jobs"
	"github.com/marsadhq/tend/internal/store"
)

// TestUserRoundTrip proves CreateUser then GetUserByEmail/GetUserByID return the
// stored row, and that the UNIQUE(org_id, email) constraint rejects a duplicate.
func TestUserRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}

		id, err := s.CreateUser(ctx, auth.User{OrgID: org.ID, Email: "a@example.com", PasswordHash: "$argon2id$hash"})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if id <= 0 {
			t.Fatalf("CreateUser returned id=%d, want > 0", id)
		}

		byEmail, err := s.GetUserByEmail(ctx, org.ID, "a@example.com")
		if err != nil {
			t.Fatalf("GetUserByEmail: %v", err)
		}
		if byEmail.ID != id || byEmail.Email != "a@example.com" || byEmail.PasswordHash != "$argon2id$hash" {
			t.Fatalf("GetUserByEmail = %+v, want id %d, email a@example.com, hash set", byEmail, id)
		}
		if byEmail.CreatedAt.IsZero() {
			t.Fatalf("GetUserByEmail CreatedAt should be set")
		}

		byID, err := s.GetUserByID(ctx, org.ID, id)
		if err != nil {
			t.Fatalf("GetUserByID: %v", err)
		}
		if byID.ID != id || byID.Email != "a@example.com" {
			t.Fatalf("GetUserByID = %+v, want id %d", byID, id)
		}

		// Missing email / id => ErrNotFound.
		if _, err := s.GetUserByEmail(ctx, org.ID, "nobody@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetUserByEmail missing: got %v, want ErrNotFound", err)
		}
		if _, err := s.GetUserByID(ctx, org.ID, id+9999); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetUserByID missing: got %v, want ErrNotFound", err)
		}

		// Duplicate (org_id, email) must be rejected.
		if _, err := s.CreateUser(ctx, auth.User{OrgID: org.ID, Email: "a@example.com", PasswordHash: "x"}); err == nil {
			t.Fatalf("CreateUser duplicate email: expected error, got nil")
		}
	})
}

// TestUserOrgScoping proves a user is invisible to another org.
func TestUserOrgScoping(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.CreateUser(ctx, auth.User{OrgID: org.ID, Email: "scoped@example.com", PasswordHash: "h"})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
		if _, err := s.GetUserByEmail(ctx, org.ID+999, "scoped@example.com"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetUserByEmail wrong org: got %v, want ErrNotFound", err)
		}
		if _, err := s.GetUserByID(ctx, org.ID+999, id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetUserByID wrong org: got %v, want ErrNotFound", err)
		}
	})
}

// TestMembershipRoundTrip proves CreateMembership + GetMembership return the role.
func TestMembershipRoundTrip(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		userID, err := s.CreateUser(ctx, auth.User{OrgID: org.ID, Email: "m@example.com", PasswordHash: "h"})
		if err != nil {
			t.Fatalf("CreateUser: %v", err)
		}

		id, err := s.CreateMembership(ctx, auth.Membership{OrgID: org.ID, UserID: userID, Role: "admin"})
		if err != nil {
			t.Fatalf("CreateMembership: %v", err)
		}
		if id <= 0 {
			t.Fatalf("CreateMembership returned id=%d, want > 0", id)
		}

		m, err := s.GetMembership(ctx, org.ID, userID)
		if err != nil {
			t.Fatalf("GetMembership: %v", err)
		}
		if m.ID != id || m.UserID != userID || m.Role != "admin" {
			t.Fatalf("GetMembership = %+v, want id %d role admin", m, id)
		}
		if m.CreatedAt.IsZero() {
			t.Fatalf("GetMembership CreatedAt should be set")
		}

		// Missing membership => ErrNotFound.
		if _, err := s.GetMembership(ctx, org.ID, userID+9999); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetMembership missing: got %v, want ErrNotFound", err)
		}
		// Wrong org => ErrNotFound.
		if _, err := s.GetMembership(ctx, org.ID+999, userID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetMembership wrong org: got %v, want ErrNotFound", err)
		}
	})
}

// TestTokenAuthenticate proves CreateToken + AuthenticateToken(hash) returns the
// (orgID, name), and an unknown hash gives ErrNotFound.
func TestTokenAuthenticate(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		hash := auth.HashToken("tend_secretplaintext")
		id, err := s.CreateToken(ctx, auth.APIToken{OrgID: org.ID, Name: "ci", TokenHash: hash})
		if err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		if id <= 0 {
			t.Fatalf("CreateToken returned id=%d, want > 0", id)
		}

		gotOrg, gotName, err := s.AuthenticateToken(ctx, hash)
		if err != nil {
			t.Fatalf("AuthenticateToken: %v", err)
		}
		if gotOrg != org.ID || gotName != "ci" {
			t.Fatalf("AuthenticateToken = (%d, %q), want (%d, ci)", gotOrg, gotName, org.ID)
		}

		// Unknown hash => ErrNotFound (no info leak, no partial match).
		if _, _, err := s.AuthenticateToken(ctx, auth.HashToken("tend_nope")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("AuthenticateToken unknown: got %v, want ErrNotFound", err)
		}
		// A prefix of the real hash must NOT match (exact match only).
		if _, _, err := s.AuthenticateToken(ctx, hash[:len(hash)-4]); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("AuthenticateToken partial: got %v, want ErrNotFound", err)
		}
	})
}

// TestListTokensNoHashLeak proves ListTokens returns the token metadata but NEVER
// the token_hash material: every returned TokenHash must be empty.
func TestListTokensNoHashLeak(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		h1 := auth.HashToken("tend_one")
		h2 := auth.HashToken("tend_two")
		if _, err := s.CreateToken(ctx, auth.APIToken{OrgID: org.ID, Name: "one", TokenHash: h1}); err != nil {
			t.Fatalf("CreateToken one: %v", err)
		}
		if _, err := s.CreateToken(ctx, auth.APIToken{OrgID: org.ID, Name: "two", TokenHash: h2}); err != nil {
			t.Fatalf("CreateToken two: %v", err)
		}

		tokens, err := s.ListTokens(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}
		if len(tokens) != 2 {
			t.Fatalf("ListTokens len = %d, want 2", len(tokens))
		}
		for _, tk := range tokens {
			if tk.TokenHash != "" {
				t.Fatalf("ListTokens leaked hash material: token %q has TokenHash=%q, want empty", tk.Name, tk.TokenHash)
			}
			if tk.OrgID != org.ID || tk.Name == "" || tk.ID <= 0 {
				t.Fatalf("ListTokens token incomplete: %+v", tk)
			}
			if tk.CreatedAt.IsZero() {
				t.Fatalf("ListTokens token %q CreatedAt should be set", tk.Name)
			}
		}
	})
}

// TestListTokensOrgScoped proves ListTokens only returns the requesting org's tokens.
func TestListTokensOrgScoped(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.CreateToken(ctx, auth.APIToken{OrgID: org.ID, Name: "mine", TokenHash: auth.HashToken("tend_mine")}); err != nil {
			t.Fatalf("CreateToken: %v", err)
		}
		// Another org's token is invisible.
		if _, err := s.CreateToken(ctx, auth.APIToken{OrgID: org.ID + 777, Name: "theirs", TokenHash: auth.HashToken("tend_theirs")}); err != nil {
			t.Fatalf("CreateToken other org: %v", err)
		}
		tokens, err := s.ListTokens(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}
		if len(tokens) != 1 || tokens[0].Name != "mine" {
			t.Fatalf("ListTokens = %+v, want only 'mine'", tokens)
		}
	})
}

// TestDeleteTokenOrgScoped proves DeleteToken is org-scoped: a token belonging to
// another org must NOT be deletable.
func TestDeleteTokenOrgScoped(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		id, err := s.CreateToken(ctx, auth.APIToken{OrgID: org.ID, Name: "victim", TokenHash: auth.HashToken("tend_victim")})
		if err != nil {
			t.Fatalf("CreateToken: %v", err)
		}

		// Wrong org delete must not remove the token.
		if err := s.DeleteToken(ctx, org.ID+999, id); err != nil {
			t.Fatalf("DeleteToken wrong org returned error: %v", err)
		}
		tokens, err := s.ListTokens(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListTokens: %v", err)
		}
		if len(tokens) != 1 {
			t.Fatalf("token wrongly deleted across orgs: ListTokens len = %d, want 1", len(tokens))
		}

		// Correct org delete removes it.
		if err := s.DeleteToken(ctx, org.ID, id); err != nil {
			t.Fatalf("DeleteToken correct org: %v", err)
		}
		tokens, err = s.ListTokens(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListTokens after delete: %v", err)
		}
		if len(tokens) != 0 {
			t.Fatalf("token not deleted: ListTokens len = %d, want 0", len(tokens))
		}
	})
}

// TestGetRun proves GetRun(orgID, runID) returns the run including Output, and
// returns ErrNotFound for a foreign/absent id.
func TestGetRun(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		orgID, jobID := seedJob(t, ctx, s)

		runID, err := s.EnqueueRun(ctx, orgID, jobID)
		if err != nil {
			t.Fatalf("EnqueueRun: %v", err)
		}
		if _, ok, err := s.ClaimRun(ctx, "worker"); err != nil || !ok {
			t.Fatalf("ClaimRun: ok=%v err=%v", ok, err)
		}
		if err := s.FinishRun(ctx, runID, jobs.StatusSucceeded, 0, "the-output"); err != nil {
			t.Fatalf("FinishRun: %v", err)
		}

		run, err := s.GetRun(ctx, orgID, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.ID != runID || run.JobID != jobID || run.Status != jobs.StatusSucceeded {
			t.Fatalf("GetRun = %+v, want id %d job %d succeeded", run, runID, jobID)
		}
		if run.Output != "the-output" {
			t.Fatalf("GetRun Output = %q, want %q", run.Output, "the-output")
		}

		// Absent id => ErrNotFound.
		if _, err := s.GetRun(ctx, orgID, runID+9999); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetRun absent: got %v, want ErrNotFound", err)
		}
		// Foreign org id => ErrNotFound (org-scoped).
		if _, err := s.GetRun(ctx, orgID+999, runID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetRun foreign org: got %v, want ErrNotFound", err)
		}
	})
}

// TestListSecretsNoCiphertextLeak proves ListSecrets returns names + created_at and
// NEVER the ciphertext (the SecretMeta type has no field for it), scoped to the org.
func TestListSecretsNoCiphertextLeak(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}
		const ciphertext = "TOP-SECRET-CIPHERTEXT"
		if err := s.PutSecret(ctx, org.ID, "api_key", ciphertext); err != nil {
			t.Fatalf("PutSecret: %v", err)
		}
		if err := s.PutSecret(ctx, org.ID, "db_pw", "another-cipher"); err != nil {
			t.Fatalf("PutSecret: %v", err)
		}
		// Another org's secret must be invisible.
		if err := s.PutSecret(ctx, org.ID+555, "theirs", "their-cipher"); err != nil {
			t.Fatalf("PutSecret other org: %v", err)
		}

		metas, err := s.ListSecrets(ctx, org.ID)
		if err != nil {
			t.Fatalf("ListSecrets: %v", err)
		}
		if len(metas) != 2 {
			t.Fatalf("ListSecrets len = %d, want 2", len(metas))
		}
		names := make(map[string]bool)
		for _, m := range metas {
			names[m.Name] = true
			if m.CreatedAt.IsZero() {
				t.Fatalf("ListSecrets %q CreatedAt should be set", m.Name)
			}
			// The ciphertext must never appear anywhere in the returned metadata.
			if strings.Contains(m.Name, ciphertext) {
				t.Fatalf("ListSecrets leaked ciphertext in name field: %q", m.Name)
			}
		}
		if !names["api_key"] || !names["db_pw"] {
			t.Fatalf("ListSecrets names = %v, want api_key + db_pw", names)
		}
		if names["theirs"] {
			t.Fatalf("ListSecrets leaked another org's secret name")
		}
	})
}

// TestTokenHashGloballyUnique proves the token hash is unique ACROSS orgs, not
// just within one: AuthenticateToken looks up by hash alone, so a duplicate
// hash in another org would make authentication ambiguous. (Parity with the
// identical Ward fix.)
func TestTokenHashGloballyUnique(t *testing.T) {
	forEachBackend(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		org, err := s.BootstrapDefaultOrg(ctx)
		if err != nil {
			t.Fatal(err)
		}

		hash := auth.HashToken("tend_sametoken")
		if _, err := s.CreateToken(ctx, auth.APIToken{OrgID: org.ID, Name: "a", TokenHash: hash}); err != nil {
			t.Fatalf("CreateToken org1: %v", err)
		}
		if _, err := s.CreateToken(ctx, auth.APIToken{OrgID: org.ID + 1, Name: "b", TokenHash: hash}); err == nil {
			t.Fatal("CreateToken with a duplicate hash in another org succeeded; token_hash must be globally unique")
		}

		// The unambiguous lookup still authenticates the original row.
		gotOrg, gotName, err := s.AuthenticateToken(ctx, hash)
		if err != nil {
			t.Fatalf("AuthenticateToken: %v", err)
		}
		if gotOrg != org.ID || gotName != "a" {
			t.Fatalf("AuthenticateToken = (org %d, %q), want (org %d, %q)", gotOrg, gotName, org.ID, "a")
		}
	})
}
