# Product model: what Sannad is, and who uses it

Recorded 2026-08-20, from a design conversation that corrected several assumptions the
existing documents left open to misreading. The ADRs describe mechanisms; this describes
what the mechanisms are *for*, which is what makes the tier boundaries make sense.

## What Sannad is

A headless, open-source Business Operating System framework and kernel. Developers use
it to deploy an ERP or CRM, or to build their own modules. The comparison points are
Odoo for scope and Shopify for developer experience — with the explicit goal of beating
Odoo on DX, which mostly means not repeating its upgrade-breakage failure mode.

**It is a framework, not a project to fork.** A developer imports it:

```go
import (
    sannad "github.com/sannados/sannad"
    "github.com/acme/community-erp"
)

app, _ := sannad.New(cfg)
app.RegisterModule(erp.New(app.Store()))
app.Start(ctx)
```

Their `main.go` is theirs. Sannad is a line in their `go.mod`. Nobody forks the kernel to
ship a module, and any design that would require forking is wrong.

## Two audiences

**Developers** self-host the framework. They build their own modules, or take official
ones and customize them. They own their binary and their risk.

**Merchants** use the Sannad SaaS: drag-and-drop, install from a marketplace, never see
Go. They are not writing modules; they are assembling a system from ones others wrote.

Most architectural confusion comes from applying one audience's rules to the other.

## Module tiers, correctly stated

The tiers in [ADR 0001](adr/0001-module-tiers-and-trust.md) describe **isolation**, not
authorship. The short phrasing invited the wrong reading, so plainly:

**Embedded is a technical tier, not a permission tier.** Anyone may write an embedded
module. A community developer compiling their own module into their own binary is the
primary use case, not an exception.

What differs is *where it runs*:

| Context | Embedded (in-process Go) | Sandboxed / external |
|---|---|---|
| **Self-hosted** | Anything the operator chooses, at their own risk | Anything |
| **Sannad SaaS** | Official modules, plus approved third-party ones | Marketplace modules |

**Approval is a build-time gate.** Go cannot dynamically load code, so an approved
third-party embedded module is approved by being imported into the SaaS entrypoint and
compiled into that binary. Approved embedded modules therefore ship on the SaaS release
cadence, and every approval is a deploy. A marketplace module that installs without a
deploy is necessarily sandboxed or external.

## Customization: all three paths, escalating ownership

A developer who wants official CRM to behave differently has three options, deliberately
all supported. The ordering matters: each step gives more power and takes more
responsibility.

**1. Hooks — extend without touching it.** Official CRM ships unmodified; an extension
module subscribes to its hook points to add fields, derive values, or veto writes.
Upgrades never conflict, because nothing was edited. This is the Shopify model and the
path most customization should take. Already built (ADR 0003), and the conformance suite
proves an extension attaches with no shared code and no edit to the host.

**2. Replace — provide the contract yourself.** The developer ships their own module
fulfilling the same contract; official CRM is not installed. Full control, no upgrade
path, and they now own it. This is what late binding through the registry is for: if a
consumer imported the CRM *module*, replacement would be impossible.

**3. Fork — copy the source and edit.** Maximum power, and every upgrade becomes a merge
conflict. This is Odoo's failure mode, kept as an escape hatch rather than a
recommendation. Nothing prevents it; the framework simply should not make it necessary.

## Module-to-module calls are optional

Most modules stand alone: they import Sannad and nothing else. Building on another
module is a capability the framework offers, **not a shape it imposes**.

When one module does need another — a community ERP calling official CRM — the wiring is
what makes replacement possible:

```go
// Community ERP imports the CONTRACT, not the CRM module.
import crmcontract "github.com/sannad/contracts/crm"

resp, err := modulekit.CallTyped[crmcontract.ListReq, crmcontract.ListResp](
    ctx, bus, crmcontract.Reader, crmcontract.ListReq{})
```

The consumer depends on the contract; the entrypoint decides who provides it. Two Go
modules keep compile-time type checking, and the provider stays swappable.

A direct import of the provider would weld both modules into one binary forever, make
replacement impossible, and prevent the provider ever moving to WASM or an external app.

**The entrypoint is different and should import everything.** `main.go` wiring modules
together is correct and always will be. The indirection exists for module-to-module
calls, not for entrypoint-to-module wiring.

## The optional path, and how it works

Fixed 2026-08-20. Both halves of the mechanism were previously incomplete, and neither
had ever been exercised: `examples/embedded/crm` is its own only caller, so **no module
had ever called another module's capability in this codebase.**

- `modulekit.CallTyped[Req, Resp]` gives the caller the compile-time checking the
  provider already had through `HandleTyped`.
- Contract types belong in a package with no implementation, imported by both sides.
  `examples/contracts/directory` is the reference.

`examples/embedded/people` provides that contract and `examples/embedded/greeter`
consumes it, with neither importing the other — enforced by an AST test, because both
live in one repository and nothing else would stop a future edit from importing the
provider directly and quietly making the decoupling false.

## Storage is a module's choice

A module may use the kernel's storage or bring its own. Both are supported; the choice is
declared in `Descriptor.Storage` and refused if left unsaid.

| | `kernel` | `own` | `none` |
|---|---|---|---|
| Tenant isolation | enforced, cannot be forgotten | the author's responsibility | n/a |
| `Idempotent`, `RunSaga`, `Project` | available | unavailable | n/a |
| Startup audit | inspects it | cannot see it | n/a |

`own` is legitimate — a module wrapping an existing datastore, or whose data lives
somewhere the kernel has no business knowing about. What it is not is free: the isolation
guarantee becomes a claim rather than a mechanism, and nothing outside the module can
check it.

**On the SaaS, `own` is a marketplace review item**, for the same reason embedded is: it is
a risk the platform runs rather than one it bounds. A self-hosted operator makes that call
themselves.

The reason this is enforced rather than documented: the one self-managed module in this
repository had the exact bug the declaration warns about — tenant read from the request,
and an absent tenant returning every tenant's rows. It was the reference an author would
copy.

## Naming: what "capability" means here

The registered unit is called a **capability**, and the name is settled — reviewed on
2026-08-20 and deliberately kept.

> A capability in Sannad is a **named contract that a module fulfils**. It is not an
> object-capability security token: registering one grants no access to anything, and
> holding a `CapabilityRef` confers no permission. That is precisely why the external
> bridge needs a separate `Exposed` list — being registered does not make a capability
> publicly callable.

The term collides with object-capability security, where a capability *is* an unforgeable
token granting access. That collision was weighed against the alternatives and accepted:
"capability" is standard vocabulary in modular-system design (OSGi, Eclipse, Kubernetes
API-machinery), it is already load-bearing across ADRs 0001-0006, and it carries the
meaning the design needs — *what the system can do*, which is what survives when
`parties.reader@v1` moves from Go to WASM to a remote app.

The main alternative considered was **Contract**, which is more precise for this design
but overloads a word already used for payload types (`contracts/proto/`,
"transport-neutral contracts"). The overloading roughly cancels the collision, and a
rename would touch six ADRs, the SDK, the conformance suite, generator templates, and a
public endpoint for a marginal gain. Documented distinction beats mechanical churn.

Refs are nouns (`crm.contacts.reader`) rather than verbs because a capability names a
role a provider fills, not a single act.
