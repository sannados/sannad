---
applyTo: "**/*.go"
description: "Use when editing Sannad OS Go code in the kernel, gateway, modules, bootstrap, or platform packages."
---

- Keep the architecture transport-neutral. Business handlers should not care whether the caller is internal or external.
- Use Kayan packages for authentication, sessions, OAuth2, OIDC, and authorization-related work.
- Keep tenant and caller identity available in `context.Context` for new capability handlers.
- Favor small packages with explicit responsibilities: bootstrap, kernel, module, bridge, auth.
- When adding cross-module behavior, model it as a capability contract first, then wire transport and routing.
- Avoid direct storage access across module boundaries.
- Keep the gateway thin. It should authenticate, authorize, resolve tenant context, and dispatch.
