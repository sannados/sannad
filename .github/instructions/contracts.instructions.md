---
applyTo: "contracts/**/*.proto"
description: "Use when editing Sannad OS protobuf capability contracts or external integration schemas."
---

- Keep protobuf packages versioned, for example `sannad.crm.v1`.
- Do not make breaking changes to an existing version. Add fields with new numbers or create `v2`.
- Model contracts so they can be used equally by in-process handlers, gRPC, and generated clients.
- Use domain-driven capability names such as `crm.contacts.reader` and keep them stable.
- Include tenant identifiers and traceable request metadata when a contract crosses process boundaries.
- Document new contracts in `contracts/README.md` and the relevant architecture docs.
