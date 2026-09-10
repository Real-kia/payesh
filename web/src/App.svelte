<script lang="ts">
  import { onMount } from 'svelte';
  import ChartPreview from './ChartPreview.svelte';
  import Sparkline from './Sparkline.svelte';
  import type { DisplayState, PreviewChartData, PreviewLogEntry, PreviewServer } from './preview/fixtures';

  type Theme = 'light' | 'dark';
  type Page = 'overview' | 'server' | 'onboarding';
  type DetailTab = 'metrics' | 'traffic' | 'logs';
  type ChartRange = '15m' | '1h' | '24h';
  type PreviewState = 'ready' | 'loading' | 'empty' | 'error';

  const PREVIEW_MODE = import.meta.env.DEV || import.meta.env.VITE_PAYESH_PREVIEW === 'true';
  const totalTrafficBytes = '1526000000000';
  const totalAllowanceBytes = '2500000000000';

  let servers: PreviewServer[] = [];
  let displayServers: PreviewServer[] = [];
  let previewLogEntries: PreviewLogEntry[] = [];
  let activePage: Page = 'overview';
  let selectedServerId = '';
  let detailTab: DetailTab = 'metrics';
  let chartRange: ChartRange = '15m';
  let theme: Theme = initialTheme();
  let previewState: PreviewState = PREVIEW_MODE ? 'loading' : 'ready';
  let previewLoadFailed = false;
  let notice = '';
  let setupStep = 1;
  let workspaceName = PREVIEW_MODE ? "Kia's workspace" : '';
  let retention = '30';
  let setupSecret = '';
  let ownerPassword = '';
  let notifications = 'none';
  let enrollmentMode: 'skip' | 'connect' = 'skip';
  let pairingToken = '';
  let setupError = '';
  let savedUiState: { activePage?: Page; selectedServerId?: string; detailTab?: DetailTab } | null = null;
  let chartData: PreviewChartData | null = null;
  let availableTabs: DetailTab[] = [];

  $: selectedServer = servers.find((server) => server.id === selectedServerId) ?? servers[0];
  $: displayServers = PREVIEW_MODE ? servers : [];
  $: healthyCount = displayServers.filter((server) => server.displayState === 'healthy').length;
  $: attentionCount = displayServers.filter((server) => server.displayState !== 'healthy').length;
  $: chartData = selectedServer?.metricHistory?.ranges[chartRange] ?? null;
  $: availableTabs = selectedServer ? (['metrics', 'traffic', 'logs'] as DetailTab[]).filter((tab) => hasCapability(selectedServer, tab)) : [];
  $: if (selectedServer && availableTabs.length > 0 && !availableTabs.includes(detailTab)) detailTab = availableTabs[0];

  function initialTheme(): Theme {
    if (typeof window === 'undefined') return 'light';
    const saved = window.localStorage.getItem('payesh-theme');
    if (saved === 'light' || saved === 'dark') return saved;
    return window.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }

  function applyTheme(persist = false) {
    if (typeof document !== 'undefined') document.documentElement.dataset.theme = theme;
    if (persist && typeof window !== 'undefined') window.localStorage.setItem('payesh-theme', theme);
  }

  function saveUiState() {
    if (typeof window === 'undefined') return;
    window.localStorage.setItem('payesh-ui-state', JSON.stringify({ activePage, selectedServerId, detailTab }));
  }

  function navigate(page: Page, serverId = selectedServerId) {
    activePage = page;
    selectedServerId = serverId;
    saveUiState();
    if (typeof window !== 'undefined') {
      window.history.pushState({ activePage, selectedServerId, detailTab }, '', window.location.pathname);
    }
  }

  function selectServer(server: PreviewServer) {
    detailTab = 'metrics';
    navigate('server', server.id);
  }

  function toggleTheme() {
    theme = theme === 'light' ? 'dark' : 'light';
    applyTheme(true);
  }

  function showNotice(message: string) {
    notice = message;
    window.setTimeout(() => { notice = ''; }, 4200);
  }

  function completeOnboarding() {
    setupError = '';
    if (setupStep === 1) {
      if (setupSecret.length < 16) {
        setupError = 'Use a bootstrap secret with at least 16 characters.';
        return;
      }
      if (ownerPassword.length < 12) {
        setupError = 'Use an owner password with at least 12 characters.';
        return;
      }
    }
    if (setupStep < 3) {
      setupStep += 1;
      return;
    }
    if (enrollmentMode === 'connect' && pairingToken.length < 16) {
      setupError = 'Use a pairing token with at least 16 characters, or choose skip for now.';
      return;
    }
    showNotice(`Preview setup saved for ${workspaceName}. No credential was sent.`);
    navigate('overview');
  }

  function formatBytes(value: string): string {
    try {
      const bytes = BigInt(value);
      const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
      let scaled = Number(bytes);
      let unit = 0;
      while (scaled >= 1000 && unit < units.length - 1) {
        scaled /= 1000;
        unit += 1;
      }
      return `${scaled >= 100 ? scaled.toFixed(0) : scaled.toFixed(1)} ${units[unit]}`;
    } catch {
      return '—';
    }
  }

  function percentage(counted: string, allowance: string): number {
    try {
      const result = (BigInt(counted) * 100n) / BigInt(allowance);
      return Math.max(0, Math.min(100, Number(result)));
    } catch {
      return 0;
    }
  }

  function stateLabel(state: DisplayState): string {
    return state.replace('-', ' ');
  }

  function metricValue(value: number | null): string {
    return value === null ? '—' : `${value}%`;
  }

  function hasCapability(server: PreviewServer, capability: DetailTab): boolean {
    return server.capabilities.includes(capability);
  }

  function tabLabel(tab: DetailTab): string {
    return tab.charAt(0).toUpperCase() + tab.slice(1);
  }

  function metricHistoryValues(server: PreviewServer, metric: 'cpu' | 'memory' | 'disk'): Array<number | null> {
    return server.metricHistory?.ranges[chartRange]?.[metric] ?? [];
  }

  function seriesRange(values: Array<number | null>): string {
    const numeric = values.filter((value): value is number => value !== null && Number.isFinite(value));
    if (numeric.length === 0) return 'unavailable';
    return `${Math.min(...numeric)}–${Math.max(...numeric)}%`;
  }

  function capabilityMessage(server: PreviewServer, capability: DetailTab): string {
    if (server.displayState === 'unsupported') return `${tabLabel(capability)} are unavailable: this server does not advertise the required capability.`;
    if (server.displayState === 'pending' || server.displayState === 'installing') return `${tabLabel(capability)} are unavailable until enrollment finishes.`;
    return `${tabLabel(capability)} are unavailable for this server.`;
  }

  async function loadPreviewData() {
    if (!PREVIEW_MODE) return;
    previewState = 'loading';
    previewLoadFailed = false;
    servers = [];
    previewLogEntries = [];
    selectedServerId = '';
    try {
      const preview = await import('./preview/fixtures');
      servers = preview.getPreviewServers();
      previewLogEntries = preview.previewLogEntries;
      selectedServerId = servers[0]?.id ?? '';
      restoreState(savedUiState);
      previewState = servers.length ? 'ready' : 'empty';
      if (typeof window !== 'undefined') window.history.replaceState({ activePage, selectedServerId, detailTab }, '', window.location.pathname);
    } catch {
      activePage = 'overview';
      previewLoadFailed = true;
      previewState = 'error';
    }
  }

  function restoreState(state: { activePage?: Page; selectedServerId?: string; detailTab?: DetailTab } | null) {
    if (!state) return;
    if (state.activePage === 'overview' || state.activePage === 'onboarding') activePage = state.activePage;
    if (state.activePage === 'server') activePage = servers.some((server) => server.id === state.selectedServerId) ? 'server' : 'overview';
    if (state.selectedServerId && servers.some((server) => server.id === state.selectedServerId)) selectedServerId = state.selectedServerId;
    if (state.detailTab === 'metrics' || state.detailTab === 'traffic' || state.detailTab === 'logs') detailTab = state.detailTab;
  }

  onMount(() => {
    try {
      savedUiState = JSON.parse(window.localStorage.getItem('payesh-ui-state') ?? 'null');
    } catch {
      // Corrupt local UI state is non-critical; the overview remains usable.
    }
    applyTheme(false);
    if (!PREVIEW_MODE) restoreState(savedUiState);
    void loadPreviewData();
    window.history.replaceState({ activePage, selectedServerId, detailTab }, '', window.location.pathname);
    const onPopState = (event: PopStateEvent) => { restoreState(event.state); saveUiState(); };
    window.addEventListener('popstate', onPopState);
    return () => window.removeEventListener('popstate', onPopState);
  });
</script>

<svelte:head>
  <title>Payesh — fleet overview</title>
  <meta name="description" content="Payesh local monitoring and fleet operations preview" />
</svelte:head>

<div class="app-shell" data-theme={theme}>
  <aside class="sidebar" aria-label="Primary navigation">
    <div class="brand-lockup">
      <div class="brand-mark" aria-hidden="true">P</div>
      <div><strong>payesh</strong><small>local operations</small></div>
    </div>

    <nav class="nav-list">
      <button class:active={activePage === 'overview'} class="nav-item" type="button" on:click={() => navigate('overview')} aria-current={activePage === 'overview' ? 'page' : undefined}>
        <span aria-hidden="true">⌂</span><span>Overview</span>
      </button>
      <button class="nav-item" type="button" on:click={() => showNotice('Alerts will use the shared incident API in package 03.')}>
        <span aria-hidden="true">!</span><span>Alerts</span><span class="nav-count">{attentionCount}</span>
      </button>
      <button class="nav-item" type="button" on:click={() => showNotice('Extras are reserved for approved modules.')}>
        <span aria-hidden="true">＋</span><span>Extras</span>
      </button>
      <button class:active={activePage === 'onboarding'} class="nav-item" type="button" on:click={() => navigate('onboarding')} aria-current={activePage === 'onboarding' ? 'page' : undefined}>
        <span aria-hidden="true">⚙</span><span>Settings</span>
      </button>
    </nav>

    <div class="sidebar-footer">
      <span class="connection-dot"></span>
      <span>Local hub</span>
      <small>{PREVIEW_MODE ? 'v0.1 preview' : 'API adapter'}</small>
    </div>
  </aside>

  <main class="main-content">
    <header class="topbar">
      <div class="breadcrumbs"><span>Workspace</span><span aria-hidden="true">/</span><strong>{activePage === 'server' ? selectedServer?.name : activePage === 'onboarding' ? 'Setup' : 'Overview'}</strong></div>
      <div class="topbar-actions">
        {#if PREVIEW_MODE}
          <label class="preview-control">Data
            <select bind:value={previewState} on:change={() => { if (previewLoadFailed) previewState = 'error'; }} aria-label="Preview data state">
              <option value="ready">Ready</option><option value="loading">Loading</option><option value="empty">Empty</option><option value="error">API error</option>
            </select>
          </label>
        {/if}
        <button class="icon-button" type="button" on:click={toggleTheme} aria-label={`Switch to ${theme === 'light' ? 'dark' : 'light'} theme`}>{theme === 'light' ? '☾' : '☀'}</button>
        <button class="button primary small" type="button" on:click={() => navigate('onboarding')}>＋ Add server</button>
      </div>
    </header>

    {#if notice}<div class="notice" role="status">{notice}</div>{/if}

    {#if activePage === 'onboarding'}
      <section class="page onboarding-page" aria-labelledby="setup-title">
        <div class="page-heading"><div><p class="eyebrow">Workspace setup</p><h1 id="setup-title">Prepare your local hub</h1><p class="lede">A short, validated setup path for a safe first enrollment.</p></div></div>
        <div class="stepper" aria-label="Setup progress">
          {#each ['Access', 'Defaults', 'Enroll'] as label, index}
            <div class:current={setupStep === index + 1} class:done={setupStep > index + 1} class="step"><span>{setupStep > index + 1 ? '✓' : index + 1}</span>{label}</div>
          {/each}
        </div>
        <div class="onboarding-card">
          {#if setupStep === 1}
            <p class="eyebrow">Step 1 of 3</p><h2>Protect the first connection</h2><p class="muted">These values stay in this preview and are never transmitted. Production enrollment will exchange them through the authenticated setup flow.</p>
            <div class="form-grid">
              <label>Workspace name<input bind:value={workspaceName} autocomplete="organization" /></label>
              <label>Bootstrap secret<input type="password" bind:value={setupSecret} minlength="16" autocomplete="new-password" aria-invalid={setupError ? 'true' : undefined} /><small>At least 16 characters.</small></label>
              <label>Owner password<input type="password" bind:value={ownerPassword} minlength="12" autocomplete="new-password" aria-invalid={setupError ? 'true' : undefined} /><small>At least 12 characters.</small></label>
            </div>
          {:else if setupStep === 2}
            <p class="eyebrow">Step 2 of 3</p><h2>Choose safe defaults</h2><p class="muted">Retention and notifications can be changed later without changing the enrollment secret.</p>
            <div class="form-grid"><label>Metric and log retention<select bind:value={retention}><option value="7">7 days</option><option value="30">30 days</option><option value="90">90 days</option></select></label><label>Notifications<select bind:value={notifications}><option value="none">None for now</option><option value="email">Email digest</option><option value="webhook">Webhook (later)</option></select></label></div>
          {:else}
            <p class="eyebrow">Step 3 of 3</p><h2>Enroll the first server</h2><p class="muted">Enrollment is optional. The next package will exchange a short-lived, single-use pairing token and hub fingerprint through the authenticated job flow.</p>
            <div class="choice-row"><label class:chosen={enrollmentMode === 'skip'}><input type="radio" bind:group={enrollmentMode} value="skip" /> Skip for now</label><label class:chosen={enrollmentMode === 'connect'}><input type="radio" bind:group={enrollmentMode} value="connect" /> Connect with a pairing token</label></div>
            {#if enrollmentMode === 'connect'}<label class="pairing-field">Pairing token<input type="password" bind:value={pairingToken} minlength="16" autocomplete="off" aria-invalid={setupError ? 'true' : undefined} /><small>At least 16 characters. Preview only; not persisted or transmitted.</small></label>{/if}
            <div class="review-box"><span>Workspace</span><strong>{workspaceName || 'Unnamed workspace'}</strong><span>Retention</span><strong>{retention} days · {notifications === 'none' ? 'notifications off' : notifications}</strong><span>Enrollment</span><strong>{enrollmentMode === 'skip' ? 'skipped' : 'token ready'}</strong></div>
          {/if}
          {#if setupError}<p class="form-error" role="alert">{setupError}</p>{/if}
          <div class="setup-actions"><button class="button ghost" type="button" on:click={() => setupStep > 1 ? setupStep -= 1 : navigate('overview')}>{setupStep > 1 ? 'Back' : 'Cancel'}</button><button class="button primary" type="button" on:click={completeOnboarding}>{setupStep === 3 ? 'Save setup' : 'Continue'}</button></div>
        </div>
      </section>
    {:else if activePage === 'server' && selectedServer}
      <section class="page server-page" aria-labelledby="server-title">
        <button class="back-link" type="button" on:click={() => navigate('overview')}>← Back to overview</button>
        <div class="page-heading server-heading"><div><p class="eyebrow">Server detail · {selectedServer.role}</p><h1 id="server-title">{selectedServer.name}</h1><p class="lede">{selectedServer.platform} · {selectedServer.architecture} · {selectedServer.version}</p></div><span class={`status-pill ${selectedServer.displayState}`}><i></i>{stateLabel(selectedServer.displayState)}</span></div>
        <div class="server-meta"><span>Connection: <strong>{selectedServer.connectionState}</strong></span><span>Freshness: <strong>{selectedServer.freshnessState}</strong></span><span>Revision: <strong>{selectedServer.configurationRevision}</strong></span>{#if selectedServer.freshnessReason}<span>{selectedServer.freshnessReason}</span>{/if}</div>
        <div class="tabs" role="tablist" aria-label="Server detail sections">
          {#each availableTabs as tab}
            <button class:active={detailTab === tab} type="button" role="tab" aria-selected={detailTab === tab} on:click={() => { detailTab = tab; saveUiState(); }}>{tabLabel(tab)}</button>
          {/each}
          {#if availableTabs.length === 0}<span class="muted tab-empty">No supported detail views</span>{/if}
        </div>
        {#if detailTab === 'metrics' && hasCapability(selectedServer, 'metrics')}
          <div class="metric-grid">
            {#each [['CPU', 'cpu', selectedServer.metrics.cpu, 'teal'], ['Memory', 'memory', selectedServer.metrics.memory, 'purple'], ['Disk', 'disk', selectedServer.metrics.disk, 'blue'] ] as metric}
              <article class="metric-card"><div class="card-top"><span>{metric[0]}</span><strong>{metricValue(metric[2] as number | null)}</strong></div>{#if metricHistoryValues(selectedServer, metric[1] as 'cpu' | 'memory' | 'disk').length > 1}<div class="sparkline"><Sparkline values={metricHistoryValues(selectedServer, metric[1] as 'cpu' | 'memory' | 'disk')} tone={metric[3] as 'teal' | 'purple' | 'blue'} /></div>{:else}<div class="sparkline-unavailable">No samples</div>{/if}<small>latest sample · {chartRange}</small></article>
            {/each}
          </div>
          <article class="panel chart-panel"><div class="panel-heading"><div><p class="eyebrow">Resource history</p><h2>CPU and memory</h2></div><select bind:value={chartRange} aria-label="Chart time range"><option value="15m">Last 15 minutes</option><option value="1h">Last hour</option><option value="24h">Last 24 hours</option></select></div>{#if chartData && chartData.coverage !== 'unavailable'}<div class="legend"><span><i class="legend-dot teal"></i>CPU</span><span><i class="legend-dot purple"></i>Memory</span><span>Unit: percent</span><span>Range: {chartRange}</span></div>{#key `${chartRange}-${theme}-${selectedServer.id}`}<ChartPreview data={chartData} range={chartRange} label="CPU and memory history over the selected time range" />{/key}<p class="chart-summary">CPU latest {metricValue(selectedServer.metrics.cpu)}; range {seriesRange(chartData.cpu)}. Memory latest {metricValue(selectedServer.metrics.memory)}; range {seriesRange(chartData.memory)}. Coverage: {chartData.coverage === 'gap' ? 'gaps shown; exact timing is uncertain.' : 'complete for this preview.'}</p>{:else}<div class="unavailable-panel"><strong>Resource history unavailable</strong><span>No valid samples are available for this server and range.</span></div>{/if}</article>
        {:else if detailTab === 'traffic' && hasCapability(selectedServer, 'traffic')}
          <article class="panel traffic-panel"><div class="panel-heading"><div><p class="eyebrow">Traffic allowance</p><h2>{selectedServer.traffic.scope} window</h2></div><span class="status-pill {selectedServer.traffic.continuity === 'complete' ? 'healthy' : 'stale'}"><i></i>{selectedServer.traffic.continuity}</span></div><div class="traffic-number"><strong>{formatBytes(selectedServer.traffic.countedBytes)}</strong><span>of {formatBytes(selectedServer.traffic.allowanceBytes)}</span></div><div class="progress"><span style={`width:${percentage(selectedServer.traffic.countedBytes, selectedServer.traffic.allowanceBytes)}%`}></span></div><div class="traffic-details"><span>Direction<strong>{selectedServer.traffic.direction}</strong></span><span>Timezone<strong>{selectedServer.traffic.timezone}</strong></span><span>Window<strong>{selectedServer.traffic.from.slice(0, 10)} → {selectedServer.traffic.to.slice(0, 10)}</strong></span><span>Continuity<strong>{selectedServer.traffic.continuity}</strong></span></div></article>
        {:else if detailTab === 'logs' && hasCapability(selectedServer, 'logs')}
          <article class="panel logs-panel"><div class="panel-heading"><div><p class="eyebrow">Bounded snapshot</p><h2>Recent logs</h2></div><button class="button ghost small" type="button" on:click={() => showNotice('Live tail will be connected to the bounded stream contract in package 03.')}>Enable live tail</button></div><div class="log-list">{#each previewLogEntries as entry}<div class="log-entry"><time>{entry.time}</time><span class={`log-level ${entry.level.toLowerCase()}`}>{entry.level}</span><span class="log-text">{entry.text}<small>{entry.source} · {entry.cursor}</small></span></div>{/each}</div></article>
        {:else if selectedServer && detailTab !== 'metrics' && !hasCapability(selectedServer, detailTab)}
          <div class="unavailable-panel large"><strong>{tabLabel(detailTab)} unavailable</strong><span>{capabilityMessage(selectedServer, detailTab)}</span></div>
        {:else if selectedServer && !hasCapability(selectedServer, 'metrics')}
          <div class="unavailable-panel large"><strong>Monitoring data unavailable</strong><span>{capabilityMessage(selectedServer, 'metrics')}</span></div>
        {/if}
      </section>
    {:else}
      <section class="page overview-page" aria-labelledby="overview-title">
        <div class="page-heading"><div><p class="eyebrow">{PREVIEW_MODE ? 'Live preview' : 'API connection required'}</p><h1 id="overview-title">{PREVIEW_MODE ? 'Good afternoon, Kia' : 'Fleet overview'}</h1><p class="lede">A clear view of your fleet, with freshness and uncertainty kept visible.</p></div>{#if PREVIEW_MODE}<span class="date-stamp">09 Sep 2026 · 14:42 UTC</span>{:else}<span class="date-stamp">Awaiting authenticated workspace</span>{/if}</div>
        {#if !PREVIEW_MODE}
          <div class="state-panel"><div class="state-icon">◎</div><h2>Preview mode is disabled</h2><p>Connect the approved API adapter to load fleet data. Fixtures are not used in production builds.</p><button class="button primary" type="button" on:click={() => navigate('onboarding')}>Open setup</button></div>
        {:else if previewState === 'loading'}
          <div class="state-panel"><div class="loading-spinner" aria-hidden="true"></div><h2>Loading fleet data</h2><p>Reading the bounded server summary.</p></div>
        {:else if previewState === 'empty'}
          <div class="state-panel"><div class="state-icon">＋</div><h2>No servers enrolled</h2><p>Start with one local server to see health and traffic here.</p><button class="button primary" type="button" on:click={() => navigate('onboarding')}>Add first server</button></div>
        {:else if previewState === 'error'}
          <div class="state-panel error-state"><div class="state-icon">!</div><h2>Could not load the fleet</h2><p>The API error is explicit and retryable; no stale fixture data is substituted.</p><button class="button primary" type="button" on:click={() => void loadPreviewData()}>Retry preview</button></div>
        {:else}
          <div class="summary-grid"><article class="summary-card"><span>Healthy servers</span><strong>{healthyCount}<small> / {displayServers.length}</small></strong><span class="summary-note positive">↑ Fresh enough to act</span></article><article class="summary-card"><span>Needs attention</span><strong>{attentionCount}</strong><span class="summary-note warning">Includes stale and offline</span></article><article class="summary-card"><span>Month-to-date traffic</span><strong>{formatBytes(totalTrafficBytes)}</strong><span class="summary-note">of {formatBytes(totalAllowanceBytes)} allowance</span></article></div>
          <div class="section-heading"><div><p class="eyebrow">Fleet health</p><h2>Servers</h2></div><span class="muted">Sorted by attention first</span></div>
          <div class="server-list">{#each displayServers as server}<button class="server-row" type="button" on:click={() => selectServer(server)}><span class={`server-state ${server.displayState}`} aria-label={stateLabel(server.displayState)}><i></i></span><span class="server-identity"><strong>{server.name}</strong><small>{server.platform} · {server.architecture}</small></span><span class="server-status"><span class={`status-pill ${server.displayState}`}><i></i>{stateLabel(server.displayState)}</span><small>{server.freshnessState === 'unknown' ? server.freshnessReason : `last heartbeat ${server.lastHeartbeat?.slice(11, 16)} UTC`}</small></span><span class="server-metric"><strong>{metricValue(server.metrics.cpu)}</strong><small>CPU</small></span><span class="server-arrow" aria-hidden="true">→</span></button>{/each}</div>
          <div class="lower-grid"><article class="panel"><div class="panel-heading"><div><p class="eyebrow">Traffic</p><h2>Allowance overview</h2></div><button class="text-button" type="button" on:click={() => selectedServer && selectServer(selectedServer)}>View details →</button></div><div class="traffic-number"><strong>{formatBytes(totalTrafficBytes)}</strong><span>used this month</span></div><div class="progress"><span style={`width:${percentage(totalTrafficBytes, totalAllowanceBytes)}%`}></span></div><p class="muted">{formatBytes(totalAllowanceBytes)} combined allowance · UTC</p></article><article class="panel"><div class="panel-heading"><div><p class="eyebrow">Recent activity</p><h2>Latest log signal</h2></div><span class="status-dot-label"><i></i> bounded</span></div><div class="activity-item"><span class="activity-icon">✓</span><div><strong>Heartbeat accepted</strong><small>Frankfurt edge · 14:42 UTC</small></div></div><div class="activity-item"><span class="activity-icon warning">!</span><div><strong>Stale sample detected</strong><small>Ashburn API · 14:34 UTC</small></div></div></article></div>
        {/if}
      </section>
    {/if}

    <footer><span>{PREVIEW_MODE ? 'Preview adapter · values are fixtures' : 'API adapter required'}</span><span>Payesh foundation · local-first</span></footer>
  </main>
</div>

<style>
  :global(*) { box-sizing: border-box; }
  :global(html) { color-scheme: light; }
  :global(html[data-theme='dark']) { color-scheme: dark; }
  :global(body) { margin: 0; min-width: 320px; background: var(--canvas); color: var(--ink); font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
  :global(:root) { --canvas: #f6f7f4; --surface: #fff; --surface-muted: #eef1ee; --ink: #17211d; --muted: #5c6a63; --line: #dfe6e1; --teal: #086b5c; --teal-bg: #d8eee9; --purple: #6e4bb2; --blue: #2365a0; --warning: #8a4b00; --warning-bg: #fbe8c9; --danger: #a82d2d; --danger-bg: #f8dddd; --primary-contrast: #fff; --shadow: 0 14px 34px rgba(30, 47, 39, .06); }
  :global(:root[data-theme='dark']) { --canvas: #111715; --surface: #18211e; --surface-muted: #202c27; --ink: #eef6f0; --muted: #b4c4ba; --line: #30423a; --teal: #65d9c2; --teal-bg: #173e37; --purple: #bb9aff; --blue: #81b7ff; --warning: #f4bb66; --warning-bg: #4b361b; --danger: #ff8d8d; --danger-bg: #4d2528; --primary-contrast: #10221d; --shadow: 0 14px 34px rgba(0, 0, 0, .2); }
  :global(button), :global(input), :global(select) { font: inherit; }
  :global(button) { cursor: pointer; }
  :global(:focus-visible) { outline: 3px solid color-mix(in srgb, var(--teal) 55%, transparent); outline-offset: 2px; }
  .app-shell { display: grid; grid-template-columns: 236px minmax(0, 1fr); min-height: 100vh; }
  .sidebar { display: flex; flex-direction: column; gap: 38px; padding: 28px 18px 20px; border-right: 1px solid var(--line); background: var(--surface); }
  .brand-lockup, .nav-item, .sidebar-footer, .topbar, .topbar-actions, .page-heading, .card-top, .panel-heading, .legend, .server-row, .activity-item, .setup-actions, .server-meta { display: flex; align-items: center; }
  .brand-lockup { gap: 10px; padding: 0 10px; }
  .brand-mark { display: grid; place-items: center; width: 32px; height: 32px; border-radius: 10px; background: var(--teal); color: white; font-weight: 800; }
  .brand-lockup strong, .brand-lockup small { display: block; }
  .brand-lockup small { margin-top: 2px; color: var(--muted); font-size: 11px; }
  .nav-list { display: flex; flex-direction: column; gap: 5px; }
  .nav-item { gap: 12px; width: 100%; padding: 11px 12px; border: 0; border-radius: 10px; background: transparent; color: var(--muted); text-align: left; }
  .nav-item:hover, .nav-item.active { background: var(--surface-muted); color: var(--ink); }
  .nav-item.active { box-shadow: inset 3px 0 var(--teal); font-weight: 700; }
  .nav-count { margin-left: auto; min-width: 20px; padding: 2px 6px; border-radius: 20px; background: var(--warning-bg); color: var(--warning); font-size: 11px; text-align: center; }
  .sidebar-footer { gap: 8px; margin-top: auto; padding: 12px 10px; color: var(--muted); font-size: 12px; }
  .sidebar-footer small { margin-left: auto; }
  .connection-dot, .status-dot-label i { width: 8px; height: 8px; border-radius: 50%; background: var(--teal); box-shadow: 0 0 0 3px color-mix(in srgb, var(--teal) 15%, transparent); }
  .main-content { min-width: 0; }
  .topbar { justify-content: space-between; min-height: 76px; padding: 0 42px; border-bottom: 1px solid var(--line); background: color-mix(in srgb, var(--surface) 90%, transparent); }
  .breadcrumbs { display: flex; gap: 10px; color: var(--muted); font-size: 13px; }.breadcrumbs strong { color: var(--ink); }
  .topbar-actions { gap: 12px; }.preview-control { display: flex; align-items: center; gap: 7px; color: var(--muted); font-size: 12px; }.preview-control select { padding: 6px 8px; border: 1px solid var(--line); border-radius: 7px; background: var(--surface); color: var(--ink); }
  .icon-button { width: 34px; height: 34px; border: 1px solid var(--line); border-radius: 9px; background: var(--surface); color: var(--ink); }
  .page { max-width: 1240px; margin: 0 auto; padding: 44px 42px 30px; }.page-heading { justify-content: space-between; gap: 20px; margin-bottom: 34px; }.eyebrow { margin: 0 0 8px; color: var(--teal); font-size: 11px; font-weight: 800; letter-spacing: .11em; text-transform: uppercase; }h1, h2, p { margin-top: 0; }h1 { margin-bottom: 8px; font-size: clamp(29px, 4vw, 42px); letter-spacing: -.04em; }h2 { margin-bottom: 0; font-size: 18px; letter-spacing: -.02em; }.lede { margin: 0; color: var(--muted); }.date-stamp, .muted { color: var(--muted); font-size: 12px; }
  .button { border: 1px solid var(--line); border-radius: 9px; padding: 10px 15px; background: var(--surface); color: var(--ink); font-weight: 700; }.button.small { padding: 8px 12px; font-size: 12px; }.button.primary { border-color: var(--teal); background: var(--teal); color: var(--primary-contrast); }.button.ghost { background: transparent; }.text-button, .back-link { border: 0; background: transparent; color: var(--teal); font-weight: 700; }.back-link { margin-bottom: 26px; padding: 0; }.notice { position: fixed; z-index: 5; top: 88px; right: 28px; max-width: 360px; padding: 12px 15px; border: 1px solid color-mix(in srgb, var(--teal) 35%, var(--line)); border-radius: 10px; background: var(--surface); box-shadow: var(--shadow); color: var(--ink); font-size: 13px; }
  .summary-grid, .metric-grid, .lower-grid { display: grid; gap: 14px; }.summary-grid { grid-template-columns: repeat(3, 1fr); margin-bottom: 42px; }.summary-card, .metric-card, .panel, .onboarding-card { border: 1px solid var(--line); border-radius: 14px; background: var(--surface); box-shadow: var(--shadow); }.summary-card { padding: 20px; }.summary-card > span:first-child { color: var(--muted); font-size: 12px; }.summary-card strong { display: block; margin: 10px 0 4px; font-size: 28px; letter-spacing: -.04em; }.summary-card strong small { color: var(--muted); font-size: 13px; font-weight: 500; }.summary-note { color: var(--muted); font-size: 11px; }.summary-note.positive { color: var(--teal); }.summary-note.warning { color: var(--warning); }.section-heading { display: flex; align-items: end; justify-content: space-between; margin-bottom: 12px; }.section-heading h2 { font-size: 24px; }
  .server-list { overflow: hidden; border: 1px solid var(--line); border-radius: 14px; background: var(--surface); box-shadow: var(--shadow); }.server-row { gap: 15px; width: 100%; padding: 17px 18px; border: 0; border-bottom: 1px solid var(--line); background: transparent; color: var(--ink); text-align: left; }.server-row:last-child { border-bottom: 0; }.server-row:hover { background: var(--surface-muted); }.server-state { width: 9px; height: 9px; flex: 0 0 auto; border-radius: 50%; background: var(--teal); }.server-state.stale, .server-state.pending, .server-state.installing { background: var(--warning); }.server-state.unreachable, .server-state.failed { background: var(--danger); }.server-state.disabled, .server-state.unsupported { background: var(--muted); }.server-identity { display: flex; flex: 1; flex-direction: column; gap: 4px; min-width: 0; overflow: hidden; }.server-identity strong, .server-identity small { overflow-wrap: anywhere; word-break: break-word; }.server-identity small, .server-status small, .server-metric small { color: var(--muted); font-size: 11px; }.server-status { display: flex; flex: 1; flex-direction: column; gap: 4px; min-width: 0; }.server-metric { display: flex; flex-direction: column; align-items: end; gap: 3px; min-width: 50px; }.server-arrow { color: var(--muted); font-size: 19px; }.status-pill { display: inline-flex; align-items: center; gap: 6px; width: fit-content; padding: 5px 8px; border-radius: 99px; background: var(--teal-bg); color: var(--teal); font-size: 11px; font-weight: 700; text-transform: capitalize; }.status-pill i { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }.status-pill.stale, .status-pill.pending, .status-pill.installing { background: var(--warning-bg); color: var(--warning); }.status-pill.unreachable, .status-pill.failed { background: var(--danger-bg); color: var(--danger); }.status-pill.disabled, .status-pill.unsupported { background: var(--surface-muted); color: var(--muted); }
  .lower-grid { grid-template-columns: 1fr 1fr; margin-top: 20px; }.panel { padding: 22px; }.panel-heading { justify-content: space-between; gap: 14px; margin-bottom: 20px; }.panel-heading select, .form-grid select, .form-grid input { width: 100%; padding: 10px 11px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); color: var(--ink); }.panel-heading select { width: auto; }.traffic-number { display: flex; align-items: baseline; gap: 8px; margin: 10px 0 16px; }.traffic-number strong { font-size: 32px; letter-spacing: -.05em; }.traffic-number span { color: var(--muted); font-size: 12px; }.progress { height: 8px; overflow: hidden; border-radius: 20px; background: var(--surface-muted); }.progress span { display: block; height: 100%; border-radius: inherit; background: var(--teal); }.traffic-panel .progress span { background: var(--purple); }.panel > .muted { margin: 12px 0 0; }.activity-item { gap: 12px; padding: 12px 0; border-bottom: 1px solid var(--line); }.activity-item:last-child { border-bottom: 0; }.activity-icon { display: grid; place-items: center; width: 27px; height: 27px; border-radius: 8px; background: var(--teal-bg); color: var(--teal); }.activity-icon.warning { background: var(--warning-bg); color: var(--warning); }.activity-item strong, .activity-item small { display: block; }.activity-item small { margin-top: 3px; color: var(--muted); font-size: 11px; }.status-dot-label { display: flex; align-items: center; gap: 7px; color: var(--muted); font-size: 11px; }
  .server-heading { margin-bottom: 14px; }.server-meta { flex-wrap: wrap; gap: 8px 18px; margin-bottom: 25px; color: var(--muted); font-size: 12px; }.server-meta strong { color: var(--ink); font-weight: 600; }.tabs { display: flex; align-items: end; gap: 21px; margin-bottom: 20px; border-bottom: 1px solid var(--line); }.tabs button { padding: 10px 2px 12px; border: 0; border-bottom: 2px solid transparent; background: transparent; color: var(--muted); }.tabs button.active { border-color: var(--teal); color: var(--ink); font-weight: 700; }.tab-empty { padding-bottom: 13px; }.metric-grid { grid-template-columns: repeat(3, 1fr); margin-bottom: 14px; }.metric-card { padding: 17px; }.card-top { justify-content: space-between; }.card-top span { color: var(--muted); font-size: 12px; }.card-top strong { font-size: 22px; }.sparkline { height: 45px; margin: 12px 0 6px; }.sparkline-unavailable { display: grid; place-items: center; height: 45px; margin: 12px 0 6px; border-radius: 7px; background: var(--surface-muted); color: var(--muted); font-size: 10px; }.metric-card small { color: var(--muted); font-size: 10px; }.legend { flex-wrap: wrap; gap: 16px; margin-bottom: 8px; color: var(--muted); font-size: 11px; }.legend span { display: inline-flex; align-items: center; gap: 5px; }.legend-dot { width: 7px; height: 7px; border-radius: 50%; }.legend-dot.teal { background: var(--teal); }.legend-dot.purple { background: var(--purple); }.chart-summary { margin: 7px 0 0; color: var(--muted); font-size: 11px; }.unavailable-panel { display: grid; gap: 6px; padding: 32px 18px; border: 1px dashed var(--line); border-radius: 10px; background: var(--surface-muted); color: var(--muted); font-size: 12px; }.unavailable-panel strong { color: var(--ink); }.unavailable-panel.large { padding: 60px 22px; text-align: center; }.traffic-details { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; margin-top: 24px; }.traffic-details span { color: var(--muted); font-size: 11px; }.traffic-details strong { display: block; margin-top: 5px; color: var(--ink); font-size: 12px; text-transform: capitalize; }.log-list { border-top: 1px solid var(--line); }.log-entry { display: grid; grid-template-columns: 70px 50px 1fr; gap: 10px; padding: 13px 0; border-bottom: 1px solid var(--line); font-size: 12px; }.log-entry time, .log-text small { color: var(--muted); }.log-level { font-weight: 800; }.log-level.info { color: var(--teal); }.log-level.warn { color: var(--warning); }.log-text small { display: block; margin-top: 4px; font-size: 10px; }
  .stepper { display: flex; gap: 24px; margin-bottom: 24px; }.step { display: flex; align-items: center; gap: 8px; color: var(--muted); font-size: 13px; }.step span { display: grid; place-items: center; width: 25px; height: 25px; border: 1px solid var(--line); border-radius: 50%; }.step.current { color: var(--ink); font-weight: 700; }.step.current span, .step.done span { border-color: var(--teal); background: var(--teal); color: white; }.onboarding-card { max-width: 760px; padding: 30px; }.onboarding-card h2 { margin-bottom: 10px; font-size: 24px; }.form-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin: 26px 0; }.form-grid label { display: flex; flex-direction: column; gap: 7px; color: var(--muted); font-size: 12px; }.form-grid label:first-child { grid-column: 1 / -1; }.form-grid small { color: var(--muted); font-size: 10px; }.setup-actions { justify-content: space-between; padding-top: 20px; border-top: 1px solid var(--line); }.form-error { margin: 0 0 15px; color: var(--danger); font-size: 12px; }.review-box { display: grid; grid-template-columns: 150px 1fr; gap: 12px; margin: 26px 0; padding: 18px; border-radius: 10px; background: var(--surface-muted); color: var(--muted); font-size: 12px; }.review-box strong { color: var(--ink); }
  .choice-row { display: flex; flex-wrap: wrap; gap: 10px; margin: 24px 0 14px; }.choice-row label { display: flex; align-items: center; gap: 8px; padding: 11px 13px; border: 1px solid var(--line); border-radius: 9px; color: var(--muted); font-size: 12px; }.choice-row label.chosen { border-color: var(--teal); background: var(--teal-bg); color: var(--ink); }.pairing-field { display: flex; flex-direction: column; gap: 7px; max-width: 470px; color: var(--muted); font-size: 12px; }.pairing-field input { padding: 10px 11px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); color: var(--ink); }.pairing-field small { color: var(--muted); font-size: 10px; }
  .state-panel { display: grid; justify-items: center; gap: 10px; padding: 80px 20px; border: 1px dashed var(--line); border-radius: 14px; background: var(--surface); text-align: center; }.state-panel h2 { margin: 0; }.state-panel p { max-width: 460px; margin-bottom: 8px; color: var(--muted); font-size: 13px; }.state-icon { display: grid; place-items: center; width: 44px; height: 44px; border-radius: 50%; background: var(--surface-muted); color: var(--teal); font-size: 24px; }.error-state .state-icon { color: var(--danger); }.loading-spinner { width: 32px; height: 32px; border: 3px solid var(--line); border-top-color: var(--teal); border-radius: 50%; animation: spin 800ms linear infinite; }@keyframes spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) { .loading-spinner { animation: none; } }
  footer { display: flex; justify-content: space-between; gap: 15px; max-width: 1240px; margin: 20px auto 0; padding: 0 42px 25px; color: var(--muted); font-size: 11px; }
  @media (max-width: 900px) { .app-shell { grid-template-columns: 1fr; }.sidebar { position: sticky; top: 0; z-index: 4; flex-direction: row; align-items: center; gap: 20px; padding: 12px 18px; border-right: 0; border-bottom: 1px solid var(--line); }.brand-lockup { flex: 0 0 auto; }.nav-list { flex-direction: row; flex: 1; gap: 2px; overflow-x: auto; }.nav-item { flex: 0 0 auto; width: auto; padding: 9px 11px; white-space: nowrap; }.nav-item.active { box-shadow: inset 0 -3px var(--teal); }.sidebar-footer { display: none; }.topbar { padding: 0 24px; }.page { padding: 32px 24px 20px; }footer { padding: 0 24px 20px; } }
  @media (max-width: 680px) { .topbar { align-items: flex-start; flex-direction: column; gap: 12px; padding: 16px 18px; }.topbar-actions { width: 100%; justify-content: space-between; }.preview-control { margin-right: auto; }.page { padding: 28px 16px 18px; }.page-heading { align-items: flex-start; flex-direction: column; margin-bottom: 25px; }.date-stamp { align-self: flex-start; }.summary-grid, .metric-grid, .lower-grid { grid-template-columns: 1fr; }.server-row { gap: 10px; }.server-status { min-width: 0; }.server-metric { display: none; }.server-arrow { margin-left: auto; }.traffic-details { grid-template-columns: 1fr 1fr; }.form-grid { grid-template-columns: 1fr; }.form-grid label:first-child { grid-column: auto; }.onboarding-card { padding: 22px 18px; }.stepper { justify-content: space-between; gap: 8px; }.step { font-size: 11px; }.notice { top: 130px; right: 16px; left: 16px; max-width: none; }.panel { padding: 18px; }footer { flex-direction: column; padding: 0 16px 18px; }.chart-panel .panel-heading { align-items: flex-start; flex-direction: column; }.panel-heading select { width: 100%; } }
</style>
