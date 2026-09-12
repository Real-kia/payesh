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
