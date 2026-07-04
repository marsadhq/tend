-- 0004_token_hash_unique: API tokens authenticate by hash ALONE
-- (AuthenticateToken looks up token_hash with no org filter), so the hash must
-- be globally unique. The original UNIQUE(org_id, token_hash) would allow two
-- orgs to hold the same hash, making the hash-only lookup ambiguous. This
-- index enforces global uniqueness; the old composite constraint becomes
-- redundant (strictly weaker) and is left in place on already-created tables.
-- Same fix Ward received.
CREATE UNIQUE INDEX idx_api_tokens_hash ON api_tokens(token_hash);
