# ADR 0006: Storage Boundary and Transaction Semantics

- Status: Accepted
- Date: 2026-08-17

## Context

Sannad is intended to be database-agnostic. It currently is not: `*gorm.DB` is
passed directly into modules.

```go
// examples/embedded/crm/module.go
type Module struct {
    db *gorm.DB
    // ...
}

func New(db *gorm.DB, env string) *Module
```

Every module written against that signature is bound to GORM, and through GORM to
the SQL engines it supports. The dependency Kayan deliberately avoided —
`kayan/core/go.mod` has no GORM requirement, and `kayan-gorm` is one adapter among
possible others — Sannad reintroduced one layer down.

Two questions are tangled here and must be separated:

1. **Where may engine types appear?** A layering question, cheap to answer, and
   mostly a refactor.
2. **What consistency does a module get?** A semantic question, expensive to answer
   late, and the one that actually constrains engine choice.

The second is urgent for a specific reason. The project plan is to generate a large
number of modules once the kernel stabilises. Whatever storage contract exists at
that moment is what every generated module is written against. A contract that
promises cross-module transactions cannot later be implemented on an engine that
does not have them, and a contract that does not promise them cannot be tightened
without auditing every module that assumed otherwise.

This must therefore be decided before module generation begins, not before the
first engine is swapped.

Egypt-first localisation (ETA e-invoicing) sharpens it further: invoice posting,
tax computation, and submission-state transitions are exactly the kind of flow
where authors will reach for a transaction spanning modules if one is offered.

## Decision

### 1. Engine types are confined to the bootstrap layer

`pkg/modulekit` must never expose `*gorm.DB`, `*sql.DB`, a driver type, a DSN, or
any other engine-specific value. Modules receive a storage handle defined by the
SDK.

The layering, top to bottom:

- **bootstrap** — selects the engine, opens the connection, registers Kayan tenant
  isolation (ADR 0005), constructs the adapter;
- **kernel** — passes the adapter to modules; knows the interface, not the engine;
- **module** — holds the interface; cannot name an engine type.

This mirrors Kayan exactly: `core/` is storage-neutral, `kayan-gorm` is a binding.
Sannad's kernel earns the same property by the same means.

### 2. Modules are transactionally independent

**A module gets ACID guarantees within its own data. It gets no transaction spanning
another module's data.** There is no kernel-provided unit of work shared across
modules.

Cross-module consistency is achieved by the mechanisms already chosen in ADR 0004 —
capability calls for authoritative reads, events and local projections for
everything else — and, where a multi-step operation must eventually converge, by a
saga: a sequence of local transactions with compensating actions, coordinated over
the JetStream backbone.

This follows from bounded contexts rather than adding to them. ADR 0004 already
forbids a module reading another module's tables or joining across module schemas.
A shared transaction would require exactly the coupling ADR 0004 rejects: a common
connection, a common engine, and mutual visibility of uncommitted state.

It is also the only choice under which "database-agnostic" is true rather than
aspirational. Cross-module transactions are trivial on PostgreSQL, require replica
sets on MongoDB, and are bounded on DynamoDB. A contract promising them selects an
engine class by implication.

### 3. Consistency is declared, not discovered

Where an operation spans modules, the contract states its guarantee — immediate or
eventual — as ADR 0004 requires for data access. An integrator is told, rather than
surprised in production.

Sagas are explicit. A compensating action is part of the design of the flow that
needs one, not an error path bolted on afterwards.

### 4. The SDK provides the hard parts

Independence pushes work onto module authors, and the failure mode is thirty modules
each implementing it slightly wrong. The SDK owns:

- a **projection helper** — out-of-order events, replay, rebuild-from-scratch
  (already named as a requirement in ADR 0004's consequences);
- **idempotency** for event consumers, since at-least-once delivery is the backbone's
  guarantee;
- a **saga coordinator** — step sequencing, compensation, and durable state.

Without these, decision 2 is a burden rather than a design. This is a deliverable of
the kernel phase, not a follow-up.

### 5. Sannad ships one adapter

GORM over PostgreSQL is the supported adapter. Agnosticism is a property of the
**boundary**, not a promise of multiple maintained backends.

A second adapter is written when a deployment needs one, and the cost of writing it
is the test of whether decisions 1–3 held. Claiming multi-engine support without a
second implementation would be claiming something untested — the same posture ADR
0001 takes about isolation it cannot deliver.

## Consequences

- Modules can be developed, versioned, and deployed independently, which is the
  premise of the whole architecture. Shared transactions would have quietly removed
  it.
- Engine choice stays open at a real boundary rather than a documented intention.
- **Some business operations become eventually consistent, and this is a genuine
  cost.** An order that reserves stock in another module is not atomic. Authors must
  design compensation, and reviewers must check it exists.
- Financial flows need care. Within a module — an invoice and its lines, its tax
  lines, its ledger entries — one transaction covers it, and those must therefore be
  **one module's data**. Module boundaries in accounting are a correctness decision,
  not an organisational one.
- Reporting across modules requires a projection or a dedicated reporting module, as
  ADR 0004 already established.
- The SDK carries more weight: projection, idempotency, and saga support are
  kernel-phase deliverables.
- `crm.Module` and every other module signature changes from `*gorm.DB` to the SDK
  interface. Small now; large after generation.

## Sequencing

This ADR blocks module generation. The order is:

1. Tenant isolation adopted (ADR 0005) — storage must be safe before it is abstracted.
2. Storage interface defined; `*gorm.DB` removed from `pkg/modulekit` and from module
   constructors.
3. Projection, idempotency, and saga helpers in the SDK.
4. Hooks and typed handlers (ADR 0003) — required before anything can extend a module
   without modifying it.
5. Hook conformance suite: fixture modules exercising hooks and storage together.
6. Then generation.

Steps 2 and 3 are what this ADR adds to the previously agreed order.

**Amended 2026-08-19.** Steps 5 and 6 originally named a country-neutral invoicing
module and `eg-localization` as deliverables. That was scope drift: this project is a
kernel, and a saleable domain module is not a kernel deliverable. The validation those
steps provided is retained as a conformance suite of throwaway fixtures. Real domain
modules are downstream consumers built against `pkg/modulekit` in their own
repositories. The decision this ADR records is unchanged; only the sequencing tail is.

## Alternatives considered

- **Kernel-provided shared unit of work across modules.** Strong consistency, and
  markedly easier for module authors — the operation either happens or it does not,
  with no compensation to design. Rejected: it requires a common engine and a common
  connection, contradicts ADR 0004's bounded contexts, makes engine-agnosticism
  unachievable, and couples module deployment. It is also the path by which a modular
  system becomes a monolith with namespaces, which is the specific outcome this
  project exists to avoid.
- **Keep `*gorm.DB` in the module SDK.** Zero work, full query expressiveness, and
  GORM is a capable library. Rejected: it makes agnosticism false, and it is the one
  decision here that becomes unfixable at scale — after N generated modules, changing
  it means rewriting all of them.
- **Define the storage interface as a lowest-common-denominator query API across SQL
  and document engines.** Maximum portability. Rejected: it would be too weak for
  real business queries, and authors would route around it, which is worse than not
  offering it. The interface should be shaped by what modules need, with the adapter
  responsible for meeting it.
- **Defer the whole question until a second engine is actually required.** Tempting,
  since only one adapter ships either way. Rejected on the sequencing argument: the
  transaction contract is what modules are written against, so deferring it does not
  postpone the decision — it makes it implicitly, and expensively.

## Amendment: storage is optional (2026-08-20)

This ADR describes what a module gets from the kernel's storage. It did not say whether
taking it is compulsory. In practice it never was — the entrypoint passes a `Store` into a
module's constructor, so a module that does not want one simply does not take it, and
`examples/embedded/announcements` already did exactly that.

That silent version was the problem. "I brought my own storage" and "I forgot to take one"
looked identical, and the module in the tree that had made the choice had also made the
mistake it invites: it read the tenant from the request and returned every tenant's data
when the request named none. A module on the kernel store cannot write that bug; the
adapter scopes the query and fails closed with no tenant.

`Descriptor.Storage` now declares the choice — `kernel`, `own`, or `none` — and an
undeclared value is refused at registration rather than defaulted, because a default would
be silently applied to exactly the modules whose authors never considered the question.

**Everything this ADR guarantees holds for `kernel` only.** Under `own`, tenant isolation
is the module author's claim rather than a mechanism, the SDK helpers are unavailable
(their state must commit in the same transaction as the module's work), and the startup
audit — which inspects database tables — cannot see the module's data at all. The audit
now reports those modules as uninspected rather than staying silent, because a clean audit
that skipped three modules reads as a clean bill of health.
