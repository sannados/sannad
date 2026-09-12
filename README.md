# Sannad OS

Sannad OS is an open-source Go framework for building modular business systems such as CRM, LMS, SIS, and ERP-style products.

It is built around a Go kernel, embedded modules, external apps, and transport-neutral capability contracts.

Applications import the framework directly; they do not fork this repository:

```go
import (
    sannad "github.com/sannados/sannad"
    "github.com/acme/community-erp"
)

app, err := sannad.New(sannad.ConfigFromEnv())
if err != nil { return err }
if err := app.RegisterModule(erp.New(app.Store())); err != nil { return err }
if err := app.Start(ctx); err != nil { return err }
```

The repository currently provides:

- Fiber as the main HTTP framework for the kernel, gateway, and example plugin surfaces
- a kernel runtime with a capability registry and in-process bus
- a public embedded-module SDK for Go module authors
- a public gateway with Kayan-backed auth entry points
- an isolated WASM module tier registered through the same capability lifecycle
- domain-neutral examples under `examples/`
- versioned protobuf contracts for external integrations
- repository instructions for AI agents and contributors

## Architecture shape

- Embedded to Embedded: direct Go capability handlers through the kernel registry and bus.
- Sandboxed to Internal: WASM capability handlers through the same registry, with tenant,
  memory, time, and syscall boundaries enforced by the host.
- External to Internal: HTTP or gRPC through the Sannad Gateway.
- Internal to External: bridge client owned by the kernel.
- Any to Any async: event bus, intended to be backed by NATS JetStream.
- Auth, sessions, and authorization primitives: Kayan.

## Extension model

- Official and community embedded modules may live in separate repositories and are
  compiled into the operator's application.
- Community embedded modules can live in separate GitHub repos and compile against `pkg/modulekit` for zero network-latency integration.
- Dynamically installed marketplace code runs sandboxed or externally; approved WASM
  bytes can already be registered at bootstrap, while the marketplace control plane is
  still future work.
- External apps remain the polyglot and lower-trust integration path through the gateway and event bus.

## Repository layout

```text
cmd/
  sannad-gateway/
  sannad-kernel/
contracts/
  proto/
docs/
  architecture/
  operations/
internal/
  app/
  kernel/
  platform/
examples/
  contracts/
  embedded/
  plugins/
pkg/
  config/
  kernel/
  modulekit/
deploy/
  docker-compose/
```

## Local start

```bash
go mod tidy
go run ./cmd/sannad-kernel
go run ./cmd/sannad-gateway
```

Gateway endpoints in the current skeleton:

- `POST /api/v1/auth/register`
- `POST /api/v1/auth/login`
- `GET /api/v1/me`
- `GET /api/v1/capabilities`
- `POST /api/v1/capabilities/{name}@{version}`

Example login payload:

```json
{
  "tenant": "demo",
  "email": "admin@example.com",
  "password": "StrongPass1234"
}
```

`tenant` is only read on the unauthenticated register and login endpoints, and only
when `SANNAD_TENANT_BASE_DOMAIN` is unset — with it configured, the tenant comes from
the request's subdomain instead. Every authenticated request takes its tenant from
the signed session token, never from a header or query parameter. See
[ADR 0005](docs/architecture/adr/0005-multi-tenancy.md).

## Kayan usage policy

All auth-related work in this repository must use Kayan packages instead of custom auth logic:

- `github.com/getkayan/kayan/core/flow`
- `github.com/getkayan/kayan/core/session`
- `github.com/getkayan/kayan/core/oauth2`
- `github.com/getkayan/kayan/core/oidc`
- `github.com/getkayan/kayan/kgorm`

The current skeleton wires password auth and JWT sessions through Kayan and leaves OAuth2 and OIDC integration points documented for the next phase.

## Embedded module authoring

Embedded Go modules should compile against the public `pkg/modulekit` surface instead of importing private kernel packages. This is the stable boundary intended for community-authored in-process modules.

Applications import the root `sannad` package. `pkg/kernel` remains only as a
deprecated compatibility façade for its former lightweight API.

