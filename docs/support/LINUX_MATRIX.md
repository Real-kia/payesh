# Linux support matrix (foundation CI)

Package 01 records these explicit representatives for compatibility checks:

| Family | Oldest representative | Current representative |
| --- | --- | --- |
| Ubuntu | 22.04 | 24.04 |
| Debian | 12 | 13 |
| Fedora | 42 | 43 |
| Rocky Linux | 9 | 10 |
| AlmaLinux | 9 | 10 |
| Alpine/OpenRC | 3.22 | 3.23 |

Ubuntu runs the Go test/lint and amd64/arm64 build matrix. The
`linux-distributions` CI job pulls the pinned distribution images, verifies
their release metadata and package-manager/init surfaces, and launches the
minimal Linux agent smoke artifact. It does not claim that privileged service, cgroup,
firewall, or OpenRC behavior is proven; those require disposable Linux VM
tests in the package that owns the behavior. No distribution outside this
matrix is a release gate.
