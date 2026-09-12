# Architecture Overview

Sannad OS is structured as an open-source Go framework with a business kernel plus pluggable modules.

For what the framework is for and who uses it — the framework/SaaS split, the three
customization paths, and why embedded is a technical tier rather than a permission tier —
see [Product model](product-model.md).

## Runtime layers

- Gateway: public HTTP or gRPC ingress, app installation surface, webhook ingress, tenant resolution, authn and authz.
- Kernel: capability registry, transport dispatch, module lifecycle, bridge orchestration, and event routing.
- Embedded Modules: first-party and community Go modules running in-process through the public module SDK.
- External Apps: third-party integrations in any language using the gateway and shared contracts.

## Transport policy

- Embedded to Embedded: in-process handler dispatch.
- External to Internal: gateway-mediated gRPC or HTTP.
- Internal to External: bridge-owned client adapters.
- Any to Any async: NATS JetStream.

Callers use one call site regardless of tier; registration decides whether a
capability runs in-process, in a WASM sandbox, or over the network. See
[Plugin tiers and cross-tier communication](plugin-tiers.md).

## Public SDK policy

Community-authored Go modules should integrate through the public `pkg/modulekit` surface. Kernel implementation details remain under `internal/`, but module authoring must not require importing private runtime packages.

## Auth policy

Kayan is the mandatory IAM foundation for Sannad OS. The gateway and any future app-installation flow should build on Kayan packages for registration, login, sessions, OAuth2, OIDC, authorization, and storage adapters.
