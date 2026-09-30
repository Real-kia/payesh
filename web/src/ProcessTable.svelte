<script lang="ts">
  import { onMount } from 'svelte';
  import { ApiError, apiClient, type ProcessSnapshot } from './api';
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
        <td>{process.cpu_percent === null ? '—' : `${process.cpu_percent.toFixed(1)}%`}</td><td>{memory(process.memory_bytes)}</td><td>{rate(process.read_bytes_per_second)}</td><td>{rate(process.write_bytes_per_second)}</td><td>{process.connections ?? '—'}</td><td>{process.threads}</td>
      </tr>{/each}</tbody>
    </table></div>
    {#if !snapshot.items.length}<div class="process-empty">No matching processes</div>{/if}
    <div class="process-footer"><span>{snapshot.items.length} of {snapshot.matched} matches{snapshot.truncated ? ' · Scan limit reached' : ''}</span><span>CPU: 100% = one core · Network: socket counts</span></div>
    {#if snapshot.interval_seconds === 0}<p class="muted process-note">Collecting rate sample…</p>{/if}
    <p class="muted process-note">— = unavailable or waiting for a second sample.</p>
  {/if}
</article>

<style>
  .process-panel { padding: 20px; border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface); }
  .muted { color: var(--muted); }
  .button { padding: 6px 12px; border: 1px solid var(--line); border-radius: 4px; background: transparent; color: var(--muted); font-size: 12px; font-weight: 600; cursor: pointer; }
  .button:hover { background: var(--surface-muted); color: var(--ink); }
  .button:disabled { opacity: .5; cursor: default; }
  .button.primary { background: var(--teal); border-color: var(--teal); color: var(--primary-contrast); }
  .process-heading, .process-actions, .process-footer { display: flex; justify-content: space-between; gap: 12px; align-items: center; }
  h2 { margin: 0 0 6px; font-size: 17px; }
  .process-heading .muted, .process-footer, .process-note { font-size: 12px; }
  .process-toolbar { display: grid; grid-template-columns: minmax(160px, 1fr) 180px; gap: 12px; margin: 20px 0 16px; }
  label { display: grid; gap: 6px; font-size: 12px; color: var(--muted); }
  input, select { width: 100%; padding: 9px 10px; background: var(--surface); color: var(--ink); border: 1px solid var(--line); border-radius: 4px; box-sizing: border-box; }
  .process-scroll { overflow-x: auto; }
  table { border-collapse: collapse; width: 100%; font-size: 12px; font-variant-numeric: tabular-nums; white-space: nowrap; }
  th { text-align: right; color: var(--muted); font-weight: 500; padding: 10px 12px; background: var(--surface-muted); }
  td { text-align: right; padding: 10px 12px; border-bottom: 1px solid var(--line); }
  th:first-child, td:first-child { text-align: left; }
  .process-name { max-width: 220px; overflow: hidden; text-overflow: ellipsis; font-weight: 500; }
  tbody tr:hover { background: var(--surface-muted); }
  .process-empty { padding: 30px 0; text-align: center; color: var(--muted); }
  .process-footer { padding-top: 14px; color: var(--muted); flex-wrap: wrap; }
  .process-note { margin: 10px 0 0; }
  @media (max-width: 640px) { .process-toolbar { grid-template-columns: 1fr; } .process-heading { align-items: flex-start; flex-wrap: wrap; } }
</style>
