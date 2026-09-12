# Implementation Roadmap

The order in which Sannad OS is built, and why that order is not arbitrary.

The architecture decisions themselves live in [`adr/`](adr/README.md). This document
records only sequencing: what is built, in what order, and what blocks what.

## Strategy

This project is a kernel, not a product. What ships from here is the extension
machinery — tenancy, storage boundary, hooks, generation — and nothing that presumes a
particular industry, country, or business domain.

That constraint has a cost: extension machinery designed against no real load is
designed wrong, and the wrongness only surfaces once someone builds on it. The answer
is a **conformance suite**, not a product. Each kernel capability ships with an
adversarial test module that exercises the hardest shape the capability claims to
support. The test module lives in the test tree, is thrown away if the design changes,
and never becomes a deliverable.

Real domain modules — invoicing, any country's tax localization, industry verticals —
are downstream consumers built in their own repositories against `pkg/modulekit`. They
are out of scope here.

### 0. Domain code removed from the kernel — **done**

The repository shipped a CRM module as production code: `internal/modules/crm`, with
a bespoke gRPC bridge in `internal/kernel/bridge` that made a **kernel** package
import a business domain. Same category as the invoicing and localisation steps cut
from this plan, but structural rather than prospective.

What moved:

- `internal/modules/crm` to `examples/embedded/crm`. `internal/modules` no longer
  exists. The module is example and test material, not a deliverable.
- The gRPC bridge to `examples/embedded/crm/grpc.go`, and the gateway's
  `/api/v1/crm/contacts` handler to `examples/embedded/crm/http.go`.
  `internal/kernel/bridge` no longer exists.

What replaced the coupling — three registration seams, all called from `cmd/`:

| Seam | Replaces |
|---|---|
| `App.RegisterScopedModels` | bootstrap naming `&crm.Contact{}` |
| `App.RegisterGRPCService` | bootstrap naming `crmV1.RegisterContactReaderServer` |
| `App.RegisterAuthenticatedRoutes` | bootstrap's hard-coded CRM route and handler |

Bootstrap now names no module, model, route, or service. Coverage was preserved:
`TestGateway_CRMContacts_CannotReachAnotherTenant` still proves cross-tenant reads
fail through the real HTTP stack, now with the module wired the way an entrypoint
wires one.

**Bug this surfaced:** the servers were built in `New`, so anything registered
afterwards was silently dropped — both new seams were dead on arrival, and the gRPC
one would not have failed any test. Construction moved to `Start`. That in turn made
`Shutdown` nil-panic on a New-but-never-Started app, which is the path taken when
wiring fails and the database pool still needs releasing. Guarded, and covered by
`TestShutdownWithoutStart`, verified to panic when the guard is removed.

**Left in place:** migration `00002_crm_contacts.sql` still creates `contacts` in the
kernel migration set. Renumbering an applied goose migration breaks every existing
database. It moves once modules own their migrations; noted in the file.

## Order

### 1. Tenant isolation fix — **done**

Everything downstream needed a trustworthy tenant. Until the tenant was derived from
an authenticated session rather than client input, no module built on top of it was
safe, and every module written in the meantime would have encoded the wrong
assumption.

Decision and rationale: [ADR 0005](adr/0005-multi-tenancy.md).

What shipped:

1. `Account` gained tenant membership, with email uniqueness scoped per tenant
   (migration `00003_account_tenancy.sql`).
2. Sessions carry a signed `tenant_id` claim, via `auth.TenantJWTStrategy`.
3. Subdomain resolution for unauthenticated requests; header and query resolvers are
   gone. Authenticated requests take the tenant from the token only.
4. `tenancy.RegisterIsolation(db)` at bootstrap — GORM callbacks on Query, Row,
   Update, Delete, and Create.
5. `crm.Contact` implements `tenant.TenantAware`.
6. The hand-rolled comparison in the CRM module and the `?tenant=` read in the
   gateway are deleted.

Adversarial testing during implementation found three live bypasses, all now closed
and covered by `internal/platform/tenancy/bypass_test.go`: `Table("name")` queries
and raw SQL both ran unscoped, and a query joining into a scoped table from an
unscoped one returned every tenant's rows.

Verified by `internal/platform/tenancy/isolation_test.go`,
`TestGateway_CRMContacts_CannotReachAnotherTenant`, and the negative control
`TestIsolationIsWhatEnforcesScoping`.

**Finding that changed the plan:** Kayan `core@v0.1.0` supplies tenant resolution and
the `TenantAware` interface, but **no storage enforcement** — there is no
`kayan-gorm` module, no `gormstore.RegisterTenantIsolation`, no `tenant.Scoper`, and
no `tenant.WithSystemContext`. Enforcement is therefore Sannad's, in
`internal/platform/tenancy`, written against Kayan's interface. Two further gaps
found during implementation: Kayan's `session.JWTClaims` is a closed struct and
cannot carry a tenant claim, and its password strategy looks accounts up by
identifier alone, which is ambiguous once one address exists in two tenants. Both are
handled locally; see ADR 0005.

### 2. Storage boundary — **done**

No module can be written before it knows what consistency it is guaranteed.

Decision: [ADR 0006](adr/0006-storage-boundary-and-transactions.md).

What shipped:

- `modulekit.Store` — the engine-neutral storage interface modules receive, with a
  declarative `Condition` set rather than raw SQL strings, so the contract does not
  bind every module to a SQL engine.
- `internal/platform/storage.GormStore` — the one adapter (ADR 0006 decision 5),
  built in bootstrap from the connection that already carries the isolation
  callbacks, so module queries are tenant-scoped by construction.
- `crm.Module` takes `modulekit.Store`; `app.Database()` is replaced by
  `app.Store()`, so an entrypoint cannot hand an engine type to a module.
- `Transact` gives a module ACID semantics over its own data and nothing wider,
  which is the exchange ADR 0006 decision 2 makes.
- Unconditional `Update` and `Delete` are refused (`ErrUnconditional`), and column
  names are validated as bare identifiers — the one place a caller string reaches
  SQL, since a column cannot be a bind parameter.

`pkg/modulekit/boundary_test.go` enforces the layering rule by parsing the package's
imports and failing on any engine or driver import. Verified against an injected
violation, so it fails when the rule is broken rather than only passing when it
holds.

**Tier portability** (ADR 0001). The ADR stakes the design on one capability living
at any tier, and names its cost: contracts must be serializable. Nothing enforced
that, because tier 1 hands payloads over by pointer and never encodes them — a
contract holding a func or unexported state passes every in-process test and fails
only when the module moves to WASM or the gateway, the one moment the design
promised nothing would change. `modulekit.AssertPortable` closes it, and the
conformance suite runs it against real hook payloads.

**SDK helpers** (ADR 0006 decision 4). Without these, transactional independence is a
burden on module authors rather than a design. All three are done.

*Idempotency* — `modulekit.Idempotent` runs a handler exactly once per
(tenant, consumer, event), writing its marker in the same transaction as the
handler's own writes. Separate commits would either lose the work or repeat it,
depending on which went first. The claim is an insert against a composite primary
key, not a check-then-act: concurrent deliveries race, and the loser violates the
key. Reverting it to check-then-act ran the handler **6 times** under 8 concurrent
deliveries while passing every sequential test.

*Saga coordinator* — `modulekit.RunSaga` sequences steps across modules and undoes
the committed ones in reverse when a later step fails. Reverse order is not
cosmetic: later steps may depend on earlier ones, so undoing forward could remove a
prerequisite while a dependent effect is still live.

The hard part is not the happy path but that the coordinator itself can crash
between any two steps. Every transition is durable *before* the step it describes
runs: recording afterwards would make "crashed before the step" and "crashed after
it committed" indistinguishable. Recording first means the worst case is a step
whose outcome is unknown — which recovery can act on, because steps are required to
be idempotent. "Unrecorded" is not actionable at all.

Three outcomes are kept distinct, because callers must treat them differently:
completed, `ErrSagaCompensated` (the operation cleanly did not happen — the expected
failure, safe to retry), and `ErrSagaFailed` (compensation itself broke, committed
work is still live, needs a human, and must never be blindly retried). Compensation
continues past a failed undo rather than stopping, so an operator learns which steps
still hold effects instead of only the first.

`RecoverSaga` resumes a saga the previous process left in flight, refusing to run if
the recorded step names no longer match the definition — a deploy that reordered
steps between the failure and the recovery would otherwise run one step's undo
against another step's effect.

*Projections* — `modulekit.Project` discards events older than the state already
stored, compared by source sequence rather than timestamp, since two events in one
clock tick are indistinguishable by time and clocks on separate machines disagree.
`Rebuild` marks a projection while it rebuilds, so a reader can tell partial data
from complete; a failed rebuild keeps the mark, because advertising incomplete data
as complete is worse than an obvious outage.

**A boundary violation this work exposed.** The first version of both models carried
`gorm:"..."` struct tags. `boundary_test.go` checks imports, and a struct tag names an
engine without importing one — so the SDK was coupled to GORM exactly as an import
would couple it, and the test said nothing. Tags removed, schema moved to migration
`00004_modulekit_sdk_tables.sql`, and `TestSDKCarriesNoEngineStructTags` now closes
the gap. Verified against a reinstated tag.

That change had a consequence worth recording: without tags, `AutoMigrate` builds
`modulekit_processed_events` with **no primary key**, so the duplicate-key claim never
conflicts and idempotency silently does nothing. The storage tests now run the real
migration. An SDK model and its migration have to agree, and only the migration is
enforceable.

**Carried:** a projection model must key on (tenant, projection key). Two tenants
legitimately project the same source ID, and tenant isolation scopes queries but
cannot widen a primary key — so a model that gets this wrong collides across tenants
and nothing reports it. Documented on `Projected`; a startup audit would be better.

### 3. Hooks and typed handlers — **done**

Prerequisite for extension-without-modification, which is the kernel's core claim.
Two coupled changes:

- The hook registry from [ADR 0003](adr/0003-extension-points-and-hooks.md): hook
  kinds (observer, validator, transformer), deterministic priority ordering, declared
  `fail_open` / `fail_closed` policy, mandatory timeouts.
- Typed handlers (`modulekit.HandleTyped`). The current
  `Handler = func(ctx, any) (any, error)` is in-process only; anything written
  against it cannot be served from the WASM or external tiers without a rewrite.

Hook debuggability — showing which subscriber vetoed a flow and why — is built
alongside the registry, not after it.

What shipped:

- `pkg/modulekit/hook.go` — the SDK contract: `HookPoint`, `HookRef`, the three
  kinds, `FailurePolicy`, `Subscription`, `Rejection`. A hook point declares its
  kind, policy, and timeout or is refused at registration.
- `internal/kernel/hooks` — the registry and the three dispatch paths (`Notify`,
  `Validate`, `Transform`). Dispatching a hook point as the wrong kind is refused,
  since the kind is what decides whether a return value can veto.
- `pkg/modulekit/typed.go` — `HandleTyped`, `SubscribeTyped`, `TransformTyped`.
  The type assertion every untyped handler used to open with now happens once, in
  the wrapper, and a mismatch is reported as `ErrTypeMismatch` rather than a panic.
- Registry integration: hook points declared in a `Descriptor` are offered before
  `Install`, so a module can subscribe to its own points and a later module finds
  them already declared.

Guarantees enforced, each with a negative control that fails when the mechanism is
removed:

| Guarantee | Verified by removing |
|---|---|
| Ordering is deterministic under registration reordering | the module-ID tie-break |
| A validator collects every veto, not just the first | early exit on first rejection |
| Rejections are attributed by the dispatcher, not self-reported | the ID stamp |
| A transformer chains values through subscribers | passing the original each time |
| `fail_closed` aborts, `fail_open` skips | the policy branch |
| A subscriber panic cannot crash the host | the recover |
| Subscriber identity comes from the host, not the subscription | the ID override |

**Two defects found by adversarial probing, neither by the tests above.** A caller
cancellation was reported as a hook timeout — both arrive as `ctx.Done()`, and
conflating them sends whoever debugs the failure looking for a slow subscriber that
was never slow. And `invoke` and `applyPolicy` each wrapped the error, so a failure
read as two problems. Both fixed, both now regression tests in
`internal/kernel/hooks/guarantees_test.go`.

**Carried from this step:** a subscriber that ignores its context leaks a goroutine.
Go cannot preempt one, so the timeout bounds the caller's wait rather than the
subscriber's life; the abandoned goroutine's return value is discarded. WASM (ADR
0003 phase 2) is what actually removes this, and it is the honest argument for that
tier. `-race` was not run: this machine has no C compiler, so race-freedom is
argued from the locking discipline and exercised by a concurrency test, not verified.

### 4. Hook conformance suite — **done**

Proof that the step 3 design survives contact with a demanding extension, without
building a product to prove it.

A fixture module in the test tree declares hook points covering every shape the
registry claims to support, and a second fixture extends the first **without
modifying it**:

| Shape exercised | Hook kind | What it proves |
|---|---|---|
| Mutate a computed collection | transformer | A subscriber can rewrite derived values, not just observe them |
| Veto a state transition | validator | `fail_closed` refusal propagates with an attributable reason |
| Replace a generated identifier | transformer | A subscriber can own a value the host would otherwise supply |
| React after commit | observer | Post-commit side effects run without widening the transaction |
| Ordering between subscribers | any | Declared priority is deterministic under registration reordering |
| Subscriber timeout and panic | any | One bad subscriber cannot hang or crash the flow |

Several of these are already covered at the unit level by step 3's suite. What step 4
adds is the *cross-module* form: a second module attaching to a first through the
registry, with no shared code and no edit to the module being extended.

The bar is the same one adversarial testing set in steps 1 and 2: a negative control
that fails when the mechanism is removed, not only a test that passes when it is
present. If the second fixture cannot attach without editing the first, the hook design
is wrong and step 3 reopens.

What shipped, in `internal/kernel/conformance` (test tree only — these fixtures are
throwaway and must never become a deliverable):

- `host_test.go` — a module owning a four-stage flow: compute, validate, number,
  commit, notify. Domain-neutral on purpose. A fixture named after an industry
  invites the design to bend toward it.
- `extension_test.go` — a module attaching to all four points, plus fixtures for
  ordering between two independent extensions and for hostile subscribers that
  panic, hang, or fail.
- 15 tests covering every shape in the table above.

**The design held.** The extension mutates derived values, vetoes a transition with
an attributed reason, takes over the document number, and observes the commit —
without the host naming it or an edit to `host_test.go`. Two independent extensions
compose in priority order regardless of registration order. A panicking subscriber
aborts a `fail_closed` stage with the culprit named and commits nothing; a failing
observer cannot undo a commit that already happened.

One ordering property was worth asserting directly: the validator sees the record as
the transformers left it, not as it arrived. `TestVetoSeesTransformedValue` uses a
limit that only the transformed total exceeds, so it fails if the stages are ever
reordered.

**The structural claim is enforced, not asserted.** The fixtures share a package, so
nothing would stop a future edit from calling a host method directly and leaving every
behavioural test green while the claim quietly became false. `isolation_test.go`
parses both fixtures and fails if the extension names a host type or selects a host
member, or if the host names anything the extension owns. Verified in both directions
by injecting a violation. The extension may name the shared *contract* — hook refs and
their value types — because that is what a third-party module would import from a
published package; sharing a contract is the mechanism, holding a reference is not.

Negative controls, each removing one mechanism and counting what fails: host ignores
transformer output (5 tests fail), vetoes discarded (2), subscriptions dropped (9).

### 5. Generation — **done**

`cmd/sannad-gen` scaffolds a module against the public SDK. Its purpose is not to
save typing: the rules a correct module follows are not discoverable from the SDK's
type signatures. A module compiles perfectly while importing kernel internals, while
carrying a tenant ID in its request, and while holding a contract that cannot cross a
tier boundary — each forbidden by an ADR, each producing working code that fails
later, in a different tier or a different tenant's data.

So the generator emits the tests that keep the module correct after the author starts
editing. Four rules, each verified by breaking it in a generated module: an unportable
contract, a tenant field in the request, a model that stops being tenant-aware, and an
`internal/` import. The first three fail the generated tests. The fourth is refused by
the Go compiler when the module lives in its own repository — the generated test is
the only guard if it is ever vendored into the kernel tree, which is exactly when the
mistake is easy to commit unnoticed.

**Generating a module exposed a real gap in the SDK.** `HookRegistrar` could offer and
subscribe but not *dispatch*: `Notify`, `Validate` and `Transform` lived only on the
internal hook registry. A module could therefore declare a hook point and have no
public way to fire it — the offer compiling, subscribers attaching, and the hook
simply never firing. The conformance host fixture had not caught this because it
reaches into `internal/kernel/hooks` directly, which no community module can do.
`modulekit.HookDispatcher` closes it, with the kernel registry forwarding and a
compile-time assertion on both sides.

The generator is deliberately not a framework. It writes a starting point and refuses
to overwrite, because the second run is exactly when an author wants to compare rather
than lose work.

## Carried flags

- ~~A model not marked `tenant.TenantAware` has no isolation and nothing reports
  that.~~ **Closed.** `tenancy.AuditScopedModels` runs at startup, after every module
  has installed, and reports each table carrying a tenant column that nothing
  registered as scoped. It asks the database rather than the code, because the set of
  scoped models is assembled at runtime from modules the kernel does not know about
  until they install.

  Findings warn rather than refuse to boot: a table with a tenant column may be
  legitimately global, and locking an operator out over a module they did not write
  would make running the audit the risky move. Deliberate exceptions are declared —
  `accounts` is exempt because login looks an account up by email before any tenant
  context exists — so an exemption is a decision somebody recorded rather than a
  warning everybody learned to ignore.

  **It found five unscoped tables on its first run, four of them SDK tables written
  in this project**: `modulekit_processed_events`, `modulekit_projection_state`,
  `modulekit_sagas`, and `modulekit_saga_steps`. Each implements the tenant-aware
  interface, so queries passing the model were already scoped; none was registered by
  name, so a `Table("modulekit_sagas")` or raw-SQL query against them would have run
  across every tenant. That is precisely the path the scoped-table registry exists to
  close, and precisely the kind of omission no test was going to notice.
- ~~Session revocation before token expiry needs a server-side session store; JWT
  sessions are stateless and `Delete` is a no-op today.~~ **Closed**, without the
  session store. A store would have restored revocation by removing the reason JWTs
  were chosen: every request becomes a lookup, and the signature stops being the
  authority.

  `auth.RevocationStore` records the inverse — not which sessions are valid, but the
  few killed early. Entries are dropped once the token they revoke would have expired
  anyway, and the common path (a token nobody revoked) is answered from memory.
  Revocation is wired in `NewService` rather than left opt-in, because a deployment
  that forgot to enable it would have a logout endpoint that silently did nothing,
  which is the state this replaced.

  Two properties are worth stating because they were bugs first. `Refresh` reuses the
  session ID, so without a check there it minted a fresh token pair from a
  logged-out session — **the client could undo its own logout**; the test caught it
  on the first run. And a database error fails the request rather than answering
  "not revoked": treating an unreachable database as permission would let an outage
  silently restore every revoked session at once.

  The cost is stated on `RevocationTTL` rather than hidden: a revocation reaches
  other processes within that window, so this is eventual, not immediate. A
  deployment needing faster containment lowers it and pays the read.
- ~~Tenant existence is not validated at the edge.~~ **Closed.** There was no tenant
  record at all: a tenant was any string that reached the edge. Isolation held — that
  data was correctly scoped — but scoped to an identifier nobody had provisioned,
  billed, or could find in a list. A tenant also could not be turned off, because
  suspending one meant deleting its users.

  `tenancy.Directory` is the missing record, and deliberately small: not a customer
  table, a billing row, or a settings store, but an answer to one question asked on
  every unauthenticated request — may this tenant be used right now.
  `ValidatingResolver` wraps whatever resolution strategy a deployment uses, so *how*
  a tenant is identified stays independent of *whether* the result is usable.
  Registration, login, and the OAuth callback all check: a suspension that only
  blocked password login would be no suspension.

  Self-service signup needs registration to create tenants, so `TenantSelfProvision`
  exists — defaulting to **off**, because silently creating a tenant is the behaviour
  being removed. When on, it creates only *unknown* tenants; a suspended one is an
  operator decision and is never overridden by a registration.

  Migration 00007 backfills the directory from `accounts`. Without it, every existing
  tenant becomes unknown at the next login and every existing user is locked out the
  moment the migration lands.
- Cross-module SQL joins are impossible by design (ADR 0004). Every cross-module
  report needs a hand-built projection. This is the largest cost the architecture
  imposes on itself and it should be budgeted for, not discovered.

## Contract-call defects — **closed 2026-08-20**

Both surfaced in a design conversation, not from a failing test, because nothing in the
tree exercised the path. **No module had ever called another module's capability here** —
`examples/embedded/crm` is its own only caller, and the conformance suite proves *hooks*
work with no shared code, which is a different mechanism. The central claim of the
capability system was asserted in ADRs and never executed.

1. **No `CallTyped`.** ~~`Bus.Call` returns `any`, so every caller casts at runtime.~~
   `modulekit.CallTyped[Req, Resp]` (`pkg/modulekit/typed.go`) closes it. The registry
   stays untyped — it holds capabilities whose types it cannot know — but that is now an
   implementation detail of the registry rather than a tax on every call site. It takes a
   `modulekit.Caller` interface rather than the kernel bus, so a module still names no
   kernel package and a test double is one struct.

2. **Contract types lived in the provider's package.** ~~A consumer imported CRM to call
   CRM without importing CRM.~~ `examples/contracts/directory` demonstrates the fix: the
   request type, response type, and the reference live in a package with no
   implementation, which both sides import.

`examples/embedded/people` (provider) and `examples/embedded/greeter` (consumer)
demonstrate the pattern, and `crossmodule_test.go` is the first test in this repository
of one module calling another. It covers the call, provider replacement by a second
module registering the same reference, a missing provider failing at wiring rather than
on first request, and an AST check that neither module imports the other — verified by
adding the import and watching it fail.

`examples/embedded/crm` still holds its contract types in the provider package. It is
left alone deliberately; the new pair is the reference for how to do it.

## Naming: "capability" — reviewed and kept

Reviewed 2026-08-20 and deliberately kept. A capability here is a named contract a module
fulfils, not an object-capability security token: registering one grants no access, which
is why the external bridge needs a separate `Exposed` list. The collision with ocap
terminology was weighed against a rename to `Contract` — which would overload a word
already used for payload types — and the documented distinction won over churn across six
ADRs, the SDK, the conformance suite, and a public endpoint. See
[Product model](product-model.md).

## External capability bridge — **done 2026-08-20**

ADR 0001 stakes the design on one claim: "the same capability lives at any tier… and
callers cannot tell the difference." That was **false for tier 3** until now. A module
got an external API only if someone hand-wrote a `.proto` and a gRPC service for it,
which is why the only external surface in the tree belonged to the example module. Tier 3
is where the marketplace lives, so the gap sat in the tier that matters most.

`POST /api/v1/capabilities/{name}@{version}` dispatches any exposed capability by
reference. One endpoint, every module, no proto and no handler per module.

**The blocker, found while planning rather than while coding.** `coerce`
(`pkg/modulekit/typed.go`) was a type assertion only, so a JSON body — which decodes to
`map[string]any` — could never reach a handler registered with `HandleTyped`. That is why
a generic bridge did not already exist. `modulekit.RawPayload` is an explicit marker type
a transport wraps a body in; `coerce` decodes it *after* both assertions, so the
in-process path still hands the typed value over directly and pays nothing. It is a
marker rather than a sniff for `map[string]any` because "a module passed a map" and "a
transport passed an encoded body" cannot be told apart after the fact, and a dispatch
boundary is exactly where a silent reinterpretation becomes a security question.

**Registration is not publication.** `Descriptor.Exposed` is a separate, deliberate
declaration; empty means externally invisible, which is the safe default for a module
whose author never considered the question. The registry refuses a descriptor exposing a
capability it does not provide. An unexposed capability and a nonexistent one return an
identical 404 with an identical body, so the endpoint is not an oracle for enumerating
what a deployment runs internally.

Tenant and caller come from the signed token exactly as on every other authenticated
route, so the bridge adds no new tenant path — a bridge that accepted a tenant from the
request body would be a cross-tenant read with extra steps.

Eight tests through the real gateway, since the risky parts are the seams rather than the
dispatch logic: the request context must survive the Fiber-to-`net/http` adaptor carrying
what the auth middleware put there, or every call arrives unauthenticated. Three negative
controls confirmed — removing the exposure check let an unexposed capability return
**200**; removing `RawPayload` decoding turned a working call into **400**; dropping the
version requirement let an unversioned reference through, which is how an integration
silently starts talking to `v2` after an upgrade.

**gRPC parity shipped alongside it.** `sannad.kernel.v1.Capabilities` is the kernel's own
and only proto — every other one in the tree belongs to a module. The payload is opaque
JSON bytes rather than a typed message, because the kernel cannot know the shape of a
contract defined by a module it has never seen; a typed field would mean compiling every
module's schema into the kernel, which is the per-module hand-writing this removes. The
cost is stated rather than hidden: gRPC's schema checking stops at that boundary, and a
module wanting generated clients with per-field validation still publishes its own proto.

Both transports share one dispatch function and one exposure check, so a capability
reachable over one and not the other is not expressible. The kernel registers the service
itself rather than leaving it to an entrypoint: a deployment that forgot to wire it would
silently have no external gRPC API, and the modules affected would be exactly the ones
whose authors never wrote gRPC code.

Six more tests run against a real gRPC server over an in-memory listener, so the
server-wide auth interceptor is exercised rather than assumed — if it were ever scoped
per-service, this surface would become an unauthenticated path to every exposed
capability.

**A negative control found a missing test rather than a broken mechanism.** Returning the
raw error instead of a generic message compiled and passed everything, because nothing
covered error-detail leakage on either transport. The fixture now fails with an error
carrying a query and another tenant's identifier, and both transports are checked; with
the redaction removed, both report the full `SELECT ... WHERE tenant_id = 'other-tenant'`
reaching the caller.

Incidental: `go get` for the test-only `bufconn` package upgraded gRPC 1.81.0 to 1.83.1
and briefly broke the module graph. Repaired with `go mod tidy`; all 12 packages green on
the new version.

## Event consumption and webhooks — **done 2026-08-20**

Nothing consumed the event bus before this. `Publisher` had exactly `Publish` and
`Close`, so an event went into JetStream and no part of the system could react to it —
which meant webhooks were never blocked on webhook code, they were blocked on there
being a consumer at all.

**Three real bugs in the publish path, fixed first.**

- **No stream was ever declared.** JetStream only retains messages on subjects a stream
  binds, and nothing declared one, so every publish failed on ack unless an operator had
  configured the server by hand. `EnsureStream` now runs at connect: file storage, so an
  unconsumed event survives a broker restart, and `LimitsPolicy` rather than
  `WorkQueue`, because several consumers legitimately read the same event — a
  projection, a webhook dispatcher, an audit log — and WorkQueue would give it to
  exactly one. An existing stream is widened if it binds less, never otherwise
  overwritten: an operator's retention settings are theirs.
- **Connection failure degraded silently.** A misconfigured broker logged a warning and
  substituted `NoopPublisher`, so every event vanished while the system reported healthy.
  A deployment that configured NATS asked for events; startup now fails instead.
- **JSON vs protobuf** is resolved in favour of JSON, matching the bridge payloads.

**The tenant lives in the subject**, and that is a security property rather than a naming
convention. The layout is `sannad.<tenant>.<event>`, so a consumer bound to
`sannad.acme.>` is never handed another tenant's events whatever its handler does. A
consumer filtering on a payload field has already received the data before deciding to
ignore it. Tenants and event names are refused if they contain a delimiter or wildcard
rather than escaped — a tenant identifier has no business containing either, and `*`
accepted as a tenant subscribes to everything. A cross-tenant filter exists for
infrastructure that needs one, named `AllTenantsSubject` so granting it is visible in
review.

Consumers are durable by construction: an ephemeral consumer silently loses everything
published while it was down, which is indistinguishable from working until something is
missing. An empty durable name is refused. Handlers must be idempotent — delivery is
at-least-once, and `Delivery.Deliveries` makes a redelivery visible.

Three negative controls confirmed: removing the delimiter guard let `*` through as a
tenant; widening the per-tenant filter to `sannad.>` was caught; falling back to a shared
subject with no tenant was caught. **One initially passed for the wrong reason** — the
edit that was supposed to remove the guard silently matched nothing, so the control
tested unmodified code. Verified by checking what was actually removed rather than
trusting the empty output.

**Webhook delivery — done 2026-08-20.** All five of ADR 0002's table stakes:
`internal/platform/webhooks` plus migration 00008.

*Signing.* HMAC-SHA256 over `timestamp.payload`, with the timestamp **inside** the signed
material rather than merely alongside it. Signing the body alone would let anyone who
captured one delivery replay it forever, and the receiver could not tell — the signature
would still verify. Header names follow Stripe's shape, because an integrator who has
implemented one webhook receiver should not have to learn a new scheme.

*Retry and dead-letter.* Exponential backoff, capped, with the next attempt time **stored
rather than held in memory** — a restart that retried everything at once is how a
recovering system finishes off a receiver that was only briefly down. A 4xx is not
retried: a 410 or a 401 fails identically forever, and eight attempts waste a day before
reaching the same conclusion while delaying the dead-letter that tells an operator to
look. 408 and 429 are the exceptions, since both mean "not now". Dead deliveries are kept
rather than discarded, because "which events did this integration miss" is the first
question after an outage and a deleted row cannot answer it.

*Idempotency.* The **source event's** ID travels as the idempotency key, not the delivery
row's — a receiver deduplicating on it must treat two attempts of one event as one event.
Delivery is at-least-once by construction: a response lost in transit is
indistinguishable from a request never received.

**A negative control that no functional test could catch.** Replacing `hmac.Equal` with
`!=` passes every behavioural test — signatures still match when they should and differ
when they should. What changes is that a byte-by-byte compare returns faster the earlier
it finds a mismatch, leaking how much of a forged signature was correct, enough to derive
the rest one byte at a time without ever knowing the secret. Enforced structurally
instead, by an AST test that requires `hmac.Equal` and refuses direct signature
comparison — verified by making the substitution and watching it name the line.

Three other controls confirmed: unsigning the timestamp let a rewritten one verify;
treating every status as retryable let 410, 401 and 404 through; removing the attempt cap
retried past the limit and never dead-lettered.

**The runner wires it up.** `Runner.Enqueue` turns a bus event into one delivery per
matching endpoint; `DrainOnce` attempts everything due.

Queue-then-send, not send-on-arrival. Sending inline would tie an event's
acknowledgement to a third party's availability — a receiver down for an hour would hold
the bus consumer for an hour, and every event behind it. Writing the row is fast and
local; the network happens on somebody else's schedule.

Event identity comes from the **JetStream stream sequence**, which is unique and
monotonic, so a redelivered message carries the same identity and the unique index on
`(endpoint_id, event_id)` turns it into a no-op rather than a second webhook. A deleted
endpoint dead-letters rather than retrying forever against a destination that no longer
exists. Retries are jittered, because without it every delivery queued during an outage
retries in the same instant and the recovering receiver is knocked over by the recovery.

Ten integration tests through the shipped adapter and the real migration, since the
properties are storage properties: the unique index is what makes redelivery safe, and
tenant isolation is what stops one tenant's event reaching another's endpoint. AutoMigrate
would build neither.

**A negative control passed for a reason worth keeping.** Removing the attempt reset from
`Replay` broke nothing, because the replayed delivery succeeded on its first attempt and
never exercised the counter. The reset only matters when the receiver is still flaky:
carrying the old count forward puts the delivery at its limit, so it dead-letters again on
the first failure and the operator gets one attempt instead of the retry budget they think
they asked for. Test added for that case; with the reset removed it now reports 8 attempts
carried forward.

Two others confirmed: dropping the unique index let a redelivered event queue twice;
ignoring the due time attempted a delivery scheduled an hour out.

**Startup wiring and endpoint management — done 2026-09-03.** The two things left above.

`Runner.StartConsuming` durably subscribes to the whole event stream
(`events.StreamSubjects`, every tenant) under one fixed consumer name
(`ConsumerDurableName`), not one subscription per event — an endpoint's own `Events` field
is what narrows delivery (`Endpoint.Wants`), so subscribing per event would mean a new
event type never reaches an endpoint that asked for "everything" until this wiring learned
about it. `RunDrainLoop` ticks `DrainOnce` on an interval; both are started in
`bootstrap.App.Start` only when the configured event publisher actually implements
`events.Consumer` — `events.NoopPublisher` (no broker configured) does not, so a deployment
with no NATS URL runs with webhooks registrable but never fired, logged rather than failed
closed, the same choice already made for a missing broker generally. Both stop at
`Shutdown`, waited on via a `chan struct{}` closed when the drain goroutine returns, before
the database and broker connections they depend on are closed.

The one design point worth recording: the drain loop runs under
`modulekit.WithSystemContext`, deliberately, because it is the one place a single pass
drains every tenant's due deliveries at once. Every row it touches was already written
under its owning tenant's own context by `Enqueue` — the system context does not decide
which tenant a delivery belongs to, only which of the already-scoped rows are due. The
event-consumer handler builds that owning-tenant context explicitly from
`delivery.TenantID`, since the bus consumer it runs under sees every tenant's events on one
connection and there is nothing else to scope a write by.

Endpoint management (`internal/platform/webhooks/endpoints.go`): create, list, get,
partial update, secret rotation, delete, and per-endpoint delivery history, all through
`modulekit.Store` so tenant isolation is the same mechanism as everywhere else — a caller
reading another tenant's endpoint by ID gets `ErrNotFound`, indistinguishable from a
nonexistent ID. A created endpoint's secret is generated host-side (`crypto/rand`, never
caller-supplied — a caller-chosen secret could be short, reused, or simply not random) and
returned exactly once, on creation; every other read redacts it, and list responses redact
it unconditionally since a list is the shape most likely to end up logged or screen-shared.
`modulekit.Store` has no ORDER BY/LIMIT (the query surface is deliberately small), so
delivery history ordering and truncation happen in Go over what is, in practice, one
endpoint's bounded delivery set.

Exposed over the gateway at `/api/v1/webhooks/endpoints` (CRUD, rotate-secret, replay,
deliveries) and `/api/v1/webhooks/deliveries/:id/replay`, behind the same `requireAuth` and
signed-token tenant every other authenticated route uses — there is no tenant parameter for
a caller to forge here either.

**Negative controls:** the partial-update pointer check for `Active` disabled →
`TestUpdateEndpointLeavesUnsetFieldsUnchanged` fails ("Active was not applied"); the
event-consumer handler's tenant substituted for a fixed wrong one → the delivery vanishes
from the real tenant's view (`TestConsumedEventIsQueuedUnderItsOwnTenant` fails with "got 0
deliveries, want 1"). Both restored after.

## Modules own their data — **done 2026-08-20**

A module may use the kernel's storage or bring its own. Both supported, neither forced.

**Most of it already worked, which is what made it dangerous.** The entrypoint passes a
`Store` into a module's constructor, so a module that does not want one simply does not
take it — `examples/embedded/announcements` already did. The silent version of the choice
was shipping, and "I brought my own storage" was indistinguishable from "I forgot to take
one".

**The module that had made the choice had also made the mistake it invites.**
`announcements` read the tenant from the *request* and returned **every tenant's data**
when the request named none. Two flaws in one line, in the reference a module author
copies. A module on the kernel store cannot write that bug: the adapter scopes the query
and fails closed with no tenant in context.

`Descriptor.Storage` — `kernel`, `own`, or `none` — is now required, and an undeclared
value is **refused at registration rather than defaulted**. A default would be applied
silently to exactly the modules whose authors never considered the question, which is the
population the field exists for. The error names the three options, so the fix is in the
message.

The startup report names every `own` module with its consequence, next to the tenancy
audit, because it is the same question: what isolation surface is an operator trusting.
The audit inspects database tables, so a self-managed module is invisible to it — and an
audit reporting "every table is scoped" while three modules hold unexamined tenant data
reads as a clean bill of health.

Six tests on the fixed example; four of them fail against the original and name the leak
exactly — *"a demo caller read a school announcement"*, *"a call with no tenant in context
returned data"*. Two more controls confirmed: dropping the registration check let an
undeclared module register silently; that test is kept.

`Idempotent`, `RunSaga` and `Project` now state that they require the kernel store — their
state must commit in the same transaction as the module's work, and there is no way to do
that across storage the kernel does not control. The generator emits `StorageKernel` and a
README section naming what `own` costs.

Not in this change: **per-module databases**, and **module-shipped migrations** — still the
open blocker for a `kernel` module in a third-party repo, since migrations are
`//go:embed *.sql` inside the kernel package. `own` is now a legitimate answer, which
narrows it without removing it.

## Module-shipped migrations — **done 2026-08-20**

The last thing standing between this kernel and a third-party module. A module needs its
tables to exist before it can run, and only the kernel could create them: migrations were
`//go:embed *.sql` inside the kernel package, so a community module's schema had to be
added to the kernel repository. That means forking the framework to ship a module, which
is the one thing the framework model forbids.

A module now implements `modulekit.Migrator` and carries its own SQL. The kernel applies
it and **never reads what is inside** — ADR 0004 forbids one module reading another's
tables and the kernel is not an exception. Migrations run at registration, before
`Install`, because a module may query its own tables during `Start` and migrating
afterwards would leave a window where it is callable and its tables do not exist.

**Each module gets its own version table**, derived from its ID rather than chosen by it:
a module that could name its own version table could claim another's and make its
migrations look applied. Every module numbers its first migration `00001`, so two modules
from unrelated authors collide — with one shared table the second looks already-applied
and starts against a database with none of its tables. The negative control shows this
precisely: removing `WithTableName` produced `no such table: notes`, because the kernel's
own version table already recorded versions past 1.

The module ID reaches SQL as an identifier, so it is **validated rather than escaped** —
an ID has no business containing a quote, a space, or a semicolon, and the derived name is
refused if it would exceed Postgres's 63-character identifier limit, where a truncation
could collide with another module.

`examples/embedded/notes` is the worked example: a module whose table nothing in this
repository declares, queried through the bus, tenant-scoped by the kernel because it
declared `StorageKernel`. The generator now emits a `migrations/` directory with a starter
schema, and a generated module compiles and passes standalone.

## WASM tier — **done 2026-08-20**

ADR 0001's tier 2, deferred since the beginning: untrusted code running in-process at
in-process speed. `internal/kernel/wasmhost` on `wazero` — pure Go, so cross-compilation
and static binaries survive.

**The tier exists because the other two each give something up.** An embedded module runs
with full process privileges, so a marketplace cannot put untrusted code there. An
external app is safe but pays a network hop. This is the only tier where a stranger's
pricing rule can run in a request path.

**It is also the only real fix for a flaw tier 1 cannot address.** A tier-1 subscriber
that ignores its context leaks a goroutine the kernel can only abandon. A guest is
interrupted, because the runtime owns its execution — the negative control is decisive:
with `WithCloseOnContextDone` removed, a guest in an infinite loop was still running when
the harness killed it at **41 seconds against a 100ms limit**.

**The sandbox denies by default.** Wiring wazero's WASI implementation would have been one
line and would have handed untrusted code a filesystem, a clock, an environment, and the
host's entropy. `wasi.go` is a shim instead: filesystem calls refused, environment and
args empty, entropy zeroed, `proc_exit` closing the module rather than the process. Only
`fd_write` to stdout and stderr is real, because a guest panic that produces no output
leaves a plugin author with nothing.

Three findings from making a real guest run, none of which a design document would have
surfaced:

- **A frozen clock is untenable.** Go's runtime aborts with `fatal error: nanotime
  returning zero`. The shim advances a monotonic counter instead — a guest can measure
  elapsed time within one invocation, which it needs to run, but learns no date, no
  timezone, and nothing correlatable across calls.
- **`ENOTCAPABLE` is the wrong refusal for descriptor enumeration.** Go's runtime treats
  it as fatal (`fd_prestat: Capabilities insufficient`). `EBADF` — "no such descriptor" —
  is both true and what a guest runtime is written to expect.
- **A plugin is a library, not a program.** Go's default `wasip1` build exports only
  `_start`, which runs `main` and exits, so the module was gone before the first call.
  Blocking in `main` either deadlocked (Go's own detector) or never returned control.
  `-buildmode=c-shared` produces a reactor exporting `_initialize`, and a guest without it
  is refused at load with that instruction in the error.

A fresh instance per invocation, so nothing one tenant's call writes can be read by
another's — verified rather than asserted. The tenant is written into the envelope by the
host from request context, since a Go context does not cross the boundary and a guest must
not choose its own tenant.

`AssertPortable` runs before encoding, which is the first time that check has faced a real
tier boundary rather than a synthetic one. Tests against a genuine compiled guest in
`testdata/`, because a fixture that never went through a compiler proves nothing about
whether the ABI is implementable.

**Installation and lifecycle wired 2026-08-22.** `ApprovedManifest` separates operator
policy from guest bytes: a guest cannot grant itself a capability or publish itself through
the gateway. `wasmhost.Module` implements the ordinary module lifecycle and
`App.RegisterWASM` loads, registers, and closes it. The same sandboxed capability is tested
through the in-process bus and authenticated HTTP and gRPC bridges. Failed registration
removes both handlers and public exposure.

That end-to-end path found two transport defects hidden by the embedded examples. Fiber's
stock net/http adaptor dropped the signed tenant context, and `RawPayload` requests and
responses were encoded a second time as byte strings. The bridge now attaches the
authenticated Fiber user context explicitly, and both bridge and WASM codec preserve
validated raw JSON.

**Still outside this milestone:** package download/signature verification, persisted
installation state, per-tenant enablement/configuration, guest-to-host capability and event
calls, and WASM hook subscriptions. Those belong to the marketplace control plane and the
next ABI extension; they are not silently implied by loading approved bytes at bootstrap.

## Public framework API — **done 2026-08-24**

The product model said developers import Sannad, but its sample imported
`internal/app/bootstrap`, which Go refuses from another module. The only public runtime,
`pkg/kernel`, was a second implementation missing descriptor validation, hooks, rollback,
storage, tenancy, Kayan, bridges, migrations, events, and WASM.

The module root is now package `sannad`: `New`, public configuration, module and WASM
registration, engine-neutral storage and events, capability calls, optional HTTP/gRPC
projections, lifecycle, and listeners all delegate to the one bootstrap runtime.
`pkg/kernel` keeps its source-compatible lightweight methods but delegates registration,
dispatch, hooks, validation, and lifecycle to the real registry and bus; it is deprecated
for applications.

Modules can implement `modulekit.ScopedModelProvider`. Registration enrolls those models
before migrations and installation, so importing a module and forgetting a second tenancy
call is no longer a valid wiring state. The manual method remains for older modules.

The deliberate cross-tenant marker also moved to `modulekit.WithSystemContext`; trusted
embedded modules no longer import the internal tenancy implementation for administration
or background work. The internal name remains a compatibility delegate.

The acceptance test creates a temporary, separate Go module with a local `replace`, imports
only root `sannad` and `pkg/modulekit`, registers a community module, starts it, calls its
typed capability, and shuts it down. This is the first proof that the framework works under
Go's actual `internal`-package rule rather than only from inside its own repository.

## Sandboxed module communication — **done 2026-09-03**

[ADR 0007](adr/0007-sandbox-permissions-and-host-calls.md) defines the missing
guest-to-host boundary without changing ABI v1. ABI v2 adds operator-approved capability,
event, and hook grants; invocation-scoped result handles; tenant-preserving calls through
the ordinary registry; and explicit payload, depth, cycle, deadline, and handle limits.

Implementation is deliberately sequenced as an expansion:

1. Add v2 manifest policy and host-call runtime while retaining the v1 path. **Done.**
2. Prove calls and denials with a real compiled v2 guest, including execute-once result
   handling and tenant propagation. **Done.**
3. Adapt approved WASM hook subscriptions to the existing hook registry. **Done.**
4. Add the full embedded/WASM/external cross-tier conformance matrix. **Done.**

### Step 4: the cross-tier matrix

`internal/kernel/conformance/crosstier_test.go` wires a real `registry.Registry` and
`bus.Bus` — not the wasmhost package's own stand-in `stubCaller` — with one embedded Go
module and one WASM guest (the same `testdata/guestv2.wasm` fixture wasmhost's own tests
compile against; a second copy of it here would drift from the ABI it exists to check, not
add coverage). The guest calls the embedded module's capability through `capability_call`,
and the result is checked byte-for-byte against calling the same capability directly through
`bus.Call` — proving ADR 0007's "one route, one policy check" claim against the real registry
a deployment actually runs, not a fixture built to make the claim easy to satisfy. A second
test confirms the embedded module sees the real caller's tenant, never one the guest could
have named — there is no field in the wire envelope for it to supply one in. A third confirms
the same setup with no grant refuses the call, exercised through the real registry the way an
operator's deployment would hit it, complementing the wasmhost package's own unit-level proof
of the identical check.

This closes ADR 0007. Package signatures, persisted installs, per-tenant enablement,
marketplace UI, and Kayan service identity for remote providers remain separate control-plane
work, as before.

`ApprovedManifest` gained `Consumes`, `PublishesEvents`, and `SubscribesHooks` — all v2-only.
`CurrentABIVersion` moved to 2; `MinSupportedABIVersion` stays 1, and a v1 manifest is refused
outright if it names any v2 grant, because a v1 guest has no import table to exercise one.
`sannad_v2` is wired unconditionally at `Load`, even for a plugin with no grants: an
ungranted guest that imports it gets an ordinary denial through the status/error channel
rather than a link failure that reads as a build problem.

`capability_call` resolves through the same `modulekit.Caller` every module uses — the ADR's
point that there is no sandbox-specific service locator turned out to be literally true in the
code, not just in the design: the host function is a thin adapter over one interface method.
The execute-once result handle is a `map[uint32][]byte` scoped to one invocation via Go
context, discarded when `Invoke` returns; a guest sizing its buffer too small re-reads the
same handle rather than re-running the call. `event_publish` builds the tenant-scoped subject
host-side from context — a guest never receives a subject-shaped field to fill in, so there is
no wire shape for it to get wrong.

Recursion is checked once, at every WASM capability's own `Handler` entry point, not inside
`capability_call`. That covers a cycle that would otherwise pass back through an *embedded*
module before returning — a check only inside the host-call function could not see that,
since it never regains control until the whole chain unwinds. The chain itself travels as an
unexported Go context value, never a guest-suppliable field, so nothing in guest memory can
extend or clear it.

One finding came only from running the recursion tests against the real guest rather than
reasoning about the design: a cycle detected deep in a re-entrant `Handler` call does not
bubble up as `ErrCallCycle` to the original caller. It is caught by the *inner* Handler call,
returned as a Go error to the *guest's* `capability_call`, which the guest reports as an
ordinary status-byte rejection — so it re-emerges at the top as `modulekit.ErrForbidden`
naming the cycle in its message, exactly as ADR 0007 §6 specifies ("cycle errors name the
chain for operators"). The first version of this test asserted the sentinel would survive
unwrapped; it does not, by design, and the test was wrong, not the implementation.

A second defect was a test bug, not a host bug: a six-capability call-chain test kept
"succeeding" past its configured depth of three, because each guest only forwards a call if
its *own* request names one — a top-level request naming only the first hop never reached the
fourth. Fixed by building the chain's nested payloads from the innermost step outward before
issuing the top-level call, so each hop's guest genuinely receives instructions to call the
next.

Hook subscriptions install through the ordinary `modulekit.HookRegistrar` a WASM module's
`Install` already had access to — no separate adapter path. A new `Registry.OfferedPoints()`
passthrough (mirroring the existing `Notify`/`Validate`/`Transform` ones) lets a hook grant's
declared kind be checked against how the point is actually offered *before* subscribing,
which the tests confirm as a startup error rather than a runtime surprise the first time the
hook fires. Validator, transformer, and observer responses translate through the same
status-byte convention v1 capability responses already use.

**Negative controls**, each confirmed to fail before restoring the check: the depth/cycle
check disabled at `Handler`'s entry point (`TestCapabilityCallCycleIsDenied` fails);
`capability_call`'s consume-grant check disabled (`TestUndeclaredCapabilityCallIsDenied`
fails); `event_publish`'s publish-grant check disabled (`TestUndeclaredEventPublishIsDenied`
fails).

## Kayan core/kayan-gorm v0.1.0 → v0.3.0 — **done 2026-09-04**

A breaking upgrade, not an in-place migration: 744 files changed upstream. `kgorm` was
renamed to `kayan-gorm` (package `gormstore`); `core/config`, `core/oauth2`, and `core/scim`
were removed outright; `session.Strategy` and `tenant.Resolver` gained `context.Context` on
every method; `flow.NewLoginManager` gained a `factory` parameter; and — the one that
mattered — `flow.OIDCManager` is gone.

**Found by inspection before touching any code, per the user's explicit ask to verify
against upstream rather than assume:** `OIDCManager`'s removal is not a capability
regression, and upstream's own migration notes confirm why deliberately: its callback took
no `state` parameter (no CSRF protection), it requested no nonce (a captured ID token could
be replayed), and it resolved a federated login to an existing local account by bare email
without checking `email_verified` — an account takeover at any provider that lets a user
assert an address they do not own. `flow.KayanOIDCStrategy` is the fixed replacement, and —
despite a name that reads as "log in with Kayan as your IdP" — its `OIDCClient` and
`IDTokenParser` interfaces are provider-agnostic: a caller supplies its own implementation
wrapping any standard OAuth2/OIDC library, and Kayan owns only the state/PKCE/nonce
orchestration. `internal/platform/auth/oidc.go` implements both over `golang.org/x/oauth2`
and `coreos/go-oidc/v3` — both already-vetted, already-transitive dependencies, not
homegrown crypto or token handling, honoring the AGENTS.md rule against custom auth even
while writing the adapter code the new interface asks for.

The lookup key changed too, and had to: `FindOrCreateByProviderSub` resolves by
`(provider, sub)`, added as `oidc_identities` in migration 00009 — never by email, which is
exactly the bare-email bug being closed. A new `oidc_states` table (migration 00009,
deliberately untenanted — see its own comment) replaces the previous cookie-based CSRF
token: state, PKCE verifier, and nonce are now server-side, single-use records, consumed
atomically inside a transaction (`ConsumeOIDCState`) so two concurrent callbacks for the
same state resolve to exactly one success. `handleOAuthAuthorize`/`handleOAuthCallback` in
`internal/app/bootstrap/app.go` lost their manual state cookie entirely — Kayan's strategy
now does that job, and does it with PKCE and nonce the old cookie approach never had.

**A real bug, caught by a test that was written to prove something else.**
`TestFindOrCreateByProviderSubIsScopedToItsProvider` was meant to confirm the same `sub`
from two providers doesn't collide — it also crashed on a `UNIQUE constraint failed:
accounts.tenant_id, accounts.email`, because both provider logins were left permanently
untenanted in the test, and `Account`'s uniqueness is `(tenant_id, email)`. Traced to a
non-issue in the real flow: `HandleOAuthCallback` always stamps a tenant immediately after
this method returns, synchronously, so two accounts with the same email from different
providers only collide if both requests are still mid-flight in that narrow window — the
same pre-existing race password `Register` already has, not something this migration
introduced. Fixed the test to stamp a tenant between the two calls, matching what the real
call path always does, rather than leaving an artificial state the service never reaches.

**Negative control:** the state-consuming `DELETE` gated to never match (`1 = 0 AND ...`) →
`TestConsumeOIDCStateIsSingleUse` fails on its *first* consume, not just the second —
confirming the delete is what makes the read meaningful at all, not merely load-bearing for
replay protection. Restored after.

Elsewhere: `tenant.Resolver.Resolve` takes `tenant.ResolveInfo` now, built from a request via
`tenant.ResolveInfoFromRequest` — `ValidatingResolver` and its test fixtures updated to
match. `TenantJWTStrategy`'s four `session.Strategy` methods gained `ctx`; `CreateForTenant`
and `ValidateWithTenant` — our own extension methods, not part of the interface — deliberately
did not, to keep the blast radius to what the version bump actually forces. All 20 packages
green, gofmt and vet clean except the pre-existing lock-copy warning.

**Not done here, by design:** the machine-to-machine service identity work
(`flow.APIKeyStrategy`, new in v0.3.0) this migration was undertaken to unblock. That is
tracked as its own next step, on top of this now-migrated base.

## Service identity — **done 2026-09-04**

Closes the "Service identity, and service-to-service tenancy" item ADR 0005 deferred.
`HeaderResolver` was rejected there because it is only safe behind a gateway that overwrites
the header from an authenticated source, and none existed. A service account *is* that
authenticated source now: it logs in like any account and gets the same signed, tenant-bound
session a human login produces, so nothing downstream needs a second notion of "who is
calling" or a second tenant-propagation path to get right — every existing `requireAuth`
route, the capability bridge included, already works for it unmodified.

Kayan v0.3.0's `flow.APIKeyStrategy` supplies the actual credential handling — hashing,
constant-time comparison, the strategy contract — so `internal/platform/auth/service_account.go`
is the storage adapter and the tenant wiring around it, the same shape as the password and
OIDC strategies already in this service, not a fourth hand-rolled scheme.

`ServiceAccount` deliberately does **not** implement `tenant.TenantAware`, for the identical
reason `Account` does not: authenticating by API key means looking a row up by its key hash
before any tenant context exists, and a scoped lookup would fail that closed — nobody could
ever authenticate. Isolation is enforced explicitly in this file's own methods instead —
`ListServiceAccounts` and `RevokeServiceAccount` filter by `tenant.IDFromContext(ctx)` in the
query itself, and `service_accounts` joins `accounts` in `auditExemptTables` with the same
justification.

A created service account's raw key is returned exactly once, in the creation response —
generated host-side via `flow.GenerateAPIKey`, never caller-supplied, the same rule webhook
endpoint secrets already follow. `LoginWithAPIKey` takes no tenant parameter: unlike human
login, where a caller names a tenant and membership is checked against it, a service
account's tenant is fixed at creation and is exactly what a valid key proves — a second,
caller-supplied tenant here would be a second path to the same claim, and the two could
disagree.

Exposed at `POST /api/v1/auth/service-login` (unauthenticated — the key *is* the credential
being presented, the same reasoning `/auth/login` already follows) and
`/api/v1/service-accounts` (create/list/revoke, behind `requireAuth` like webhook endpoint
management).

**Negative controls**, each confirmed to fail before restoring: `RevokeServiceAccount`'s
tenant filter dropped from its `WHERE` clause → a cross-tenant revoke that should return
`ErrNotFound` succeeds instead; the key lookup's `active = true` filter dropped → a revoked
service account's key authenticates again. Both restored.

Tested at two levels: `internal/platform/auth/service_account_test.go` against the service
directly (key uniqueness, tenant-bound session issuance, wrong-key rejection, revocation,
tenant-scoped listing, cross-tenant revoke), and
`internal/app/bootstrap/service_accounts_http_test.go` end to end through the real gateway —
create a service account as an authenticated human, log in with its key, and confirm the
resulting token passes an ordinary `requireAuth` route exactly like a human session would,
which is the whole claim this feature makes.

## RBAC and audit trail — **done 2026-09-04**

Enterprise-readiness gaps named directly by the user, independent of any module: nothing
limited what an authenticated caller could do inside its own tenant, and there was no
compliance-grade record of who did what. Kayan v0.3.0 ships both — `core/rbac` (hierarchical
roles, wildcard permissions, inheritance) and `core/audit` (SOC 2/ISO 27001-shaped event
records with tenant, actor, risk, and change tracking) — unused until now.

**Not kayan-gorm's own `RBACRepository`.** Inspected before writing anything: its
`GetIdentityRoles` and `GetRole` issue no `tenant_id` predicate at all, despite
`role_definitions` being keyed on `(name, tenant_id)` — an assignment or definition from one
tenant is readable from any other. Its `TenantID()`/`SetTenantID()` methods don't match this
codebase's `tenant.TenantAware` shape either, so the isolation callback can't recognise or
fix it. `internal/platform/auth/rbac.go` is a from-scratch adapter instead, same shape as the
OIDC and service-account adapters already in this package.

**Tried this codebase's own isolation callback first, reverted it.** `RoleAssignment` and
`RoleDefinition` implementing `tenant.TenantAware` and registered as scoped models looked
like the right call — until `AssignRole`'s `FirstOrCreate` produced a row with an empty
`tenant_id`. Rather than chase exactly which GORM call shapes the isolation callback's
`Create` hook does and does not intercept, every method in `rbacRepo` now filters and stamps
`tenant_id` explicitly — the same pattern `ServiceAccount` and the OIDC state store already
use, and one degree less magic to get wrong a second time. `role_assignments`,
`role_definitions`, and `audit_events` (written through `gormstore.Repository`, which already
takes an explicit `TenantID`/`Filter.TenantID` on every call) join `accounts` and
`service_accounts` in `auditExemptTables`, with the reasoning recorded there.

**A permission-model bug, caught by the very first end-to-end test.** `owner`'s catalog entry
was `Permissions: []string{"*"}`. The assignment resolved, the role resolved, and the check
still failed. Kayan's `PermissionGranted` treats `*` (`WildcardSegment`) as matching exactly
one path segment — `PermissionGranted("*", "service_accounts:write")` is false, because the
grant has one segment and the request has two. `**` (`WildcardSuffix`) is what matches one or
more remaining segments; that is what "owner: everything" actually needed. Found by running a
guest — sorry, a real HTTP request — through the full stack rather than by reading the
matching function's doc comment, which does describe this correctly in an example that simply
wasn't the shape being granted here.

**A second bug, in this session's own `Register`, not Kayan's.** The very first account in a
tenant is meant to get `owner` — otherwise nobody could ever assign a role, including to
themselves. The "is this the first account" check reused the existing per-email duplicate
pre-check (`WHERE tenant_id = ? AND email = ?`), which is always zero for any new distinct
address — so *every* registration read as "first" and got `owner`. A second, genuinely
tenant-wide count (`WHERE tenant_id = ?`, no email) fixed it; caught by
`TestRegisterAssignsMemberToASubsequentAccount`, which the first version of the fix did not
have and should have.

**A third bug, in kayan-gorm's `audit.AuditStore.Query`.** Every audit test failed identically
on `sql: Scan error ... storing driver.Value type string into type *time.Time`, on read only —
`SaveEvent` (ordinary GORM `Create`) worked. Traced to the `audit_events` table's `created_at`
column, declared `TIMESTAMPTZ` since this deployment's original Kayan v0.1.0 migration; every
other table in this codebase with a `created_at` column declares plain `TIMESTAMP` and reads
back fine. SQLite (via glebarez/modernc) round-trips the two type names inconsistently through
GORM's time serialization — a Postgres-only difference in practice, since production runs
Postgres, but the test suite runs SQLite deliberately and needed to actually pass. Fixed at the
schema level: migration 00011 rebuilds `audit_events` (same rebuild-and-copy shape migration
00003 already used for `accounts`) with `created_at TIMESTAMP`, and
`internal/platform/auth/audit_log.go` reads through this codebase's own GORM-mapped model
rather than `gormstore.Repository.Query`, sidestepping whatever that path does differently
regardless of the column fix.

**Default catalog**: `owner` (`**`), `admin` (`webhooks:*`, `service_accounts:*`, `roles:*`),
`member` (`webhooks:read`, `service_accounts:read`), `viewer` (nothing beyond
authentication) — seeded per tenant, idempotently, on first registration and safe to call
again without overwriting a tenant's own customisation. Exposed at
`/api/v1/identities/{id}/roles` (assign/revoke/list, behind `roles:write`/`roles:read`) and
`/api/v1/audit-events` (query, behind `audit:read` — deliberately not in any default role
except `owner`, unlike webhook or service-account management, which `admin` gets by default).
`requirePermission` gates webhook and service-account management routes on
`webhooks:*`/`service_accounts:*`-shaped permissions; the generic capability-dispatch endpoint
is deliberately **not** gated by this catalog — per-capability permissions are a
module-authoring concern for whenever real modules exist, not something the kernel can name on
their behalf.

**Negative controls**, each confirmed to fail before restoring the check: `GetIdentityRoles`'s
tenant filter dropped → `TestRolesAreIsolatedPerTenant` fails (a role assignment from one
tenant becomes visible from another); `QueryAuditEvents`'s tenant filter dropped →
`TestAuditEventsAreTenantScoped` fails identically for the audit trail. Both restored.

16 new tests across `rbac_test.go`, `audit_test.go`, `service_account_test.go`'s existing
suite, plus a bootstrap-level end-to-end permission-gating pass. All 20 packages green; gofmt
and vet clean except the pre-existing lock-copy warning.

## REST/OpenAPI projection — **done 2026-09-03**

The gateway already had a REST surface — the generic capability bridge shipped with the
external capability bridge work above — and no machine-readable description of it. An
integrator had to read this package's source to build against it.

**Deliberately not grpc-gateway codegen from `sannad.kernel.v1.Capabilities`.** That proto
describes one RPC: `Call(capability, opaque bytes)`. A REST projection generated from it
would describe exactly that — one path, one opaque body — which is strictly less than what
the actual gateway already exposes (typed auth payloads, per-field webhook endpoint
schemas). Generating from the proto would also be *wrong* in the other direction: nothing
about `/api/v1/capabilities/{ref}`'s request shape changes when a module registers a new
capability, because a capability's own contract is that module's to publish, not the
kernel's to guess at. So `internal/app/bootstrap/openapi.go` hand-builds the document
instead, describing what the kernel can honestly describe — its own fixed endpoints,
precisely — and states the capability-dispatch endpoint's payload as what it actually is:
opaque JSON, exactly as the proto already says. A module wanting a fully-typed REST and
OpenAPI surface for its own contract still publishes its own proto or schema, unaffected by
this endpoint's existence.

Served at `GET /api/v1/openapi.json`, deliberately unauthenticated like the health
endpoints: requiring a bearer token to read API documentation is what sends an integrator
to read source code instead of the docs — and there is no token to send until after they
have read them. Entrypoint-registered routes (`RegisterAuthenticatedRoutes`) are not
included: they are named by the entrypoint, not the kernel, for the identical reason
capability payloads are opaque here — the kernel does not know their schema either. An
entrypoint wanting its own routes documented merges its own OpenAPI fragment with this one;
nothing here forecloses that.

Covers: health/liveness, auth (register/login/me, OIDC authorize/callback when configured),
the generic capability list and dispatch, and the full webhook endpoint-management surface
added just above (CRUD, rotate-secret, replay, delivery history) — every fixed REST route
this package wires, in one pass, generated from the same route table `gatewayHandler`
registers against rather than maintained by hand as a second list that can drift from it.

**Negative control:** one path entry removed from the document → the coverage test fails
naming exactly that path missing, confirming the test checks the document's actual content
rather than merely that a 200 came back. Restored after.

**Not this milestone:** generated client SDKs. Once a document exists, standard tooling
(`openapi-generator`, `swagger-codegen`) produces those from it directly — nothing here
needs to generate or vendor a client itself.

## Admin operations on identities — **done 2026-09-04**

RBAC and the audit trail (above) gave an operator the means to see who did what and to
grant permissions; nothing yet let one act on a compromised or misbehaving account short
of the tenant-wide suspend already in `tenancy.Directory`. Three operations close that:
`LockAccount` (blocks future logins and invalidates every session already issued, not only
new ones), `UnlockAccount` (restores login; does not resurrect the invalidated sessions —
a fresh login gets a fresh session), and `RevokeAllSessions` ("log out everywhere" without
locking the account, for a suspected-compromise response that should not also block a
legitimate owner from logging back in immediately).

**Storage, not a new session store.** `Account` gained two columns
(`00012_account_admin_ops.sql`): `active bool` and `token_version int`. The JWT strategy is
deliberately stateless (`TenantJWTStrategy`, backed only by a negative revocation list) —
adding a positive session store to support "revoke everywhere" would have meant carrying
two different session-tracking mechanisms side by side. Instead `token_version` is signed
into every issued token (`TenantClaims.Ver`) and `ValidateForCaller` now looks the account
up by claim subject, rejecting on `!Active` (`ErrAccountLocked`) or a version mismatch
(`ErrSessionRevoked`) — the same pattern the revocation store's own design comment already
named as the intended extension point, unused until now. A lock bumps the version (existing
tokens stop matching immediately); a plain revoke-sessions bumps it without touching
`Active`; an unlock only flips `Active` back, leaving the version where the lock left it.
Old tokens issued before this migration carry no `ver` claim, which parses as the zero
value and matches a freshly-migrated account's zero-value `TokenVersion` — no forced
mass-logout on deploy.

Gated on `identities:write`, deliberately **not** included in `admin`'s default permission
set (unlike `webhooks:*`/`service_accounts:*`, which `admin` does get) — only `owner` holds
it by default, the same deliberate-omission policy already applied to `audit:read`.

**Self-caught bug, fixed before merge, not after:** the first pass filtered every one of
these three operations by the *target account's own* tenant, read back from its own row —
not by the caller's tenant from `ctx`. That let any tenant's owner lock, unlock, or
revoke-all-sessions for an account in an unrelated tenant purely by knowing its UUID, since
nothing compared the two. Caught while writing the cross-tenant test itself, before the bug
ever reached a review — the original test draft actually asserted the insecure behavior was
correct, on the reasoning that "the HTTP layer's permission gate is the boundary." That
reasoning does not hold: a permission gate proves the caller may act on identities in
*their own* tenant, not on any identity anywhere. Fixed by making `tenantOfAccount` resolve
and filter by `tenant.IDFromContext(ctx)` — the caller's own tenant — refusing with
`ErrNotFound` (not a distinguishable "forbidden") when the target belongs to a different
tenant, matching the shape `RevokeServiceAccount` already used correctly. Both the
resolving lookup and the subsequent `Updates`/`Update` WHERE clause carry the filter,
independently sufficient defense in depth — confirmed by two separate negative controls,
each removing one of the two filters and watching `TestLockAccountAcrossTenantsIsNotFound`
fail before restoring.

**Tests:** service-level (`internal/platform/auth/admin_test.go`) — lock blocks future
login, lock invalidates an already-issued token, unlock restores login without reviving old
sessions, revoke-all invalidates without locking, a service account's own token is
unaffected by the Account-shaped checks (no `Account` row exists for its subject), and the
cross-tenant case now asserts the secure outcome. HTTP-level
(`internal/app/bootstrap/admin_http_test.go`) — owner locks/unlocks a member end to end
through the real login flow, a member (default role, no `identities:write`) is forbidden
from locking anyone including itself, revoke-sessions invalidates a live token while
leaving login itself open, a cross-tenant lock attempt reads 404, and all three routes
reject an unauthenticated caller.

**Not this milestone:** SAML SSO, and per-tenant/per-service-account rate limiting (the
current limiter is global/IP-based) — both named as follow-ups when this work was scoped,
neither started.

## Per-tenant rate limiting — **done 2026-09-05**

The only rate limiter in the gateway was global and IP-keyed (`SANNAD_RATE_LIMIT`,
`fiber/middleware/limiter` on `c.IP()`). That leaves two real gaps: every tenant behind
the same NAT or corporate proxy IP shares one budget, so one noisy tenant starves the
others' legitimate traffic; and a compromised or misbehaving service account (no IP of
its own worth keying on — it may call from anywhere) has no throttle scoped to it at all
short of the blunt, whole-deployment IP limit.

Added a second limiter, keyed by `tenant:<tenant_id>:<subject>` rather than IP
(`SANNAD_RATE_LIMIT_PER_TENANT`, default 300/minute), wired once per `gatewayHandler`
build and invoked from inside `requireAuth` right after the caller and tenant are placed
in context — the same place `tenantLimiter` needed them to exist to key on. This runs in
addition to the IP limiter, not instead of it: the IP limiter is the pre-auth DoS floor
(protects login/register from being hammered before any caller identity exists), the
tenant limiter is the post-auth fairness boundary (protects tenants and service accounts
from each other once identity is known). A caller with no injected `CallerInfo` — not
reachable in practice, since `requireAuth` always sets one before this runs — falls back
to an IP-keyed bucket rather than a single key shared by every request, so a future
ordering bug degrades to over-throttling instead of an unbounded shared counter.

Reused `fiber/middleware/limiter`'s own in-memory store instead of reaching for
`golang.org/x/time/rate` and a hand-rolled map: it already does fixed-window counting
with automatic per-key expiration, which is exactly what per-tenant/per-service-account
buckets need and avoids introducing a second dependency and a manual eviction path for a
map that would otherwise grow with every distinct tenant+subject a self-provisioning
deployment ever sees.

**Tests** (`internal/app/bootstrap/rate_limit_test.go`, against a low deterministic limit
— production's 300/minute cannot be exercised in a unit test): exceeding the per-tenant
limit returns 429; a second tenant's first request still succeeds after the first
tenant's budget is exhausted, proving the isolation the IP limiter cannot provide (both
requests in the test share one `httptest` client IP, which is the point). **Negative
control:** the tenant limiter's construction gated off entirely → the 429 test fails,
confirming it is this middleware doing the blocking and not the pre-existing IP limiter
coincidentally tripping at the same threshold. Restored after.

**Not this milestone:** per-route limits (all authenticated routes share one budget per
caller today, rather than e.g. a stricter separate budget for write endpoints); a
configurable limit per tenant plan/tier, which would need a place to store it — the
default 300/minute env value is deployment-wide today, not adjustable per tenant.

## Enterprise SAML SSO — **done 2026-09-12**

The last of the three follow-ups named when admin operations shipped (SAML SSO, rate
limiting, a broader admin surface). Rate limiting landed above; this closes SAML.

Built on `kayan-saml`'s `ServiceProvider` — a transport-neutral SAML 2.0 SP that owns
AuthnRequest generation, XML signature verification (including XML Signature Wrapping
defenses), replay detection, and assertion validation. Nothing here reimplements any of
that; `internal/platform/auth/saml.go` supplies only a session store for pending
authentications, a per-tenant identity provider registry, and reconciliation against
`Account` — the same division of responsibility as the OIDC integration.

**Tenant model differs from OIDC on purpose.** OIDC is social login: the tenant is not
known until after the caller authenticates, so `HandleOAuthCallback` stamps it onto a
freshly created account afterward, compensating on failure. SAML is enterprise SSO: one
identity provider belongs to exactly one customer, decided when that IdP is configured
(`SAMLProviderConfig.TenantID`), never chosen by the caller. `InitiateSAMLLogin` takes no
tenant parameter at all — only an IdP ID — and a newly provisioned account is created with
its tenant already stamped, in the same transaction as its identity link. No untenanted
row, no compensating write, because the ambiguity that pattern exists for doesn't apply
here.

**Reconciliation bypasses Kayan's generic identity-storage path, deliberately, for the
same reason OIDC already does.** `ServiceProvider.NewServiceProvider` accepts a
`domain.IdentityStorage`, and its default reconciliation path creates identities via a
bare factory call with no ID and no tenant set — broken for `Account`, whose primary key
is never assigned that way — and on every first-time login also writes into Kayan's own
generic `credentials` table (`kayan-gorm`'s `gormCredential`), which carries a `tenant_id`
column this deployment's schema does not have (nothing else here uses that table; password
auth stores its hash directly on `Account` instead). Rather than migrate a table nothing
else touches to match an upstream library's internal schema, `saml.go` follows OIDC's own
precedent: `Hooks.UserLoader`/`Hooks.UserFactory` own reconciliation against a purpose-built
`saml_identities` table (migration `00013_saml.sql`), keyed on `(idp_id, name_id)` — what
the identity provider actually vouches for, never email — and a `noopIdentityStorage`
satisfies the interface `ServiceProvider` requires without ever touching Kayan's own tables.
The first identity provider a tenant's workforce arrives through also seeds default roles
and grants its first account `owner`, mirroring what `Register` already does for password
signup — otherwise an all-SAML tenant would have nobody able to grant any permission at all.

**Endpoints**, gated on `HasSAML()` so they don't appear when no provider is configured:
`GET /api/v1/auth/saml/metadata` (unauthenticated SP metadata, for an IdP administrator
configuring the integration), `GET /api/v1/auth/saml/{idp}/login` (redirect to the IdP),
`POST /api/v1/auth/saml/acs` (Assertion Consumer Service — unauthenticated, since this is
how a session is created in the first place). Configured the same way OIDC is — a JSON map
of providers in an env var (`SANNAD_SAML_PROVIDERS_JSON`), each entry either naming a
static SSO URL and PEM certificate or a `metadata_url` to fetch and parse them from,
per-IdP `attribute_mapping`, and the required `tenant_id`.

**Tests** (`internal/platform/auth/saml_test.go`) run a genuine SAML round trip rather than
mocking any of it: a real `kayan-saml` `IdentityProvider`, with its own generated RSA key
pair and self-signed certificate, signs a real assertion; the `Service` under test verifies
it with `kayan-saml`'s own XML-DSig verifier. The AuthnRequest is deflated exactly as the
HTTP-Redirect binding requires and re-encoded the way an HTTP-Redirect-aware IdP endpoint
would decode it, since `kayan-saml`'s own test `IdentityProvider.HandleSSORequest` only
decodes plain base64. Covers: first-time login provisions an account in the IdP's
configured tenant and issues a working session; a second login by the same NameID resolves
to the same account rather than creating another; a response signed by a key this
deployment never configured is rejected; naming an unconfigured IdP is refused before any
redirect is produced; a locked account cannot sign in through SAML either — SSO is a second
door into the same account, not a separate one. **Negative control:** the lock check
disabled → the last test fails; restored after.

**Not this milestone:** an admin API for self-service per-tenant SAML configuration
(providers are deployment-wide env JSON today, the same limitation OIDC already has); SP
request signing (`Config.SignRequests`) and assertion encryption (`WithDecrypter`) — both
supported by `kayan-saml` and wireable later without a schema change, left off because no
identity provider this deployment has been asked to integrate with requires either yet;
Single Logout (`kayan-saml` has `idp_logout.go`/`logout.go` for it, unused here) — a session
this deployment issued is still revoked the ordinary way, but there is no IdP-initiated SLO
endpoint that also invalidates the corresponding session at the identity provider's end.

## Account deletion and data export — **done 2026-09-12**

The first item off the SaaS-readiness gap list: admin operations (lock, unlock,
revoke-sessions) existed, but nothing erased or exported an account's personal data —
conspicuous given RBAC and audit were already in place, and a real blocker for any tenant
with EU customers under GDPR's right to erasure and right of access.

**Anonymize, not hard-delete — for a concrete reason, not caution.** `ValidateForCaller`
distinguishes "no such account" (treated as a service-account subject, exempt from the
Active/TokenVersion checks) from "an account, but blocked" purely by whether the `Account`
row exists. Hard-deleting it would put a still-unexpired token issued before the deletion
on the "no such account" branch — validating with no check at all, the opposite of what
erasing someone's access is supposed to do. `DeleteAccount` instead scrubs the row in place
(tombstone email, unusable password hash, empty traits, `Active: false`, `TokenVersion`
bumped) inside the same transaction that clears its role assignments and OIDC/SAML identity
links — reusing exactly the lock/revocation machinery admin operations already built, rather
than adding a second "is this account gone" code path for the rest of the system to learn.
Audit events naming the account by ID are deliberately left alone: once the email is gone,
an audit row is safe to retain for the compliance trail the deletion request itself becomes
part of.

**Boundary reuses `tenantOfAccount` from admin.go as-is** — same caller-tenant check,
same `ErrNotFound` (not a distinguishable "forbidden") for a cross-tenant target, same
`identities:write` permission gate, owner-only by default like lock/unlock/revoke-sessions.
`ExportAccountData` deliberately returns only what `DeleteAccount` actually clears (email,
active state, tenant, roles) — an export promising more than the deletion path removes
would misrepresent what "delete my data" accomplishes.

**Two access paths**, both landing on the same two service methods: self-service
(`GET /api/v1/me/export`, `DELETE /api/v1/me` — no permission grant needed beyond an
authenticated session, since acting on one's own data needs no separate authorization) and
operator-initiated (`DELETE /api/v1/identities/{id}` — for a data subject request made on
behalf of someone who can no longer sign in to make it themselves).

**Tests:** service-level (`internal/platform/auth/gdpr_test.go`) — deletion blocks future
login and invalidates an already-issued token immediately; the tombstoned email frees the
address for re-registration; a cross-tenant deletion attempt is refused and leaves the
account untouched; an export reflects the account's actual tenant, email, and current
roles; a cross-tenant export attempt is refused. HTTP-level
(`internal/app/bootstrap/gdpr_http_test.go`) — export then self-delete through the real
login flow, confirming the deleted account can no longer log in; an owner deleting a member
end to end; a member (no `identities:write`) forbidden from deleting anyone including
itself; both self-service routes reject an unauthenticated caller. **Negative control:** the
email-scrubbing field renamed so the update silently no-ops it → the re-registration test
fails on the still-held unique email; restored after.

**Not this milestone:** exporting webhook delivery history or other module-owned data tied
to the account (this covers what the kernel itself holds — email, roles, identity links);
a configurable retention/anonymization delay before erasure takes effect, which some
compliance regimes want instead of immediate erasure.

## Session secret rotation, and `pkg/config` test coverage — **done 2026-09-12**

Items 2 and 3 off the SaaS-readiness gap list, done together since the second was
mostly exercising the first.

**The gap:** `SANNAD_SESSION_SECRET` was a single HMAC key with no rotation path.
Changing it in a live deployment would make every unexpired access and refresh token
everywhere fail signature verification the instant the new secret deployed — a forced
mass-logout as the only way to respond to a suspected key compromise, which is exactly
the situation where forcing every legitimate session to re-authenticate all at once,
with no warning, is the wrong tool.

**Multi-key verification, single-key signing — not a `kid` header.** `TenantJWTStrategy`
gained `previousSecrets [][]byte`, tried in order after the active secret when a token's
signature fails verification. Signing (`sign`) touches only the active secret, always; a
token is never issued against a retired key. This was chosen over adding a `kid` claim to
select which key verifies a given token: with the small number of keys realistic here (the
active one, plus whatever is still being retired out), trying each in turn is simpler than
threading a key-selection header through Kayan's claims and costs nothing in the common
case — a token still signed with the active secret matches on the first attempt, and only
tokens minted before a rotation ever fall through to a previous one.

`NewService`, `NewTenantJWTStrategy`, and the public `pkg/auth/kayan.New` adapter all gained
a variadic `previousSecrets ...string` trailing parameter — additive, so none of the
existing 2-arg call sites needed to change. `pkg/config.Config` gained
`SessionPreviousSecrets []string`, parsed from a comma-separated `SANNAD_SESSION_SECRET_PREVIOUS`,
validated to the same ≥32-character floor as the active secret — a retiring key is still a
signing key until its last issued token expires, and a weak one is exactly as dangerous as
a weak active one.

**Rotation procedure this enables:** move the current `SANNAD_SESSION_SECRET` value into
`SANNAD_SESSION_SECRET_PREVIOUS`, set a new `SANNAD_SESSION_SECRET`, redeploy. Every session
issued before the rotation keeps validating against the previous-secret list until it
expires naturally (access tokens within a day, refresh tokens within a week); every session
issued after signs and validates against the new secret alone. Once enough time has passed
that no token signed under the old secret can still be unexpired, drop it from the
previous-secrets list — there is no other cleanup, and nothing here needs to know which
specific old secret a given legacy token was signed with.

**`pkg/config`'s own validation logic had no test file at all** before this — the one
guard standing between a misconfigured environment and a broken or insecure production
boot was itself unverified. `pkg/config/config_test.go` now covers every branch of
`Validate()` (short secret, placeholder secret, short previous secret, SQLite-in-production,
wildcard-CORS-in-production, and their accepting counterparts across all three documented
Postgres DSN shapes) and `FromEnv()`'s parsing (defaults, overrides, the new previous-secrets
list — including whitespace and empty entries between commas — malformed-int fallback, and
every accepted/rejected boolean spelling).

**Tests:** `internal/platform/auth/session_rotation_test.go` — a token signed under a
retired secret still validates against a strategy configured with the new one and the old
one listed; a token issued after rotation validates even against a strategy that no longer
knows the retired secret at all; a token signed under a secret nobody configured (not
merely retired — never listed) is rejected. **Negative control:** the previous-secrets
fallback loop replaced with active-secret-only → the first test fails; restored after.

## Public WASM API parity, and an authoring guide — **done 2026-09-12**

The last item off the SaaS-readiness gap list: a third-party developer building against
this framework has only ever had the public `sannad` package to embed against — importing
`internal/kernel/wasmhost` directly is not just discouraged, Go's own internal-package rule
makes it uncompilable outside this module's own tree. That package's `WASMManifest` and
`WASMConfig` predated ADR 0007's host-call surface (sandboxed module communication —
outbound capability calls, event publishes, hook subscriptions), and were never updated
after it shipped: `sannad.CurrentWASMABIVersion` was still pinned at `1`, and neither public
struct had a field for `Consumes`, `PublishesEvents`, or `SubscribesHooks` at all. A real
embedder using only the public API had no way to grant a sandboxed guest any of ADR 0007's
capabilities — not a documentation gap, a load-bearing one: the feature was there and
unreachable.

**Fixed by extending the public types to mirror the internal ones**, the same pattern
`WASMManifest`/`WASMConfig` already established for `Provides`/`Exposed` — a public mirror
type (`WASMHookGrant` alongside `wasmhost.HookGrant`) rather than naming the internal type
directly, since an external consumer's own module cannot import it regardless of what a
public signature named. `CurrentWASMABIVersion` now reads `wasmhost.CurrentABIVersion` (2)
rather than a stale literal, so a new embedder gets the current ABI by default instead of
silently building against a two-versions-old one; a manifest may still declare `ABIVersion:
1` for a guest with no need of the host-call surface. `RegisterWASM` forwards every new
field into `wasmhost.ModuleConfig` — `Caller`/`Events` stay auto-filled by bootstrap, never
constructed by the caller, since a guest's outbound calls must resolve through the same
registry every module uses.

**Proven through the actual public surface, not the internal one.** `sannad_test.go` gained
`TestPublicAppRegistersWASMWithHostCallGrants`: an ordinary embedded module (registered via
`app.RegisterModule`, the public API) provides a capability, a v2 WASM guest is granted it
via `Consumes` on the public `WASMManifest`, and `app.Call` on the guest's own capability
proves the outbound call actually reaches the granted downstream and returns its real
result — end to end through only what a third party importing this module as a dependency
could write. Before this test the same feature was covered only by
`internal/kernel/wasmhost`'s own tests, which prove the mechanism works, not that the
public API can reach it.

**`docs/architecture/wasm-authoring.md`** closes the other half of the gap: the build
instructions for a working guest existed only as comments inside internal test files
(`host_test.go`, `testdata/guest/main.go`) — invisible to anyone not reading kernel source.
The new document walks a complete guest end to end: the ABI in one paragraph (the two
exported functions, the envelope JSON, the status-byte response convention), a full minimal
Go guest, the exact build command and *why* `-buildmode=c-shared` specifically (a plain
command-mode build exports only `_start`, runs `main`, and exits before the host can call
anything — the reactor/library distinction is load-bearing, not a flag choice), registration
through the public API for both a plain capability and the ABI v2 host-call grants, debugging
via `Stdout`/`Stderr`, and what the tier deliberately cannot do. Linked from
`docs/architecture/plugin-tiers.md` (the architecture rationale) and from `cmd/sannad-gen`'s
generated module README (so a developer who just scaffolded an embedded module and wants
more isolation finds the path to it).

**Not this milestone:** a `sannad-gen` scaffold mode for a WASM guest itself (the generator
still only scaffolds tier 1); SP request signing/encryption and Single Logout for SAML,
per-route rate limits, per-tenant rate tiers, and per-module databases — all previously
named as deliberate non-goals, still not requested.

## Backup, disaster recovery, and hardened compose deployment — **done 2026-09-12**

The last item off the SaaS-readiness gap list. Before this, `deploy/docker-compose/docker-compose.yml`
ran Postgres with a hardcoded `sannad`/`sannad` password and no backup of any kind — a dropped
volume or a bad migration meant total data loss, and the credentials were guessable by
anything on the same host or network.

**No Kubernetes or Helm manifest is included, deliberately.** Writing one without a real
cluster to validate it against would be untested YAML that reads as production-ready and
isn't — worse than the gap it claims to close. Everything shipped here was actually run.

**Primary recommendation, documented rather than coded:** `docs/operations/backup-and-dr.md`
recommends a managed Postgres (RDS, Cloud SQL, or equivalent) with automated snapshots and
point-in-time recovery for any deployment with a real recovery-time requirement — that
configuration lives on the provider's side and a generic module guessing at one provider's
API would be exactly the kind of unvalidated artifact this work is refusing to ship.

**Self-hosted fallback, actually built and tested:** `deploy/backup/backup.sh`
(`pg_dump -Fc`, atomic write via a `.partial` rename, retention pruning) and
`deploy/backup/restore.sh` (`pg_restore --clean --if-exists`), wired into
`docker-compose.yml` as a `postgres-backup` sidecar running nightly. **Verified against a
real, disposable PostgreSQL 18 instance** — not the project's usual SQLite test path, since a
backup drill has to prove something about the actual production engine: a table was seeded,
backed up, dropped entirely (simulating real data loss), and restored via the two scripts
with the original rows confirmed intact afterward; retention pruning was verified separately
by backdating a dump 20 days and confirming a run with `RETENTION_DAYS=14` removed it while
keeping a fresh one. The isolated test instance (its own scratch data directory and port) was
created and torn down without touching the host's existing Postgres install.

**Compose hardening, alongside the backup work:** `POSTGRES_PASSWORD` and
`SANNAD_SESSION_SECRET` both changed from a hardcoded value / silently-empty default to
`${VAR:?message}` — compose now refuses to start at all, naming exactly which variable is
missing, rather than starting a database or signing service with no real credential. Verified
both ways: `docker compose config` with the required variables set produces the expected
interpolated environment with no warnings; with them unset, it fails with the exact intended
message rather than silently substituting an empty value. `SANNAD_SESSION_SECRET_PREVIOUS`
and `SANNAD_RATE_LIMIT_PER_TENANT` — both added to the auth service in earlier work this
session — are now wired through the compose file too, which had not caught up to either.
`deploy/docker-compose/.env.example` documents every required and optional variable.

**Not this milestone:** WAL-archiving/PITR for the self-hosted path (the honest answer for
that recovery-time requirement is a managed Postgres, not reimplementing what one already
does); Kubernetes/Helm manifests, for the reason stated above; encrypting the backup dumps at
rest (the current volume relies on the host's own disk security, same as `postgres-data`
already does).

## Known gaps not yet sequenced

Named here so they are not mistaken for oversights:

- No UI layer exists, and no ADR covers one. Whether the kernel owns one at all is
  itself undecided: a kernel may legitimately expose only contracts and leave
  presentation to consumers.
- No reporting or analytics capability (see the projection cost above).
- Marketplace package verification, installation persistence, and per-tenant module policy.
