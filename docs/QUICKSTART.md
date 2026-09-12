# Payesh quickstart (current source build)

This is the verified local workflow for the current repository. It is a
development/preview workflow, not a published installer: the repository has
no production signing key and `payesh-install --install` intentionally fails
closed unless a caller supplies an artifact verifier.

## Build and inspect the host

Payesh targets Linux amd64 and arm64. On a supported Linux host, build the
executables and run the side-effect-free preflight first:

```sh
make build
sudo ./dist/payesh-install --role standalone
```

The preflight reports the detected distribution, init system, architecture,
and optional `ip`, `tc`, `nft`, and cgroup capabilities. It exits non-zero for
an unsupported host. Do not use `--install` with these unsigned local files.

## Run a local monitoring preview

The server uses SQLite and listens on loopback by default. In one terminal:

```sh
./dist/payesh-server -db ./payesh.db -listen 127.0.0.1:8787
```

In another terminal, register the local server and feed samples to the same
database:

```sh
./dist/payesh register-server -ensure -db ./payesh.db -identity-file ./server-id
./dist/payesh-agent -interval 15s -identity-file ./server-id \
  | ./dist/payesh ingest -db ./payesh.db -identity-file ./server-id -follow
```

The health endpoint is `http://127.0.0.1:8787/healthz`. Protected API
resources require the configured bearer token or browser setup/session flow;
do not expose this development listener publicly. Stop the processes with
Ctrl-C. The SQLite database and identity file remain in the paths you chose.

## Build a local release bundle

After installing the pinned web dependencies, `make web-check` produces the
static bundle. `make release-package` then cross-builds every core executable
and the three optional module executables for Linux amd64 and arm64, archives
them deterministically, and writes `manifest.json` and `SHA256SUMS` under
`dist/releases/0.1.0/`.

```sh
(cd web && npm ci --ignore-scripts --no-audit --no-fund)
make web-check
SOURCE_DATE_EPOCH=1768089600 make release-package
make release-validate RELEASE_VERSION=0.1.0
```

The bundle is unsigned by design (`signing_key_id` is
`unavailable-local`). It is useful for byte-integrity and staging tests only;
it is not an official release and must not be installed by an updater. An
authorized release owner can use `make release-sign` with an external
owner-only private key, then rerun `make release-validate` with its explicit
public-key anchor; see `docs/CONTRIBUTING.md`.

## Disposable installation acceptance

Run the repository-side clean-host lifecycle gate before using a live target:

```sh
scripts/install-acceptance.sh
```

It uses a private filesystem fixture and injected account/supervisor seams to
exercise first install, artifact replacement, failed-verification recovery,
restart/resume state, and safe uninstall with and without explicit data
removal. It does not modify `/`, systemd, OpenRC, or a real host. Live-host
acceptance still requires a disposable supported Linux machine.
