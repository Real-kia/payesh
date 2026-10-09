# GitHub package catalog

Payesh reads `packages/catalog.json` from the default branch of `Real-kia/payesh`
through the GitHub Contents API whenever Packages opens. It does not use the
catalog embedded in the release as its primary listing. Browser cache is only
for immediate display: it never suppresses a background fetch. The server
coalesces requests for 30 seconds, and **Refresh catalog** forces conditional
revalidation with GitHub's ETag. GitHub outages show the last fetched catalog,
or bundled entries before the first successful fetch, with a short status.

Public repositories require no token. While the repository is private, the
server's `GITHUB_TOKEN` supplies access; credentials never enter the browser.
Making the repository public is an owner action, separate from this code.

## Publishing entries without a core release

Edit `packages/catalog.json` on the repository's default branch and commit it.
Keep `format: payesh.package-catalog.v1`; add entries to `items`. Each entry
requires `id`, `name`, `latest_version` and `resource_estimate_source`. Optional
fields include `description`, `repository`, `release`, `dependencies`,
`required_privileges` and decimal-string size estimates. `release` is a core
GitHub release tag (for example `v0.2.11`) or `latest`; `latest_version` is the
package's own version. A package can reference a different owner/repository.
The catalog contains at most 200 entries and is bounded to 1 MiB.

Publish the architecture-specific archive, manifest and detached signature to
the specified release. Default assets are `{id}-linux-{arch}.tar.gz`,
`{id}-linux-{arch}.manifest.json`, and `{id}-linux-{arch}.manifest.sig`.
GitHub, local path and URL installation sources remain available. The entry's
repository/release defaults apply when the user leaves the corresponding fields
empty; explicit source inputs override them.

Discovery is independent from execution. New entries appear on installed
versions without updating the core. `install_supported` is computed by the
server, never trusted from repository JSON. Entries whose ID/version is not
supported by this core show **Requires Payesh update**. New privileged
integrations still need a reviewed executor and trusted signing key; publishing
metadata does not authorize arbitrary executables. Existing signature,
checksum, eligibility and core-compatibility checks remain in place.

## Installing, updating and following package changes

Install from the Packages page or from a server's **Packages** tab. Choosing a
server that already has the package updates it in place: the new archive goes
through the same signature, checksum and eligibility checks, and a package that
was enabled is restarted on the new version. **All connected servers** installs
on each connected server in turn and reports every server's result separately;
one failure does not hide the others, and offline servers are not targeted.

Each server's **Packages** tab lists what is installed with the installed and
catalog versions, and enables, disables, updates or removes a package.
Successful installs and updates, and failures with their reason, are recorded
in that server's logs under the **Package manager** source on the Logs page.
