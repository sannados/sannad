-- +goose Up
CREATE TABLE IF NOT EXISTS identities (
    id TEXT PRIMARY KEY,
    traits JSON,
    roles JSON,
    permissions JSON,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    mfa_enabled BOOLEAN,
    mfa_secret TEXT,
    verified BOOLEAN,
    verified_at TIMESTAMPTZ,
    state TEXT
);

CREATE TABLE IF NOT EXISTS credentials (
    id TEXT PRIMARY KEY,
    identity_id TEXT,
    type TEXT,
    identifier TEXT,
    secret TEXT,
    config JSON,
    created_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS sessions (
    id TEXT PRIMARY KEY,
    identity_id TEXT,
    refresh_token TEXT,
    expires_at TIMESTAMPTZ,
    refresh_expires_at TIMESTAMPTZ,
    issued_at TIMESTAMPTZ,
    active BOOLEAN
);

CREATE TABLE IF NOT EXISTS auth_tokens (
    token TEXT PRIMARY KEY,
    identity_id TEXT,
    type TEXT,
    expires_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS audit_events (
    id TEXT PRIMARY KEY,
    type TEXT,
    actor_id TEXT,
    subject_id TEXT,
    status TEXT,
    message TEXT,
    metadata JSON,
    created_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS oauth2_clients (
    id TEXT PRIMARY KEY,
    secret TEXT,
    redirect_uris TEXT,
    grant_types TEXT,
    scopes TEXT,
    app_name TEXT,
    back_channel_logout_uri TEXT
);

CREATE TABLE IF NOT EXISTS oauth2_auth_codes (
    code TEXT PRIMARY KEY,
    client_id TEXT,
    identity_id TEXT,
    redirect_uri TEXT,
    scopes TEXT,
    code_challenge TEXT,
    code_challenge_method TEXT,
    expires_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS oauth2_refresh_tokens (
    token TEXT PRIMARY KEY,
    client_id TEXT,
    identity_id TEXT,
    scopes TEXT,
    expires_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS rebac_relation_tuples (
    id TEXT PRIMARY KEY,
    subject_type TEXT,
    subject_id TEXT,
    subject_relation TEXT,
    relation TEXT,
    object_type TEXT,
    object_id TEXT,
    created_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS accounts (
    id TEXT PRIMARY KEY,
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT,
    traits JSON
);

CREATE INDEX IF NOT EXISTS idx_identities_deleted_at ON identities(deleted_at);

CREATE INDEX IF NOT EXISTS idx_credentials_identity_id ON credentials(identity_id);
CREATE INDEX IF NOT EXISTS idx_credentials_type ON credentials(type);
CREATE INDEX IF NOT EXISTS idx_credentials_identifier ON credentials(identifier);

CREATE INDEX IF NOT EXISTS idx_sessions_identity_id ON sessions(identity_id);
CREATE INDEX IF NOT EXISTS idx_sessions_refresh_token ON sessions(refresh_token);

CREATE INDEX IF NOT EXISTS idx_auth_tokens_identity_id ON auth_tokens(identity_id);
CREATE INDEX IF NOT EXISTS idx_auth_tokens_type ON auth_tokens(type);
CREATE INDEX IF NOT EXISTS idx_auth_tokens_expires_at ON auth_tokens(expires_at);

CREATE INDEX IF NOT EXISTS idx_audit_events_type ON audit_events(type);
CREATE INDEX IF NOT EXISTS idx_audit_events_actor_id ON audit_events(actor_id);
CREATE INDEX IF NOT EXISTS idx_audit_events_subject_id ON audit_events(subject_id);
CREATE INDEX IF NOT EXISTS idx_audit_events_status ON audit_events(status);
CREATE INDEX IF NOT EXISTS idx_audit_events_created_at ON audit_events(created_at);

CREATE INDEX IF NOT EXISTS idx_oauth2_auth_codes_client_id ON oauth2_auth_codes(client_id);
CREATE INDEX IF NOT EXISTS idx_oauth2_auth_codes_identity_id ON oauth2_auth_codes(identity_id);
CREATE INDEX IF NOT EXISTS idx_oauth2_auth_codes_expires_at ON oauth2_auth_codes(expires_at);

CREATE INDEX IF NOT EXISTS idx_oauth2_refresh_tokens_client_id ON oauth2_refresh_tokens(client_id);
CREATE INDEX IF NOT EXISTS idx_oauth2_refresh_tokens_identity_id ON oauth2_refresh_tokens(identity_id);
CREATE INDEX IF NOT EXISTS idx_oauth2_refresh_tokens_expires_at ON oauth2_refresh_tokens(expires_at);

CREATE INDEX IF NOT EXISTS idx_rebac_full ON rebac_relation_tuples(subject_type, subject_id, subject_relation, relation, object_type, object_id);
CREATE INDEX IF NOT EXISTS idx_rebac_object ON rebac_relation_tuples(object_type, object_id);
CREATE INDEX IF NOT EXISTS idx_rebac_relation ON rebac_relation_tuples(relation);
CREATE INDEX IF NOT EXISTS idx_rebac_subject ON rebac_relation_tuples(subject_type, subject_id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_email ON accounts(email);

-- +goose Down
DROP TABLE IF EXISTS accounts;
DROP TABLE IF EXISTS rebac_relation_tuples;
DROP TABLE IF EXISTS oauth2_refresh_tokens;
DROP TABLE IF EXISTS oauth2_auth_codes;
DROP TABLE IF EXISTS oauth2_clients;
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS auth_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS credentials;
DROP TABLE IF EXISTS identities;
