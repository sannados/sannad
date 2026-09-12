-- +goose Up
-- Enterprise SAML 2.0 SSO, via kayan-saml's ServiceProvider. Unlike OIDC
-- social login, a SAML identity provider belongs to exactly one enterprise
-- customer's tenant, fixed at configuration time — see
-- internal/platform/auth/saml.go's package comment for why reconciliation
-- runs through its own Hooks rather than Kayan's generic identity/credential
-- storage path.
--
-- saml_sessions tracks one pending SP-initiated authentication attempt
-- between "redirect to the IdP" and "the IdP posts back to the ACS
-- endpoint". Deliberately not tenant-scoped: which tenant a response belongs
-- to is not known until the session is looked up and its IdP resolved.
CREATE TABLE IF NOT EXISTS saml_sessions (
    id                       TEXT NOT NULL,
    request_id               TEXT NOT NULL,
    idp_id                   TEXT NOT NULL,
    relay_state              TEXT NOT NULL,
    return_url               TEXT NOT NULL,
    force_authn              BOOLEAN NOT NULL DEFAULT 0,
    requested_authn_contexts JSON,
    create_time              TIMESTAMP NOT NULL,
    expires_at               TIMESTAMP NOT NULL,
    PRIMARY KEY (id)
);

CREATE INDEX IF NOT EXISTS idx_saml_sessions_request_id ON saml_sessions (request_id);
CREATE INDEX IF NOT EXISTS idx_saml_sessions_expires ON saml_sessions (expires_at);

-- saml_identities maps one identity provider's NameID to a local account —
-- the SAML equivalent of oidc_identities, and for the identical reason: the
-- lookup key is what the provider actually vouches for, never email.
--
-- Deliberately not tenant-scoped either. Unlike OIDC, the tenant for a SAML
-- login IS known at creation time (it is the IdP's own configured tenant),
-- so this table could carry tenant_id — it does not, because the lookup that
-- reads it (idp_id, name_id) already narrows to one IdP, which already
-- narrows to one tenant. Adding a redundant tenant_id column and predicate
-- would protect nothing an idp_id match does not already guarantee.
CREATE TABLE IF NOT EXISTS saml_identities (
    id         TEXT NOT NULL,
    idp_id     TEXT NOT NULL,
    name_id    TEXT NOT NULL,
    account_id TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (id)
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_saml_identities_idp_name
    ON saml_identities (idp_id, name_id);

CREATE INDEX IF NOT EXISTS idx_saml_identities_account
    ON saml_identities (account_id);

-- +goose Down
DROP INDEX IF EXISTS idx_saml_identities_account;
DROP INDEX IF EXISTS idx_saml_identities_idp_name;
DROP TABLE IF EXISTS saml_identities;
DROP INDEX IF EXISTS idx_saml_sessions_expires;
DROP INDEX IF EXISTS idx_saml_sessions_request_id;
DROP TABLE IF EXISTS saml_sessions;
