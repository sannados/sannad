-- +goose Up
-- The tenant directory (ADR 0005 deferred work).
--
-- Before this table, a tenant was any string that reached the edge. Isolation
-- worked — data was correctly scoped — but scoped to an identifier nobody had
-- provisioned, so registering with an arbitrary tenant name silently created a
-- working tenant, and suspending one meant deleting its users because the
-- identifier itself carried no state.
--
-- This table is deliberately small. It is not a customer record, a billing row,
-- or a settings store; those belong to whatever product is built on the kernel.
-- It answers one question asked on every unauthenticated request: may this
-- tenant be used right now.
--
-- It is global by nature — it is the list of tenants, so scoping it to a tenant
-- would be circular — and is therefore never registered as a scoped model. The
-- startup audit ignores it because it carries no tenant_id column.
CREATE TABLE IF NOT EXISTS tenants (
    id         TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    -- active | suspended | archived. Suspension preserves data, which is what
    -- makes non-payment or an investigation reversible; a suspension that
    -- deleted rows would not be.
    status     TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMP NOT NULL,
    updated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (id)
);

-- Supports listing tenants by state, which is how an operator finds everything
-- currently suspended.
CREATE INDEX IF NOT EXISTS idx_tenants_status ON tenants (status);

-- Existing deployments have accounts whose tenants predate this table. Without
-- this backfill every one of those tenants becomes unknown at the next login,
-- locking out every existing user the moment the migration lands.
--
-- The accounts table is the only record of which tenants were ever in use, so it
-- is the only thing that can seed the directory.
INSERT INTO tenants (id, name, status, created_at, updated_at)
SELECT DISTINCT tenant_id, tenant_id, 'active', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
FROM accounts
WHERE tenant_id <> ''
  AND tenant_id NOT IN (SELECT id FROM tenants);

-- +goose Down
DROP INDEX IF EXISTS idx_tenants_status;
DROP TABLE IF EXISTS tenants;
