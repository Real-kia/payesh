<script lang="ts">
  import { serverLabel } from '../lib/flags';
  import Icon from '../Icon.svelte';
  import Sparkline from '../Sparkline.svelte';
  import StatusPill from './StatusPill.svelte';
  import { formatNetworkRate } from '../network';
  import { allAddresses, clampPercent, currentNetworkRate, displayAddress, formatBytes, metricValue, resourceLevel, sampleAge, stateLabel } from '../lib/format';
  import type { PreviewServer } from '../preview/fixtures';

  let { server, onSelect }: { server: PreviewServer; timezone?: string; onSelect: (server: PreviewServer) => void } = $props();

  const rows = [
    { label: 'CPU', key: 'cpu', tone: 'teal', spark: true },
    { label: 'Memory', key: 'memory', tone: 'purple', spark: true },
    { label: 'Disk', key: 'disk', tone: 'blue', spark: false },
    { label: 'Swap', key: 'swap', tone: 'blue', spark: false }
  ] as const;

  function open(event: KeyboardEvent) {
    if (event.target !== event.currentTarget) return;
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onSelect(server);
    }
  }
</script>

<div class="card" role="button" tabindex="0" aria-label={`${serverLabel(server)}, ${stateLabel(server.displayState)}. Open details`} onclick={() => onSelect(server)} onkeydown={open}>
  <div class="top">
    <div class="identity">
      <h2>{serverLabel(server)}</h2>
      <small><span class="mono">{displayAddress(server)}{#if allAddresses(server).length > 1}<span class="more-addresses" title={allAddresses(server).join(', ')}> +{allAddresses(server).length - 1}</span>{/if}</span>{#each (server.tags ?? []).slice(0, 3) as tag (tag)}<span class="tag">{tag}</span>{/each}</small>
    </div>
    <StatusPill state={server.displayState} label={stateLabel(server.displayState)} />
  </div>

  {#if server.connectionState === 'connected'}
    <div class="rows">
      {#each rows as row (row.key)}
        {@const value = server.metrics[row.key]}
        {@const history = server.metricHistory?.ranges['15m']?.[row.key] ?? []}
        <div class={`row ${row.tone} ${resourceLevel(value)}`}>
          <span class="label">{row.label}</span>
          <span class="bar" role="meter" aria-label={`${row.label} usage`} aria-valuemin="0" aria-valuemax="100" aria-valuenow={value ?? undefined}><i style:width={`${clampPercent(value)}%`}></i></span>
          {#if row.spark}<span class="spark">{#if history.filter((point) => point !== null).length > 1}<Sparkline values={history} tone={row.tone} />{/if}</span>{:else}<span class="spark"></span>{/if}
          <strong class="tabular">{metricValue(value)}</strong>
        </div>
      {/each}
    </div>
    <div class="net tabular">
      <span><Icon name="download" size={11} />{formatNetworkRate(currentNetworkRate(server, 'download'))}</span>
      <span><Icon name="upload" size={11} />{formatNetworkRate(currentNetworkRate(server, 'upload'))}</span>
    </div>
  {:else}
    <div class="offline"><Icon name="alert-circle" size={14} /><span>{server.freshnessReason || 'Not connected'}</span></div>
  {/if}

  <div class="bottom">
    <span><Icon name="clock" size={11} /> {sampleAge(server)}</span>
    {#if server.connectionState === 'connected' && server.traffic.allowanceBytes !== '0'}<span>Billing {formatBytes(server.traffic.countedBytes)}</span>{/if}
  </div>
</div>

<style>
  .card { display: flex; flex-direction: column; gap: 10px; min-width: 0; padding: 12px 14px; border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface); cursor: pointer; transition: border-color var(--dur) var(--ease-out), box-shadow var(--dur) var(--ease-out); }
  .card:hover { border-color: color-mix(in srgb, var(--accent) 45%, var(--line)); box-shadow: var(--shadow); }
  .top { display: flex; align-items: flex-start; justify-content: space-between; gap: 10px; }
  .identity { min-width: 0; }
  .identity h2 { margin: 0 0 2px; overflow: hidden; font-size: 14.5px; text-overflow: ellipsis; white-space: nowrap; }
  .identity small { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 6px; color: var(--muted); font-size: 11px; }
  .tag { padding: 0 6px; border: 1px solid var(--line); border-radius: var(--radius-pill); font-size: 10.5px; line-height: 16px; }

  .rows { display: grid; gap: 5px; }
  .row { display: grid; grid-template-columns: 52px 1fr 44px 52px; align-items: center; gap: 8px; --metric: var(--accent); font-size: 12px; }
  .row.purple { --metric: var(--purple); }
  .row.blue { --metric: var(--blue); }
  .row.warning { --metric: var(--warning); }
  .row.critical { --metric: var(--danger); }
  .label { color: var(--muted); }
  .bar { height: 5px; overflow: hidden; border-radius: var(--radius-pill); background: var(--line); }
  .bar i { display: block; height: 100%; border-radius: inherit; background: var(--metric); transition: width var(--dur-slow) var(--ease-out); }
  .spark { display: block; height: 16px; }
  .row strong { color: var(--metric); font-size: 12.5px; font-weight: 650; text-align: right; }

  .net { display: flex; gap: 14px; color: var(--ink); font-size: 12px; }
  .net span { display: inline-flex; align-items: center; gap: 4px; }
  .offline { display: flex; align-items: center; gap: 8px; padding: 8px 10px; border: 1px dashed var(--line-strong); border-radius: var(--radius-md); color: var(--muted); font-size: 12px; }
  .bottom { display: flex; flex-wrap: wrap; gap: 4px 14px; margin-top: auto; color: var(--muted); font-size: 11px; }
  .bottom span { display: inline-flex; align-items: center; gap: 4px; }
  .more-addresses { color: var(--muted); font-weight: 600; }
</style>
