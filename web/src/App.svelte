<script lang="ts">
  import { onMount } from 'svelte';
  import ChartPreview from './ChartPreview.svelte';
  import Sparkline from './Sparkline.svelte';
  import TrafficChart from './TrafficChart.svelte';
  import { ApiError, apiClient, mapWithConcurrency, type AlertState, type Job, type MetricQuery, type Module, type ModuleInstallation, type ModuleManifest, type Server as ApiServer } from './api';
  import type { DisplayState, PreviewChartData, PreviewLogEntry, PreviewServer } from './preview/fixtures';

  type Theme = 'light' | 'dark';
  type Page = 'overview' | 'servers' | 'server' | 'alerts' | 'packages' | 'settings' | 'add-server' | 'onboarding';
  type DetailTab = 'metrics' | 'traffic' | 'logs';
  type ChartRange = '15m' | '1h' | '24h';
  type PreviewState = 'ready' | 'loading' | 'empty' | 'error';

  // Development builds use the real API by default. Opt into fixtures
  // explicitly so a local preview can never accidentally mask API failures.
  const PREVIEW_MODE = import.meta.env.VITE_PAYESH_PREVIEW === 'true';
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
  let previewState: PreviewState = 'loading';
  let previewLoadFailed = false;
  let apiError = '';
  let partialWarning = '';
  let authExpired = false;
  let logState: PreviewState = 'ready';
  let logError = '';
  let logEntries: PreviewLogEntry[] = [];
  let apiAbortController: AbortController | null = null;
  let logAbortController: AbortController | null = null;
  let notice = '';
  let setupStep = 1;
  let workspaceName = PREVIEW_MODE ? "Kia's workspace" : '';
  let retention = '30';
  let setupSecret = '';
  let ownerPassword = '';
  let ownerUsername = '';
  let notifications = 'none';
  let enrollmentMode: 'skip' | 'connect' = 'skip';
  let pairingToken = '';
  let setupError = '';
  let savedUiState: { activePage?: Page; selectedServerId?: string; detailTab?: DetailTab } | null = null;
  let chartData: PreviewChartData | null = null;
  let availableTabs: DetailTab[] = [];
  type SessionState = 'unknown' | 'authenticated' | 'signed-out';
  let sessionState: SessionState = PREVIEW_MODE ? 'authenticated' : 'unknown';
  let authPassword = '';
  let authUsername = '';
  let authBusy = false;
  let authError = '';
  let setupCompleted = false;
  let latestJob: Job | null = null;
  let jobError = '';
  let jobBusy = false;
  let jobPollController: AbortController | null = null;
  let updateRelease = '';
  let updateBusy = false;
  let installHost = '';
  let installPort = '22';
  let installUser = '';
  let installPassword = '';
  let installKey = '';
  let installFingerprint = '';
  let installBusy = false;
  let labelDraft = '';
  let labelBusy = false;
  let newServerName = '';
  let createServerBusy = false;
  let modules: Module[] = [];
  let modulesState: PreviewState = 'loading';
  let modulesError = '';
  let packageServerId = '';
  let moduleInstallations: ModuleInstallation[] = [];
  let packageBusy = '';
  let packageError = '';
  let packageManifestFile: File | null = null;
  let packageArchiveFile: File | null = null;
  let packageSignature = '';
  let alerts: AlertState[] = [];
  let alertsState: PreviewState = 'loading';
  let alertsError = '';

  $: selectedServer = servers.find((server) => server.id === selectedServerId) ?? servers[0];
  $: displayServers = servers;
  $: healthyCount = displayServers.filter((server) => server.displayState === 'healthy').length;
  $: attentionCount = displayServers.filter((server) => server.displayState !== 'healthy').length;
  $: firingAlertCount = alerts.filter((alert) => alert.state === 'firing').length;
  $: overviewTrafficBytes = PREVIEW_MODE ? totalTrafficBytes : displayServers.reduce((total, server) => { try { return (BigInt(total) + BigInt(server.traffic.countedBytes)).toString(); } catch { return total; } }, '0');
  $: overviewAllowanceBytes = PREVIEW_MODE ? totalAllowanceBytes : displayServers.reduce((total, server) => { try { return (BigInt(total) + BigInt(server.traffic.allowanceBytes)).toString(); } catch { return total; } }, '0');
  $: chartData = selectedServer?.metricHistory?.ranges[chartRange] ?? null;
  $: availableTabs = selectedServer ? (['metrics', 'traffic', 'logs'] as DetailTab[]).filter((tab) => hasCapability(selectedServer, tab)) : [];
  $: if (selectedServer && availableTabs.length > 0 && !availableTabs.includes(detailTab)) detailTab = availableTabs[0];
  $: if (selectedServer && !labelDraft) labelDraft = selectedServer.name;

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
    if (page === 'packages' && !PREVIEW_MODE) void loadModules();
    if (page === 'alerts' && !PREVIEW_MODE) void loadAlerts();
  }

  async function loadModules(): Promise<void> {
    modulesState = 'loading'; modulesError = '';
    try { const page = await apiClient.listModules(); modules = page.items; modulesState = modules.length ? 'ready' : 'empty'; if (!packageServerId && servers.length) packageServerId = servers[0].id; if (packageServerId) await loadServerModules(); }
    catch (error) { modulesError = error instanceof Error ? error.message : 'Unable to load modules.'; modulesState = 'error'; if (error instanceof ApiError && error.authExpired) authExpired = true; }
  }

  async function loadServerModules(): Promise<void> {
    if (!packageServerId) { moduleInstallations = []; return; }
    packageError = '';
    try { moduleInstallations = (await apiClient.listServerModules(packageServerId)).items; }
    catch (error) { packageError = error instanceof Error ? error.message : 'Unable to load package state.'; if (error instanceof ApiError && error.authExpired) authExpired = true; }
  }

  function moduleState(moduleId: string): ModuleInstallation | undefined {
    return moduleInstallations.find((item) => item.module_id === moduleId);
  }

  async function packageAction(module: Module, action: 'install' | 'enable' | 'disable' | 'remove'): Promise<void> {
    if (!packageServerId || packageBusy) return;
    packageBusy = `${module.id}:${action}`; packageError = '';
    try {
      const current = moduleState(module.id);
      let result: ModuleInstallation;
      if (action === 'install') {
        if (!packageManifestFile || !packageArchiveFile || !packageSignature.trim()) throw new Error('Choose the signed manifest and archive, then paste the release signature.');
        const manifest = JSON.parse(await packageManifestFile.text()) as ModuleManifest;
        const bytes = new Uint8Array(await packageArchiveFile.arrayBuffer());
        let binary = ''; for (let index = 0; index < bytes.length; index += 0x8000) binary += String.fromCharCode(...bytes.subarray(index, index + 0x8000));
        result = await apiClient.installModule(packageServerId, module.id, { manifest, manifest_signature_b64: packageSignature.trim(), archive_base64: btoa(binary), expected_revision: current?.revision ?? '0', idempotency_key: operationKey('package-install') });
      } else result = await apiClient.moduleAction(packageServerId, module.id, action, current?.revision ?? '0', operationKey(`package-${action}`));
      moduleInstallations = [...moduleInstallations.filter((item) => item.module_id !== module.id), result];
      showNotice(`${module.name} is now ${result.state}.`);
    } catch (error) { packageError = error instanceof Error ? error.message : 'Package operation failed.'; if (error instanceof ApiError && error.authExpired) authExpired = true; }
    finally { packageBusy = ''; }
  }

  async function loadAlerts(): Promise<void> {
    alertsState = 'loading'; alertsError = '';
    try { alerts = (await apiClient.listAlerts()).items; alertsState = alerts.length ? 'ready' : 'empty'; }
    catch (error) { alertsError = error instanceof Error ? error.message : 'Unable to load alerts.'; alertsState = 'error'; if (error instanceof ApiError && error.authExpired) authExpired = true; }
  }

  async function createPendingServer(): Promise<void> {
    if (!newServerName.trim() || !installHost.trim() || !installUser.trim() || (!installPassword && !installKey) || createServerBusy) return;
    createServerBusy = true; jobError = '';
    try {
      const created = await apiClient.createServer({ name: newServerName.trim(), address: installHost.trim() });
      const server = emptyApiServer(created); servers = [...servers, server]; selectedServerId = server.id; labelDraft = server.name;
      const job = await apiClient.enqueueInstall({ server_id: server.id, host: installHost.trim(), port: Number(installPort), user: installUser.trim(), ...(installPassword ? { password: installPassword } : {}), ...(installKey ? { private_key: installKey } : {}), expected_host_key_fingerprint: installFingerprint.trim() || undefined, role: 'node', start: true, idempotency_key: operationKey('install') });
      newServerName = ''; installPassword = ''; installKey = ''; navigate('server', server.id); recordJob(job);
      showNotice('Server created. Payesh is detecting the operating system and architecture over SSH.');
    } catch (error) { jobError = error instanceof Error ? error.message : 'Unable to create server.'; if (error instanceof ApiError && error.authExpired) authExpired = true; }
    finally { createServerBusy = false; }
  }

  function selectServer(server: PreviewServer) {
    detailTab = 'metrics';
    labelDraft = server.name;
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

  function operationKey(prefix: string): string {
    if (typeof crypto === 'undefined') throw new Error('Secure operation identity is unavailable in this browser context.');
    if (typeof crypto.randomUUID === 'function') return `${prefix}-${crypto.randomUUID()}`;
    if (typeof crypto.getRandomValues !== 'function') throw new Error('Secure operation identity is unavailable in this browser context.');
    const random = crypto.getRandomValues(new Uint8Array(16));
    return `${prefix}-${Array.from(random, (value) => value.toString(16).padStart(2, '0')).join('')}`;
  }

  function recordJob(job: Job): void {
    latestJob = job;
    jobError = '';
    void pollJob(job.id);
  }

  async function pollJob(jobId: string): Promise<void> {
    jobPollController?.abort();
    const controller = new AbortController();
    jobPollController = controller;
    jobBusy = true;
    try {
      for (let attempt = 0; attempt < 120; attempt += 1) {
        const current = await apiClient.getJob(jobId, { signal: controller.signal });
        if (controller.signal.aborted) return;
        latestJob = current;
        if (['succeeded', 'failed', 'cancelled', 'recovery-required'].includes(current.state)) return;
        await new Promise<void>((resolve, reject) => {
          const timer = window.setTimeout(resolve, 1500);
          controller.signal.addEventListener('abort', () => { window.clearTimeout(timer); reject(new DOMException('The request was aborted.', 'AbortError')); }, { once: true });
        });
      }
      jobError = 'Job is still running. Reload its status to continue monitoring.';
    } catch (error) {
      if (controller.signal.aborted) return;
      jobError = error instanceof Error ? error.message : 'Unable to read job status.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      if (jobPollController === controller) jobBusy = false;
    }
  }

  async function cancelLatestJob(): Promise<void> {
    if (!latestJob || !['queued', 'running'].includes(latestJob.state)) return;
    jobError = '';
    try {
      latestJob = await apiClient.cancelJob(latestJob.id, latestJob.revision, operationKey('cancel'));
      void pollJob(latestJob.id);
    } catch (error) {
      jobError = error instanceof Error ? error.message : 'Unable to cancel the job.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    }
  }

  async function submitLogin(): Promise<void> {
    if (authPassword.length < 1 || authBusy) return;
    authBusy = true; authError = '';
    try {
      await apiClient.login(authUsername, authPassword);
      authPassword = '';
      authExpired = false;
      sessionState = 'authenticated';
      await loadApiData();
    } catch (error) {
      authError = error instanceof ApiError && error.retryAfterSeconds ? `${error.message}. Try again in ${error.retryAfterSeconds} seconds.` : error instanceof Error ? error.message : 'Unable to sign in.';
    } finally { authBusy = false; }
  }

  async function signOut(): Promise<void> {
    authError = '';
    try { await apiClient.logout(); } catch (error) { if (!(error instanceof ApiError && error.authExpired)) authError = error instanceof Error ? error.message : 'Unable to sign out.'; }
    sessionState = 'signed-out'; authExpired = true; servers = []; previewState = 'error'; navigate('overview');
  }

  async function submitUpdate(): Promise<void> {
    if (!updateRelease.trim() || !servers.length || updateBusy) return;
    updateBusy = true; jobError = '';
    try {
      const job = await apiClient.createUpdate({ release: updateRelease.trim(), selected_server_ids: servers.map((server) => server.id), idempotency_key: operationKey('update') });
      recordJob(job);
    } catch (error) {
      jobError = error instanceof Error ? error.message : 'Unable to queue the update.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally { updateBusy = false; }
  }

  async function submitInstall(): Promise<void> {
    if (!selectedServer || !installHost.trim() || !installUser.trim() || installBusy) return;
    installBusy = true; jobError = '';
    try {
      const job = await apiClient.enqueueInstall({ server_id: selectedServer.id, host: installHost.trim(), port: Number(installPort), user: installUser.trim(), ...(installPassword ? { password: installPassword } : {}), ...(installKey ? { private_key: installKey } : {}), expected_host_key_fingerprint: installFingerprint.trim() || undefined, role: 'node', idempotency_key: operationKey('install') });
      installPassword = ''; installKey = '';
      recordJob(job);
    } catch (error) {
      jobError = error instanceof Error ? error.message : 'Unable to queue the installation.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally { installBusy = false; }
  }

  async function renameSelectedServer(): Promise<void> {
    if (!selectedServer || !labelDraft.trim() || labelDraft.trim() === selectedServer.name || labelBusy) return;
    labelBusy = true; jobError = '';
    try {
      const updated = await apiClient.updateServerLabel(selectedServer.id, labelDraft.trim(), selectedServer.configurationRevision, operationKey('label'));
      const replacement = emptyApiServer(updated);
      Object.assign(selectedServer, replacement);
      labelDraft = updated.name;
      showNotice('Server label updated.');
    } catch (error) {
      jobError = error instanceof Error ? error.message : 'Unable to rename this server.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally { labelBusy = false; }
  }

  async function revokeSelectedServer(): Promise<void> {
    if (!selectedServer || labelBusy) return;
    labelBusy = true; jobError = '';
    try {
      const job = await apiClient.revokeServer(selectedServer.id, operationKey('revoke'));
      recordJob(job);
    } catch (error) {
      jobError = error instanceof Error ? error.message : 'Unable to revoke this server.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally { labelBusy = false; }
  }

  async function completeOnboarding(): Promise<void> {
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
      if (!PREVIEW_MODE && !setupCompleted) {
        try {
          await apiClient.completeSetup({ setup_secret: setupSecret, username: ownerUsername, password: ownerPassword });
          await apiClient.login(ownerUsername, ownerPassword);
          setupCompleted = true;
          sessionState = 'authenticated';
          setupSecret = '';
          ownerPassword = '';
          authExpired = false;
          await loadApiData();
        } catch (error) {
          setupError = error instanceof Error ? error.message : 'Unable to complete owner setup.';
          return;
        }
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
    if (!PREVIEW_MODE && enrollmentMode === 'connect') {
      if (!servers.length) { setupError = 'Pairing is unavailable until a pending server identity exists; choose skip for now.'; return; }
      try {
        const job = await apiClient.enrollServer(selectedServerId || servers[0].id, { token: pairingToken, idempotency_key: operationKey('enroll') });
        recordJob(job);
        pairingToken = '';
      } catch (error) {
        setupError = error instanceof Error ? error.message : 'Unable to queue enrollment.';
        if (error instanceof ApiError && error.authExpired) authExpired = true;
        return;
      }
    }
    showNotice(PREVIEW_MODE ? `Preview setup saved for ${workspaceName}. No credential was sent.` : `Owner setup saved for ${workspaceName}.`);
    navigate('overview');
    if (!PREVIEW_MODE && sessionState === 'authenticated') void loadApiData();
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
    return value === null ? '—' : `${value.toFixed(2)}%`;
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

  function displayState(server: ApiServer): DisplayState {
    if (server.connection_state === 'revoked') return 'disabled';
    if (server.connection_state === 'never-connected') return 'pending';
    if (server.connection_state === 'disconnected') return 'unreachable';
    if (server.freshness_state === 'stale') return 'stale';
    return server.freshness_state === 'fresh' ? 'healthy' : 'failed';
  }

  function emptyApiServer(server: ApiServer): PreviewServer {
    return {
      id: server.id, name: server.name, address: server.address, role: server.role, architecture: server.architecture,
      platform: server.platform, capabilities: [...server.capabilities], version: server.version ?? '—',
      lastHeartbeat: server.last_heartbeat ?? null, connectionState: server.connection_state,
      freshnessState: server.freshness_state, freshnessReason: server.freshness_reason,
      configurationRevision: server.configuration_revision, displayState: displayState(server),
      metrics: { cpu: null, memory: null, disk: null }, traffic: { scope: 'monthly', from: '', to: '', timezone: 'UTC', allowanceBytes: '0', direction: 'combined', countedBytes: '0', continuity: 'uncertain' }
    };
  }

  function rangeWindow(range: ChartRange): { from: string; to: string } {
    const to = new Date();
    const minutes = range === '15m' ? 15 : range === '1h' ? 60 : 24 * 60;
    return { from: new Date(to.getTime() - minutes * 60_000).toISOString(), to: to.toISOString() };
  }

  function metricChart(query: MetricQuery): PreviewChartData {
    const samples = [...query.samples].sort((a, b) => Date.parse(a.observed_at) - Date.parse(b.observed_at));
    const value = (sample: MetricQuery['samples'][number], name: string) => {
      const candidates = name === 'cpu' ? ['cpu', 'cpu.utilization'] : name === 'memory' ? ['memory', 'memory.used_percent', 'memory.utilization'] : ['disk', 'disk.used_percent', 'disk.utilization'];
      const found = candidates.map((candidate) => sample.values[candidate] ?? sample.values[candidate.toUpperCase()]).find((entry) => typeof entry === 'number');
      return typeof found === 'number' && Number.isFinite(found) ? found : null;
    };
    const startedAt = samples.length ? Date.parse(samples[0].observed_at) : 0;
    const networkRate = (name: string) => samples.map((sample, index) => {
      if (index === 0) return null;
      try {
        const current = BigInt(sample.counters?.[name] ?? '');
        const previous = BigInt(samples[index - 1].counters?.[name] ?? '');
        const seconds = (Date.parse(sample.observed_at) - Date.parse(samples[index - 1].observed_at)) / 1000;
        return current >= previous && seconds > 0 ? Number(current - previous) / seconds : null;
      } catch { return null; }
    });
    return {
      timestamps: samples.map((sample) => (Date.parse(sample.observed_at) - startedAt) / 1000),
      cpu: samples.map((sample) => value(sample, 'cpu')),
      memory: samples.map((sample) => value(sample, 'memory')),
      disk: samples.map((sample) => value(sample, 'disk')),
      networkRx: networkRate('net.billing.rx_bytes'),
      networkTx: networkRate('net.billing.tx_bytes'),
      coverage: samples.length === 0 ? 'unavailable' : (query.gaps?.length || Object.values(query.coverage).some((coverage) => coverage < 1) ? 'gap' : 'complete')
    };
  }

  function applyMetric(query: MetricQuery, server: PreviewServer): void {
    const latest = [...query.samples].sort((a, b) => Date.parse(b.observed_at) - Date.parse(a.observed_at))[0];
    if (!latest) return;
    const read = (names: string[]) => names.map((name) => latest.values[name]).find((value) => typeof value === 'number' && Number.isFinite(value)) ?? null;
    server.metrics = { cpu: read(['cpu', 'cpu.utilization']), memory: read(['memory', 'memory.used_percent', 'memory.utilization']), disk: read(['disk', 'disk.used_percent', 'disk.utilization']) };
  }

  async function enrichApiServer(server: PreviewServer, signal: AbortSignal): Promise<{ server: PreviewServer; partial: boolean }> {
    const [detailResult, metricResults, trafficResult] = await Promise.all([
      apiClient.getServer(server.id, { signal }).catch((error) => { if (error instanceof ApiError && error.authExpired) authExpired = true; return null; }),
      Promise.allSettled((['15m', '1h', '24h'] as ChartRange[]).map((range) => { const window = rangeWindow(range); return apiClient.queryMetrics(server.id, { ...window, resolution: range === '15m' ? 'raw' : range === '1h' ? 'minute' : 'hour', signal }); })),
      apiClient.queryTraffic(server.id, { ...rangeWindow('24h'), limit: 200, signal }).catch((error) => { if (error instanceof ApiError && error.authExpired) authExpired = true; return null; })
    ]);
    const enriched = detailResult ? emptyApiServer(detailResult) : { ...server, capabilities: [...server.capabilities] };
    let partial = !detailResult;
    const ranges: Record<ChartRange, PreviewChartData> = { '15m': { timestamps: [], cpu: [], memory: [], disk: [], coverage: 'unavailable' }, '1h': { timestamps: [], cpu: [], memory: [], disk: [], coverage: 'unavailable' }, '24h': { timestamps: [], cpu: [], memory: [], disk: [], coverage: 'unavailable' } };
    (['15m', '1h', '24h'] as ChartRange[]).forEach((range, index) => { const result = metricResults[index]; if (result.status === 'fulfilled') { ranges[range] = metricChart(result.value); if (range === '15m') applyMetric(result.value, enriched); } else { partial = partial || server.capabilities.includes('metrics'); if (result.reason instanceof ApiError && result.reason.authExpired) authExpired = true; } });
    if (metricResults.some((result) => result.status === 'fulfilled')) enriched.metricHistory = { ranges };
    if (trafficResult?.periods[0]) { const period = trafficResult.periods[0]; enriched.traffic = { scope: period.scope, from: period.from, to: period.to, timezone: period.timezone, allowanceBytes: period.allowance_bytes, direction: period.direction, countedBytes: period.counted_bytes, continuity: period.continuity }; } else partial = partial || server.capabilities.includes('traffic');
    return { server: enriched, partial };
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

  async function loadApiData() {
    apiAbortController?.abort();
    const controller = new AbortController();
    apiAbortController = controller;
    previewState = 'loading';
    apiError = '';
    partialWarning = '';
    authExpired = false;
    servers = [];
    try {
      const page = await apiClient.listServers({ signal: controller.signal });
      if (controller.signal.aborted) return;
      sessionState = 'authenticated';
      servers = page.items.map(emptyApiServer);
      selectedServerId = servers[0]?.id ?? '';
      restoreState(savedUiState);
      previewState = servers.length ? 'ready' : 'empty';
      const enriched = await mapWithConcurrency(servers, 4, controller.signal, (server, signal) => enrichApiServer(server, signal));
      if (!controller.signal.aborted) {
        servers = enriched.map((result) => result.server);
        if (enriched.some((result) => result.partial)) partialWarning = 'Some server data could not be loaded; unavailable values are shown explicitly.';
        if (activePage === 'server' && detailTab === 'logs' && selectedServerId) void loadLogs(selectedServerId);
      }
    } catch (error) {
      if (controller.signal.aborted) return;
      apiError = error instanceof Error ? error.message : 'Unable to reach the Payesh API.';
      authExpired = error instanceof ApiError && error.authExpired;
      if (authExpired) sessionState = 'signed-out';
      previewState = 'error';
    }
  }

  async function loadLogs(serverId: string) {
    if (PREVIEW_MODE) return;
    logAbortController?.abort();
    const controller = new AbortController();
    logAbortController = controller;
    logState = 'loading'; logError = ''; logEntries = [];
    try {
      const sources = await apiClient.listLogSources(serverId, { signal: controller.signal });
      const source = sources.items[0];
      if (!source) { logState = 'empty'; return; }
      const window = rangeWindow('24h');
      const result = await apiClient.queryLogs(serverId, { source: source.id, ...window, limit: 200, signal: controller.signal });
      if (controller.signal.aborted) return;
      logEntries = result.entries.map((entry) => ({ time: entry.timestamp.slice(11, 19), level: (entry.severity ?? 'INFO').toUpperCase() as PreviewLogEntry['level'], text: entry.text, source: source.label, cursor: entry.cursor }));
      logState = logEntries.length ? 'ready' : 'empty';
    } catch (error) {
      if (controller.signal.aborted) return;
      logError = error instanceof Error ? error.message : 'Unable to load logs.';
      authExpired = error instanceof ApiError && error.authExpired;
      logState = 'error';
    }
  }

  function restoreState(state: { activePage?: Page; selectedServerId?: string; detailTab?: DetailTab } | null) {
    if (!state) return;
    if (state.activePage && ['overview', 'servers', 'server', 'alerts', 'packages', 'settings', 'add-server', 'onboarding'].includes(state.activePage)) activePage = state.activePage;
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
    if (PREVIEW_MODE) void loadPreviewData(); else void loadApiData();
    window.history.replaceState({ activePage, selectedServerId, detailTab }, '', window.location.pathname);
    const onPopState = (event: PopStateEvent) => { restoreState(event.state); saveUiState(); };
    window.addEventListener('popstate', onPopState);
    return () => { window.removeEventListener('popstate', onPopState); apiAbortController?.abort(); logAbortController?.abort(); jobPollController?.abort(); };
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
      <button class:active={activePage === 'servers' || activePage === 'server' || activePage === 'add-server'} class="nav-item" type="button" on:click={() => navigate('servers')}>
        <span aria-hidden="true">▦</span><span>Servers</span><span class="nav-count">{servers.length}</span>
      </button>
      <button class:active={activePage === 'alerts'} class="nav-item" type="button" on:click={() => navigate('alerts')}>
        <span aria-hidden="true">!</span><span>Alerts</span><span class="nav-count">{firingAlertCount}</span>
      </button>
      <button class:active={activePage === 'packages'} class="nav-item" type="button" on:click={() => navigate('packages')}>
        <span aria-hidden="true">＋</span><span>Packages</span>
      </button>
      <button class:active={activePage === 'settings'} class="nav-item" type="button" on:click={() => navigate('settings')} aria-current={activePage === 'settings' ? 'page' : undefined}>
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
      <div class="breadcrumbs"><span>Workspace</span><span aria-hidden="true">/</span><strong>{activePage === 'server' ? selectedServer?.name : activePage === 'add-server' ? 'Add server' : activePage.charAt(0).toUpperCase() + activePage.slice(1)}</strong></div>
      <div class="topbar-actions">
        {#if PREVIEW_MODE}
          <label class="preview-control">Data
            <select bind:value={previewState} on:change={() => { if (previewLoadFailed) previewState = 'error'; }} aria-label="Preview data state">
              <option value="ready">Ready</option><option value="loading">Loading</option><option value="empty">Empty</option><option value="error">API error</option>
            </select>
          </label>
        {/if}
        <button class="icon-button" type="button" on:click={toggleTheme} aria-label={`Switch to ${theme === 'light' ? 'dark' : 'light'} theme`}>{theme === 'light' ? '☾' : '☀'}</button>
        {#if !PREVIEW_MODE && sessionState === 'authenticated'}<button class="button ghost small" type="button" on:click={() => void signOut()}>Sign out</button>{/if}
        <button class="button primary small" type="button" on:click={() => navigate('add-server')}>＋ Add server</button>
      </div>
    </header>

    {#if notice}<div class="notice" role="status">{notice}</div>{/if}
    {#if partialWarning}<div class="partial-warning" role="status">{partialWarning}</div>{/if}

    {#if activePage === 'servers'}
      <section class="page" aria-labelledby="servers-title">
        <div class="page-heading"><div><p class="eyebrow">Fleet inventory</p><h1 id="servers-title">Servers</h1><p class="lede">Manage every master and node from one place.</p></div><button class="button primary" type="button" on:click={() => navigate('add-server')}>＋ Add server</button></div>
        <div class="server-list">{#each displayServers as server}<button class="server-row" type="button" on:click={() => selectServer(server)}><span class={`server-state ${server.displayState}`}><i></i></span><span class="server-identity"><strong>{server.name}</strong><small>{server.address || 'Address unavailable'} · {server.role} · {server.platform} · {server.architecture}</small></span><span class="server-status"><span class={`status-pill ${server.displayState}`}><i></i>{stateLabel(server.displayState)}</span><small>{server.freshnessReason || server.connectionState}</small></span><span class="server-arrow">→</span></button>{/each}</div>
      </section>
    {:else if activePage === 'add-server'}
      <section class="page" aria-labelledby="add-server-title">
        <button class="back-link" type="button" on:click={() => navigate('servers')}>← Back to servers</button>
        <div class="page-heading"><div><p class="eyebrow">Fleet expansion</p><h1 id="add-server-title">Add a server</h1><p class="lede">Connect over SSH. Payesh detects the operating system and architecture automatically.</p></div></div>
        <article class="panel"><form class="form-grid" on:submit|preventDefault={() => void createPendingServer()}><label>Server name<input bind:value={newServerName} maxlength="128" placeholder="Production node" required /></label><label>IP address or hostname<input bind:value={installHost} maxlength="255" placeholder="203.0.113.10" required /></label><label>SSH port<input type="number" min="1" max="65535" bind:value={installPort} required /></label><label>SSH user<input bind:value={installUser} placeholder="root" required /></label><label>Password<input type="password" bind:value={installPassword} autocomplete="off" /></label><label>Private key<textarea bind:value={installKey} rows="3" autocomplete="off" placeholder="Use either a password or private key"></textarea></label><label>Expected host-key fingerprint <small>(recommended)</small><input bind:value={installFingerprint} placeholder="SHA256:…" /></label><div class="setup-actions"><button class="button ghost" type="button" on:click={() => navigate('servers')}>Cancel</button><button class="button primary" type="submit" disabled={createServerBusy || (!installPassword && !installKey)}>{createServerBusy ? 'Connecting…' : 'Add and install server'}</button></div></form>{#if jobError}<p class="form-error">{jobError}</p>{/if}</article>
      </section>
    {:else if activePage === 'alerts'}
      <section class="page" aria-labelledby="alerts-title">
        <div class="page-heading"><div><p class="eyebrow">Incident monitoring</p><h1 id="alerts-title">Alerts</h1><p class="lede">Current alert states reported by the shared incident API.</p></div><button class="button ghost" type="button" on:click={() => void loadAlerts()}>Reload</button></div>
        {#if alertsState === 'loading'}<div class="state-panel"><div class="loading-spinner"></div><h2>Loading alerts</h2></div>
        {:else if alertsState === 'error'}<div class="state-panel error-state"><h2>Could not load alerts</h2><p>{alertsError}</p><button class="button primary" on:click={() => void loadAlerts()}>Retry</button></div>
        {:else if alertsState === 'empty'}<div class="state-panel"><div class="state-icon">✓</div><h2>No active alerts</h2><p>The incident API returned no current alert states.</p></div>
        {:else}<div class="server-list">{#each alerts as alert}<article class="server-row"><span class={`server-state ${alert.state === 'firing' ? 'failed' : alert.state === 'pending' ? 'stale' : 'healthy'}`}><i></i></span><span class="server-identity"><strong>{alert.rule_id}</strong><small>{servers.find((server) => server.id === alert.server_id)?.name || alert.server_id || 'Fleet-wide'}</small></span><span class="server-status"><span class={`status-pill ${alert.state === 'firing' ? 'failed' : alert.state === 'pending' ? 'stale' : 'healthy'}`}><i></i>{alert.state}</span><small>{alert.last_observation ? new Date(alert.last_observation).toLocaleString() : 'Awaiting observation'}</small></span>{#if alert.last_value !== undefined}<span class="server-metric"><strong>{alert.last_value.toFixed(2)}</strong><small>value</small></span>{/if}</article>{/each}</div>{/if}
      </section>
    {:else if activePage === 'packages'}
      <section class="page" aria-labelledby="packages-title">
        <div class="page-heading"><div><p class="eyebrow">Optional capabilities</p><h1 id="packages-title">Packages</h1><p class="lede">Install and manage verified Payesh packages per server.</p></div><button class="button ghost" type="button" on:click={() => void loadModules()}>Reload</button></div>
        <article class="panel package-target"><label>Target server<select bind:value={packageServerId} on:change={() => void loadServerModules()}>{#each servers as server}<option value={server.id}>{server.name} · {server.architecture}</option>{/each}</select></label><p class="muted">Package releases must be signed by the trust key configured on this hub.</p></article>
        {#if modulesState === 'loading'}<div class="state-panel"><div class="loading-spinner"></div><h2>Loading packages</h2></div>
        {:else if modulesState === 'error'}<div class="state-panel error-state"><h2>Could not load packages</h2><p>{modulesError}</p><button class="button primary" on:click={() => void loadModules()}>Retry</button></div>
        {:else if modulesState === 'empty'}<div class="state-panel"><h2>No packages available</h2><p>The approved catalog is currently empty.</p></div>
        {:else}<div class="module-grid">{#each modules as module}<article class="panel module-card"><p class="eyebrow">{module.id}</p><h2>{module.name}</h2><p class="muted">{module.description || 'Optional Payesh capability.'}</p><div class="server-meta"><span>Release <strong>{module.latest_version}</strong></span><span>Status <strong>{moduleState(module.id)?.state || 'not installed'}</strong></span></div>{#if !moduleState(module.id) || ['unavailable', 'available', 'failed'].includes(moduleState(module.id)?.state || '')}<div class="package-files"><label>Manifest<input type="file" accept="application/json,.json" on:change={(event) => packageManifestFile = event.currentTarget.files?.[0] ?? null} /></label><label>Archive<input type="file" accept="application/gzip,.gz,.tgz" on:change={(event) => packageArchiveFile = event.currentTarget.files?.[0] ?? null} /></label><label>Release signature<input bind:value={packageSignature} placeholder="Unpadded base64url signature" /></label><button class="button primary small" type="button" disabled={!packageServerId || !!packageBusy} on:click={() => void packageAction(module, 'install')}>{packageBusy === `${module.id}:install` ? 'Installing…' : 'Install verified release'}</button></div>{:else if moduleState(module.id)?.state === 'installed-disabled'}<div class="job-actions"><button class="button primary small" disabled={!!packageBusy} on:click={() => void packageAction(module, 'enable')}>Enable</button><button class="button ghost small" disabled={!!packageBusy} on:click={() => void packageAction(module, 'remove')}>Remove</button></div>{:else if moduleState(module.id)?.state === 'enabled'}<button class="button ghost small" disabled={!!packageBusy} on:click={() => void packageAction(module, 'disable')}>Disable</button>{/if}</article>{/each}</div>{/if}
        {#if packageError}<p class="form-error" role="alert">{packageError}</p>{/if}
      </section>
    {:else if activePage === 'settings'}
      <section class="page" aria-labelledby="settings-title"><div class="page-heading"><div><p class="eyebrow">Administration</p><h1 id="settings-title">Settings</h1><p class="lede">This hub is configured and protected by your owner account.</p></div></div><article class="panel"><div class="panel-heading"><div><h2>Account and access</h2><p class="muted">Owner authentication is active. Generated credentials are stored on the host and are never shown in the browser.</p></div></div><button class="button ghost" type="button" on:click={() => void signOut()}>Sign out</button></article></section>
    {:else if activePage === 'onboarding' && (PREVIEW_MODE || sessionState !== 'authenticated')}
      <section class="page onboarding-page" aria-labelledby="setup-title">
        <div class="page-heading"><div><p class="eyebrow">Workspace setup</p><h1 id="setup-title">Prepare your local hub</h1><p class="lede">A short, validated setup path for a safe first enrollment.</p></div></div>
        <div class="stepper" aria-label="Setup progress">
          {#each ['Access', 'Defaults', 'Enroll'] as label, index}
            <div class:current={setupStep === index + 1} class:done={setupStep > index + 1} class="step"><span>{setupStep > index + 1 ? '✓' : index + 1}</span>{label}</div>
          {/each}
        </div>
        <div class="onboarding-card">
          {#if setupStep === 1}
            <p class="eyebrow">Step 1 of 3</p><h2>Protect the first connection</h2><p class="muted">{PREVIEW_MODE ? 'These values stay in this preview and are never transmitted.' : 'The one-time setup secret is exchanged once over the authenticated API. Passwords are not persisted by this browser.'}</p>
            <div class="form-grid">
              <label>Workspace name<input bind:value={workspaceName} autocomplete="organization" /></label>
              <label>Bootstrap secret<input type="password" bind:value={setupSecret} minlength="16" autocomplete="new-password" aria-invalid={setupError ? 'true' : undefined} /><small>At least 16 characters.</small></label>
              <label>Owner username<input bind:value={ownerUsername} minlength="3" autocomplete="username" required aria-invalid={setupError ? 'true' : undefined} /><small>Use the generated username from installation.</small></label><label>Owner password<input type="password" bind:value={ownerPassword} minlength="12" autocomplete="new-password" aria-invalid={setupError ? 'true' : undefined} /><small>At least 12 characters.</small></label>
            </div>
          {:else if setupStep === 2}
            <p class="eyebrow">Step 2 of 3</p><h2>Choose safe defaults</h2><p class="muted">Retention and notifications can be changed later without changing the enrollment secret.</p>
            <div class="form-grid"><label>Metric and log retention<select bind:value={retention}><option value="7">7 days</option><option value="30">30 days</option><option value="90">90 days</option></select></label><label>Notifications<select bind:value={notifications}><option value="none">None for now</option><option value="email">Email digest</option><option value="webhook">Webhook (later)</option></select></label></div>
          {:else}
            <p class="eyebrow">Step 3 of 3</p><h2>Enroll the first server</h2><p class="muted">Enrollment is optional. The next package will exchange a short-lived, single-use pairing token and hub fingerprint through the authenticated job flow.</p>
            <div class="choice-row"><label class:chosen={enrollmentMode === 'skip'}><input type="radio" bind:group={enrollmentMode} value="skip" /> Skip for now</label><label class:chosen={enrollmentMode === 'connect'}><input type="radio" bind:group={enrollmentMode} value="connect" /> Connect with a pairing token</label></div>
            {#if enrollmentMode === 'connect'}
              {#if PREVIEW_MODE}<label class="pairing-field">Pairing token<input type="password" bind:value={pairingToken} minlength="16" autocomplete="off" aria-invalid={setupError ? 'true' : undefined} /><small>At least 16 characters. Preview only; not persisted or transmitted.</small></label>
              {:else if servers.length > 0}<label class="pairing-field">Pending server<select bind:value={selectedServerId}>{#each servers as server}<option value={server.id}>{server.name} · {server.id}</option>{/each}</select></label><label class="pairing-field">Pairing token<input type="password" bind:value={pairingToken} minlength="16" autocomplete="off" aria-invalid={setupError ? 'true' : undefined} /><small>Single-use token; it is sent only to the authenticated enrollment endpoint.</small></label>
              {:else}<div class="unavailable-panel"><strong>No pending server identity</strong><span>The server must appear in the authenticated fleet before this pairing route can target it.</span></div>{/if}
            {/if}
            <div class="review-box"><span>Workspace</span><strong>{workspaceName || 'Unnamed workspace'}</strong><span>Retention</span><strong>{retention} days · {notifications === 'none' ? 'notifications off' : notifications}</strong><span>Enrollment</span><strong>{enrollmentMode === 'skip' ? 'skipped' : 'token ready'}</strong></div>
          {/if}
          {#if setupError}<p class="form-error" role="alert">{setupError}</p>{/if}
          <div class="setup-actions"><button class="button ghost" type="button" on:click={() => setupStep > 1 ? setupStep -= 1 : navigate('overview')}>{setupStep > 1 ? 'Back' : 'Cancel'}</button><button class="button primary" type="button" on:click={() => void completeOnboarding()}>{setupStep === 3 ? (enrollmentMode === 'connect' ? 'Queue enrollment' : 'Save setup') : 'Continue'}</button></div>
        </div>
      </section>
    {:else if activePage === 'server' && selectedServer}
      <section class="page server-page" aria-labelledby="server-title">
        <button class="back-link" type="button" on:click={() => navigate('overview')}>← Back to overview</button>
        <div class="page-heading server-heading"><div><p class="eyebrow">Server detail · {selectedServer.role}</p><h1 id="server-title">{selectedServer.name}</h1><p class="lede">{selectedServer.platform} · {selectedServer.architecture} · {selectedServer.version}</p></div><span class={`status-pill ${selectedServer.displayState}`}><i></i>{stateLabel(selectedServer.displayState)}</span></div>
        <div class="server-meta"><span>Address: <strong>{selectedServer.address || 'Unavailable'}</strong></span><span>Connection: <strong>{selectedServer.connectionState}</strong></span><span>Freshness: <strong>{selectedServer.freshnessState}</strong></span><span>Revision: <strong>{selectedServer.configurationRevision}</strong></span>{#if selectedServer.freshnessReason}<span>{selectedServer.freshnessReason}</span>{/if}</div>
        {#if !PREVIEW_MODE && selectedServer.connectionState === 'never-connected'}<article class="panel server-actions"><div class="panel-heading"><div><p class="eyebrow">Connect server</p><h2>Install over SSH</h2></div></div><form class="form-grid" on:submit|preventDefault={() => void submitInstall()}><label>SSH host<input bind:value={installHost} placeholder="hostname or address" required /></label><label>Port<input type="number" min="1" max="65535" bind:value={installPort} required /></label><label>SSH user<input bind:value={installUser} required /></label><label>Password (or private key)<input type="password" bind:value={installPassword} autocomplete="off" /></label><label>Private key<textarea bind:value={installKey} rows="2" autocomplete="off"></textarea></label><label>Expected host-key fingerprint<input bind:value={installFingerprint} placeholder="SHA256:…" /></label><button class="button primary small" type="submit" disabled={installBusy || (!installPassword && !installKey)}>{installBusy ? 'Queueing…' : 'Install Payesh'}</button></form>{#if jobError}<p class="form-error" role="alert">{jobError}</p>{/if}</article>{/if}
        <div class="tabs" role="tablist" aria-label="Server detail sections">
          {#each availableTabs as tab}
            <button class:active={detailTab === tab} type="button" role="tab" aria-selected={detailTab === tab} on:click={() => { detailTab = tab; saveUiState(); if (tab === 'logs') void loadLogs(selectedServer.id); }}>{tabLabel(tab)}</button>
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
          <article class="panel chart-panel"><div class="panel-heading"><div><p class="eyebrow">Live bandwidth</p><h2>Download and upload rate</h2></div><select bind:value={chartRange} aria-label="Traffic chart time range"><option value="15m">Last 15 minutes</option><option value="1h">Last hour</option><option value="24h">Last 24 hours</option></select></div>{#if chartData?.networkRx?.some((v) => v !== null) || chartData?.networkTx?.some((v) => v !== null)}{#key `${chartRange}-${selectedServer.id}-traffic`}<TrafficChart data={chartData} />{/key}<div class="legend"><span><i class="legend-dot teal"></i>Download</span><span><i class="legend-dot blue"></i>Upload</span><span>Rate: Mbit/s</span></div>{:else}<div class="unavailable-panel"><strong>Waiting for traffic samples</strong><span>At least two consecutive network counter samples are needed to calculate a rate.</span></div>{/if}</article>
          {#if selectedServer.traffic.from}<article class="panel traffic-panel"><div class="panel-heading"><div><p class="eyebrow">Traffic allowance</p><h2>{selectedServer.traffic.scope} window</h2></div><span class="status-pill {selectedServer.traffic.continuity === 'complete' ? 'healthy' : 'stale'}"><i></i>{selectedServer.traffic.continuity}</span></div><div class="traffic-number"><strong>{formatBytes(selectedServer.traffic.countedBytes)}</strong><span>of {formatBytes(selectedServer.traffic.allowanceBytes)}</span></div><div class="progress"><span style={`width:${percentage(selectedServer.traffic.countedBytes, selectedServer.traffic.allowanceBytes)}%`}></span></div><div class="traffic-details"><span>Direction<strong>{selectedServer.traffic.direction}</strong></span><span>Timezone<strong>{selectedServer.traffic.timezone}</strong></span><span>Window<strong>{selectedServer.traffic.from.slice(0, 10)} → {selectedServer.traffic.to.slice(0, 10)}</strong></span><span>Continuity<strong>{selectedServer.traffic.continuity}</strong></span></div></article>{:else}<div class="unavailable-panel"><strong>Traffic allowance not configured</strong><span>Bandwidth monitoring is active. Configure a monthly allowance to enable quota usage and continuity tracking.</span></div>{/if}
        {:else if detailTab === 'logs' && hasCapability(selectedServer, 'logs')}
          <article class="panel logs-panel"><div class="panel-heading"><div><p class="eyebrow">Bounded snapshot</p><h2>Recent logs</h2></div><button class="button ghost small" type="button" on:click={() => PREVIEW_MODE ? showNotice('Live tail will be connected to the bounded stream contract in package 03.') : void loadLogs(selectedServer.id)}>Reload</button></div>{#if !PREVIEW_MODE && logState === 'loading'}<div class="state-panel"><div class="loading-spinner" aria-hidden="true"></div><h2>Loading logs</h2></div>{:else if !PREVIEW_MODE && logState === 'error'}<div class="unavailable-panel"><strong>Could not load logs</strong><span>{logError}</span><button class="button ghost small" type="button" on:click={() => void loadLogs(selectedServer.id)}>Retry</button></div>{:else if !PREVIEW_MODE && logState === 'empty'}<div class="unavailable-panel"><strong>No log entries</strong><span>No entries were returned for the selected source.</span></div>{:else}<div class="log-list">{#each PREVIEW_MODE ? previewLogEntries : logEntries as entry}<div class="log-entry"><time>{entry.time}</time><span class={`log-level ${entry.level.toLowerCase()}`}>{entry.level}</span><span class="log-text">{entry.text}<small>{entry.source} · {entry.cursor}</small></span></div>{/each}</div>{/if}</article>
        {:else if selectedServer && detailTab !== 'metrics' && !hasCapability(selectedServer, detailTab)}
          <div class="unavailable-panel large"><strong>{tabLabel(detailTab)} unavailable</strong><span>{capabilityMessage(selectedServer, detailTab)}</span></div>
        {:else if selectedServer && !hasCapability(selectedServer, 'metrics')}
          <div class="unavailable-panel large"><strong>Monitoring data unavailable</strong><span>{capabilityMessage(selectedServer, 'metrics')}</span></div>
        {/if}
      </section>
    {:else}
      <section class="page overview-page" aria-labelledby="overview-title">
        <div class="page-heading"><div><p class="eyebrow">{PREVIEW_MODE ? 'Live preview' : 'Authenticated workspace'}</p><h1 id="overview-title">{PREVIEW_MODE ? 'Good afternoon, Kia' : 'Fleet overview'}</h1><p class="lede">A clear view of your fleet, with freshness and uncertainty kept visible.</p></div>{#if PREVIEW_MODE}<span class="date-stamp">09 Sep 2026 · 14:42 UTC</span>{:else}<span class="date-stamp">Live API data</span>{/if}</div>
        {#if authExpired}
          <div class="login-screen"><div class="login-card"><div class="brand-mark">P</div><p class="eyebrow">PAYESH · LOCAL OPERATIONS</p><h2>Welcome back</h2><p class="muted">Sign in to manage your fleet securely.</p><form class="auth-form" on:submit|preventDefault={() => void submitLogin()}><label>Username<input bind:value={authUsername} autocomplete="username" required /></label><label>Password<input type="password" bind:value={authPassword} autocomplete="current-password" required /></label>{#if authError}<p class="form-error" role="alert">{authError}</p>{/if}<button class="button primary" type="submit" disabled={authBusy}>{authBusy ? 'Signing in…' : 'Sign in'}</button></form></div></div>
        {:else if previewState === 'loading'}
          <div class="state-panel"><div class="loading-spinner" aria-hidden="true"></div><h2>Loading fleet data</h2><p>Reading the bounded server summary.</p></div>
        {:else if previewState === 'empty'}
          <div class="state-panel"><div class="state-icon">＋</div><h2>No servers enrolled</h2><p>Add a Linux server to begin monitoring health and traffic.</p><button class="button primary" type="button" on:click={() => navigate('add-server')}>Add first server</button></div>
        {:else if previewState === 'error'}
          <div class="state-panel error-state"><div class="state-icon">!</div><h2>Could not load the fleet</h2><p>{apiError || 'The API error is explicit and retryable; no stale fixture data is substituted.'}</p><button class="button primary" type="button" on:click={() => PREVIEW_MODE ? void loadPreviewData() : void loadApiData()}>Retry</button></div>
        {:else}
          <div class="summary-grid"><article class="summary-card"><span>Healthy servers</span><strong>{healthyCount}<small> / {displayServers.length}</small></strong><span class="summary-note positive">↑ Fresh enough to act</span></article><article class="summary-card"><span>Needs attention</span><strong>{attentionCount}</strong><span class="summary-note warning">Includes stale and offline</span></article><article class="summary-card"><span>Month-to-date traffic</span><strong>{overviewAllowanceBytes === '0' ? 'Not configured' : formatBytes(overviewTrafficBytes)}</strong><span class="summary-note">{overviewAllowanceBytes === '0' ? 'Live bandwidth remains available per server' : `of ${formatBytes(overviewAllowanceBytes)} allowance`}</span></article></div>
          <div class="section-heading"><div><p class="eyebrow">Fleet health</p><h2>Servers</h2></div><span class="muted">Sorted by attention first</span></div>
          <div class="server-list">{#each displayServers as server}<button class="server-row" type="button" on:click={() => selectServer(server)}><span class={`server-state ${server.displayState}`} aria-label={stateLabel(server.displayState)}><i></i></span><span class="server-identity"><strong>{server.name}</strong><small>{server.address || 'Address unavailable'} · {server.platform} · {server.architecture}</small></span><span class="server-status"><span class={`status-pill ${server.displayState}`}><i></i>{stateLabel(server.displayState)}</span><small>{server.freshnessState === 'unknown' ? server.freshnessReason : `last heartbeat ${server.lastHeartbeat?.slice(11, 16)} UTC`}</small></span><span class="server-metric"><strong>{metricValue(server.metrics.cpu)}</strong><small>CPU</small></span><span class="server-arrow" aria-hidden="true">→</span></button>{/each}</div>
          <div class="lower-grid"><article class="panel"><div class="panel-heading"><div><p class="eyebrow">Traffic</p><h2>Allowance overview</h2></div><button class="text-button" type="button" on:click={() => selectedServer && selectServer(selectedServer)}>View details →</button></div>{#if overviewAllowanceBytes === '0'}<div class="unavailable-panel"><strong>No allowance configured</strong><span>Open a server’s Traffic tab to view live upload and download rates.</span></div>{:else}<div class="traffic-number"><strong>{formatBytes(overviewTrafficBytes)}</strong><span>used this month</span></div><div class="progress"><span style={`width:${percentage(overviewTrafficBytes, overviewAllowanceBytes)}%`}></span></div><p class="muted">{formatBytes(overviewAllowanceBytes)} combined allowance · UTC</p>{/if}</article><article class="panel"><div class="panel-heading"><div><p class="eyebrow">Recent activity</p><h2>Latest log signal</h2></div><span class="status-dot-label"><i></i> bounded</span></div><div class="activity-item"><span class="activity-icon">✓</span><div><strong>Heartbeat accepted</strong><small>Live API activity</small></div></div><div class="activity-item"><span class="activity-icon warning">!</span><div><strong>Freshness visible</strong><small>Inspect each server for current state</small></div></div></article></div>
          {#if latestJob}<article class="panel job-panel" aria-live="polite"><div class="panel-heading"><div><p class="eyebrow">Job status</p><h2>{latestJob.kind}</h2></div><span class={`status-pill ${latestJob.state}`}><i></i>{latestJob.state}</span></div><div class="job-progress"><span style={`width:${Math.max(0, Math.min(100, latestJob.progress))}%`}></span></div><p class="muted">{latestJob.progress}% · revision {latestJob.revision} · {latestJob.id}</p>{#if latestJob.error}<p class="form-error">{latestJob.error.message || 'The job failed.'}</p>{/if}{#if jobError}<p class="form-error" role="alert">{jobError}</p>{/if}<div class="job-actions">{#if ['queued', 'running'].includes(latestJob.state)}<button class="button ghost small" type="button" on:click={() => void cancelLatestJob()}>Cancel job</button>{/if}<button class="button ghost small" type="button" on:click={() => void pollJob(latestJob?.id ?? '')} disabled={jobBusy}>Reload status</button></div></article>{/if}
        {/if}
      </section>
    {/if}

    <footer><span>{PREVIEW_MODE ? 'Preview adapter · values are fixtures' : 'Authenticated API adapter · live data'}</span><span>Payesh foundation · local-first</span></footer>
  </main>
</div>

<style>
  :global(*) { box-sizing: border-box; }
  .login-screen { position: fixed; inset: 0; z-index: 20; display: grid; place-items: center; padding: 24px; background: rgba(10, 18, 16, .96); }
  .login-card { width: min(440px, 100%); padding: 42px; border: 1px solid var(--line); border-radius: 24px; background: var(--surface); box-shadow: 0 24px 80px rgba(0,0,0,.35); }
  .login-card h2 { margin: 8px 0; font-size: 38px; }
  .login-card .auth-form { margin-top: 28px; }
  .brand-mark { width: 48px; height: 48px; display: grid; place-items: center; border-radius: 14px; background: var(--accent); color: #10201c; font-size: 24px; font-weight: 800; }
  .module-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: 18px; }
  .module-card h2 { margin: 6px 0 10px; }
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
  .partial-warning { margin: 14px 42px 0; padding: 10px 13px; border: 1px solid var(--warning); border-radius: 9px; background: var(--warning-bg); color: var(--warning); font-size: 12px; }
  .page { max-width: 1240px; margin: 0 auto; padding: 44px 42px 30px; }.page-heading { justify-content: space-between; gap: 20px; margin-bottom: 34px; }.eyebrow { margin: 0 0 8px; color: var(--teal); font-size: 11px; font-weight: 800; letter-spacing: .11em; text-transform: uppercase; }h1, h2, p { margin-top: 0; }h1 { margin-bottom: 8px; font-size: clamp(29px, 4vw, 42px); letter-spacing: -.04em; }h2 { margin-bottom: 0; font-size: 18px; letter-spacing: -.02em; }.lede { margin: 0; color: var(--muted); }.date-stamp, .muted { color: var(--muted); font-size: 12px; }
  .button { border: 1px solid var(--line); border-radius: 9px; padding: 10px 15px; background: var(--surface); color: var(--ink); font-weight: 700; }.button.small { padding: 8px 12px; font-size: 12px; }.button.primary { border-color: var(--teal); background: var(--teal); color: var(--primary-contrast); }.button.ghost { background: transparent; }.text-button, .back-link { border: 0; background: transparent; color: var(--teal); font-weight: 700; }.back-link { margin-bottom: 26px; padding: 0; }.notice { position: fixed; z-index: 5; top: 88px; right: 28px; max-width: 360px; padding: 12px 15px; border: 1px solid color-mix(in srgb, var(--teal) 35%, var(--line)); border-radius: 10px; background: var(--surface); box-shadow: var(--shadow); color: var(--ink); font-size: 13px; }
  .summary-grid, .metric-grid, .lower-grid { display: grid; gap: 14px; }.summary-grid { grid-template-columns: repeat(3, 1fr); margin-bottom: 42px; }.summary-card, .metric-card, .panel, .onboarding-card { border: 1px solid var(--line); border-radius: 14px; background: var(--surface); box-shadow: var(--shadow); }.summary-card { padding: 20px; }.summary-card > span:first-child { color: var(--muted); font-size: 12px; }.summary-card strong { display: block; margin: 10px 0 4px; font-size: 28px; letter-spacing: -.04em; }.summary-card strong small { color: var(--muted); font-size: 13px; font-weight: 500; }.summary-note { color: var(--muted); font-size: 11px; }.summary-note.positive { color: var(--teal); }.summary-note.warning { color: var(--warning); }.section-heading { display: flex; align-items: end; justify-content: space-between; margin-bottom: 12px; }.section-heading h2 { font-size: 24px; }
  .server-list { overflow: hidden; border: 1px solid var(--line); border-radius: 14px; background: var(--surface); box-shadow: var(--shadow); }.server-row { gap: 15px; width: 100%; padding: 17px 18px; border: 0; border-bottom: 1px solid var(--line); background: transparent; color: var(--ink); text-align: left; }.server-row:last-child { border-bottom: 0; }.server-row:hover { background: var(--surface-muted); }.server-state { width: 9px; height: 9px; flex: 0 0 auto; border-radius: 50%; background: var(--teal); }.server-state.stale, .server-state.pending, .server-state.installing { background: var(--warning); }.server-state.unreachable, .server-state.failed { background: var(--danger); }.server-state.disabled, .server-state.unsupported { background: var(--muted); }.server-identity { display: flex; flex: 1; flex-direction: column; gap: 4px; min-width: 0; overflow: hidden; }.server-identity strong, .server-identity small { overflow-wrap: anywhere; word-break: break-word; }.server-identity small, .server-status small, .server-metric small { color: var(--muted); font-size: 11px; }.server-status { display: flex; flex: 1; flex-direction: column; gap: 4px; min-width: 0; }.server-metric { display: flex; flex-direction: column; align-items: end; gap: 3px; min-width: 50px; }.server-arrow { color: var(--muted); font-size: 19px; }.status-pill { display: inline-flex; align-items: center; gap: 6px; width: fit-content; padding: 5px 8px; border-radius: 99px; background: var(--teal-bg); color: var(--teal); font-size: 11px; font-weight: 700; text-transform: capitalize; }.status-pill i { width: 6px; height: 6px; border-radius: 50%; background: currentColor; }.status-pill.stale, .status-pill.pending, .status-pill.installing { background: var(--warning-bg); color: var(--warning); }.status-pill.unreachable, .status-pill.failed { background: var(--danger-bg); color: var(--danger); }.status-pill.disabled, .status-pill.unsupported { background: var(--surface-muted); color: var(--muted); }
  .lower-grid { grid-template-columns: 1fr 1fr; margin-top: 20px; }.panel { padding: 22px; }.panel-heading { justify-content: space-between; gap: 14px; margin-bottom: 20px; }.panel-heading select, .form-grid select, .form-grid input { width: 100%; padding: 10px 11px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); color: var(--ink); }.panel-heading select { width: auto; }.traffic-number { display: flex; align-items: baseline; gap: 8px; margin: 10px 0 16px; }.traffic-number strong { font-size: 32px; letter-spacing: -.05em; }.traffic-number span { color: var(--muted); font-size: 12px; }.progress { height: 8px; overflow: hidden; border-radius: 20px; background: var(--surface-muted); }.progress span { display: block; height: 100%; border-radius: inherit; background: var(--teal); }.traffic-panel .progress span { background: var(--purple); }.panel > .muted { margin: 12px 0 0; }.activity-item { gap: 12px; padding: 12px 0; border-bottom: 1px solid var(--line); }.activity-item:last-child { border-bottom: 0; }.activity-icon { display: grid; place-items: center; width: 27px; height: 27px; border-radius: 8px; background: var(--teal-bg); color: var(--teal); }.activity-icon.warning { background: var(--warning-bg); color: var(--warning); }.activity-item strong, .activity-item small { display: block; }.activity-item small { margin-top: 3px; color: var(--muted); font-size: 11px; }.status-dot-label { display: flex; align-items: center; gap: 7px; color: var(--muted); font-size: 11px; }
  .server-heading { margin-bottom: 14px; }.server-meta { flex-wrap: wrap; gap: 8px 18px; margin-bottom: 25px; color: var(--muted); font-size: 12px; }.server-meta strong { color: var(--ink); font-weight: 600; }.tabs { display: flex; align-items: end; gap: 21px; margin-bottom: 20px; border-bottom: 1px solid var(--line); }.tabs button { padding: 10px 2px 12px; border: 0; border-bottom: 2px solid transparent; background: transparent; color: var(--muted); }.tabs button.active { border-color: var(--teal); color: var(--ink); font-weight: 700; }.tab-empty { padding-bottom: 13px; }.metric-grid { grid-template-columns: repeat(3, 1fr); margin-bottom: 14px; }.metric-card { padding: 17px; }.card-top { justify-content: space-between; }.card-top span { color: var(--muted); font-size: 12px; }.card-top strong { font-size: 22px; }.sparkline { height: 45px; margin: 12px 0 6px; }.sparkline-unavailable { display: grid; place-items: center; height: 45px; margin: 12px 0 6px; border-radius: 7px; background: var(--surface-muted); color: var(--muted); font-size: 10px; }.metric-card small { color: var(--muted); font-size: 10px; }.legend { flex-wrap: wrap; gap: 16px; margin-bottom: 8px; color: var(--muted); font-size: 11px; }.legend span { display: inline-flex; align-items: center; gap: 5px; }.legend-dot { width: 7px; height: 7px; border-radius: 50%; }.legend-dot.teal { background: var(--teal); }.legend-dot.purple { background: var(--purple); }.chart-summary { margin: 7px 0 0; color: var(--muted); font-size: 11px; }.unavailable-panel { display: grid; gap: 6px; padding: 32px 18px; border: 1px dashed var(--line); border-radius: 10px; background: var(--surface-muted); color: var(--muted); font-size: 12px; }.unavailable-panel strong { color: var(--ink); }.unavailable-panel.large { padding: 60px 22px; text-align: center; }.traffic-details { display: grid; grid-template-columns: repeat(4, 1fr); gap: 12px; margin-top: 24px; }.traffic-details span { color: var(--muted); font-size: 11px; }.traffic-details strong { display: block; margin-top: 5px; color: var(--ink); font-size: 12px; text-transform: capitalize; }.log-list { border-top: 1px solid var(--line); }.log-entry { display: grid; grid-template-columns: 70px 50px 1fr; gap: 10px; padding: 13px 0; border-bottom: 1px solid var(--line); font-size: 12px; }.log-entry time, .log-text small { color: var(--muted); }.log-level { font-weight: 800; }.log-level.info { color: var(--teal); }.log-level.warn { color: var(--warning); }.log-text small { display: block; margin-top: 4px; font-size: 10px; }
  .stepper { display: flex; gap: 24px; margin-bottom: 24px; }.step { display: flex; align-items: center; gap: 8px; color: var(--muted); font-size: 13px; }.step span { display: grid; place-items: center; width: 25px; height: 25px; border: 1px solid var(--line); border-radius: 50%; }.step.current { color: var(--ink); font-weight: 700; }.step.current span, .step.done span { border-color: var(--teal); background: var(--teal); color: white; }.onboarding-card { max-width: 760px; padding: 30px; }.onboarding-card h2 { margin-bottom: 10px; font-size: 24px; }.form-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 16px; margin: 26px 0; }.form-grid label { display: flex; flex-direction: column; gap: 7px; color: var(--muted); font-size: 12px; }.form-grid label:first-child { grid-column: 1 / -1; }.form-grid small { color: var(--muted); font-size: 10px; }.setup-actions { justify-content: space-between; padding-top: 20px; border-top: 1px solid var(--line); }.form-error { margin: 0 0 15px; color: var(--danger); font-size: 12px; }.review-box { display: grid; grid-template-columns: 150px 1fr; gap: 12px; margin: 26px 0; padding: 18px; border-radius: 10px; background: var(--surface-muted); color: var(--muted); font-size: 12px; }.review-box strong { color: var(--ink); }
  .choice-row { display: flex; flex-wrap: wrap; gap: 10px; margin: 24px 0 14px; }.choice-row label { display: flex; align-items: center; gap: 8px; padding: 11px 13px; border: 1px solid var(--line); border-radius: 9px; color: var(--muted); font-size: 12px; }.choice-row label.chosen { border-color: var(--teal); background: var(--teal-bg); color: var(--ink); }.pairing-field { display: flex; flex-direction: column; gap: 7px; max-width: 470px; color: var(--muted); font-size: 12px; }.pairing-field input { padding: 10px 11px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); color: var(--ink); }.pairing-field small { color: var(--muted); font-size: 10px; }
  .state-panel { display: grid; justify-items: center; gap: 10px; padding: 80px 20px; border: 1px dashed var(--line); border-radius: 14px; background: var(--surface); text-align: center; }.state-panel h2 { margin: 0; }.state-panel p { max-width: 460px; margin-bottom: 8px; color: var(--muted); font-size: 13px; }.state-icon { display: grid; place-items: center; width: 44px; height: 44px; border-radius: 50%; background: var(--surface-muted); color: var(--teal); font-size: 24px; }.error-state .state-icon { color: var(--danger); }.loading-spinner { width: 32px; height: 32px; border: 3px solid var(--line); border-top-color: var(--teal); border-radius: 50%; animation: spin 800ms linear infinite; }@keyframes spin { to { transform: rotate(360deg); } }
  .auth-form { display: grid; gap: 12px; width: min(100%, 340px); text-align: left; }.auth-form label { display: grid; gap: 6px; color: var(--muted); font-size: 12px; }.auth-form input { width: 100%; padding: 10px 11px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); color: var(--ink); }.server-actions, .job-panel { margin-top: 20px; }.package-target { display: flex; align-items: end; justify-content: space-between; gap: 18px; margin-bottom: 18px; }.package-target label, .package-files label { display: grid; gap: 6px; color: var(--muted); font-size: 12px; }.package-target select, .package-files input { width: 100%; padding: 10px 11px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); color: var(--ink); }.package-files { display: grid; gap: 10px; margin-top: 16px; }.job-progress { height: 8px; overflow: hidden; border-radius: 20px; background: var(--surface-muted); }.job-progress span { display: block; height: 100%; border-radius: inherit; background: var(--teal); transition: width .2s ease; }.job-actions { display: flex; gap: 10px; justify-content: end; }
  @media (prefers-reduced-motion: reduce) { .loading-spinner { animation: none; } }
  footer { display: flex; justify-content: space-between; gap: 15px; max-width: 1240px; margin: 20px auto 0; padding: 0 42px 25px; color: var(--muted); font-size: 11px; }
  @media (max-width: 900px) { .app-shell { grid-template-columns: 1fr; }.sidebar { position: sticky; top: 0; z-index: 4; flex-direction: row; align-items: center; gap: 20px; padding: 12px 18px; border-right: 0; border-bottom: 1px solid var(--line); }.brand-lockup { flex: 0 0 auto; }.nav-list { flex-direction: row; flex: 1; gap: 2px; overflow-x: auto; }.nav-item { flex: 0 0 auto; width: auto; padding: 9px 11px; white-space: nowrap; }.nav-item.active { box-shadow: inset 0 -3px var(--teal); }.sidebar-footer { display: none; }.topbar { padding: 0 24px; }.page { padding: 32px 24px 20px; }footer { padding: 0 24px 20px; } }
  @media (max-width: 680px) { .topbar { align-items: flex-start; flex-direction: column; gap: 12px; padding: 16px 18px; }.topbar-actions { width: 100%; justify-content: space-between; }.preview-control { margin-right: auto; }.page { padding: 28px 16px 18px; }.page-heading { align-items: flex-start; flex-direction: column; margin-bottom: 25px; }.date-stamp { align-self: flex-start; }.summary-grid, .metric-grid, .lower-grid, .action-grid { grid-template-columns: 1fr; }.server-row { gap: 10px; }.server-status { min-width: 0; }.server-metric { display: none; }.server-arrow { margin-left: auto; }.traffic-details { grid-template-columns: 1fr 1fr; }.form-grid { grid-template-columns: 1fr; }.form-grid label:first-child { grid-column: auto; }.onboarding-card { padding: 22px 18px; }.stepper { justify-content: space-between; gap: 8px; }.step { font-size: 11px; }.notice { top: 130px; right: 16px; left: 16px; max-width: none; }.panel { padding: 18px; }footer { flex-direction: column; padding: 0 16px 18px; }.chart-panel .panel-heading { align-items: flex-start; flex-direction: column; }.panel-heading select { width: 100%; } }
</style>
