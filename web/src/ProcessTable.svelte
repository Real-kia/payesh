<script lang="ts">
  import { onMount } from 'svelte';
  import { ApiError, apiClient, type ProcessSnapshot } from './api';
  import { PREVIEW_MODE } from './lib/env';
  import { previewProcesses } from './preview/fixtures';
  export let serverId: string;
  export let onPackages: () => void;
  let snapshot: ProcessSnapshot | null = null;
  let search = '';
  let sort = 'cpu';
  let error = '';
  let packageDisabled = false;
  let busy = false;
  let paused = false;
  let mounted = false;
  let controller: AbortController | null = null;
  let debounce: ReturnType<typeof setTimeout> | null = null;

  async function load(): Promise<void> {
    controller?.abort();
    const current = new AbortController(); controller = current; busy = true;
    if (PREVIEW_MODE) {
      const query = search.trim().toLowerCase();
      const items = previewProcesses.filter((process) => !query || process.name.toLowerCase().includes(query) || String(process.pid).includes(query));
      const key = (process: (typeof previewProcesses)[number]): number => sort === 'memory' ? Number(process.memory_bytes) : sort === 'read' ? process.read_bytes_per_second ?? -1 : sort === 'write' ? process.write_bytes_per_second ?? -1 : sort === 'connections' ? process.connections ?? -1 : sort === 'pid' ? -process.pid : process.cpu_percent ?? -1;
      snapshot = { sampled_at: new Date().toISOString(), interval_seconds: 3, total: previewProcesses.length, matched: items.length, truncated: false, network_accounting: 'connections-only', items: [...items].sort((a, b) => key(b) - key(a)) };
      error = ''; busy = false;
      return;
    }
    try {
      const result = await apiClient.processes(serverId, sort, search, { signal: current.signal });
      if (current.signal.aborted) return;
      snapshot = result; error = ''; packageDisabled = false;
    } catch (cause) {
      if (current.signal.aborted) return;
      snapshot = null; error = cause instanceof Error ? cause.message : 'Could not load processes';
      packageDisabled = cause instanceof ApiError && cause.code === 'package_disabled';
    } finally { if (controller === current) busy = false; }
  }

  $: if (mounted) { search; sort; if (debounce) clearTimeout(debounce); debounce = setTimeout(() => void load(), 250); }

  onMount(() => {
    mounted = true;
    const timer = setInterval(() => { if (!paused && !busy && !document.hidden) void load(); }, 3000);
    return () => { mounted = false; controller?.abort(); clearInterval(timer); if (debounce) clearTimeout(debounce); };
  });

  function memory(text: string): string {
    let value: bigint; try { value = BigInt(text); } catch { return '—'; }
    const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB']; let divisor = 1n; let index = 0;
    while (value >= divisor * 1024n && index < units.length - 1) { divisor *= 1024n; index++; }
    const tenths = value * 10n / divisor;
    return `${tenths / 10n}${index ? `.${tenths % 10n}` : ''} ${units[index]}`;
  }
  // Share of the largest resident set in this snapshot, for the inline bar.
  function memoryShare(text: string): number {
    const largest = Math.max(1, ...(snapshot?.items ?? []).map((item) => Number(item.memory_bytes) || 0));
    return Math.min(100, ((Number(text) || 0) / largest) * 100);
  }
  function rate(value: number | null): string {
    if (value === null) return '—';
    const units = ['B/s', 'KiB/s', 'MiB/s', 'GiB/s']; let index = 0;
    while (value >= 1024 && index < units.length - 1) { value /= 1024; index++; }
    return `${value.toFixed(index ? 1 : 0)} ${units[index]}`;
  }
</script>

<article class="panel process-panel">
  <div class="process-heading">
    <div><h2>Processes</h2><span class="muted">{snapshot ? `${snapshot.total} processes · ${paused ? 'Paused' : `Sampled ${new Date(snapshot.sampled_at).toLocaleTimeString()}`}` : 'Advanced Process Monitoring'}</span></div>
    <div class="process-actions"><button class="button ghost small" type="button" on:click={() => { paused = !paused; if (!paused) void load(); }}>{paused ? 'Resume' : 'Pause'}</button><button class="button ghost small" type="button" disabled={busy} on:click={() => void load()}>Refresh</button></div>
  </div>
  <div class="process-toolbar">
    <label>Search<input bind:value={search} placeholder="Process name or PID" /></label>
    <label>Sort by<select bind:value={sort}><option value="cpu">CPU usage</option><option value="memory">Memory</option><option value="read">Disk read</option><option value="write">Disk write</option><option value="connections">Sockets</option><option value="pid">PID</option></select></label>
  </div>
  {#if error}
    <div class="process-empty" role="status"><p>{error}</p>{#if packageDisabled}<button class="button primary small" type="button" on:click={onPackages}>Open Packages</button>{:else}<button class="button ghost small" type="button" disabled={busy} on:click={() => void load()}>Retry</button>{/if}</div>
  {:else if !snapshot}
    <div class="process-empty" role="status">Loading processes…</div>
  {:else}
    <div class="process-scroll"><table>
      <thead><tr><th>Process</th><th>PID</th><th>UID</th><th>State</th><th>CPU</th><th>Memory</th><th>Disk read</th><th>Disk write</th><th>Sockets</th><th>Threads</th></tr></thead>
      <tbody>{#each snapshot.items as process (process.pid)}<tr>
        <td class="process-name" title={process.name}>{process.name}</td><td>{process.pid}</td><td>{process.uid ?? '—'}</td><td>{process.state}</td>
        <td class="bar" style:--p={Math.min(100, process.cpu_percent ?? 0)}>{process.cpu_percent === null ? '—' : `${process.cpu_percent.toFixed(1)}%`}</td><td class="bar memory" style:--p={memoryShare(process.memory_bytes)}>{memory(process.memory_bytes)}</td><td>{rate(process.read_bytes_per_second)}</td><td>{rate(process.write_bytes_per_second)}</td><td>{process.connections ?? '—'}</td><td>{process.threads}</td>
      </tr>{/each}</tbody>
    </table></div>
    {#if !snapshot.items.length}<div class="process-empty">No matching processes</div>{/if}
    <div class="process-footer"><span>{snapshot.items.length} of {snapshot.matched} matches{snapshot.truncated ? ' · Scan limit reached' : ''}</span><span>CPU: 100% = one core · Network: socket counts</span></div>
    {#if snapshot.interval_seconds === 0}<p class="muted process-note">Collecting rate sample…</p>{/if}
    <p class="muted process-note">— = unavailable or waiting for a second sample.</p>
  {/if}
</article>

<style>
  .process-panel { padding: var(--panel-pad); border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface); box-shadow: var(--shadow); animation: fade-in var(--dur) var(--ease-out); }
  .process-heading, .process-actions, .process-footer { display: flex; justify-content: space-between; gap: 12px; align-items: center; }
  .process-actions { gap: 8px; }
  h2 { margin: 0 0 4px; font-size: 16px; }
  .process-heading .muted, .process-footer, .process-note { font-size: 12px; }
  .process-toolbar { display: grid; grid-template-columns: minmax(160px, 1fr) 200px; gap: 12px; margin: 18px 0 14px; }
  label { display: grid; gap: 6px; color: var(--muted); font-size: 12px; font-weight: 550; }
  input, select { width: 100%; }
  .process-scroll { overflow-x: auto; border: 1px solid var(--line); border-radius: var(--radius-md); }
  table { width: 100%; border-collapse: collapse; font-size: 12.5px; font-variant-numeric: tabular-nums; white-space: nowrap; }
  th { position: sticky; top: 0; padding: 9px 12px; background: var(--surface-muted); color: var(--muted); font-size: 11.5px; font-weight: 600; letter-spacing: 0.02em; text-align: right; text-transform: uppercase; }
  td { padding: 9px 12px; border-top: 1px solid var(--line-light); text-align: right; }
  th:first-child, td:first-child { text-align: left; }
  .process-name { max-width: 240px; overflow: hidden; font-weight: 550; text-overflow: ellipsis; }
  tbody tr { transition: background-color var(--transition-fast); }
  /* A thin usage bar under the value, right-aligned with the number. */
  td.bar { --bar: var(--accent); background: linear-gradient(var(--bar), var(--bar)) right 12px bottom 5px / calc((100% - 24px) * var(--p) / 100) 2px no-repeat; }
  td.bar.memory { --bar: var(--purple); }
  tbody tr:hover { background: var(--surface-hover); }
  .process-empty { padding: 30px 0; color: var(--muted); text-align: center; }
  .process-footer { flex-wrap: wrap; padding-top: 12px; color: var(--muted); }
  .process-note { margin: 10px 0 0; }
  @media (max-width: 640px) { .process-toolbar { grid-template-columns: 1fr; } .process-heading { flex-wrap: wrap; align-items: flex-start; } }
</style>
