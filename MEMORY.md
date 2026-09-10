# Payesh memory

Updated: 2026-09-10. Keep this file under roughly 500 words; revise instead of appending a transcript. Start with [plans/README.md](plans/README.md) for AI assignments; [PLAN.md](PLAN.md) is the full specification; [PROJECT_STATE.md](PROJECT_STATE.md) and [docs/IMPLEMENTATION_STATUS.md](docs/IMPLEMENTATION_STATUS.md) carry current implementation evidence — do not duplicate it here.

## Confirmed preferences and scope

- Keep chat replies short; put extensive explanations in project documents.
- Continue only in the package order and scope requested; do not publish, deploy, install skills, or operate production systems without asking.
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
- plans/ splits implementation into 11 packages plus mandatory shared context. Workers read shared context, their package, approved contracts and dependency handoffs; one lead integrates/reviews.
- Status as of this update: package 01/M0 ready for stronger-model review; package 02 has a fixture-only Checkpoint A preview; packages 03–06 and 08 have Checkpoint A implemented, each with acceptance still pending (see per-package `docs/handoffs/*.md` and `docs/IMPLEMENTATION_STATUS.md` for exact gaps); packages 07, 09–11 unstarted. A discovered OpenAPI contract mismatch (async job-wrapped `/policies`+`/modules` sketch vs. the synchronous idempotency-keyed routes actually built for 05/06/08) has been reconciled; see `docs/handoffs/06.md`/`08.md`.
- Repo git history starts fresh at this session (origin set to the GitHub URL above; not yet pushed — no GitHub auth in this environment).
- Keep confirmed requirements distinct from defaults; consult the plan before changing scope.
