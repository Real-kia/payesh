# Optional module protocol v1

Optional modules are separate, signed executables downloaded only on request.
The core does not import their implementation or ship their artifacts.

## Metadata and lifecycle

Catalog metadata includes module ID/version, compatible core and protocol range,
OS/architecture/capabilities, dependencies, compressed and unpacked size,
signing key ID, required privileges, and resource measurements. States are:
`unavailable`, `available`, `downloading`, `verifying`, `installing`,
`installed-disabled`, `enabled`, `updating`, `removing`, and `failed`.

Install is: eligibility and dependency preview → signed manifest/artifact fetch
→ signature/checksum/size/platform/archive-path verification → staging → typed
helper activation → bounded health check. Download completion is not success.
Failed upgrades preserve the previous working state; disabling/removing first
reverts active policies and verifies kernel cleanup.

## Typed invocation

```json
{"protocol":"payesh.module.v1","module_id":"port-traffic","module_version":"1.0.0","request_id":"…","operation":"status","arguments":{}}
```

Operations and arguments are module-defined schemas, never shell commands.
Every response reports module ID/version, operation result, capability limits,
and a stable error. Modules cannot access browser sessions, another node's
history, or undeclared host paths.

Core transport uses the shared `ModuleInvocationRequest`; module-specific
argument schemas are versioned with the module manifest and validated before
invocation.

## Package-06 framework (Checkpoint A)

`internal/modules` implements the curated catalog, eligibility check, signed
manifest verification (`internal/trust`, JCS + Ed25519 per
`RELEASE_FORMAT.md`), safe archive staging, atomic activation, and the
`contracts.ModuleState` lifecycle above as a durable per-server
`ModuleInstallation` row (`internal/monitoring/modules_store.go`). Browser
routes: `GET /api/v1/modules` (catalog), `GET /api/v1/servers/{id}/modules`
(status), `POST /api/v1/servers/{id}/modules/{moduleId}/{install,enable,disable,remove}`
(idempotency-key + expected-revision, mirroring the package-05 traffic
configuration endpoint). `Enable`/`Disable` do not yet drive a real
`payesh-privd module.invoke` call — that wiring, plus a provisioned
production signing key, is the next integration step. See
`docs/handoffs/06.md`.
