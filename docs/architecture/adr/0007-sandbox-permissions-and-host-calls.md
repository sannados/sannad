# ADR 0007: Sandbox Permissions and Host-Call ABI

- Status: Accepted
- Date: 2026-08-26

## Context

The WASM tier can currently provide capabilities through ABI v1, but it cannot
consume another capability, publish an event, or subscribe to a hook. That makes
it isolated but not composable. A marketplace module must participate in the same
module graph as embedded Go modules and external apps without receiving ambient
access to that graph.

This is a security boundary and a public protocol. Two rules dominate the design:

1. Guest-declared intent is not authority. Installation policy belongs to the
   operator, and a guest cannot enlarge it from inside its bytes.
2. Tenant identity belongs to the invocation. A guest cannot name a tenant for an
   outbound call or event, even if the payload contains a field called `tenant_id`.

ABI v1 is already public and supported. Existing guests only export
`sannad_alloc` and `sannad_handle` and import no Sannad host functions. They must
continue to install and run unchanged.

## Decision

### 1. ABI v1 remains supported; host calls require ABI v2

The host accepts ABI versions 1 and 2. Version 1 keeps exactly its current export
and envelope contract. Version 2 is additive at the package level: it retains the
same guest exports and adds an import namespace named `sannad_v2`.

An ABI v1 manifest cannot contain outbound grants or hook subscriptions. This is
refused at installation instead of accepting policy the guest can never exercise.

The current version constant points to v2 for newly built guests; a separate
minimum/supported-version check preserves v1. Removing v1 requires a future ADR
and a measured compatibility window.

### 2. Requested access and approved access are different artifacts

A distributable package may describe what it wants for marketplace review, but
the runtime receives only an **operator-approved manifest**. The runtime does not
read authority from guest memory or a custom WASM section.

The approved manifest contains:

```text
provides            capabilities implemented by this guest
exposed             provided capabilities published through the gateway
consumes            capabilities this guest may call
publishes_events    event names this guest may publish
subscribes_hooks    hook references, kind, and approved priority
```

Every list is an allow-list. Empty means no access. Capability and hook references
are exact name/version pairs. Event grants are exact logical event names; wildcards,
broker subjects, and tenant segments are forbidden.

`provides` is not a permission to consume, and `exposed` is not implied by
`provides`. A module calling its own provided capability still needs that
capability in `consumes`, making recursion visible to the operator.

### 3. All synchronous calls pass through the kernel registry

The guest imports these v2 host functions:

```text
sannad_v2.capability_call(request_ptr u32, request_len u32) u64
sannad_v2.result_len(handle u32) u32
sannad_v2.result_read(handle u32, output_ptr u32, output_capacity u32) u32
sannad_v2.result_drop(handle u32) u32
sannad_v2.event_publish(request_ptr u32, request_len u32) u32
sannad_v2.error_len() u32
sannad_v2.error_read(output_ptr u32, output_capacity u32) u32
```

`capability_call` receives JSON containing only a capability reference and its
payload. The host resolves it through the ordinary kernel caller. Consequently
WASM-to-embedded, WASM-to-WASM, and WASM-to-external use one route and one policy
check; there is no sandbox-specific service locator.

The call executes once and stores its encoded result in invocation-scoped host
state. The returned `u64` packs a status code in the high 32 bits and a result
handle in the low 32 bits. The guest asks for the length, copies the result into
its own allocated memory, and drops the handle.

This handle protocol is deliberate. A conventional caller-provided output buffer
would discover that the buffer is too small only after the operation ran. Retrying
the host call to obtain a larger response could repeat a payment, write, or other
side effect. A result handle separates execute-once from copying bytes.

Handles and errors live only for one top-level guest invocation and are discarded
when it ends. They cannot carry data between calls or tenants.

`event_publish` receives a logical event name and payload. The host constructs the
tenant-scoped broker subject from the invocation context. Guests never supply a
subject or tenant.

### 4. Hook subscriptions are ordinary approved module subscriptions

An approved hook subscription records an exact hook reference, its expected kind,
and priority. During module installation the host registers a normal
`modulekit.Subscription`; the registry supplies the sandbox module ID rather than
trusting one from the manifest or guest.

The adapter invokes the existing `sannad_handle` export with a v2 inbound envelope:

```json
{
  "operation": "hook",
  "name": "orders.before_submit@v1",
  "hook_kind": "validator",
  "tenant_id": "host-derived",
  "payload": {}
}
```

Capability invocations use `operation: "capability"`. The v1 envelope remains
unchanged. Observer, validator, and transformer responses are translated into the
existing hook contract; hook ordering, timeout, and failure policy remain owned by
the hook registry and offering module, not the guest.

The manifest's expected hook kind must match the offered point at installation.
A mismatch is a startup error, not a runtime interpretation.

### 5. Context and identity propagate; authority does not

For every outbound host call the host reuses the current invocation context. This
preserves its tenant, deadline, cancellation, tracing, and authenticated caller
metadata. The guest can pass business payload only. It cannot replace caller
identity, create system context, choose another tenant, or extend a deadline.

The same rule applies when a sandbox is invoked by another sandbox: no direct
guest reference or shared memory exists. The second guest receives a fresh
instance and a host-written envelope derived from the same context.

### 6. Resource and recursion limits are host policy

In addition to the existing memory, invocation-time, and response limits, ABI v2
enforces:

- maximum outbound request bytes,
- maximum stored result bytes and result handles per invocation,
- maximum event payload bytes,
- maximum nested capability-call depth,
- rejection of repeated capability keys already present in the active call chain.

The call chain is host context, never a guest field. Cycle errors name the chain
for operators without exposing another tenant's data. A tighter parent deadline
always wins over a module's configured timeout.

Guest code receives stable status codes and a bounded diagnostic string through
`error_len`/`error_read`. Internal errors are logged with module ID and tracing
context; secrets and raw cross-module responses are not copied into diagnostics.

### 7. External providers obey the same boundary

An external capability provider is represented by a kernel bridge handler and
registered under the same capability reference. A sandbox consuming it therefore
uses `capability_call` exactly as it would for an embedded or sandboxed provider.
Authentication of the kernel-to-external hop uses Kayan service identity when that
provider work lands; the WASM guest never receives service credentials.

### 8. Conformance and negative controls

The implementation is not complete until tests prove:

- ABI v1 guests still install and serve capabilities unchanged;
- an approved v2 guest calls embedded, WASM, and external-shaped providers;
- tenant, cancellation, and tracing context survive every route;
- an undeclared capability, event, or hook is denied;
- a guest cannot publish a broker subject or choose a tenant;
- result-buffer sizing never executes a capability twice;
- call cycles, excessive depth, payload size, leaked handles, and timeouts are
  bounded;
- validator rejection and transformer output preserve hook semantics;
- removing each permission check makes its corresponding negative control fail.

## Rollout and rollback

This is an expand-only protocol change:

1. Add v2 manifest types and host imports while retaining the v1 path.
2. Add a real compiled v2 guest fixture and cross-tier conformance tests.
3. Publish v2 authoring SDKs and make v2 the default for generated guests.
4. Keep accepting v1 until telemetry and a later ADR justify removal.

Rollback disables installation of new v2 packages and removes the v2 host import
module. ABI v1 packages and manifests remain valid throughout; no stored data or
manifest needs destructive conversion.

## Consequences

- Sandboxed modules become full participants in the module graph without ambient
  network, filesystem, broker, registry, or tenant access.
- Tier symmetry is real at the contract boundary: providers and consumers can move
  between embedded, sandboxed, and external tiers without changing callers.
- Operator manifests are security policy and require versioned storage, audit logs,
  and review in the future marketplace control plane.
- Nested synchronous calls can still increase latency and coupling. Events remain
  preferred when no immediate answer is required.
- The result-handle ABI is larger than a single call function, but it prevents
  accidental replay of side effects and works in guest languages with simple linear
  memory APIs.

## Alternatives considered

- **Let the guest declare its own permissions.** Rejected: metadata controlled by the
  code being contained is intent, not authority.
- **Give WASM network access and let it call the gateway.** Rejected: it leaks service
  credentials into the guest, adds a network hop, weakens tenant propagation, and
  bypasses the in-process registry.
- **Direct sandbox-to-sandbox calls.** Rejected: shared references create a second
  ungoverned module graph and make provider replacement impossible.
- **Caller-provided response buffer with retry.** Rejected: resizing can execute a
  side-effecting capability twice.
- **Replace ABI v1 in place.** Rejected: existing packages would fail at installation
  with no operational benefit over supporting both versions.
