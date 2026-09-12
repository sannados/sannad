-- +goose Up
-- Session revocation list (ADR 0005 deferred work).
--
-- A JWT is valid because it is signed, not because a server remembers it. That
-- is what makes it cheap, and it is also why logout did nothing before this
-- table existed: nothing was asked at validation time.
--
-- The alternative, a server-side session store, restores revocation by removing
-- the reason JWTs were chosen — every request becomes a lookup and the
-- signature stops being the authority. This table stores the inverse: not which
-- sessions are valid, but the few that were killed early.
--
-- session_id is the primary key because revocation is per session. It is the
-- `sid` claim, which Refresh carries forward unchanged, so revoking a session
-- also kills every refresh token derived from it — which is what logout has to
-- mean.
CREATE TABLE IF NOT EXISTS auth_revoked_sessions (
    session_id TEXT NOT NULL,
    tenant_id  TEXT NOT NULL DEFAULT '',
    expires_at TIMESTAMP NOT NULL,
    revoked_at TIMESTAMP NOT NULL,
    reason     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (session_id)
);

-- Supports the purge sweep. An entry is only useful while the token it revokes
-- would otherwise still validate; past that the token fails on expiry anyway and
-- the row is dead weight. Without this index the sweep scans the whole table.
CREATE INDEX IF NOT EXISTS idx_revoked_sessions_expires_at
    ON auth_revoked_sessions (expires_at);

-- Supports auditing: "what did this tenant revoke, and why" is the question
-- asked after an incident.
CREATE INDEX IF NOT EXISTS idx_revoked_sessions_tenant
    ON auth_revoked_sessions (tenant_id);

-- +goose Down
DROP INDEX IF EXISTS idx_revoked_sessions_tenant;
DROP INDEX IF EXISTS idx_revoked_sessions_expires_at;
DROP TABLE IF EXISTS auth_revoked_sessions;
