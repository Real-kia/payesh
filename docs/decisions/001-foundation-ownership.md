# ADR 001: foundation ownership and wire boundaries

Status: proposed for review (2026-09-09)

## Decision

- `payesh-agent` owns collection, node identity, bounded spool, and the node
  protocol. It never imports SQLite or browser/frontend packages.
- `payesh-server` owns REST routing, authentication, fleet/history storage,
  migrations, and durable job persistence.
- `payesh-privd` owns only typed, allowlisted privileged actions over its local
  socket. It never accepts shell strings or arbitrary paths.
- `payesh` is the local CLI and recovery entry point; it calls the server or
  helper through their typed interfaces.
- `api/openapi.yaml` and `internal/contracts` are the canonical shared
  contracts. Package consumers must propose changes there rather than invent
  parallel endpoints or wire formats.

## Review gates

Cryptographic release verification, enrollment/authentication implementation,
database migration SQL, and helper authorization require focused security and
schema review before implementation. This ADR does not approve those
implementations; it records ownership so later packages have an unambiguous
boundary.
