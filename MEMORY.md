# Payesh memory

Updated: 2026-09-11. Keep this file under roughly 500 words; revise instead of appending a transcript. Start with [plans/README.md](plans/README.md) for AI assignments; [PLAN.md](PLAN.md) is the full specification; [PROJECT_STATE.md](PROJECT_STATE.md) and [docs/IMPLEMENTATION_STATUS.md](docs/IMPLEMENTATION_STATUS.md) carry current implementation evidence — do not duplicate it here.

## Confirmed preferences and scope

- Keep chat short and detailed evidence in project documents. Work in package
  order; do not publish, deploy, install skills, or operate production systems
  without permission.
- Payesh is lightweight open-source Linux VPS monitoring for beginners and
  experienced owners, from standalone/CLI-only to hub/node fleets. Resource
  overhead is the top priority, then simple installation and a polished UI.
- Monitor resources, network rates/totals and useful logs with configurable
  sampling, retention and cleanup. Preserve history through reversible roles.
- Nodes install no UI or unnecessary artifacts. Port Traffic, CPU Controls and
  Bandwidth Controls are separately downloaded v1 modules; RAM limits are
  deferred.
- Support Ubuntu/Debian, Fedora, Rocky/AlmaLinux and Alpine on 1–20 VPSs.
  Provide direct and hub-assisted password/key SSH installation and
  owner-triggered updates. Use UI/UX Pro Max only during UI development.

## Engineering defaults recorded in PLAN.md

- Go; SQLite; Svelte/TypeScript/uPlot. These are engineering defaults.
- Node opens one persistent authenticated connection; hub sends requests/actions through it. No claim that connection direction itself saves resources.
- Standard sampling 15 seconds, economy 60 seconds; configurable 5 seconds–1 hour.
- Downsample aged metrics, cap storage and expire snapshots without changing
  source-log rotation. Use a typed privileged helper, signed artifacts, staged
  upgrades, independent rollback and management-traffic protection.
- Deferred: OS updates, RAM limits, centralized logs, container integrations,
  synthetic probes and read-only MCP.

## Handoff state

- PLAN.md contains competitor comparisons, design-skill workflow, interfaces, installation roles, controls, migration, updates, benchmarks and ordered milestones.
- plans/ splits implementation into 11 packages plus mandatory shared context. Workers read shared context, their package, approved contracts and dependency handoffs; one lead integrates/reviews.
- Status as of this update: package 01/M0 is ready for review; package 02 has a fixture-only preview; packages 03–06 and 08 have Checkpoint A implemented; package 07 has an accounting/nftables checkpoint; package 09 code implementation is complete with Linux acceptance still pending; packages 10–11 are unstarted. See `docs/handoffs/*.md`. Package 04 enrollment production, transport, and installers remain incomplete. Optional control routes fail closed unless their module executors are configured.
- Repo git history starts fresh at this session (origin set to the GitHub URL above; not yet pushed — no GitHub auth in this environment).
- Keep confirmed requirements distinct from defaults; consult the plan before changing scope.
