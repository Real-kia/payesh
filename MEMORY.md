# Payesh memory

Updated: 2026-09-09. Keep this file under roughly 500 words; revise instead of appending a transcript. Start with [plans/README.md](plans/README.md) for AI assignments; [PLAN.md](PLAN.md) is the full specification.

## Confirmed preferences and scope

- Keep chat replies short; put extensive explanations in project documents.
- Current authorization: package 02 UI/UX Checkpoint A was explicitly started on 2026-09-09 after package 01 foundation work. Continue only in the package order and scope requested; do not publish, deploy, install skills, or operate production systems.
- Project: Payesh, open-source Linux VPS monitoring. Repo: https://github.com/Real-kia/payesh
- Audience: VPS owners, including beginners, with one or multiple servers. GitHub popularity is a goal, not guaranteed.
- Highest priority: low resource overhead, followed by simple setup/use and modern, polished, lightweight UI/charts.
- Monitor resources, network rates, traffic totals and useful logs.
- Support standalone, hub, node and CLI-only operation, with reversible role changes and preserved history.
- Nodes must not install web assets or other unnecessary components.
- Extras are actually downloaded/installed on demand through panel buttons or CLI.
- CPU and bandwidth enforcement belong in v1 as optional modules, alongside per-port traffic accounting. RAM limits are not a current priority.
- Include basic existing-service-log access and bounded incident snapshots. Sampling, history retention and automatic deletion of Payesh-owned data must be configurable.
- Support broad Linux families: Ubuntu/Debian, Fedora, Rocky/AlmaLinux and Alpine; target/test 1–20 VPSs without a product limit.
- Offer one-command installs and hub-assisted SSH installs with password/key.
- Plan updates from the beginning: default notification followed by owner-triggered one-click updates; optional scheduled patch updates.
- Add UI/UX Pro Max to the future development workflow: https://github.com/nextlevelbuilder/ui-ux-pro-max-skill
- Owner wants a detailed plan suitable for a less capable AI, including edge cases and acceptance checks.

## Engineering defaults recorded in PLAN.md

- Go; SQLite for server/history; Svelte/TypeScript and uPlot. These are implementation defaults, not individually negotiated preferences.
- Node opens one persistent authenticated connection; hub sends requests/actions through it. No claim that connection direction itself saves resources.
- Standard sampling 15 seconds, economy 60 seconds; configurable 5 seconds–1 hour.
- Downsample aged metrics; cap storage and expire snapshots. Do not arbitrarily sample application log events or alter their original rotation policies.
- Privileged typed helper, signed module/releases, compatible staged upgrades, independent rollback, and management-traffic protection.
- Future work: OS updates, RAM limits, centralized continuous log archives, container integrations, synthetic probes and read-only MCP.

## Handoff state

- PLAN.md contains competitor comparisons, design-skill workflow, interfaces, installation roles, controls, migration, updates, benchmarks and ordered milestones.
- plans/ splits implementation into 11 packages plus mandatory shared context. Workers read shared context, their package, approved contracts and dependency handoffs; one lead integrates/reviews. Package 01/M0 is ready for stronger-model review; package 02 has a fixture-only Checkpoint A preview and packages 03–11 remain unstarted.
- Package 01 foundation scaffold is ready for stronger-model review: fail-fast Go artifact targets; node-vs-hub metric contracts; typed module/hello/audit records; full bounded/authenticated OpenAPI surface; JCS/Ed25519 release format; protocol/data-model docs; fixtures; parsed contract checks; CI distro smoke matrix; npm lockfile; minimal Svelte shell; ownership ADR; and review-resolution record. Feature behavior remains unimplemented. No skill installation has been performed.
- Package 02 preview supplies responsive light/dark onboarding, overview, and server detail fixtures with explicit unavailable/stale/loading/error states. It has no API/auth/live-data integration; see `docs/handoffs/02.md` and `design-system/payesh/MASTER.md`.
- Keep confirmed requirements distinct from defaults; consult the plan before changing scope.
