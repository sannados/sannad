-- +goose Up
-- Admin operations on a single account: lock/unlock, and force logout
-- everywhere. Before this, "suspend" only existed at the tenant level — an
-- operator could disable every account in a tenant but not one of them, and
-- nothing could invalidate a token already issued short of waiting for it to
-- expire.
--
-- active blocks future logins outright, the same shape as
-- webhooks.Endpoint.Active and tenancy.Tenant's suspension: reversible,
-- auditable, and distinct from deleting the row.
--
-- token_version is what makes "force logout on every device" possible for a
-- stateless JWT session without becoming the session store this design
-- deliberately avoided (see RevocationStore's own doc comment,
-- "RevokeAllForIdentity is deliberately absent... should carry a
-- per-identity token generation in the claim and compare it"). Every issued
-- token carries the account's token_version at signing time; validation
-- compares it against the account's current value and rejects a mismatch.
-- Incrementing it invalidates every token issued before the increment,
-- immediately, without knowing what those tokens are or where they are held.
ALTER TABLE accounts ADD COLUMN active INTEGER NOT NULL DEFAULT 1;
ALTER TABLE accounts ADD COLUMN token_version INTEGER NOT NULL DEFAULT 0;

-- +goose Down
-- SQLite cannot drop columns portably pre-3.35; left in place rather than a
-- table rebuild that risks the rows in a downgrade.
