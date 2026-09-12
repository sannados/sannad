# Auth Architecture

## Baseline

Sannad OS uses Kayan for identity and auth-related concerns.

Current wiring:

- `core/flow.RegistrationManager` + `flow.LoginManager` with a `PasswordStrategy` for local registration/login.
- `core/session.Manager` with `HS256Strategy` for JWT-backed sessions (configurable secret, 24h expiry).
- `kgorm.Repository` for persistence of identities, credentials, sessions, audit events, OAuth2 records, and ReBAC relation tuples.
- `internal/platform/auth.Service` wraps all Kayan managers and exposes `Register`, `Login`, `Validate`, `ValidateForSubject`, and OIDC methods.

## OAuth2 / OIDC (Phase E)

External OAuth2/OIDC login is backed by `core/flow.OIDCManager`. Configuration is passed via the `SANNAD_OIDC_PROVIDERS_JSON` environment variable (a JSON object mapping provider IDs to `kayancfg.OIDCProvider` structs).

Decision: configuration via a single JSON env var rather than per-provider env vars keeps the gateway stateless and container-friendly.

Routes added to the gateway:
- `GET /api/v1/oauth2/:provider/authorize` — builds authorization URL, sets an `oidc_state` cookie, redirects to provider.
- `GET /api/v1/oauth2/:provider/callback` — validates state cookie, exchanges code, returns `LoginResult` (same shape as password login).

The `oidc_state` cookie is `HttpOnly`, `SameSite=Lax`, and expires in 10 minutes.

When `SANNAD_OIDC_PROVIDERS_JSON` is empty, the routes are not registered and `auth.HasOIDC()` returns false.

## gRPC Auth (Phase D)

The kernel gRPC bridge (`:9090`) uses a `UnaryInterceptor` (`grpcAuthInterceptor`) that reads the `authorization: bearer <token>` gRPC metadata, validates the session, and injects `CallerInfo{Subject, TenantID}` into the request context. The tenant comes from the signed `tenant_id` claim in the token. The `x-tenant-id` metadata key is no longer read: it was caller-controlled, and trusting it allowed any authenticated caller to reach another tenant's data (ADR 0005).

## Session Validation (Gateway Middleware)

`requireAuth()` in the gateway Fiber app:
1. Reads `Authorization: Bearer <token>` header.
2. Calls `auth.Validate(token)` → Kayan session.
3. Injects `CallerInfo{Subject, TenantID}` into the request context, with the tenant taken from the token's signed `tenant_id` claim, and places the same tenant in the Kayan tenant context so storage isolation applies. The `X-Tenant-ID` header and `?tenant=` query parameter are no longer read (ADR 0005).
4. Returns `401` on missing/invalid token.

## Schema Migrations (Phase G)

All Kayan-internal tables (identities, credentials, sessions, audit_events, auth_tokens, oauth2_*, rebac_relation_tuples) plus the application-level `accounts` table are created via goose versioned SQL migrations in `internal/app/migrations/`.

`migrations.RunMigrations(sqlDB, dsn)` is called once in `bootstrap.New()` before constructing `auth.Service` or registering modules. The dialect is auto-detected from the DSN (SQLite for `:memory:` / `.db` files, PostgreSQL otherwise). This replaces the previous `kgorm.Repository.AutoMigrate` call.

## Near-term roadmap

1. Add tenant-aware authorization policies using Kayan's ReBAC relation tuples.
2. Add OAuth2 client credential flows for service-to-service federation.
3. Add service account provisioning via the kernel API, anchored in Kayan.

## Guardrails

- Do not add parallel auth code outside Kayan.
- Do not validate or mint JWTs outside Kayan abstractions.
- Keep gateway middleware as a thin adapter around Kayan session validation.
- All new auth endpoints, service accounts, app installation flows, and session behavior must be anchored in Kayan packages and documented here.
