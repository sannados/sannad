-- +goose Up
-- This module's own schema, shipped with the module rather than with the kernel.
--
-- The numbering starts at 00001 deliberately: every module numbers from one, and
-- two modules from unrelated authors will collide. Each module gets its own
-- version table, derived from its ID, so the collision is harmless — a shared
-- table would make this look already-applied and the module would start with no
-- tables at all.
CREATE TABLE IF NOT EXISTS notes (
    id         TEXT NOT NULL,
    tenant_id  TEXT NOT NULL,
    body       TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_notes_tenant ON notes (tenant_id);

-- +goose Down
DROP INDEX IF EXISTS idx_notes_tenant;
DROP TABLE IF EXISTS notes;
