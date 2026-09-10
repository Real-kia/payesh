# 02 — UI/UX and dashboard

Status: in progress — Checkpoint A fixture preview started 2026-09-09. Planning baseline: 2026-09-08.
Risk/assignment guidance: Medium; balanced builder plus visual review.
Master milestone: M1; production checkpoints during M3–M6.

Read [shared context](00_SHARED_CONTEXT.md) and [the package guide](README.md) before acting. This document is a future build assignment template, not permission to start coding in the documentation-only session.

## Prerequisites and ownership

**Requires:** 01 frontend scaffold/contracts. Isolated layout fixtures may start before contracts are final but cannot dictate conflicting APIs.

**Owns:** UI owner: web/, design-system/payesh/, browser routes/components and tests.

Inspect actual repository conventions before selecting source paths. The lead owns shared contracts, schema ordering, dependency locks and integration unless explicitly delegated. A dependency may be replaced by an isolated test fixture while developing, but not for final integration acceptance.

## Work to implement

1. During an authorized UI build, install/read pinned UI/UX Pro Max as specified below. It is development tooling, never a VPS dependency.
2. Checkpoint A: realistic isolated previews of onboarding, overview and server details/logs, including mobile/light/dark/loading/empty/error/stale states.
3. Use approved contract fixtures and explicit preview adapters. Production must not fall back to fabricated values when an API fails.
4. Checkpoint B after 03/04: real auth, metrics/logs, fleet enrollment and SSH job progress. Checkpoint C after 05: allowances, alerts and incident workflows.
5. Checkpoint D after 06–10: extras, permissions/cost, policy preview/apply/revert, updates and role changes. Preview acceptance is not production completion.
6. Keep one browser-code owner across checkpoints. Backend workers supply endpoints/fixtures/integration requests rather than rewriting routes concurrently.
7. Reconcile durable jobs after refresh; distinguish missing/unsupported/stale from zero, cap chart/log rendering and stop nonessential hidden-tab/live work.

## Acceptance gates

- Visual checks at 375/768/1440px, light/dark, keyboard, 200% zoom and reduced motion; screenshots and measured entry JS+CSS ≤300 KiB compressed.
- Real-API tests cover slow network, expired session, repeated actions, partial fleet failure, stale data and disconnected jobs.
- Fixtures stay preview/test-only. Final acceptance waits for all production checkpoints.

## Out of scope

Backend architecture redesign, kernel controls, heavy decorative runtime or CDN dependency.

Reference extracts below sometimes mention neighboring packages. They explain integration requirements; they do not expand this worker's editable scope. Read-only inspection of neighboring interfaces is expected.

## Handoff and stop condition

Write `docs/handoffs/02.md` using the shared-context handoff checklist: baseline/files, implemented behavior, changed/consumed interfaces, actual test commands/results, measured or unmeasured costs, limitations, and next-package inputs. Request lead acceptance. Do not mark the full milestone/product complete or continue into another package on your own. If a prerequisite is missing, report exactly which contract, implementation or test environment is required.

## Detailed reference requirements

These are relevant verbatim extracts from [the master plan](../PLAN.md) at the planning baseline. Together with shared context, they are part of this assignment's requirements, not optional background. The lead synchronizes affected copies when the master changes.

## 12. UI/UX design workflow, including UI/UX Pro Max

The owner explicitly requested [UI/UX Pro Max](https://github.com/nextlevelbuilder/ui-ux-pro-max-skill) as a design aid. Add it to the development environment in the UI milestone. It is not a runtime dependency, server module, or requirement for Payesh users.

The upstream project supplies design guidance and a generated installation for different coding assistants. Use its current documented installer, pin the selected version, and preserve required data/scripts. Do not copy only a `SKILL.md` while leaving its referenced resources behind. The package currently documents `ui-ux-pro-max-cli`; the old `uipro-cli` name is stale. Use the appropriate assistant target or its universal target, and inspect the dry-run first. Record the exact package version/source revision for reproducibility.

Read the installed skill's complete instructions. Use its dashboard design-system workflow and Svelte guidance. Save the project-specific design decisions under `design-system/payesh/MASTER.md`, with page exceptions only when justified. Follow actual generated resource paths rather than assuming a Claude-specific directory. Python/Node used for design/build tooling must not enter a production server dependency list.

Payesh-specific design brief:

> Design a modern, clear VPS monitoring dashboard for beginners managing 1–20 servers. Prioritize server health, readable numbers, synchronized resource/traffic charts, understandable logs, and safe optional controls. Use a restrained neutral palette with teal accents, accessible light/dark modes, mobile-friendly layouts, and low motion. Prefer clear labels and predictable navigation. Avoid decorative visuals that obscure monitoring data or add significant runtime work.

Treat generated recommendations as suggestions. Do not import an animation library, remote fonts, or a large component suite simply because a search result suggests it. The skill does not establish performance or accessibility compliance by itself.

### UI defaults and screens

- Use CSS variables for semantic colors, spacing, typography, borders, radii, and chart series. Neutral backgrounds, teal primary actions, and distinct warning/error meanings are the provisional visual direction.
- Use a system sans-serif stack and tabular numerals; a system monospace stack for logs/identifiers. Self-host any later custom font and justify its cost.
- Follow system light/dark preference initially; persist an explicit owner choice. Respect reduced-motion preferences. Keep routine transitions short and avoid background animations.
- Desktop navigation: Overview, Alerts, Extras, Settings. Server details contain Metrics, Traffic, Logs, and installed Controls. Collapse the fleet layer gracefully for one standalone server.
- First prototypes: onboarding, fleet overview, and server details with linked charts/logs. Use realistic fixture data, clearly marked as fixtures, for design work only.
- Present concrete previews for owner feedback. If design choices are delegated, proceed using this brief; do not block unrelated backend work waiting for cosmetic preferences.
- Overview shows unhealthy servers first, freshness, a few resource values, and monthly traffic progress. Advanced metrics appear on demand rather than turning the first page into a wall of charts.
- Distinguish healthy, pending, unreachable, stale, installing, failed, disabled, and unsupported states with text/icons as well as color.
- Charts support shared time selection, units, tooltips, gaps, and min/max context. Fetch an appropriate resolution for the displayed interval; never send millions of raw points to the browser.
- Virtualize long logs/lists, cap rendered chart points, lazy-load detailed screens, and pause nonessential subscriptions in hidden tabs. Selecting Live must not permanently enable high-frequency polling after leaving the screen.
- Use inline validation and progress for setup/modules/updates. Destructive control actions need a consequence preview and clear revert action. Normal navigation should not be interrupted by unnecessary confirmation dialogs.
- Preserve enough local UI state for refresh/back navigation. Do not put passwords, keys, API tokens, or log content in shareable URLs.

Visual acceptance: check widths 375, 768, and 1440 pixels, light/dark modes, 200% zoom, keyboard navigation, visible focus, screen-reader labels, contrast, reduced motion, long hostnames/IPv6 addresses, zero/huge values, empty fleets, partial failures, and slow networks. Labels must survive translated/long text even though English is the v1 interface language.

## 6. Installation, onboarding, and secure access

### Direct installation

1. Offer a short documented bootstrap command and a downloadable installer for users who want to inspect it. No placeholder release URLs may ship in the final README.
2. Detect the target and show role selection. Explicit role flags allow unattended installation.
3. Preflight platform capabilities, existing installation, available disk, occupied ports, and required privileges.
4. Download only role-required artifacts; verify release metadata and artifact integrity. Do not compile on the VPS.
5. Create service accounts, directories, service definitions, and restrictive credentials. Re-running the same installer must converge on the existing installation without deleting data or generating a second node identity.
6. Start services and show actual health results. A failed service is not a successful install.
7. Web roles complete onboarding in the browser. Node enrollment commands include a short-lived, single-use pairing token and the expected hub identity/fingerprint.

The first downloaded installer depends on the trustworthiness of its delivery channel. Do not pretend that a checksum downloaded alongside an untrusted installer independently authenticates that installer. Subsequent software trust is anchored by a release-signing key shipped in the installed binaries.

### Web onboarding

- Default to one owner account in v1; one owner can manage the whole fleet. Defer multi-tenant roles.
- Account creation requires the one-time bootstrap secret. Simply visiting an exposed setup page must not allow takeover.
- Guide the owner through access, collection/retention presets, notification setup, and optional server enrollment. Offer defaults and a skip path for nonessential steps.
- Use HTTPS for public dashboards. Support domain certificates and public-IP certificates with automated ACME renewal. [Let's Encrypt documents public IP certificates](https://letsencrypt.org/2026/01/15/6day-and-ip-general-availability.html); validate the selected ACME client's support and renewal behavior during implementation.
- Certificate validation still needs reachable challenge endpoints. If ports are occupied or blocked, offer existing-reverse-proxy configuration or a documented SSH tunnel. Do not silently stop nginx, publish credentials over HTTP, or replace a valid certificate with an untrusted fallback.
- Private/local mode binds to loopback unless deliberately configured otherwise. Existing reverse proxy support must validate trusted proxy addresses and WebSocket forwarding.
- Self-host necessary fonts/assets. Dashboard operation after installation must not depend on a CDN, third-party analytics, or an AI provider.

### Hub-assisted SSH installation

- Collect address, SSH port, username, and password or private key/passphrase. Support root login or sudo with the required credential flow; report the exact stage of failure.
- Verify known host keys. For a new host, show its fingerprint for confirmation; a changed key is an explicit failure, not an automatic acceptance.
- Keep credentials only for the installation job. Do not retain passwords/private keys in the database, logs, local storage, exception messages, or job command strings. Limit their in-memory lifetime.
- Transfer verified artifacts, perform the same idempotent installer steps, enroll the node, then verify real measurements reach the hub.
- After enrollment, use the authenticated agent connection for module installation and Payesh updates. Do not require SSH credentials to remain stored for routine management.
- Timeouts, failed sudo, unavailable package repositories, full disks, and partial uploads need clear job status and safe retry. A partially installed node must not appear healthy.
