# Release and artifact trust format v1

Release metadata is a signed JSON document distributed with a detached
signature. The installed trust anchor verifies metadata before any artifact is
executed; each artifact is then checked independently.

```json
{"format":"payesh.release.v1","release":"0.1.0","created_at":"2026-09-08T00:00:00Z","min_core":"0.1.0","artifacts":[{"name":"payesh-agent","os":"linux","arch":"arm64","sha256":"…","compressed_bytes":"1234","unpacked_bytes":"4567","url":"…"}],"signing_key_id":"release-2026"}
```

The API uses the same `release` field name; it must not be renamed to
`version` at an API boundary. Artifact byte sizes are decimal uint64 strings.

## Database compatibility and generation binding

New core releases declare `database_schema` with decimal-string fields, for
example `{"min_readable":"1","current":"6"}`. The minimum is the oldest
readable input schema; the current value is the schema produced by the candidate.
Both values are authenticated by the manifest signature. Older manifests omit
this field to preserve their canonical signatures; omission does not authorize
production updates of an existing database.

The package writer derives these values from the candidate’s validated store
registry. The publisher must separately specify the oldest supported core
version with `RELEASE_MIN_CORE`; it is a compatibility declaration supported by
tests, not automatically the candidate version.

For schema-bearing bundles, `SHA256SUMS` includes the exact `manifest.json`
bytes as well as the installer and archives. Signing updates that entry after
normalizing timestamps and setting the key ID, then signs the resulting index.
The production worker authenticates this complete generation and reads existing
schema metadata without opening a migrating Store before stopping services.
It supplies the prepared index digest to the installer; a different index,
even validly signed for the same release, is refused before archive download.
Production shell and SSH installation also require the verified installer’s
read-only `--check-schema` capability before applying files or services.
Interrupted activation recovery remains ahead of fresh-candidate preflight.

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

Before canonicalization, the signer and validator normalize `created_at` using
Go's `time.Time` JSON representation: a zero UTC offset becomes `Z`, and redundant
fractional trailing zeroes are removed. Nonzero offsets are retained. External
signers must use the same timestamp representation in the signed manifest.

## Offline release signing and validation

`make release-package` intentionally emits an unsigned bundle with
`signing_key_id=unavailable-local`. An authorized operator may sign that exact
bundle with `make release-sign RELEASE_DIR=... RELEASE_SIGNING_KEY=/absolute/path/to/private.key RELEASE_SIGNING_KEY_ID=release-2026`.
The signer accepts an operator-owned Ed25519 seed/private-key file (raw, hex,
base64, or an unencrypted PKCS8 `PRIVATE KEY` PEM), requires a regular owner-only file, refuses symlinked inputs and
an existing `.sig`, updates the key ID and manifest atomically, and writes the
detached signature without a trailing newline. It never generates, stores, or
prints private key material. The corresponding verifier invocation must pass
the explicit public-key anchor and matching key ID, for example:
`make release-validate RELEASE_DIR=... RELEASE_PUBLIC_KEY=/path/to/release.pub RELEASE_SIGNING_KEY_ID=release-2026`.
Unsigned local bundles remain valid for local byte-integrity checks only.
URLs are informational until an authenticated transport fetches the exact
manifest. Installers must not treat a checksum delivered beside an untrusted
bootstrap script as independent authentication.

Artifacts stage in a versioned release directory and activate atomically. Keep
the previous compatible release until health verification succeeds; rollback
must not cross an incompatible database schema. Record verification and
activation in the audit log.

## Production bootstrap and updates

The shell installer and CLI/root web update worker default to `production`.
Missing external trust inputs, missing signatures, unknown/mismatched key IDs,
tampered metadata or scripts fail before executing downloaded release code.
Legacy unsigned releases remain available through the shell installer or an
explicit local CLI update using `--release-mode preview` or
`PAYESH_RELEASE_MODE=preview`; this prints an unsigned-preview warning.
Browser and fleet worker updates refuse preview mode and retain durable
activation intent through installed-version/service health checks and recovery.
Preview means byte integrity, not publisher trust.

Each newly packaged release also includes its exact `install.sh` in
`SHA256SUMS`. The owner signing command writes `SHA256SUMS.sig` in the same
86-character, unpadded base64url Ed25519 signature representation as the JSON
manifest signature. It signs the following exact UTF-8/ASCII prefix followed by
the unchanged checksum file bytes, including their final newline:

```text
payesh.checksums.v1\n
<release>\n
<signing-key-id>\n
<exact SHA256SUMS bytes>
```

Here `\n` denotes one LF byte; there are no blank lines in the payload. This
domain-separated signature binds release version, external key ID and every
checksum. It complements the existing JCS manifest signature; it does not
change the manifest wire format. The CLI authenticates this index, then hashes
the release-asset `install.sh` before execution. The shell authenticates the
same index with locally installed OpenSSL before extracting or executing any
archive. Production requires OpenSSL Ed25519 `pkeyutl -rawin` support (OpenSSL
3 or newer) and an Ed25519 PKIX `PUBLIC KEY` PEM anchor. Go validation also
accepts the previously supported raw/hex/base64 public-key encoding.

Provision the real owner's public key independently of the downloaded release,
as a root-owned regular non-symlink file that is not group/world writable.
Never download the trust anchor beside the candidate and call that independent
verification. Obtain the real key ID from the release owner, then invoke a
previously reviewed/trusted bootstrap script:

```sh
sudo sh ./install.sh --release-public-key /etc/payesh-release.pub \
  --release-key-id OWNER_SUPPLIED_KEY_ID --version X.Y.Z
sudo env PAYESH_RELEASE_PUBLIC_KEY=/etc/payesh-release.pub \
  PAYESH_RELEASE_KEY_ID=OWNER_SUPPLIED_KEY_ID payesh update --version X.Y.Z
```

For web updates, put these three assignments in the root-managed
`/etc/payesh-update.env`, owned by root with mode 0600, and restart the update
worker after configuring it:

```text
PAYESH_RELEASE_MODE=production
PAYESH_RELEASE_PUBLIC_KEY=/etc/payesh-release.pub
PAYESH_RELEASE_KEY_ID=OWNER_SUPPLIED_KEY_ID
```

The service reads this root-managed environment; it does not trust the panel's
service-account-owned environment to select a key or enable preview mode.
Protect the public anchor's parent directories against replacement as well.
Production key creation, custody, rotation/revocation and distribution remain
the release owner's responsibilities; the repository provides no production
private key or authoritative anchor. Until the owner publishes signed release
assets and distributes their anchor, production installation remains blocked.

The initial shell script itself must come from a trusted/reviewed source.
An unsigned `curl | sh` bootstrap can alter its own verification logic;
signature verification inside that untrusted script cannot solve initial
bootstrap trust. Installed Go update code verifies the downloaded script before
executing it. Version-bound signatures prevent cross-release replay; the CLI
also refuses a downgrade from a known installed version. Explicit fresh-install
version selection remains an operator choice, and this bootstrap index does
not implement automatic anchor rotation or a publication expiry policy.

## First owner signing setup

A signing key proves that a release was approved by its owner. The private key
stays on the owner's offline signing machine; servers receive only the public
key. Test-generated keys are for fixtures and never become production anchors.
The following is an owner-run procedure, not a key provisioned by the project.

On an owner-controlled machine with encrypted storage, outside the checkout,
create an Ed25519 key pair. Use a reviewed OpenSSL installation, keep backups
under the same owner controls, and never place the private key in Git, CI logs,
chat, or a server's panel configuration:

```sh
umask 077
PAYESH_SIGNING_DIR="$HOME/payesh-signing"
mkdir -m 700 "$PAYESH_SIGNING_DIR"
openssl genpkey -algorithm ED25519 \
  -out "$PAYESH_SIGNING_DIR/release-private.key"
openssl pkey -in "$PAYESH_SIGNING_DIR/release-private.key" -pubout \
  -out "$PAYESH_SIGNING_DIR/release.pub"
```

The signer accepts this unencrypted PKCS8 private PEM only from an owner-only
regular file. It rejects encrypted PEM, other key algorithms and multiple PEM
blocks. Protect the file with offline/encrypted-storage access controls; the
signer does not implement passphrase input or key custody.

Choose a stable key ID such as `owner-release-2026-01` and distribute that ID
and the public key through an independently authenticated operator channel.
Build the reviewed release candidate, transfer its complete unsigned bundle to
the signing machine, and sign/validate it there using the reviewed tools:

```sh
make release-sign RELEASE_DIR=dist/releases/X.Y.Z \
  RELEASE_SIGNING_KEY="$PAYESH_SIGNING_DIR/release-private.key" \
  RELEASE_SIGNING_KEY_ID=owner-release-2026-01
make release-validate RELEASE_DIR=dist/releases/X.Y.Z \
  RELEASE_PUBLIC_KEY="$PAYESH_SIGNING_DIR/release.pub" \
  RELEASE_SIGNING_KEY_ID=owner-release-2026-01
```

Prepare the toolchain and dependencies before going offline. Inspect the
candidate's artifact inventory/digests and acceptance results before signing.
After separate publication approval, publish the validated archives, exact
bootstrap script, checksum index and its signature, and manifest and its
signature together. Never publish the private key. The normal tag build's
unsigned preview artifacts do not satisfy this step.

On each server, authenticate the public key/key ID independently, copy only
the public key, and install it with root ownership in a protected directory:

```sh
sudo install -o root -g root -m 0644 release.pub /etc/payesh-release.pub
```

Configure the root-managed update-worker environment shown above, using the
same real key ID. Verify a signed clean-host install and an installed-release
update on disposable hosts, including altered/missing signatures and changed
script/archive rejection, before production rollout. Rotate or revoke a key
through the same authenticated distribution procedure; changing a key ID in
an untrusted release does not grant it trust.
