-- Domain table belonging to the CRM example module, not to the kernel.
-- It stays in the kernel migration set because renumbering an applied goose
-- migration breaks every existing database. When modules own their own
-- migrations, this moves to examples/embedded/crm and is dropped from here.
-- +goose Up
CREATE TABLE IF NOT EXISTS contacts (
    id TEXT PRIMARY KEY,
    tenant_id TEXT NOT NULL,
    name TEXT,
    email TEXT
);

CREATE INDEX IF NOT EXISTS idx_contacts_tenant_id ON contacts(tenant_id);

-- +goose Down
DROP TABLE IF EXISTS contacts;
