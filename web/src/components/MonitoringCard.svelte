<script lang="ts">
  import Icon from '../Icon.svelte';
  import Sparkline from '../Sparkline.svelte';
  import CircleChart from '../CircleChart.svelte';
  import StatusPill from './StatusPill.svelte';
  import { formatNetworkRate } from '../network';
  import { formatHeartbeatInTz } from '../timezone';
  import { clampPercent, currentNetworkRate, displayAddress, formatBytes, metricValue, resourceLevel, roleLabel, sampleAge, stateLabel } from '../lib/format';
  import type { PreviewServer } from '../preview/fixtures';

  let { server, timezone, onSelect }: { server: PreviewServer; timezone: string; onSelect: (server: PreviewServer) => void } = $props();

  const gauges = [
    { label: 'CPU', key: 'cpu', tone: 'teal' },
    { label: 'Memory', key: 'memory', tone: 'purple' }
  ] as const;

  function open(event: KeyboardEvent) {
    if (event.target !== event.currentTarget) return;
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onSelect(server);
    }
  }
</script>

<div class="monitoring-card" role="button" tabindex="0" aria-label={`${server.name}, ${stateLabel(server.displayState)}. Open details`} onclick={() => onSelect(server)} onkeydown={open}>
  <div class="top">
    <div class="identity">
      <h2>{server.name}</h2>
      <small class="mono">{displayAddress(server)} · {roleLabel(server.role)}</small>
    </div>
    <StatusPill state={server.displayState} label={stateLabel(server.displayState)} />
  </div>

  {#if server.connectionState === 'connected'}
    <div class="metrics">
      {#each gauges as gauge (gauge.key)}
        {@const value = server.metrics[gauge.key]}
        {@const history = server.metricHistory?.ranges['15m']?.[gauge.key] ?? []}
        <div class={`gauge ${gauge.tone} ${resourceLevel(value)}`}>
          <div class="gauge-head"><span>{gauge.label}</span><strong class="tabular">{metricValue(value)}</strong></div>
          <div class="spark">
            {#if history.filter((point) => point !== null).length > 1}<Sparkline values={history} tone={gauge.tone} />{:else}<small class="faint">Awaiting history</small>{/if}
          </div>
          <div class="track" role="meter" aria-label={`${gauge.label} usage`} aria-valuemin="0" aria-valuemax="100" aria-valuenow={value ?? undefined} aria-valuetext={value === null ? 'Unavailable' : metricValue(value)}><i style:width={`${clampPercent(value)}%`}></i></div>
        </div>
      {/each}
      <div class="gauge disk">
        <div class="gauge-head"><span>Disk</span></div>
        <div class="disk-ring"><CircleChart value={server.metrics.disk} size={66} strokeWidth={9} label="Used" color="var(--blue)" /></div>
      </div>
      <div class="net"><span><Icon name="download" size={12} /> Download</span><strong class="tabular">{formatNetworkRate(currentNetworkRate(server, 'download'))}</strong></div>
      <div class="net"><span><Icon name="upload" size={12} /> Upload</span><strong class="tabular">{formatNetworkRate(currentNetworkRate(server, 'upload'))}</strong></div>
    </div>
  {:else}
    <div class="offline">
      <Icon name="alert-circle" size={16} />
      <span>{server.freshnessReason || 'Not connected'} — live metrics appear once the server reports.</span>
    </div>
  {/if}

  <div class="bottom">
    <span><Icon name="clock" size={12} /> {sampleAge(server)}</span>
    {#if server.connectionState === 'connected' && server.traffic.allowanceBytes !== '0'}<span>Billing period: {formatBytes(server.traffic.countedBytes)}</span>{/if}
    <span>{server.lastHeartbeat ? formatHeartbeatInTz(server.lastHeartbeat, timezone) : server.freshnessReason || server.connectionState}</span>
    <span class="open">Details <Icon name="chevron-right" size={13} /></span>
  </div>
</div>

<style>
  .monitoring-card {
    display: flex;
    flex-direction: column;
    gap: 16px;
    min-width: 0;
    padding: var(--panel-pad);
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
    cursor: pointer;
    transition: transform var(--dur) var(--ease-out), box-shadow var(--dur) var(--ease-out), border-color var(--dur) var(--ease-out);
  }
  .monitoring-card:hover { transform: translateY(-2px); border-color: color-mix(in srgb, var(--accent) 45%, var(--line)); box-shadow: var(--shadow-md); }
  .monitoring-card:active { transform: translateY(0); }
  .top { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; }
  .identity { min-width: 0; }
  .identity h2 { margin: 0 0 3px; overflow: hidden; font-size: 16px; text-overflow: ellipsis; white-space: nowrap; }
  .identity small { color: var(--muted); font-size: 11.5px; }

  .metrics { display: grid; grid-template-columns: repeat(6, minmax(0, 1fr)); gap: 10px; }
  .gauge, .net { min-width: 0; padding: 12px; border: 1px solid var(--line-light); border-radius: var(--radius-md); background: var(--surface-muted); }
  .gauge { grid-column: span 2; --metric: var(--accent); }
  .gauge.purple { --metric: var(--purple); }
  .gauge.warning { --metric: var(--warning); }
  .gauge.critical { --metric: var(--danger); }
  .gauge-head { display: flex; align-items: baseline; justify-content: space-between; gap: 6px; }
  .gauge-head span, .net span { display: inline-flex; align-items: center; gap: 5px; color: var(--muted); font-size: 12px; }
  .gauge-head strong { color: var(--metric); font-size: 17px; font-weight: 650; }
  .spark { display: grid; place-items: center; height: 38px; margin: 10px 0 8px; }
  .track { height: 4px; overflow: hidden; border-radius: var(--radius-pill); background: var(--line); }
  .track i { display: block; height: 100%; border-radius: inherit; background: var(--metric); transition: width var(--dur-slow) var(--ease-out); }
  .gauge.disk { display: flex; flex-direction: column; }
  .disk-ring { display: grid; flex: 1; place-items: center; padding-top: 4px; }
  .net { grid-column: span 3; display: flex; align-items: center; justify-content: space-between; gap: 8px; }
  .net strong { font-size: 14px; font-weight: 600; }

  .offline { display: flex; align-items: center; gap: 10px; padding: 14px; border: 1px dashed var(--line-strong); border-radius: var(--radius-md); color: var(--muted); font-size: 13px; }

  .bottom { display: flex; flex-wrap: wrap; align-items: center; gap: 6px 16px; margin-top: auto; color: var(--muted); font-size: 12px; }
  .bottom span { display: inline-flex; align-items: center; gap: 5px; }
  .open { margin-left: auto; color: var(--accent); font-weight: 600; transition: transform var(--dur) var(--ease-out); }
  .monitoring-card:hover .open { transform: translateX(3px); }

  @media (max-width: 520px) {
    .metrics { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    .gauge { grid-column: span 1; }
    .gauge.disk { grid-column: span 2; }
    .net { grid-column: span 1; flex-direction: column; align-items: flex-start; }
  }
</style>
