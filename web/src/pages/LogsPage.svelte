<script lang="ts">
  import { onDestroy, untrack } from 'svelte';
  import Icon from '../Icon.svelte';
  import LoadingSkeleton from '../components/LoadingSkeleton.svelte';
  import { ApiError, apiClient } from '../api';
  import { formatLogTimeInTz } from '../timezone';
  import { PREVIEW_MODE } from '../lib/env';
  import { generatePreviewFleetLogs, resolveLogLevel, type FleetLogEntry } from '../lib/logs';
  import type { PreviewServer } from '../preview/fixtures';

  let { servers, timezone, onAuthExpired }: { servers: PreviewServer[]; timezone: string; onAuthExpired: () => void } = $props();

  type LoadState = 'loading' | 'ready' | 'empty' | 'error';
  type LogWindow = '1h' | '6h' | '24h' | '7d';
  type Level = 'all' | 'error' | 'warn' | 'info';

  let serverFilter = $state('all');
  let levelFilter = $state<Level>('all');
  let search = $state('');
  let logWindow = $state<LogWindow>('24h');
  let fetchLimit = $state(100);
  let page = $state(0);
  let pageSize = $state(100);
  let follow = $state(false);

  let entries = $state<FleetLogEntry[]>([]);
  let loadState = $state<LoadState>('loading');
  let error = $state('');
  let partial = $state('');
  let loadedAt = $state<Date | null>(null);
  let controller: AbortController | null = null;
  let followTimer: number | undefined;

  const counts = $derived({
    error: entries.filter((entry) => entry.level === 'ERROR').length,
    warn: entries.filter((entry) => entry.level === 'WARN').length,
    info: entries.filter((entry) => entry.level === 'INFO').length
  });
  const filtered = $derived(entries.filter((entry) => {
    if (levelFilter === 'error' && entry.level !== 'ERROR') return false;
    if (levelFilter === 'warn' && entry.level !== 'WARN') return false;
    if (levelFilter === 'info' && entry.level !== 'INFO') return false;
    const query = search.trim().toLowerCase();
    return !query || entry.text.toLowerCase().includes(query) || entry.serverName.toLowerCase().includes(query) || entry.source.toLowerCase().includes(query);
  }));
  const pageCount = $derived(Math.max(1, Math.ceil(filtered.length / pageSize)));
  const paged = $derived(filtered.slice(page * pageSize, (page + 1) * pageSize));

  // Any filter change starts again from the first page.
  $effect(() => {
    void levelFilter; void search; void pageSize;
    page = 0;
  });
  $effect(() => { if (page > pageCount - 1) page = pageCount - 1; });

  function windowRange(range: LogWindow): { from: string; to: string } {
    const to = new Date();
    const minutes = range === '1h' ? 60 : range === '6h' ? 360 : range === '24h' ? 24 * 60 : 7 * 24 * 60;
    return { from: new Date(to.getTime() - minutes * 60_000).toISOString(), to: to.toISOString() };
  }

  async function load(quiet = false): Promise<void> {
    controller?.abort();
    const current = new AbortController();
    controller = current;
    if (!quiet) { loadState = 'loading'; page = 0; }
    error = '';

    if (PREVIEW_MODE) {
      entries = generatePreviewFleetLogs(servers, serverFilter);
      loadState = entries.length ? 'ready' : 'empty';
      loadedAt = new Date();
      return;
    }

    const targets = serverFilter === 'all' ? servers : servers.filter((server) => server.id === serverFilter);
    if (targets.length === 0) { entries = []; loadState = 'empty'; return; }

    const range = windowRange(logWindow);
    const results: FleetLogEntry[] = [];
    let requests = 0;
    let failures = 0;
    let lastFailure = '';
    let expired = false;
    const noteFailure = (cause: unknown) => {
      failures += 1;
      if (cause instanceof ApiError && cause.authExpired) expired = true;
      lastFailure = cause instanceof Error ? cause.message : 'Request failed';
    };

    await Promise.allSettled(targets.map(async (server) => {
      requests += 1;
      let sources;
      try {
        sources = await apiClient.listLogSources(server.id, { signal: current.signal });
      } catch (cause) { noteFailure(cause); return; }
      // Sources of one server are read one after another to keep hub load low.
      for (const source of sources.items) {
        requests += 1;
        try {
          const result = await apiClient.queryLogs(server.id, { source: source.id, ...range, limit: fetchLimit, order: 'desc', signal: current.signal });
          for (const entry of result.entries) {
            results.push({
              id: `${server.id}-${source.id}-${entry.cursor || Math.random().toString(36).slice(2)}`,
              serverId: server.id,
              serverName: server.name,
              timestamp: entry.timestamp,
              level: resolveLogLevel(entry.severity, entry.text),
              text: entry.text,
              source: source.label || source.id,
              cursor: entry.cursor
            });
          }
        } catch (cause) { noteFailure(cause); }
      }
    }));

    if (current.signal.aborted) return;
    if (expired) { onAuthExpired(); return; }
    results.sort((a, b) => Date.parse(b.timestamp) - Date.parse(a.timestamp));
    entries = results;
    loadedAt = new Date();
    partial = failures > 0 && results.length > 0 ? `Some logs could not be read (${failures} of ${requests} requests failed): ${lastFailure}` : '';
    if (results.length === 0 && failures > 0) {
      error = lastFailure;
      loadState = 'error';
    } else {
      loadState = results.length ? 'ready' : 'empty';
    }
  }

  function setFollow(on: boolean) {
    follow = on;
    window.clearInterval(followTimer);
    if (on) followTimer = window.setInterval(() => { if (!document.hidden) void load(true); }, 15000);
  }

  function clearFilters() { levelFilter = 'all'; search = ''; }

  // The page can open before the fleet list has loaded (a direct link or a
  // reload); read the logs again once the set of servers is known or changes.
  let loadedFor = '';
  $effect(() => {
    const key = servers.map((server) => server.id).join(',');
    if (key === loadedFor) return;
    loadedFor = key;
    untrack(() => {
      if (serverFilter !== 'all' && !servers.some((server) => server.id === serverFilter)) serverFilter = 'all';
      void load();
    });
  });
  onDestroy(() => { controller?.abort(); window.clearInterval(followTimer); });
</script>

<section class="page" aria-labelledby="logs-title">
  <div class="page-heading">
    <div>
      <h1 id="logs-title" tabindex="-1">Logs</h1>
      <p class="lede">Service logs collected from every server, newest first.</p>
    </div>
    <div class="heading-actions">
      <button class="button ghost small" type="button" class:following={follow} aria-pressed={follow} onclick={() => setFollow(!follow)} title="Reload every 15 seconds while this page is open">
        <span class="follow-dot" aria-hidden="true"></span>{follow ? 'Following' : 'Follow'}
      </button>
      <button class="button ghost small" type="button" disabled={loadState === 'loading'} onclick={() => void load()}>
        <Icon name="refresh" size={13} /><span>{loadState === 'loading' ? 'Loading…' : 'Reload'}</span>
      </button>
    </div>
  </div>

  <div class="toolbar panel">
    <div class="filters">
      <label class="control"><span>Node</span>
        <select bind:value={serverFilter} onchange={() => void load()}>
          <option value="all">All nodes ({servers.length})</option>
          {#each servers as server (server.id)}<option value={server.id}>{server.name} ({server.role === 'node' ? 'Node' : 'Master'})</option>{/each}
        </select>
      </label>
      <label class="control"><span>Window</span>
        <select bind:value={logWindow} onchange={() => void load()}>
          <option value="1h">Last hour</option><option value="6h">Last 6 hours</option><option value="24h">Last 24 hours</option><option value="7d">Last 7 days</option>
        </select>
      </label>
      <label class="control"><span>Fetch</span>
        <select bind:value={fetchLimit} onchange={() => void load()}>
          <option value={100}>Latest 100</option><option value={250}>Latest 250</option><option value={500}>Latest 500</option>
        </select>
      </label>
    </div>
    <div class="search">
      <Icon name="search" size={14} />
      <input type="search" placeholder="Search message, node, or source…" aria-label="Search logs" bind:value={search} />
      {#if search}<button type="button" class="clear" onclick={() => (search = '')} aria-label="Clear search"><Icon name="x" size={12} /></button>{/if}
    </div>
  </div>

  <div class="segmented levels" role="group" aria-label="Filter logs by severity">
    <button type="button" aria-pressed={levelFilter === 'all'} onclick={() => (levelFilter = 'all')}>All <span class="n">{entries.length}</span></button>
    <button type="button" class="error" aria-pressed={levelFilter === 'error'} onclick={() => (levelFilter = 'error')}><i class="dot error"></i>Errors <span class="n">{counts.error}</span></button>
    <button type="button" class="warn" aria-pressed={levelFilter === 'warn'} onclick={() => (levelFilter = 'warn')}><i class="dot warn"></i>Warnings <span class="n">{counts.warn}</span></button>
    <button type="button" class="info" aria-pressed={levelFilter === 'info'} onclick={() => (levelFilter = 'info')}><i class="dot info"></i>Info <span class="n">{counts.info}</span></button>
  </div>

  {#if partial}<p class="partial-warning inline" role="status"><Icon name="alert-triangle" size={14} /><span>{partial}</span></p>{/if}

  {#if loadState === 'loading'}
    <LoadingSkeleton cards={0} rows={6} label="Loading logs" />
  {:else if loadState === 'error'}
    <div class="state-panel error-state">
      <div class="state-icon"><Icon name="alert-triangle" size={24} /></div>
      <h2>Could not load logs</h2>
      <p>{error}</p>
      <button class="button primary" type="button" onclick={() => void load()}>Retry</button>
    </div>
  {:else if loadState === 'empty' || filtered.length === 0}
    <div class="unavailable-panel">
      <strong>No log entries found</strong>
      <span>{entries.length > 0 ? 'No entries match your current severity or search filter.' : 'No entries were returned for the selected node(s) and time window.'}</span>
      {#if entries.length > 0}
        <button class="button ghost small" type="button" onclick={clearFilters}>Clear filters</button>
      {:else}
        <button class="button ghost small" type="button" onclick={() => void load()}>Reload</button>
      {/if}
    </div>
  {:else}
    <div class="viewer">
      <div class="viewer-bar">
        <span class="summary mono">
          {page * pageSize + 1}–{Math.min((page + 1) * pageSize, filtered.length)} of {filtered.length}{#if filtered.length !== entries.length}&nbsp;(filtered from {entries.length}){/if} · {timezone}{#if loadedAt} · updated {formatLogTimeInTz(loadedAt.toISOString(), timezone)}{/if}
        </span>
        <div class="pager">
          <label class="per-page">Per page
            <select bind:value={pageSize}><option value={50}>50</option><option value={100}>100</option><option value={250}>250</option><option value={500}>500</option></select>
          </label>
          {#if pageCount > 1}
            <button type="button" title="First page" aria-label="First page" disabled={page === 0} onclick={() => (page = 0)}>«</button>
            <button type="button" aria-label="Previous page" disabled={page === 0} onclick={() => (page -= 1)}>‹</button>
            <span class="mono">{page + 1} / {pageCount}</span>
            <button type="button" aria-label="Next page" disabled={page >= pageCount - 1} onclick={() => (page += 1)}>›</button>
            <button type="button" title="Last page" aria-label="Last page" disabled={page >= pageCount - 1} onclick={() => (page = pageCount - 1)}>»</button>
          {/if}
        </div>
      </div>
      <div class="log-list">
        {#each paged as entry (entry.id)}
          <div class={`log-entry ${entry.level.toLowerCase()}`}>
            <time class="mono tabular" datetime={entry.timestamp}>{formatLogTimeInTz(entry.timestamp, timezone)}</time>
            <span class={`level ${entry.level.toLowerCase()}`}>{entry.level}</span>
            {#if serverFilter === 'all'}<span class="server">{entry.serverName}</span>{/if}
            <span class="text mono">{entry.text}<small>{entry.source} #{entry.cursor}</small></span>
          </div>
        {/each}
      </div>
      {#if pageCount > 1}
        <div class="viewer-bar bottom">
          <span class="summary mono">Page {page + 1} of {pageCount}</span>
          <div class="pager">
            <button type="button" aria-label="Previous page" disabled={page === 0} onclick={() => (page -= 1)}>‹ Prev</button>
            <button type="button" aria-label="Next page" disabled={page >= pageCount - 1} onclick={() => (page += 1)}>Next ›</button>
          </div>
        </div>
      {/if}
    </div>
  {/if}
</section>

<style>
  .heading-actions { display: flex; gap: 8px; }
  .follow-dot { width: 7px; height: 7px; border-radius: 50%; background: var(--muted); }
  .following { border-color: color-mix(in srgb, var(--success) 45%, var(--line)); color: var(--success); }
  .following .follow-dot { background: var(--success); animation: breathe 1.4s ease-in-out infinite; }

  .toolbar { display: flex; flex-wrap: wrap; align-items: flex-end; justify-content: space-between; gap: 14px; margin-bottom: 12px; padding: 12px 16px; }
  .filters { display: flex; flex-wrap: wrap; gap: 12px; }
  .control { display: grid; gap: 5px; }
  .control span { color: var(--muted); font-size: 10.5px; font-weight: 700; letter-spacing: 0.06em; text-transform: uppercase; }
  .control select { padding-top: 6px; padding-bottom: 6px; }
  .search { display: flex; flex: 1; align-items: center; gap: 8px; min-width: 220px; max-width: 360px; padding: 0 10px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); color: var(--muted); transition: border-color var(--transition-fast), box-shadow var(--transition-fast); }
  .search:focus-within { border-color: var(--accent); box-shadow: var(--ring); }
  .search input { flex: 1; padding: 7px 0; border: 0; background: transparent; box-shadow: none; }
  .search input:focus { box-shadow: none; }
  .clear { display: grid; place-items: center; padding: 2px; border: 0; background: transparent; color: var(--muted); }

  .levels { margin-bottom: 14px; }
  .n { padding: 0 6px; border-radius: var(--radius-pill); background: var(--surface-muted); color: var(--muted); font-size: 11px; font-variant-numeric: tabular-nums; }
  .dot { width: 6px; height: 6px; border-radius: 50%; }
  .dot.error { background: var(--danger); }
  .dot.warn { background: var(--warning); }
  .dot.info { background: var(--success); }
  .levels .error[aria-pressed='true'] { color: var(--danger); }
  .levels .warn[aria-pressed='true'] { color: var(--warning); }
  .levels .info[aria-pressed='true'] { color: var(--success); }
  .inline { margin: 0 0 14px; }

  .viewer { overflow: hidden; border: 1px solid rgba(255, 255, 255, 0.06); border-radius: var(--radius-lg); background: var(--term-bg); color: var(--term-ink); box-shadow: var(--shadow-md); animation: fade-in var(--dur) var(--ease-out); }
  .viewer-bar { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 10px; padding: 8px 14px; border-bottom: 1px solid var(--term-line); color: var(--term-muted); font-size: 11.5px; }
  .viewer-bar.bottom { border-top: 1px solid var(--term-line); border-bottom: 0; }
  .summary { font-size: 11.5px; }
  .pager { display: flex; align-items: center; gap: 6px; }
  .pager button { min-width: 28px; height: 26px; padding: 0 8px; border: 1px solid rgba(255, 255, 255, 0.12); border-radius: var(--radius-sm); background: transparent; color: #cbd5e1; font-size: 12px; transition: background-color var(--transition-fast); }
  .pager button:hover:not(:disabled) { background: rgba(255, 255, 255, 0.08); color: #fff; }
  .pager button:disabled { opacity: 0.35; cursor: not-allowed; }
  .per-page { display: inline-flex; align-items: center; gap: 6px; margin-right: 6px; }
  .per-page select { padding: 3px 26px 3px 8px; border-color: rgba(255, 255, 255, 0.14); background-color: rgba(255, 255, 255, 0.04); color: var(--term-ink); font-size: 11.5px; }

  .log-list { display: flex; flex-direction: column; padding: 4px 8px 8px; }
  .log-entry { display: flex; align-items: baseline; gap: 10px; padding: 6px 6px; border-bottom: 1px solid var(--term-line); border-radius: 3px; font-size: 12px; line-height: 1.5; }
  .log-entry:last-child { border-bottom: 0; }
  .log-entry:hover { background: rgba(255, 255, 255, 0.035); }
  .log-entry.error { background: linear-gradient(90deg, rgba(242, 119, 126, 0.07), transparent 30%); }
  .log-entry time { flex-shrink: 0; min-width: 76px; color: var(--term-muted); }
  .level { flex-shrink: 0; min-width: 50px; padding: 0 5px; border: 1px solid; border-radius: var(--radius-xs); font-size: 10.5px; font-weight: 700; text-align: center; }
  .level.info { border-color: rgba(79, 201, 143, 0.25); background: rgba(79, 201, 143, 0.1); color: #4fc98f; }
  .level.warn { border-color: rgba(232, 177, 94, 0.25); background: rgba(232, 177, 94, 0.1); color: #e8b15e; }
  .level.error { border-color: rgba(242, 119, 126, 0.3); background: rgba(242, 119, 126, 0.12); color: #f2777e; }
  .level.debug { border-color: rgba(148, 163, 184, 0.25); background: rgba(148, 163, 184, 0.1); color: #94a3b8; }
  .server { flex-shrink: 0; max-width: 160px; overflow: hidden; padding: 0 7px; border: 1px solid rgba(116, 165, 255, 0.25); border-radius: var(--radius-xs); background: rgba(116, 165, 255, 0.1); color: #9abfff; font-size: 11px; font-weight: 600; text-overflow: ellipsis; white-space: nowrap; }
  .text { flex: 1; min-width: 0; color: var(--term-ink); font-size: 12px; overflow-wrap: anywhere; }
  .text small { display: block; margin-top: 2px; color: var(--term-muted); font-size: 10.5px; }

  @media (max-width: 640px) {
    .search { max-width: none; }
    .log-entry { flex-wrap: wrap; gap: 4px 8px; }
    .text { flex-basis: 100%; }
    .segmented.levels { width: 100%; overflow-x: auto; }
  }
</style>
