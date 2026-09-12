# Contracts

Sannad OS contracts define transport-neutral capability boundaries.

Rules for this repository:

- Contracts live under `contracts/proto`.
- Packages are versioned, for example `sannad.crm.v1`.
- Capability names remain stable even if the transport changes.
- Breaking changes require a new versioned package.
- Internal modules and external apps should both be able to generate clients from the same source contract.

Current contract coverage:

- `sannad.crm.v1.ContactReader`
