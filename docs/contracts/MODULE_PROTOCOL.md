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
