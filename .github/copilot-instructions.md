# Copilot Instructions For Sannad OS

Sannad OS is an open-source Go framework with a kernel, embedded modules, external apps, and transport-neutral capability contracts.

When working in this repository:

- Use Kayan for all authentication, session, authorization, OAuth2, and OIDC concerns.
- Keep business modules decoupled from transport details.
- Prefer capability-oriented boundaries over package-to-package data reach-through.
- Preserve tenant context and caller identity in every new entry point.
- Keep embedded modules small and composable.
- Public module authoring should target `pkg/modulekit`, not private `internal/` runtime packages.
- Add protobuf contracts under `contracts/proto` for anything that may cross process boundaries.
- Do not bypass the gateway for external-facing traffic.
- Keep the current repo as a single Go module unless there is a clear operational reason to split it.

If you add auth endpoints, service accounts, app installation flows, scopes, or session behavior, anchor them in Kayan packages and document the decision in `docs/architecture/auth.md`.
