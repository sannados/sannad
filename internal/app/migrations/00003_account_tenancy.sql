-- +goose Up
-- Accounts gain tenant membership. This is the authoritative source for the
-- tenant claim placed in every issued session; before it existed there was
-- nothing to validate a request's claimed tenant against. See ADR 0005.
--
-- The table is rebuilt rather than altered. Email uniqueness moves from global
-- to per-tenant, and the original UNIQUE was declared as a column constraint
-- rather than a named index — DROP INDEX cannot remove it on SQLite, so the
-- constraint would survive and reject the same address in a second tenant.
--
-- Per-tenant uniqueness is required, not cosmetic: two customers must be able
-- to have a user at the same address, and a global constraint also discloses
-- that an address is registered in some other tenant at registration time.
CREATE TABLE accounts_new (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL DEFAULT 'default',
    email TEXT NOT NULL,
    password_hash TEXT,
    traits JSON
);

-- Existing rows predate tenancy and are assigned to a single default tenant.
-- An account with no tenant can never log in — login requires the stored tenant
-- to match the one being authenticated against — so NULL is not an option.
-- Deployments where more than one logical tenant shared this table must
-- reassign these rows before opening access.
INSERT INTO accounts_new (id, tenant_id, email, password_hash, traits)
SELECT id, 'default', email, password_hash, traits FROM accounts;

DROP TABLE accounts;
ALTER TABLE accounts_new RENAME TO accounts;

CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_tenant_email ON accounts(tenant_id, email);
CREATE INDEX IF NOT EXISTS idx_accounts_tenant_id ON accounts(tenant_id);

-- Scoped tables are always queried with a tenant predicate, so their indexes
-- must lead with the tenant column to be usable. A lone index on email is dead
-- weight once every query is scoped.
CREATE INDEX IF NOT EXISTS idx_contacts_tenant_email ON contacts(tenant_id, email);

-- +goose Down
-- Reversing drops tenant membership. Accounts that share an email address
-- across tenants collide under the restored global constraint; only the first
-- row for each address survives.
DROP INDEX IF EXISTS idx_contacts_tenant_email;
DROP INDEX IF EXISTS idx_accounts_tenant_id;
DROP INDEX IF EXISTS idx_accounts_tenant_email;

CREATE TABLE accounts_old (
    id TEXT PRIMARY KEY,
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT,
    traits JSON
);

INSERT INTO accounts_old (id, email, password_hash, traits)
SELECT id, email, password_hash, traits FROM accounts
WHERE id IN (SELECT MIN(id) FROM accounts GROUP BY email);

DROP TABLE accounts;
ALTER TABLE accounts_old RENAME TO accounts;

CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_email ON accounts(email);
