-- +goose Up
-- Role-based access control (Kayan v0.3.0's core/rbac) and the full
-- compliance-grade audit event shape (core/audit), neither of which this
-- deployment had storage for before.
--
-- Not kayan-gorm's own bundled schema for role_assignments/role_definitions:
-- its RBACRepository issues no tenant_id predicate in GetIdentityRoles or
-- GetRole despite role_definitions being keyed on (name, tenant_id), and its
-- TenantID()/SetTenantID() methods do not match this codebase's
-- tenant.TenantAware shape (GetTenantID()/SetTenantID()), so the isolation
-- callback registered in bootstrap.New cannot recognise or fix it either.
-- The tables below are shaped the same as Kayan's own migration for
-- consistency, but are read and written entirely through this codebase's own
-- adapter (internal/platform/auth/rbac.go), which is registered as a scoped
-- model and gets real per-tenant isolation.
CREATE TABLE IF NOT EXISTS role_assignments (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id   TEXT NOT NULL DEFAULT '',
    identity_id TEXT NOT NULL,
    role        TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_role_assignments_unique
    ON role_assignments (tenant_id, identity_id, role);
CREATE INDEX IF NOT EXISTS idx_role_assignments_identity
    ON role_assignments (tenant_id, identity_id);

-- Role definitions are tenant-scoped, not platform-global: a tenant may
-- define "billing_admin" however it needs to without touching any other
-- tenant's catalog. See auth.SeedDefaultRoles for the starter set every
-- tenant gets on provisioning.
CREATE TABLE IF NOT EXISTS role_definitions (
    name        TEXT NOT NULL,
    tenant_id   TEXT NOT NULL DEFAULT '',
    permissions TEXT,
    inherits    TEXT,
    description TEXT,
    PRIMARY KEY (name, tenant_id)
);

-- The audit_events table this deployment already had was copied from Kayan
-- v0.1.0's narrow shape: id, type, actor_id, subject_id, status, message,
-- metadata, created_at. Nothing else. core/audit's AuditEvent in v0.3.0 —
-- designed for SOC 2 / ISO 27001 — carries a tenant ID, request correlation,
-- risk classification, change tracking (old/new value), and geo/device/session
-- context, none of which this table had a column for.
--
-- Rebuilt rather than ALTER-ADD-COLUMN, and not only for the new columns: the
-- original created_at was declared TIMESTAMPTZ, and this codebase's own audit
-- tests found that SQLite (via glebarez/modernc) round-trips a TIMESTAMPTZ
-- column's writes and reads inconsistently — a value written through GORM
-- came back as "sql: Scan error ... storing driver.Value type string into
-- type *time.Time" on read, while the identical Go code against a TIMESTAMP
-- column (every other table in this codebase) works. Declared TIMESTAMP here,
-- matching role_assignments above and every other table with a created_at
-- column. Postgres treats the two identically; this is a SQLite-specific
-- affinity quirk that only a rebuilt column, not ALTER, can fix on SQLite
-- (ALTER COLUMN TYPE is not valid SQLite syntax at all).
--
-- Existing rows are preserved, the same rebuild-and-copy shape migration
-- 00003 already used for accounts.
CREATE TABLE audit_events_new (
    id            TEXT PRIMARY KEY,
    tenant_id     TEXT NOT NULL DEFAULT '',
    type          TEXT NOT NULL,
    actor_id      TEXT,
    subject_id    TEXT,
    status        TEXT,
    message       TEXT,
    metadata      TEXT,
    created_at    TIMESTAMP NOT NULL,
    ip_address    TEXT,
    user_agent    TEXT,
    device_id     TEXT,
    session_id    TEXT,
    resource_type TEXT,
    resource_id   TEXT,
    old_value     TEXT,
    new_value     TEXT,
    risk          TEXT,
    request_id    TEXT,
    geo_country   TEXT,
    geo_region    TEXT,
    geo_city      TEXT,
    geo_lat       REAL,
    geo_long      REAL
);

INSERT INTO audit_events_new (id, type, actor_id, subject_id, status, message, metadata, created_at)
SELECT id, type, actor_id, subject_id, status, message, metadata, created_at FROM audit_events;

DROP TABLE audit_events;
ALTER TABLE audit_events_new RENAME TO audit_events;

-- Audit queries are almost always "this tenant, recent first", or narrowed by
-- actor/subject/type within that. Matches the index shape Kayan's own
-- migration uses.
CREATE INDEX IF NOT EXISTS idx_audit_events_tenant_created ON audit_events (tenant_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_events_actor ON audit_events (tenant_id, actor_id);
CREATE INDEX IF NOT EXISTS idx_audit_events_subject ON audit_events (tenant_id, subject_id);
CREATE INDEX IF NOT EXISTS idx_audit_events_type ON audit_events (tenant_id, type);

-- +goose Down
DROP INDEX IF EXISTS idx_audit_events_type;
DROP INDEX IF EXISTS idx_audit_events_subject;
DROP INDEX IF EXISTS idx_audit_events_actor;
DROP INDEX IF EXISTS idx_audit_events_tenant_created;

CREATE TABLE audit_events_old (
    id TEXT PRIMARY KEY,
    type TEXT,
    actor_id TEXT,
    subject_id TEXT,
    status TEXT,
    message TEXT,
    metadata JSON,
    created_at TIMESTAMPTZ
);
INSERT INTO audit_events_old (id, type, actor_id, subject_id, status, message, metadata, created_at)
SELECT id, type, actor_id, subject_id, status, message, metadata, created_at FROM audit_events;
DROP TABLE audit_events;
ALTER TABLE audit_events_old RENAME TO audit_events;

DROP INDEX IF EXISTS idx_role_assignments_identity;
DROP INDEX IF EXISTS idx_role_assignments_unique;
DROP TABLE IF EXISTS role_definitions;
DROP TABLE IF EXISTS role_assignments;
