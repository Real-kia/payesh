# Privileged helper protocol v1

`payesh-privd` is a small root-owned process on a permission-controlled Unix
socket. It accepts typed JSON requests only; there is no shell-string or
arbitrary-file API.

## Request shape

```json
{"protocol":"payesh.helper.v1","request_id":"…","action":"service.install","target":"payesh-agent","expected_revision":"3","arguments":{"unit":"…"}}
```

The helper validates the caller (socket ownership/group), action allowlist,
target identity, argument lengths and paths, module ownership/version, and
expected revision. Requests have deadlines and are serialized per machine.
In the shared `ActionRequest`, `target` names the local service or artifact
(such as `payesh-agent`); `target_server_id` is reserved for a remote node
identity.
Credentials and command output are never persisted; responses redact secrets.
The shared foundation types provide `ServiceActionArguments` and
`ArtifactActionArguments`; each later action may add a separately reviewed
typed argument struct, but never a shell string.

## Foundation action families

`service.install`, `service.start`, `service.stop`, `service.status`,
`artifact.stage`, `artifact.activate`, `artifact.revert`, and
`module.invoke` are reserved typed actions. Each response contains
`request_id`, `accepted`, a stable error code when rejected, and the resulting
revision. Later packages add narrowly scoped arguments and verification; they
must not widen this surface into remote administration.

Actions are auditable, cancellable at safe boundaries, and fail closed when
ownership or platform capability checks are inconclusive.
