-- +goose Up
-- Machine-to-machine identity (Kayan v0.3.0's flow.APIKeyStrategy), closing
-- the "Service identity, and service-to-service tenancy" item deferred in
-- ADR 0005: HeaderResolver-based service auth was rejected there because it
-- is only safe behind a gateway that overwrites the header from an
-- authenticated source, which did not exist. This is that authenticated
-- source — a service account authenticates like any account and receives the
-- same signed, tenant-bound session a human login does, so nothing downstream
-- needs a second notion of "who is calling."
--
-- Tenant-scoped like accounts: a service account acts for exactly one tenant,
-- and ADR 0005 is exactly the reason that scoping is not optional here.
CREATE TABLE IF NOT EXISTS service_accounts (
    id           TEXT NOT NULL,
    tenant_id    TEXT NOT NULL,
    name         TEXT NOT NULL,
    -- SHA-256 hex of the raw key. The raw key itself is never stored anywhere
    -- — it is shown to the caller exactly once, at creation, the same rule
    -- webhook endpoint secrets follow.
    api_key_hash TEXT NOT NULL,
    -- Revoking without deleting preserves the audit trail of what a service
    -- account did while it was live, the same reasoning kept for webhook
    -- endpoints and dead deliveries.
    active       INTEGER NOT NULL DEFAULT 1,
    created_at   TIMESTAMP NOT NULL,
    updated_at   TIMESTAMP NOT NULL,
    PRIMARY KEY (id)
);

-- The APIKeyStrategy lookup path: find the identity whose key hash matches.
-- Unique because a hash collision here would let one key authenticate as two
-- different service accounts.
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_accounts_key_hash
    ON service_accounts (api_key_hash);

CREATE INDEX IF NOT EXISTS idx_service_accounts_tenant
    ON service_accounts (tenant_id, active);

-- +goose Down
DROP INDEX IF EXISTS idx_service_accounts_tenant;
DROP INDEX IF EXISTS idx_service_accounts_key_hash;
DROP TABLE IF EXISTS service_accounts;
