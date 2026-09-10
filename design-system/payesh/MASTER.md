# Payesh UI design system

Status: M1 Checkpoint A preview only. Updated 2026-09-09.

## Product direction

Payesh helps beginners understand the health of one to twenty Linux VPSs without
mistaking missing or delayed measurements for healthy zeroes. The interface is
calm, dense enough for monitoring, and explicit whenever data is stale,
unavailable, pending, unsupported, or fixture-only.

## Foundations

- System sans-serif type with tabular-friendly metric styling; system monospace
  for addresses, log sources, and cursors. No remote font requests.
- Neutral surfaces with teal actions/healthy state, amber for attention, muted
  gray for pending, and text plus color for every status.
- Semantic CSS variables own surfaces, text, borders, spacing, and chart series.
  The initial dashboard honors light and dark mode and reduces motion under the
  OS reduced-motion setting.
- Compact 6–10px radius, short only-on-interaction transitions, visible focus
  rings, and familiar native controls keep the UI cheap and accessible.

## Information architecture

Desktop navigation is Overview, Alerts, Extras, and Settings. A selected server
opens Metrics, Traffic, and Logs. The overview prioritizes stale/unhealthy
machines, then freshness, basic resource values, and monthly traffic. This
Checkpoint A uses local fixtures clearly labeled in the interface and never
pretends to be live data.

## State rules

- Missing, stale, pending, unsupported, and unreachable are distinct from zero.
- Traffic includes an explicit billing timezone and continuity label.
- Logs show source and cursor metadata; live mode is a request, not background
  polling that survives leaving the screen.
- API failures receive their own state instead of falling back to fixture values
  in a production path. The preview state selector exists solely for review.

## Responsive rules

At desktop, the sidebar stays visible and cards form three/two-column grids. At
tablet, page gutters tighten. At phone width, the current page remains visible
in compact navigation; summary and detail grids become a single column, metric
columns are omitted from server rows, and log cursors are hidden only visually.
Long hostnames use clipping or wrapping rather than overflowing the layout.

## Deferred until integrated checkpoints

Real authentication, API adapters, server enrollment, virtualized logs,
durable-job reconciliation, live event backpressure, chart point capping, and
full keyboard/assistive-technology test coverage need the package 03/04 inputs.
The Checkpoint A preview already uses uPlot for bounded fixture charts; it is
not yet connected to persisted rollups or live subscriptions.
