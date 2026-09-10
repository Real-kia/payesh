# Payesh build packages — start here

Planning baseline: 2026-09-08. Package 02/M1 has a fixture-only preview checkpoint;
package 03/M2 has **Checkpoint A implemented** with acceptance pending; packages 04–11
remain **not started**. Package 01/M0 foundation scaffolding is **ready for review**;
these files and the status record do not claim that the product or finalized contracts
are accepted.

## One AI or several?

Recommended low-coordination approach: use one balanced main builder sequentially and a stronger model for architecture/security/recovery reviews. One AI can follow every package; splitting the documents does not require multiple agents.

Separate workers can help once contracts are agreed, especially UI versus backend and independent module implementations. More workers are not automatically cheaper: repeated context, conflicting edits and integration repairs can increase total cost. A lower-cost model is best assigned a bounded task with concrete examples and tests, not told to independently design an entire subsystem. Difficulty labels below describe implementation risk, not measured model rankings.

Keep one lead responsible for the whole product. A worker need not read every implementation detail of every package, but it must understand the product, interfaces and its neighbors. Giving an AI only “build CPU limits” without these boundaries invites incompatible or unsafe results.

## Files and authority

- [MEMORY.md](../MEMORY.md): compact owner preferences and current state.
- [PLAN.md](../PLAN.md): canonical full specification and competitor/background research; preserved in full.
- [00_SHARED_CONTEXT.md](00_SHARED_CONTEXT.md): mandatory common architecture, units, interfaces, security, resource budgets and handoff rules.
- The assigned package: bounded tasks, dependencies, file ownership, exclusions, acceptance gates and inline detailed master-plan extracts.

The lead reads the full master. Workers read shared context and their complete assigned package, then inspect applicable repository instructions, the actual approved contracts and dependency handoffs. Relevant detailed requirements are included inline so a worker does not need to hunt through the entire master to perform its package.

Package extracts are planning-baseline copies, not separate authorities. After approved changes, the lead updates the master and all affected extracts/common context in the same documentation change. If anything disagrees, report it; do not silently substitute the easier requirement.

## Package map

| ID / assignment | Risk / recommended handling | Depends on |
| --- | --- | --- |
| [01 — Foundation and shared contracts](01_FOUNDATION.md) | Hard decisions; bounded scaffolding afterward | None |
| [02 — UI/UX and dashboard](02_UI_UX.md) | Medium; balanced builder plus visual review | 01 |
| [03 — Local metrics, storage, logs and CLI](03_LOCAL_MONITORING.md) | Medium–hard; arithmetic/retention review | 01 |
| [04 — Fleet identity, transport and installation](04_FLEET_AND_INSTALLATION.md) | Hard; security review required | 01 + 03 |
| [05 — Traffic allowances, alerts and incidents](05_TRAFFIC_ALERTS_AND_INCIDENTS.md) | Medium–hard; time/accounting review | 03 + 04 |
| [06 — Signed optional-module framework](06_MODULE_FRAMEWORK.md) | Hard; supply-chain/privilege review | 01 + 04 |
| [07 — Per-port traffic accounting](07_PORT_TRAFFIC.md) | Hard; Linux networking review | 03 + 04 + 06 |
| [08 — CPU controls for safe workload targets](08_CPU_CONTROLS.md) | Hard; cgroup/privilege review | 04 + 06 |
| [09 — Bandwidth caps and quota enforcement](09_BANDWIDTH_CONTROLS.md) | Very hard; strongest available reviewer and real Linux tests | 04 + 05 + 06 + 07 |
| [10 — Updates, recovery and role/history migration](10_UPDATES_AND_ROLE_MIGRATIONS.md) | Very hard; strongest available reviewer for data safety | 03–09; design early |
| [11 — Integration, security, performance and release readiness](11_INTEGRATION_AND_RELEASE.md) | Lead/reviewer; bounded docs tasks can use lower-cost model | All, including UI wiring |

## Work order and parallel boundaries

1. **01 first:** establish shared contracts, security/trust boundaries, artifact separation, schema ownership and build targets. Writing these handoffs has not done that work.
2. **02 previews and 03 local monitoring:** may proceed alongside each other after the initial contracts. Otherwise use one main builder sequentially.
3. **04 fleet/install:** consumes 03 ingestion. Revisit 02 to connect real APIs; a fixture dashboard does not pass M3.
4. **05 daily monitoring:** integrate billing periods, alerts and incident workflows, then wire their UI. Keep the original milestone completion gates in order.
5. **06 module framework**, then **07 port accounting and 08 CPU controls** may run in parallel in separate module files. **09 bandwidth controls** consumes accepted accounting/period interfaces and requires focused safety review.
6. **10 update/recovery/role migration:** design its contracts in 01; implement full recovery against real integrated builds after the module foundation. Early installers must already authenticate artifacts.
7. **11 release readiness:** integrate and review throughout, then run the complete final matrix. Revisit 02 for every required production feature before final acceptance.

The UI owner remains responsible for browser code across later checkpoints. Backend workers provide endpoints, realistic state fixtures, and wiring instructions. Do not assign several workers ownership of the same routes.

A worker finishes only its assigned package/checkpoint and hands off. It does not follow the master plan's single-lead “continue next milestone” prompt unless explicitly assigned that broader role.

## How to give a package to another AI

If it has repository access, give it the prompt below and the exact package path. If using a separate chat without repository access, attach:

1. This guide and shared context.
2. Its assigned package.
3. Actual repository instructions and relevant current source files.
4. Approved API/protocol/schema/module contracts and dependency handoffs relevant to its task.
5. Baseline revision, exact file ownership, and the specific checkpoint authorized.

The Markdown package alone is not sufficient to safely modify an unseen existing codebase. Do not attach secrets, SSH keys, signing keys or production logs. If required artifacts do not yet exist, start with 01; do not ask another worker to guess them.

Copyable worker prompt for a later authorized build session:

> Implement only package [ID / file] and checkpoint [name]. First read plans/00_SHARED_CONTEXT.md, the full assigned package, applicable repository instructions, approved contracts and dependency handoffs; inspect the actual code and baseline [revision]. Your editable files are [explicit scope]. Preserve unrelated changes. Propose shared contract/schema/lockfile changes to the lead before editing them. Include the required edge cases and tests; keep fixtures isolated and report unrun Linux checks honestly. Write docs/handoffs/[ID].md with changes, tests, limitations and integration needs. Stop after this assignment and handoff. Do not implement unrelated packages, publish, provision paid infrastructure, or operate on production servers. Keep chat replies short.

Copyable lead prompt:

> Read MEMORY.md, PLAN.md and plans/README.md, inspect the repository, and implement the next explicitly authorized milestone using the package boundaries. Own shared contracts, dependency and migration ordering, integration and acceptance. Maintain docs/IMPLEMENTATION_STATUS.md with evidence and distinguish preview-ready, integrated and accepted. Assign workers only non-overlapping scopes with approved interfaces, review their handoffs and run integration tests. Preserve confirmed requirements and measured resource budgets. Do not treat these planning documents as proof of implementation or permission to publish or operate production systems.

## Acceptance and cost control

- Start with one package/checkpoint, not eleven simultaneous agents. Increase parallelism only when a genuinely independent task is ready.
- Use a balanced builder for ordinary implementation. Give a cheaper model small, well-specified fixtures, documentation or mechanical tests, with review.
- Use focused stronger-model reviews for contracts/identity, kernel accounting/control, supply-chain safety and rollback/migration. A stronger model still needs tests; its approval alone is not proof.
- Reuse approved contracts and handoffs rather than repeating the entire conversation. Avoid switching main implementers halfway through a package without a written handoff.
- Lead acceptance requires inspected changes and reproducible tests, not just worker confidence. Blocked/unverified is different from complete.
- Full v1 includes all three downloadable modules and recovery/role transitions. Earlier monitoring builds can be called prototypes or staged previews, not finished v1.
