-- +goose Up
-- Kayan v0.3.0's OIDC relying-party (flow.KayanOIDCStrategy) replaces the
-- OIDCManager this deployment used before. OIDCManager's callback took no
-- state parameter, so it could not validate CSRF state, and it requested no
-- nonce, so an ID token could be replayed. This migration adds the storage
-- the fixed flow requires: state/PKCE/nonce is a server-side record now,
-- single-use and short-lived, rather than a client cookie the old flow relied
-- on for CSRF alone.
--
-- Untenanted deliberately: a state row exists only between "redirect to the
-- provider" and "the provider redirects back", before any tenant is bound to
-- the flow — the tenant is resolved from the callback's own host, same as
-- before. It carries no tenant_id column, so the startup audit correctly
-- ignores it rather than flagging it as unscoped.
CREATE TABLE IF NOT EXISTS oidc_states (
    state         TEXT NOT NULL,
    code_verifier TEXT NOT NULL,
    nonce         TEXT NOT NULL,
    expires_at    TIMESTAMP NOT NULL,
    PRIMARY KEY (state)
);

-- Supports purging expired rows without a table scan.
CREATE INDEX IF NOT EXISTS idx_oidc_states_expires ON oidc_states (expires_at);

-- Maps a provider's stable subject claim to the local account, replacing the
-- old flow's implicit lookup by bare email — which is exactly the bug this
-- migration exists to close: two providers' users asserting the same address
-- could take over each other's account. A (provider, sub) pair is the
-- identity a provider actually vouches for; email is carried as a trait, not
-- a lookup key.
--
-- Deliberately not tenant-scoped, for the same reason `accounts` is not (see
-- ADR 0005 and Account's own doc comment): resolving which account a login
-- belongs to has to happen before any tenant context exists — the tenant is
-- stamped onto a freshly created account afterwards, at the service layer,
-- the same compensating-write pattern password registration already uses.
-- A row scoped to a tenant that is not yet known cannot be written at the
-- moment it is created.
CREATE TABLE IF NOT EXISTS oidc_identities (
    id         TEXT NOT NULL,
    provider   TEXT NOT NULL,
    sub        TEXT NOT NULL,
    account_id TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_oidc_identities_provider_sub
    ON oidc_identities (provider, sub);

CREATE INDEX IF NOT EXISTS idx_oidc_identities_account
    ON oidc_identities (account_id);

-- +goose Down
DROP INDEX IF EXISTS idx_oidc_identities_account;
DROP INDEX IF EXISTS idx_oidc_identities_provider_sub;
DROP TABLE IF EXISTS oidc_identities;
DROP INDEX IF EXISTS idx_oidc_states_expires;
DROP TABLE IF EXISTS oidc_states;
