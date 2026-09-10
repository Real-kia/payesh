# Package 02 review resolution

Updated: 2026-09-09

The UI review loop covered the Checkpoint A acceptance items and the reported
implementation gaps. Resolutions:

- Moved fixtures to `web/src/preview/fixtures.ts` behind an explicit dynamic
  preview adapter. Production builds show an API-required state and a scan
  confirms fixture names are absent from the normal bundle.
- Added valid contract-shaped server IDs, decimal-string traffic values,
  connection/freshness metadata, and all eight required display states. The
  overview sorts attention states before healthy servers.
- Kept every mobile navigation item reachable in the horizontal compact nav;
  added local-storage/history restoration for page, server, tab, and theme.
- Replaced the decorative resource chart with uPlot. Time-range selection now
  changes bounded datasets and exposes cursor values, units, min/max context,
  gap summary, and an accessible chart label.
- Added system-theme initialization, explicit theme persistence, reduced-motion
  handling, and explicit loading/empty/error states with no fixture fallback.
- Expanded onboarding into validated access, retention/notification defaults,
  and optional pairing-token enrollment with a skip path. Secrets/tokens never
  enter local storage or URLs.
- Added static preview-contract checks to `web/build-check.mjs` so required
  states, production gating, chart/accessibility, and onboarding markers remain
  covered in CI.

## Verification

```text
make web-check
```

Passes with zero `svelte-check` errors/warnings and a successful Vite build.
The production artifact is 42.53 KiB JavaScript plus 4.31 KiB CSS gzip. A
browser surface is unavailable in this environment, so screenshots, 200% zoom,
and manual assistive-technology checks remain an explicit follow-up before
production checkpoint acceptance.

## Second review pass

- Raised pairing-token validation and the native input constraint to the
  approved 16-character minimum.
- Moved all metric histories into the dynamic preview fixture adapter. The
  chart now receives selected-server data, suppresses unavailable histories,
  reflects gaps and current values, and uses readable elapsed-time axis labels.
- Filtered detail tabs by advertised server capabilities and added explicit
  unavailable reasons for unsupported metrics, traffic, and logs.
- Made failed preview imports retry the loader and prevented a failed import
  from being changed into a fake ready fleet.
- Removed the hard-coded owner/time copy from the non-preview screen, persisted
  only explicit theme choices, and persisted state restored through popstate.
- Resolved uPlot canvas colors from computed theme variables and remounts the
  chart on theme changes. Updated semantic light/dark colors and status
  backgrounds to meet the small-text contrast target.
- Made metric sparklines use the selected server's bounded histories and added
  wrapping for long server names.

`npm run check`, `npm run build`, the production fixture/identity scan, and the
static preview-contract checks pass after this second pass.
