# ADR 0002: External API Surface

- Status: Accepted
- Date: 2026-08-09

## Context

External apps (ADR 0001, Tier 3) need to talk to the kernel, and the kernel needs to
talk back. These are two different problems with different constraints, and a single
protocol serves neither well.

1. **App calls kernel** — read contacts, create an invoice. Request/response,
   latency-tolerant, needs discoverability and good client ergonomics.
2. **Kernel calls app** — "an order was created", or "validate this before I save it".
   The first is a notification; the second is *synchronous extension*, where the
   kernel blocks on third-party availability.

Most systems design only (1), then discover that (2) is where ecosystem value
concentrates.

What competitors do:

- **Odoo**: XML-RPC (legacy, still primary) and JSON-RPC, partial REST in recent
  versions. No first-class webhook system. External integration is an afterthought;
  most marketplace "apps" are in fact in-process Python modules.
- **Shopify**: GraphQL Admin API (REST deprecated through 2024–25), webhooks for
  core-to-app notification, App Bridge for embedded UI, and WASM Functions for
  synchronous extension. Deliberately layered, one mechanism per problem.

Sannad already has protobuf contracts under `contracts/proto/` and a `buf` toolchain
wired up, plus NATS JetStream as the async backbone.

## Decision

### App to kernel: gRPC is the contract, REST is a generated projection

- **Protobuf contracts are the single source of truth.** They already exist and
  `buf` is already configured.
- **gRPC** is the native transport for serious integrators, generated SDKs, and
  polyglot clients.
- **REST/JSON is generated from the same protos** via `grpc-gateway`, with OpenAPI
  emitted alongside. This serves the majority who want `curl`, Postman, a Zapier
  connector, or a quick script — without a second hand-maintained surface.

One definition, two transports, no drift. Adding REST costs approximately one
codegen plugin rather than a parallel API to maintain.

**GraphQL is deliberately deferred.** It offers the best client ergonomics for
complex reads, but it is a large surface to secure well (per-field authorization,
query cost limiting, N+1 control). If demand appears, it can be added later as
another gateway projection over the same contracts, not as a competing source of
truth.

### Kernel to app, asynchronous: events and webhooks

- **NATS JetStream** remains the internal event backbone.
- **Webhooks** are one consumer of that backbone, not a separate mechanism.
- Webhook delivery must ship with: HMAC request signing, retries with exponential
  backoff, idempotency keys, a dead-letter path, and replay of missed events.
  These are table stakes; Stripe and Shopify both set this expectation.

### Kernel to app, synchronous: extension callbacks

This is the hard case, covered in detail by ADR 0003. In summary:

- **Phase 1**: synchronous HTTP/gRPC callback with a strict timeout, and an
  explicitly declared failure policy per hook (fail-open or fail-closed).
- **Phase 2**: WASM execution in-process for latency-critical hooks, removing both
  the network hop and the third-party availability risk.

### Tenant and auth context

All external calls resolve tenant and caller identity at the gateway, which injects
`modulekit.CallerInfo` before dispatch. External callers never assert their own
tenant. See ADR 0005.

## Consequences

- Protobuf becomes load-bearing. Contract review and compatibility discipline
  (`buf breaking`) move onto the critical path for every release.
- Generated SDKs in Go, TypeScript, Python, and others come nearly free from the
  proto definitions, which is a direct DX win over Odoo's XML-RPC.
- REST consumers get a slightly less idiomatic API than a hand-designed REST surface
  would provide. Accepted: consistency and zero drift are worth more than REST purism.
- Webhook infrastructure (signing, retry, replay, dead-letter) is real work that must
  be scheduled, not assumed.
- Synchronous extension over the network introduces a third-party availability
  dependency in the request path. Every such hook must declare its failure policy, and
  this is a primary motivation for the WASM tier.

## Alternatives considered

- **REST-first, gRPC optional.** Lower barrier for casual integrators and easier to
  debug by hand. Rejected because protobuf is already the contract layer, and
  hand-maintaining REST alongside it guarantees eventual drift.
- **GraphQL primary (Shopify model).** Best read ergonomics and strong introspection.
  Rejected for now on surface area and security cost; explicitly revisitable as an
  additional projection.
- **XML-RPC or JSON-RPC (Odoo model).** Broad legacy tooling support. Rejected: no
  schema evolution story, no streaming, poor generated-client quality.
