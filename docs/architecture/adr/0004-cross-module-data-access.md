# ADR 0004: Cross-Module Data Access and Shared References

- Status: Accepted
- Date: 2026-08-09

## Context

The motivating question: CRM owns a customer base. ERP needs those customers to issue
invoices. E-Commerce needs them to place orders. How do they get the data?

This looks like a transport question. It is actually a **data ownership** question,
and it is where modular business systems most often collapse into monoliths.

Three established answers:

1. **Shared table (Odoo).** ERP reads `res_partner` directly. Zero latency, always
   consistent, trivial joins. Also the reason Odoo upgrades break: every module
   depends on every other module's schema, forever.
2. **Pure service call.** ERP calls a CRM capability for each customer. Clean
   ownership. But an invoice list of 500 rows becomes 500 lookups without a batch
   API, and ERP is unavailable whenever CRM is.
3. **Ownership plus projection.** CRM owns the write model and emits events. ERP
   maintains a local read projection of only the fields it needs. Local joins, no
   fan-out, coupling to the *event contract* rather than to CRM's schema. Eventually
   consistent.

A second trap sits underneath: if ERP and E-Commerce each define their own
`Customer` type, there are now three customer concepts and no reliable way to join
them. Odoo avoids this only by having one physical table.

## Decision

### 1. Two access patterns, chosen per relationship

Neither pattern is universal. The contract declares which applies.

**Synchronous capability call** — the default when the read must be authoritative at
this instant:

- credit limit checks, stock reservation, "does this customer exist before I accept
  this order",
- strongly consistent,
- **batch operations are mandatory from day one.** Every collection-returning
  capability must accept a set of identifiers (`GetParties(ids[])`). A read API
  without batch guarantees N+1 across the entire ecosystem, and it cannot be fixed
  later without breaking every caller.

**Event plus local projection** — the default for list, report, search, and
join-heavy paths:

- invoice lists, order history, analytics, search indexes,
- the consumer keeps a local read model of just the fields it needs,
- built on the existing NATS JetStream backbone,
- eventually consistent, and the contract says so.

Both flow through the same contracts. The only difference is whether the consumer
caches.

### 2. Shared reference types, not shared entities

Common contracts define reference types that every module speaks. For parties:

```protobuf
// sannad.common.v1
message PartyRef {
  string id = 1;
  string tenant_id = 2;
  PartyKind kind = 3;  // PERSON, ORGANIZATION
}
```

- The **owning module holds the record**. Everyone else holds a `PartyRef`.
- Detail is obtained by resolving the ref (synchronous) or by subscribing to the
  projection (asynchronous).
- The capability is `parties.reader@v1`, **not** `crm.contacts.reader@v1`. CRM is one
  possible provider. A deployment can have parties owned by a different module, or by
  an external app backed by Salesforce or SAP, and ERP does not change.

That last property is a direct advantage over Odoo, where the owner of the customer
table is structurally fixed.

### 3. Bounded contexts: modules do not share entities

ERP's customer (payment terms, credit limit, tax registration) is not E-Commerce's
customer (cart, wishlist, browsing history) and is not CRM's customer (pipeline
stage, owner, lead source).

**Each module stores its own context-specific data keyed by `PartyRef`.** There is no
canonical fat `Customer` message that all modules import.

This is the single most important decision in this ADR. A shared canonical entity
becomes a god-object every module fights over, gaining fields until it is
`res_partner` again.

### 4. No cross-module database access

A module may not read another module's tables, and may not join across module
schemas. Data is obtained by resolving refs or by maintaining a projection.

This is the price of modularity, and it is exactly the price Odoo declined to pay.

### 5. External apps use the identical model

An external E-Commerce app consumes the same events and calls the same capabilities
as an embedded one. Tier symmetry (ADR 0001) holds for data access as it does for
hooks.

## Consequences

- Modules can be developed, versioned, deployed, and upgraded independently. This is
  the core promise of the whole architecture.
- Data ownership is explicit and enforceable rather than conventional.
- Cross-module SQL joins are impossible. Reporting across modules requires either a
  projection or a dedicated reporting/analytics module that subscribes broadly. This
  is a known, significant cost and should be planned for rather than discovered.
- Projections duplicate data. Consumers must handle out-of-order events, replay, and
  rebuild-from-scratch. The SDK should provide a projection helper so every module
  author does not reimplement this badly.
- Consistency guarantees become part of the contract. Integrators are told, rather
  than surprised.
- Reference resolution adds latency to synchronous paths. Batch APIs and projections
  are the mitigations, which is why batch is mandatory rather than encouraged.
- Common contracts (`sannad.common.v1`) become a small, high-stakes shared surface.
  It must stay minimal: identity and reference types only, never business fields.

## Alternatives considered

- **Synchronous capability calls only.** Simplest mental model, always strongly
  consistent, no projection infrastructure and no stale-read bugs. Rejected: it
  produces N+1 across module boundaries and makes every module's availability
  dependent on every module it reads from.
- **Event-sourced projections only.** Maximum decoupling and full local joins, and
  modules survive each other's downtime. Rejected: some reads genuinely must be
  authoritative now, and forcing eventual consistency onto credit checks and stock
  reservation is incorrect.
- **Shared canonical entity schema.** Easy joins and one shared meaning. Rejected as
  the `res_partner` failure mode.
- **Opaque string IDs with no shared types.** Loosest coupling, nothing to version.
  Rejected: no type safety, no way to know what an identifier denotes, and every
  module invents its own convention.
