# Release and artifact trust format v1

Release metadata is a signed JSON document distributed with a detached
signature. The installed trust anchor verifies metadata before any artifact is
executed; each artifact is then checked independently.

```json
{"format":"payesh.release.v1","release":"0.1.0","created_at":"2026-09-08T00:00:00Z","min_core":"0.1.0","artifacts":[{"name":"payesh-agent","os":"linux","arch":"arm64","sha256":"…","compressed_bytes":"1234","unpacked_bytes":"4567","url":"…"}],"signing_key_id":"release-2026"}
```

The API uses the same `release` field name; it must not be renamed to
`version` at an API boundary. Artifact byte sizes are decimal uint64 strings.

## Detached signature interoperability

The manifest is canonicalized with the [JSON Canonicalization Scheme (JCS),
RFC 8785](https://www.rfc-editor.org/rfc/rfc8785), encoded as UTF-8 without a
BOM, then signed with Ed25519 as specified by [RFC 8032](https://www.rfc-editor.org/rfc/rfc8032).
The detached signature file has the same name as the manifest with `.sig`
appended and contains exactly the 64 raw Ed25519 signature bytes encoded as
unpadded base64url (RFC 4648 §5); it has no JSON wrapper and no trailing
newline. The signing-key identifier selects one trusted 32-byte Ed25519 public
key. Public-key storage/rotation is outside this file, but implementations must
reject unknown, expired, or revoked IDs before verification.

The signature covers those exact canonical JSON bytes. Verification rejects
unknown key IDs, expired/revoked keys, malformed detached signatures, mismatched
hashes/sizes, unsupported OS/architecture, unsafe archive paths (including
symlink escapes), and oversized extraction.
URLs are informational until an authenticated transport fetches the exact
manifest. Installers must not treat a checksum delivered beside an untrusted
bootstrap script as independent authentication.

Artifacts stage in a versioned release directory and activate atomically. Keep
the previous compatible release until health verification succeeds; rollback
must not cross an incompatible database schema. Record verification and
activation in the audit log.
