# Payesh web foundation

The dashboard build graph is reserved for the web role and is not included in
the agent artifact. Svelte/uPlot/Vite/TypeScript versions are pinned in
`package.json` and `package-lock.json`.

The dashboard uses the authenticated `/api/v1` read contract by default. The
browser sends the `payesh_session` cookie with every request and keeps API
decimal counters/revisions as strings. It loads the server page, server
details, metric ranges, traffic periods, and bounded log snapshots. A 401 is
shown as an explicit expired-session state; network and 5xx failures can be
retried, and in-flight requests are cancelled when a newer load or navigation
replaces them.

Production owner flows are wired to the implemented mutation contract. The
first-run setup consumes `/api/v1/setup` once and signs in through
`/api/v1/session`; the in-memory CSRF token returned by login is sent on
logout, label changes, enrollment/revocation, job cancellation, updates, and
SSH-install requests. Setup and pairing secrets, SSH passwords, and private
keys are not placed in local storage and are cleared after successful use.
Enrollment targets an existing pending server identity and reports the honest
unsupported state when none exists. Durable enrollment, update, install, and
revocation jobs are polled with bounded retries and can be cancelled while
queued/running. The UI keeps decimal job/revision fields as strings and shows
terminal failures, conflicts, expired sessions, and unavailable optional
server routes instead of substituting fixtures. Optional backend services may
still return their documented 404/503 states; those are surfaced as action
errors rather than treated as success.

For a deterministic visual preview, set `VITE_PAYESH_PREVIEW=true`. Preview
fixtures are then loaded from `src/preview/fixtures.ts`; development mode no
longer silently substitutes fixtures for an unavailable API. Set
`VITE_PAYESH_API_BASE` when the API is mounted somewhere other than
`/api/v1`.

## Structure

- `src/App.svelte` is the shell: navigation, routing, session state, fleet
  data loading and the shared dialogs.
- `src/pages/` holds one component per page; `pages/server/` and
  `pages/settings/` hold the server detail tabs and settings sections. Pages
  that own their data (logs, settings sections, traffic usage) load it
  themselves and stop their timers when they close.
- `src/components/` holds shared pieces such as the sidebar, server table,
  status pill and package dialog.
- `src/styles/theme.css` defines every colour, radius, shadow and motion token
  for the light and dark themes and the accent palettes; `src/styles/ui.css`
  defines shared primitives (buttons, panels, pills, forms, skeletons).
  Component styles use only these tokens.
- `public/theme-init.js` applies the saved appearance before the bundle loads,
  so the first frame already has the right theme.
- Views that are not needed for the first screen (server charts, logs,
  settings sections, the package dialog, install progress) are loaded on
  demand through the `lazy` table in `App.svelte`, keeping the initial bundle
  small. If a chunk cannot be fetched, for example after the dashboard was
  updated underneath an open tab, the view offers a reload.

Motion is CSS only and kept cheap: entrance and hover effects animate
`opacity` and `transform`. Only transient states loop (spinners, installs,
running jobs, followed logs); steady states such as a healthy server stay
still, and the live indicator pulses once per data refresh, so an idle
dashboard lets the browser stop drawing frames. Both the operating system's
reduced-motion setting and Settings → General → Animations → Reduced turn
motion off.
