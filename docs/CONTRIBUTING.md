# Contributing and release preparation

The pinned Go toolchain is declared in `go.mod`; the frontend uses the pinned
versions in `web/package-lock.json`. Keep optional modules separate from core
artifacts and preserve the contracts in `docs/contracts/`.

Run the focused checks before handing work off:

```sh
go test ./...
PAYESH_LIVE_TRANSPORT_ACCEPTANCE=1 go test -race ./cmd/... ./internal/... ./scripts/...
make lint
make contract-check
make web-check
git diff --check
```

These checks require local socket access. Linux kernel and live SSH gates are
opt-in and must run on disposable Linux hosts; skipped tests are not acceptance
evidence. Follow the [release checklist](RELEASE_CHECKLIST.md) for operational
gates and required results.

For release packaging, install the locked web dependencies and set an explicit
timestamp so another contributor can reproduce the same bytes:

Set `RELEASE_MIN_CORE` to the oldest core version actually supported by the
candidate and verified by compatibility tests. It must be a semantic version no
newer than the release; packaging refuses a missing declaration. The tag workflow
uses the reviewed `PAYESH_RELEASE_MIN_CORE` repository variable.

```sh
(cd web && npm ci --ignore-scripts --no-audit --no-fund)
SOURCE_DATE_EPOCH=1768089600 make release-package RELEASE_MIN_CORE="$RELEASE_MIN_CORE"
make release-validate RELEASE_VERSION=0.1.0
```

The package command builds Linux amd64/arm64 core and module archives with
`-trimpath`, `-buildvcs=false`, a cleared Go build ID, normalized tar metadata,
and sorted inventory. `SHA256SUMS` and the manifest are generated from those
exact bytes. Change neither archive nor manifest after validation.
The writer refuses an existing version directory and publishes a completed
candidate with an atomic rename; remove generated output only by an explicit
operator action after checking its exact path.

The local manifest deliberately has no `.sig`. Do not generate a fake
signature, commit a private key, or call an unsigned bundle official. An
authorized release owner may sign the finished bundle with an external,
owner-only key file:

```sh
make release-sign RELEASE_DIR=dist/releases/0.1.0 \
  RELEASE_SIGNING_KEY=/secure/path/release-2026.key \
  RELEASE_SIGNING_KEY_ID=release-2026
make release-validate RELEASE_DIR=dist/releases/0.1.0 \
  RELEASE_PUBLIC_KEY=/secure/path/release-2026.pub \
  RELEASE_SIGNING_KEY_ID=release-2026
```

The signer canonicalizes with the repository's JCS implementation and writes
the detached unpadded-base64url Ed25519 signature atomically. It refuses
insecure/symlinked key inputs and refuses to replace an existing signature.
Keep private keys outside this checkout and retain the public key as the
operator-managed trust anchor.

### Publishing to GitHub Releases

Pushing a `vMAJOR.MINOR.PATCH` tag runs `.github/workflows/release.yml`, which
builds the web bundle, runs `release-package` with the tag's version and
commit timestamp, validates it, and uploads every archive plus `manifest.json`
and `SHA256SUMS` as a GitHub Release. The one-line `install.sh` installs from
the latest such release, so publish only commits that pass CI:

```sh
git tag v0.1.1 && git push origin v0.1.1
```

Record exact commands, environment, skipped Linux/kernel checks, resource
measurements, and remaining blockers in your release notes. “Ready for
review” and “accepted” are different statuses; a successful unit test is not
evidence of real kernel enforcement or a clean-host release install.
