<script lang="ts">
  import { onMount } from 'svelte';
  import ProcessTable from './ProcessTable.svelte';
  import ChartPreview from './ChartPreview.svelte';
  import Sparkline from './Sparkline.svelte';
  import TrafficChart from './TrafficChart.svelte';
  import { networkRates, networkRollupRate, formatNetworkRate } from './network';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import InstallProgress, { type InstallProgressData, type InstallStage } from './InstallProgress.svelte';
  import { ApiError, apiClient, mapWithConcurrency, type Account, type HTTPSStatus, type UpdateStatus, type AlertState, type Job, type MetricQuery, type Module, type ModuleInstallation, type PackageSource, type Server as ApiServer } from './api';
  import type { DisplayState, PreviewChartData, PreviewLogEntry, PreviewServer } from './preview/fixtures';

  type Theme = 'light' | 'dark';
  type Page = 'overview' | 'monitoring' | 'servers' | 'server' | 'alerts' | 'packages' | 'settings' | 'add-server' | 'install-progress' | 'onboarding';
  type DetailTab = 'metrics' | 'traffic' | 'logs' | 'processes';
  type ChartRange = '15m' | '1h' | '24h';
  type PreviewState = 'ready' | 'loading' | 'empty' | 'error';

  // Development builds use the real API by default. Opt into fixtures
  // explicitly so a local preview can never accidentally mask API failures.
  const PREVIEW_MODE = import.meta.env.VITE_PAYESH_PREVIEW === 'true';
  // Plain HTTP to anything but this machine sends the password unencrypted.
  const insecureConnection = typeof window !== 'undefined' && window.location.protocol === 'http:' && !['localhost', '127.0.0.1', '[::1]'].includes(window.location.hostname);
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
  let httpsStatus: HTTPSStatus | null = null;
  let settingsSection: 'users' | 'updates' | 'tls' = 'tls';
  let addingUser = false;
  let userSearch = '';
  let dashboardPort = '';
  let portBusy = false;
  let portError = '';
  let httpsDomain = '';
  let httpsEmail = '';
  let httpsToken = '';
  let httpsBusy = false;
  let httpsError = '';
  let httpsPoll: ReturnType<typeof setTimeout> | null = null;
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
  let myAccount: Account | null = PREVIEW_MODE ? { username: 'admin', role: 'owner', permission: 'edit' } : null;
  let accounts: Account[] = myAccount ? [myAccount] : [];
  let accountUsername = '';
  let accountPassword = '';
  let accountRole: 'admin' | 'member' = 'member';
  let accountPermission: 'read' | 'edit' = 'read';
  let accountError = '';
  let accountBusy = false;
  let editingAccount = '';
  let editUsername = '';
  let editPassword = '';
  let editRole: 'owner' | 'admin' | 'member' = 'member';
  let editPermission: 'read' | 'edit' = 'read';
  let setupCompleted = false;
  let latestJob: Job | null = null;
  let activeInstall: InstallProgressData | null = null;
  let jobError = '';
  let jobBusy = false;
  let jobPollController: AbortController | null = null;
  let updateRelease = '';
  let updateBusy = false;
  let updateStatus: UpdateStatus | null = null;
  let updateCheckBusy = false;
  let updateCheckError = '';
  let installHost = '';
  let installPort = '22';
  let installUser = 'root';
  let installPassword = '';
  let installKey = '';
  let installFingerprint = '';
  let installBusy = false;
  let labelDraft = '';
  let labelBusy = false;
  let newServerName = '';
  let createServerBusy = false;
  let deleteServerBusy = false;
  let modules: Module[] = [];
  let modulesState: PreviewState = 'loading';
  let modulesError = '';
  let packageServerId = '';
  let moduleInstallations: ModuleInstallation[] = [];
  let packageBusy = '';
  let packageError = '';
  let alerts: AlertState[] = [];
  let alertsState: PreviewState = 'loading';
  let alertsError = '';

  interface ModalDialogState {
    open: boolean;
    title: string;
    description: string;
    tone: 'danger' | 'warning' | 'info' | 'primary';
    icon: string;
    confirmText: string;
    cancelText: string;
    hideCancel: boolean;
    busy: boolean;
    action?: () => Promise<void> | void;
  }

  let modalDialog: ModalDialogState = {
    open: false,
    title: '',
    description: '',
    tone: 'primary',
    icon: '',
    confirmText: 'Confirm',
    cancelText: 'Cancel',
    hideCancel: false,
    busy: false,
    action: undefined
  };

  const ACTIVE_INSTALL_STORAGE_KEY = 'payesh_active_install';

  function saveActiveInstall(data: InstallProgressData | null) {
    activeInstall = data;
    if (typeof window === 'undefined') return;
    if (data) {
      window.localStorage.setItem(ACTIVE_INSTALL_STORAGE_KEY, JSON.stringify(data));
    } else {
      window.localStorage.removeItem(ACTIVE_INSTALL_STORAGE_KEY);
    }
  }

  function openConfirmModal(opts: {
    title: string;
    description: string;
    tone?: 'danger' | 'warning' | 'info' | 'primary';
    icon?: string;
    confirmText?: string;
    cancelText?: string;
    hideCancel?: boolean;
    action: () => Promise<void> | void;
  }) {
    modalDialog = {
      open: true,
      title: opts.title,
      description: opts.description,
      tone: opts.tone ?? 'primary',
      icon: opts.icon ?? (opts.tone === 'danger' ? 'trash' : 'alert-circle'),
      confirmText: opts.confirmText ?? 'Confirm',
      cancelText: opts.cancelText ?? 'Cancel',
      hideCancel: opts.hideCancel ?? false,
      busy: false,
      action: opts.action
    };
  }

  function closeConfirmModal() {
    if (modalDialog.busy) return;
    modalDialog.open = false;
  }

  async function handleModalConfirm() {
    if (!modalDialog.action) {
      modalDialog.open = false;
      return;
    }
    modalDialog.busy = true;
    try {
      await modalDialog.action();
      modalDialog.open = false;
    } catch {
      modalDialog.open = false;
    } finally {
      modalDialog.busy = false;
    }
  }

  function promptDeleteServer(server: PreviewServer = selectedServer) {
    if (!server || server.role !== 'node' || deleteServerBusy) return;
    openConfirmModal({
      title: `Delete ${server.name}?`,
      description: `Permanently delete ${server.name} and all associated metrics, logs, and monitoring history. This action cannot be undone.`,
      tone: 'danger',
      icon: 'trash',
      confirmText: 'Delete server',
      cancelText: 'Cancel',
      action: async () => {
        deleteServerBusy = true; jobError = '';
        try {
          const deletedID = server.id;
          await apiClient.deleteServer(deletedID, server.configurationRevision);
          servers = servers.filter((s) => s.id !== deletedID);
          selectedServerId = servers[0]?.id ?? '';
          navigate(servers.length ? 'servers' : 'overview', selectedServerId);
          showNotice('Server and its stored data were deleted.');
          if (activeInstall?.serverId === deletedID) {
            saveActiveInstall(null);
          }
        } catch (error) {
          jobError = error instanceof Error ? error.message : 'Unable to delete server.';
          if (error instanceof ApiError && error.authExpired) authExpired = true;
        } finally {
          deleteServerBusy = false;
        }
      }
    });
  }

  function promptCancelInstall() {
    if (!latestJob || !['queued', 'running'].includes(latestJob.state)) {
      if (activeInstall) saveActiveInstall(null);
      return;
    }
    const targetName = activeInstall?.serverName || selectedServer?.name || 'this server';
    openConfirmModal({
      title: 'Cancel installation?',
      description: `Are you sure you want to cancel installing Payesh on ${targetName}? Incomplete deployment tasks on the remote host will be stopped.`,
      tone: 'danger',
      icon: 'alert-triangle',
      confirmText: 'Cancel installation',
      cancelText: 'Keep installing',
      action: async () => {
        await cancelLatestJob();
      }
    });
  }

  function promptSignOut() {
    openConfirmModal({
      title: 'Sign out?',
      description: 'Are you sure you want to sign out of Payesh? You will need your administrator username and password to log in again.',
      tone: 'warning',
      icon: 'log-out',
      confirmText: 'Sign out',
      cancelText: 'Stay signed in',
      action: async () => {
        await signOut();
      }
    });
  }

  $: selectedServer = servers.find((server) => server.id === selectedServerId) ?? servers[0];
  $: displayServers = servers;
  $: healthyCount = displayServers.filter((server) => server.displayState === 'healthy').length;
  $: attentionCount = displayServers.filter((server) => server.displayState !== 'healthy').length;
  $: firingAlertCount = alerts.filter((alert) => alert.state === 'firing').length;
  $: overviewTrafficBytes = PREVIEW_MODE ? totalTrafficBytes : displayServers.reduce((total, server) => { try { return (BigInt(total) + BigInt(server.traffic.countedBytes)).toString(); } catch { return total; } }, '0');
  $: overviewAllowanceBytes = PREVIEW_MODE ? totalAllowanceBytes : displayServers.reduce((total, server) => { try { return (BigInt(total) + BigInt(server.traffic.allowanceBytes)).toString(); } catch { return total; } }, '0');
  $: chartData = selectedServer?.metricHistory?.ranges[chartRange] ?? null;
  $: availableTabs = selectedServer ? (['metrics', 'traffic', 'logs', 'processes'] as DetailTab[]).filter((tab) => hasCapability(selectedServer, tab)) : [];
  $: if (selectedServer && availableTabs.length > 0 && !availableTabs.includes(detailTab)) detailTab = availableTabs[0];
  $: if (selectedServer && !labelDraft) labelDraft = selectedServer.name;

  function initialTheme(): Theme {
    if (typeof window === 'undefined') return 'dark';
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

  function pageLocation(page: Page, serverId = selectedServerId): string {
    if (page === 'overview') return '/';
    if (page === 'server') return serverId ? `/servers/${encodeURIComponent(serverId)}` : '/servers';
    if (page === 'add-server') return '/servers/new';
    if (page === 'install-progress') return serverId ? `/servers/${encodeURIComponent(serverId)}/install` : '/servers/new/install';
    return `/${page}`;
  }

  function stateFromLocation(): { activePage: Page; selectedServerId?: string } {
    if (typeof window === 'undefined') return { activePage: 'overview' };
    const parts = window.location.pathname.split('/').filter(Boolean);
    if (parts[0] === 'servers' && parts[1] === 'new' && parts[2] === 'install') return { activePage: 'install-progress' };
    if (parts[0] === 'servers' && parts[1] && parts[2] === 'install') return { activePage: 'install-progress', selectedServerId: decodeURIComponent(parts[1]) };
    if (parts[0] === 'servers' && parts[1] === 'new' && parts.length === 2) return { activePage: 'add-server' };
    if (parts[0] === 'servers' && parts[1] && parts.length === 2) return { activePage: 'server', selectedServerId: decodeURIComponent(parts[1]) };
    if (parts[0] === 'servers' && parts.length === 1) return { activePage: 'servers' };
    if (parts.length === 1 && ['monitoring', 'alerts', 'packages', 'settings', 'onboarding'].includes(parts[0])) return { activePage: parts[0] as Page };
    return { activePage: 'overview' };
  }

  function openAddServer() {
    newServerName = '';
    installHost = '';
    installPort = '22';
    installUser = 'root';
    installPassword = '';
    installKey = '';
    installFingerprint = '';
    jobError = '';
    if (!activeInstall || !['connecting', 'connected', 'preflight', 'installing', 'enrolling', 'verifying'].includes(activeInstall.currentStage)) {
      saveActiveInstall(null);
    }
    navigate('add-server');
  }

  function navigate(page: Page, serverId = selectedServerId) {
    activePage = page;
    selectedServerId = serverId;
    saveUiState();
    if (typeof window !== 'undefined') {
      window.history.pushState({ activePage, selectedServerId, detailTab }, '', pageLocation(activePage, selectedServerId));
    }
    if (page === 'server' && !PREVIEW_MODE) void refreshSelectedServer();
    if (page === 'packages' && !PREVIEW_MODE) void loadModules();
    if (page === 'alerts' && !PREVIEW_MODE) void loadAlerts();
    if (page === 'settings' && !PREVIEW_MODE) { void loadHTTPS(); void loadAccounts(); void checkLatestUpdate(); }
  }

  async function checkLatestUpdate(): Promise<void> {
    if (updateCheckBusy || PREVIEW_MODE) return;
    updateCheckBusy = true; updateCheckError = '';
    try { updateStatus = await apiClient.checkLatestUpdate(); }
    catch (error) { updateCheckError = error instanceof Error ? error.message : 'Could not check GitHub Releases.'; }
    finally { updateCheckBusy = false; }
  }

  async function loadAccounts(): Promise<void> {
    try {
      myAccount = await apiClient.getMyAccount();
      accounts = myAccount.role === 'owner' ? (await apiClient.listAccounts()).items : [];
      accountError = '';
    } catch (error) { accountError = error instanceof Error ? error.message : 'Could not load accounts.'; }
  }

  async function saveAccount(): Promise<void> {
    if (!accountUsername.trim() || !accountPassword || accountBusy) return;
    accountBusy = true; accountError = '';
    try {
      await apiClient.createAccount({ username: accountUsername.trim(), password: accountPassword, role: accountRole, permission: accountPermission });
      accountUsername = ''; accountPassword = ''; addingUser = false;
      await loadAccounts(); showNotice('User added.');
    } catch (error) { accountError = error instanceof Error ? error.message : 'Could not add account.'; }
    finally { accountBusy = false; }
  }

  function startEditingAccount(account: Account): void {
    editingAccount = account.username; editUsername = account.username; editPassword = '';
    editRole = account.role; editPermission = account.permission; accountError = '';
  }

  async function editAccount(account: Account): Promise<void> {
    if (accountBusy) return;
    accountBusy = true;
    accountError = '';
    try {
      await apiClient.updateAccount(account.username, { username: editUsername.trim(), ...(editPassword ? { password: editPassword } : {}), ...(account.role === 'owner' ? {} : { role: editRole, permission: editPermission }) });
      const changedOwnPassword = account.role === 'owner' && editPassword !== '';
      editingAccount = ''; editPassword = '';
      if (changedOwnPassword) { sessionState = 'signed-out'; authExpired = true; navigate('overview'); showNotice('Password changed. Sign in again.'); }
      else { await loadAccounts(); showNotice('User updated.'); }
    } catch (error) { accountError = error instanceof Error ? error.message : 'Could not update account.'; }
    finally { accountBusy = false; }
  }

  function removeAccount(account: Account): void {
    openConfirmModal({ title: `Remove ${account.username}?`, description: 'Their active sessions will be revoked.', tone: 'warning', icon: 'alert-triangle', confirmText: 'Remove user', cancelText: 'Cancel', action: async () => {
      accountError = '';
      try { await apiClient.deleteAccount(account.username); await loadAccounts(); showNotice('User removed.'); }
      catch (error) { accountError = error instanceof Error ? error.message : 'Could not remove user.'; }
    } });
  }

  async function loadHTTPS(): Promise<void> {
    if (httpsPoll) { clearTimeout(httpsPoll); httpsPoll = null; }
    try {
      httpsStatus = await apiClient.getHTTPSSettings();
      if (!dashboardPort) dashboardPort = httpsStatus.https_port ?? '8787';
      if (!httpsDomain && httpsStatus.domain) httpsDomain = httpsStatus.domain;
      if (httpsStatus.state === 'pending' && activePage === 'settings') httpsPoll = setTimeout(() => void loadHTTPS(), 3000);
      else httpsBusy = false;
      // HTTPS just came up while this page is on plain HTTP: move to it. The
      // session cookie belongs to this address, so the user signs in again.
      if (httpsStatus.state === 'active' && insecureConnection && httpsStatus.domain) {
        notice = `HTTPS is ready. Opening ${httpsURL(httpsStatus)} …`;
        setTimeout(() => { window.location.href = httpsURL(httpsStatus!) + window.location.pathname; }, 1500);
      }
    } catch (error) {
      httpsBusy = false;
      httpsError = error instanceof Error ? error.message : 'Could not read HTTPS settings.';
    }
  }

  async function saveHTTPS(): Promise<void> {
    httpsError = '';
    httpsBusy = true;
    try {
      httpsStatus = await apiClient.setHTTPSDomain({ domain: httpsDomain.trim(), email: httpsEmail.trim() || undefined, cloudflare_api_token: httpsToken.trim() || undefined });
      httpsToken = '';
      httpsPoll = setTimeout(() => void loadHTTPS(), 3000);
    } catch (error) {
      httpsBusy = false;
      httpsError = error instanceof Error ? error.message : 'Could not start the certificate request.';
    }
  }

  async function saveDashboardPort(): Promise<void> {
    portBusy = true; portError = '';
    try {
      httpsStatus = await apiClient.setDashboardPort(Number(dashboardPort));
      const target = new URL(window.location.href);
      target.port = dashboardPort;
      if (httpsStatus.state === 'active' && httpsStatus.domain) { target.hostname = httpsStatus.domain; target.protocol = 'https:'; }
      window.location.assign(target.toString());
    } catch (error) { portError = error instanceof Error ? error.message : 'Could not change port.'; }
    finally { portBusy = false; }
  }

  function httpsURL(status: HTTPSStatus): string {
    const port = status.https_port && status.https_port !== '443' ? `:${status.https_port}` : '';
    return `https://${status.domain}${port}`;
  }

  function promptRemoveHTTPS() {
    openConfirmModal({
      title: 'Remove domain?',
      description: 'The certificate is deleted and the dashboard goes back to plain HTTP. Logins will no longer be encrypted.',
      tone: 'warning',
      icon: 'alert-triangle',
      confirmText: 'Remove domain',
      cancelText: 'Keep HTTPS',
      action: async () => {
        await apiClient.removeHTTPSDomain();
        httpsDomain = '';
        await loadHTTPS();
      }
    });
  }

  async function loadModules(force = false): Promise<void> {
    modulesError = '';
    const cacheKey = 'payesh-package-catalog-v1';
    const cacheAge = 24 * 60 * 60 * 1000;
    let hasCachedCatalog = false;
    if (!force && typeof window !== 'undefined') {
      try {
        const cached = JSON.parse(window.localStorage.getItem(cacheKey) ?? 'null') as { savedAt?: number; items?: Module[] } | null;
        if (cached?.savedAt && Array.isArray(cached.items)) {
          modules = cached.items; modulesState = modules.length ? 'ready' : 'empty';
          hasCachedCatalog = true;
          if (!packageServerId && servers.length) packageServerId = servers[0].id;
          if (packageServerId) void loadServerModules();
          if (Date.now() - cached.savedAt < cacheAge) return;
        }
      } catch { /* corrupt cache is ignored and replaced by a fresh response */ }
    }
    if (!hasCachedCatalog) modulesState = 'loading';
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 10000);
    try {
      const page = await apiClient.listModules({ signal: controller.signal });
      modules = page.items;
      modulesState = modules.length ? 'ready' : 'empty';
      if (typeof window !== 'undefined') window.localStorage.setItem(cacheKey, JSON.stringify({ savedAt: Date.now(), items: modules }));
      if (!packageServerId && servers.length) packageServerId = servers[0].id;
      if (packageServerId) void loadServerModules();
    } catch (error) {
      modulesError = error instanceof DOMException && error.name === 'AbortError' ? 'The package catalog request timed out.' : error instanceof Error ? error.message : 'Unable to load modules.';
      if (!hasCachedCatalog) modulesState = 'error';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      window.clearTimeout(timeout);
    }
  }

  async function loadServerModules(): Promise<void> {
    if (!packageServerId) { moduleInstallations = []; return; }
    packageError = '';
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 10000);
    try {
      moduleInstallations = (await apiClient.listServerModules(packageServerId, { signal: controller.signal })).items;
    } catch (error) {
      packageError = error instanceof DOMException && error.name === 'AbortError' ? 'Package status took too long to load. The cached catalog is still available.' : error instanceof Error ? error.message : 'Unable to load package state.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      window.clearTimeout(timeout);
    }
  }

  function moduleState(moduleId: string): ModuleInstallation | undefined {
    return moduleInstallations.find((item) => item.module_id === moduleId);
  }

  let packageSource: PackageSource['kind'] = 'github';
  let packageLocation = '';
  let packageVersion = 'latest';
  let packageManifest = '';
  let packageSignature = '';

  async function installPackage(module: Module): Promise<void> {
    if (!packageServerId || packageBusy) return;
    packageBusy = `${module.id}:install`; packageError = '';
    try {
      const source: PackageSource = { kind: packageSource, location: packageLocation.trim() || (packageSource === 'github' ? 'Real-kia/payesh' : '') };
      if (packageSource === 'github') source.version = packageVersion.trim() || 'latest';
      if (packageSource !== 'github' && packageManifest.trim()) source.manifest_location = packageManifest.trim();
      if (packageSource !== 'github' && packageSignature.trim()) source.signature_location = packageSignature.trim();
      const result = await apiClient.installModuleSource(packageServerId, module.id, source, moduleState(module.id)?.revision ?? '0', operationKey('package-install'));
      moduleInstallations = [...moduleInstallations.filter((item) => item.module_id !== module.id), result];
      showNotice(`${module.name} installed.`);
    } catch (error) {
      const message = error instanceof Error ? error.message : 'Package installation failed.';
      await loadServerModules();
      packageError = message;
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally { packageBusy = ''; }
  }

  function packageGitHubURL(module: Module): string {
    const arch = servers.find((server) => server.id === packageServerId)?.architecture;
    const repo = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(packageLocation.trim()) ? packageLocation.trim() : 'Real-kia/payesh';
    const release = packageVersion.trim() || 'latest';
    const base = release === 'latest' ? `https://github.com/${repo}/releases/latest/download` : `https://github.com/${repo}/releases/download/v${release.replace(/^v/, '')}`;
    return arch === 'amd64' || arch === 'arm64' ? `${base}/${module.id}-linux-${arch}.tar.gz` : `https://github.com/${repo}/releases`;
  }

  async function packageAction(module: Module, action: 'enable' | 'disable' | 'remove'): Promise<void> {
    if (!packageServerId || packageBusy) return;
    packageBusy = `${module.id}:${action}`; packageError = '';
    try {
      const current = moduleState(module.id);
      const result = await apiClient.moduleAction(packageServerId, module.id, action, current?.revision ?? '0', operationKey(`package-${action}`));
      moduleInstallations = [...moduleInstallations.filter((item) => item.module_id !== module.id), result];
      showNotice(`${module.name} is now ${result.state}.`);
    } catch (error) {
      packageError = error instanceof Error ? error.message : 'Package operation failed.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      packageBusy = '';
    }
  }

  async function loadAlerts(): Promise<void> {
    alertsState = 'loading'; alertsError = '';
    try {
      alerts = (await apiClient.listAlerts()).items;
      alertsState = alerts.length ? 'ready' : 'empty';
    } catch (error) {
      alertsError = error instanceof Error ? error.message : 'Unable to load alerts.';
      alertsState = 'error';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    }
  }

  function updateInstallProgress(job: Job) {
    if (!activeInstall || activeInstall.jobId !== job.id) return;
    const nowStr = new Date().toTimeString().slice(0, 8);
    const p = job.progress;

    if (job.state === 'failed') {
      activeInstall.currentStage = 'failed';
      activeInstall.simulatedProgress = 100;
      const errMsg = job.error?.message || 'Error occurred during SSH installation.';
      if (!activeInstall.logs.some((l) => l.text.includes(errMsg))) {
        activeInstall.logs = [...activeInstall.logs, { time: nowStr, text: `Installation failed: ${errMsg}`, level: 'error' }];
      }
      saveActiveInstall(activeInstall);
      return;
    }
    if (job.state === 'cancelled') {
      activeInstall.currentStage = 'cancelled';
      if (!activeInstall.logs.some((l) => l.text.includes('Installation cancelled'))) {
        activeInstall.logs = [...activeInstall.logs, { time: nowStr, text: 'Installation cancelled by user.', level: 'warn' }];
      }
      saveActiveInstall(activeInstall);
      return;
    }
    if (job.state === 'succeeded') {
      const id = activeInstall.serverId;
      saveActiveInstall(null);
      detailTab = 'metrics';
      navigate('server', id);
      showNotice('Server installed.');
      return;
    }

    const elapsed = (Date.now() - activeInstall.startedAt) / 1000;
    let targetStage: InstallStage = 'connecting';

    if (p >= 90) {
      targetStage = 'verifying';
    } else if (p >= 80) {
      targetStage = 'enrolling';
    } else if (p >= 65) {
      targetStage = 'installing';
    } else if (p >= 45) {
      targetStage = 'preflight';
    } else if (p >= 25) {
      targetStage = 'connected';
    } else {
      targetStage = 'connecting';
    }

    activeInstall.simulatedProgress = Math.max(activeInstall.simulatedProgress, Math.max(p, 12));

    if (p >= 20 && !activeInstall.logs.some((l) => l.text.includes('Checking installation files'))) {
      activeInstall.logs = [...activeInstall.logs, { time: nowStr, text: 'Checking installation files.', level: 'info' }];
    }
    if (p === 35 && !activeInstall.logs.some((l) => l.text.includes('Downloading from hub'))) {
      activeInstall.logs = [...activeInstall.logs, { time: nowStr, text: 'Downloading from hub.', level: 'info' }];
    }

    if (targetStage !== activeInstall.currentStage) {
      activeInstall.currentStage = targetStage;
      if (targetStage === 'connected' && !activeInstall.logs.some((l) => l.text.includes('Connected via SSH'))) {
        activeInstall.logs = [...activeInstall.logs, { time: nowStr, text: 'Connected via SSH. Host key verified & trusted.', level: 'success' }];
      } else if (targetStage === 'preflight' && !activeInstall.logs.some((l) => l.text.includes('Preflight inspection'))) {
        activeInstall.logs = [...activeInstall.logs, { time: nowStr, text: 'Running preflight inspection: Linux OS detected.', level: 'info' }];
      } else if (targetStage === 'installing' && !activeInstall.logs.some((l) => l.text.includes('Executing payesh-install'))) {
        activeInstall.logs = [...activeInstall.logs, { time: nowStr, text: 'Executing payesh-install and configuring systemd service in background...', level: 'info' }];
      } else if (targetStage === 'enrolling' && !activeInstall.logs.some((l) => l.text.includes('Enrolling'))) {
        activeInstall.logs = [...activeInstall.logs, { time: nowStr, text: 'Service started. Enrolling node TLS certificate with hub...', level: 'info' }];
      } else if (targetStage === 'verifying' && !activeInstall.logs.some((l) => l.text.includes('Awaiting'))) {
        activeInstall.logs = [...activeInstall.logs, { time: nowStr, text: 'Awaiting initial telemetry heartbeat...', level: 'info' }];
      }
    }
    saveActiveInstall(activeInstall);
  }

  async function createPendingServer(): Promise<void> {
    if (!newServerName.trim() || !installHost.trim() || !installUser.trim() || (!installPassword && !installKey) || createServerBusy) return;
    createServerBusy = true; jobError = '';
    const host = installHost.trim();
    const port = Number(installPort);
    const user = installUser.trim();
    const serverName = newServerName.trim();
    try {
      const created = await apiClient.createServer({ name: serverName, address: host });
      const server = emptyApiServer(created);
      server.displayState = 'installing';
      servers = [...servers, server];
      selectedServerId = server.id;
      labelDraft = server.name;
      const job = await apiClient.enqueueInstall({
        server_id: server.id,
        host,
        port,
        user,
        ...(installPassword ? { password: installPassword } : {}),
        ...(installKey ? { private_key: installKey } : {}),
        expected_host_key_fingerprint: installFingerprint.trim() || undefined,
        role: 'node',
        start: true,
        idempotency_key: operationKey('install')
      });
      installPassword = ''; installKey = '';

      const timeStr = new Date().toTimeString().slice(0, 8);
      saveActiveInstall({
        serverId: server.id,
        serverName,
        host,
        port,
        user,
        jobId: job.id,
        startedAt: Date.now(),
        currentStage: 'connecting',
        simulatedProgress: 12,
        logs: [
          { time: timeStr, text: `Created server '${serverName}' · Job ${job.id}`, level: 'info' },
          { time: timeStr, text: `Connecting over SSH to ${user}@${host}:${port}...`, level: 'info' }
        ]
      });

      recordJob(job);
      showNotice(`Connecting to ${serverName} over SSH to install Payesh...`);
      navigate('install-progress', server.id);
    } catch (error) {
      jobError = error instanceof Error ? error.message : 'Unable to create server.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      createServerBusy = false;
    }
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
        updateInstallProgress(current);
        if (['succeeded', 'failed', 'cancelled', 'recovery-required'].includes(current.state)) {
          if (current.state === 'succeeded') {
            await loadApiData();
            if (activeInstall?.serverId) {
              const updated = servers.find((s) => s.id === activeInstall?.serverId);
              if (updated) selectedServerId = updated.id;
            }
          }
          return;
        }
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
    } finally {
      authBusy = false;
    }
  }

  async function signOut(): Promise<void> {
    authError = '';
    try {
      await apiClient.logout();
    } catch (error) {
      if (!(error instanceof ApiError && error.authExpired)) authError = error instanceof Error ? error.message : 'Unable to sign out.';
    }
    sessionState = 'signed-out';
    authExpired = true;
    servers = [];
    previewState = 'error';
    navigate('overview');
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
    } finally {
      updateBusy = false;
    }
  }

  async function submitInstall(): Promise<void> {
    if (!selectedServer || !installHost.trim() || !installUser.trim() || installBusy) return;
    installBusy = true; jobError = '';
    const host = installHost.trim();
    const port = Number(installPort);
    const user = installUser.trim();
    const serverName = selectedServer.name;
    const serverId = selectedServer.id;
    try {
      const job = await apiClient.enqueueInstall({
        server_id: serverId,
        host,
        port,
        user,
        ...(installPassword ? { password: installPassword } : {}),
        ...(installKey ? { private_key: installKey } : {}),
        expected_host_key_fingerprint: installFingerprint.trim() || undefined,
        role: 'node',
        start: true,
        idempotency_key: operationKey('install')
      });
      installPassword = ''; installKey = '';

      const timeStr = new Date().toTimeString().slice(0, 8);
      saveActiveInstall({
        serverId,
        serverName,
        host,
        port,
        user,
        jobId: job.id,
        startedAt: Date.now(),
        currentStage: 'connecting',
        simulatedProgress: 12,
        logs: [
          { time: timeStr, text: `Queued SSH installation for '${serverName}' · Job ${job.id}`, level: 'info' },
          { time: timeStr, text: `Connecting over SSH to ${user}@${host}:${port}...`, level: 'info' }
        ]
      });

      recordJob(job);
      showNotice(`Connecting to ${serverName} over SSH to install Payesh...`);
    } catch (error) {
      jobError = error instanceof Error ? error.message : 'Unable to queue the installation.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      installBusy = false;
    }
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
    } finally {
      labelBusy = false;
    }
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
    } finally {
      labelBusy = false;
    }
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
      if (!servers.length) {
        setupError = 'Pairing is unavailable until a pending server identity exists; choose skip for now.';
        return;
      }
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
    if (capability === 'processes') return server.role === 'standalone' || server.role === 'hub';
    if (capability === 'metrics' || capability === 'traffic') return true;
    return server.capabilities?.includes(capability) ?? false;
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
    return `${Math.min(...numeric).toFixed(2)}–${Math.max(...numeric).toFixed(2)}%`;
  }

  function capabilityMessage(server: PreviewServer, capability: DetailTab): string {
    if (server.displayState === 'unsupported') return `${tabLabel(capability)} are unavailable: this server does not advertise the required capability.`;
    return `${tabLabel(capability)} are unavailable for this server.`;
  }

  function displayState(server: ApiServer): DisplayState {
    if (activeInstall && activeInstall.serverId === server.id) {
      if (activeInstall.currentStage === 'failed') return 'failed';
      if (activeInstall.currentStage !== 'succeeded') return 'installing';
    }
    if (server.connection_state === 'revoked') return 'disabled';
    if (server.connection_state === 'never-connected') return 'pending';
    if (server.connection_state === 'disconnected') return 'unreachable';
    if (server.freshness_state === 'stale') return 'stale';
    return server.freshness_state === 'fresh' ? 'healthy' : 'failed';
  }

  function emptyApiServer(server: ApiServer): PreviewServer {
    const caps = (server.capabilities && server.capabilities.length > 0) ? server.capabilities : ['metrics', 'traffic'];
    return {
      id: server.id, name: server.name, address: server.address, role: server.role, architecture: server.architecture,
      platform: server.platform, capabilities: [...caps], version: server.version ?? '—',
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
    if (!query.samples.length && query.rollups.length) {
      const timestamps = [...new Set(query.rollups.map((rollup) => Date.parse(String(rollup.bucket_start)) / 1000))].filter(Number.isFinite).sort((a, b) => a - b);
      const bucket = (timestamp: number, name: string) => query.rollups.find((rollup) => Date.parse(String(rollup.bucket_start)) / 1000 === timestamp && rollup.metric === name);
      const gauge = (timestamp: number, names: string[]) => {
        const point = names.map((name) => bucket(timestamp, name)).find((entry) => typeof entry?.weighted_mean === 'number');
        return typeof point?.weighted_mean === 'number' && Number.isFinite(point.weighted_mean) ? point.weighted_mean : null;
      };
      return { timestamps, cpu: timestamps.map((t) => gauge(t, ['cpu.utilization', 'cpu'])), memory: timestamps.map((t) => gauge(t, ['memory.used_percent', 'memory.utilization', 'memory'])), disk: timestamps.map((t) => gauge(t, ['disk.root.used_percent', 'disk.used_percent', 'disk'])), networkRx: timestamps.map((t) => networkRollupRate(bucket(t, 'net.billing.rx_bytes'))), networkTx: timestamps.map((t) => networkRollupRate(bucket(t, 'net.billing.tx_bytes'))), coverage: query.truncated || query.rollups.some((rollup) => rollup.coverage !== 'complete') ? 'gap' : 'complete' };
    }
    const samples = [...query.samples].sort((a, b) => Date.parse(a.observed_at) - Date.parse(b.observed_at));
    const value = (sample: MetricQuery['samples'][number], name: string) => {
      const candidates = name === 'cpu' ? ['cpu', 'cpu.utilization'] : name === 'memory' ? ['memory', 'memory.used_percent', 'memory.utilization'] : ['disk', 'disk.used_percent', 'disk.utilization', 'disk.root.used_percent'];
      const found = candidates.map((candidate) => sample.values[candidate] ?? sample.values[candidate.toUpperCase()]).find((entry) => typeof entry === 'number');
      return typeof found === 'number' && Number.isFinite(found) ? found : null;
    };
    return {
      timestamps: samples.map((sample) => Date.parse(sample.observed_at) / 1000),
      cpu: samples.map((sample) => value(sample, 'cpu')),
      memory: samples.map((sample) => value(sample, 'memory')),
      disk: samples.map((sample) => value(sample, 'disk')),
      networkRx: networkRates(samples, 'net.billing.rx_bytes'),
      networkTx: networkRates(samples, 'net.billing.tx_bytes'),
      coverage: samples.length === 0 ? 'unavailable' : (query.gaps?.length || Object.values(query.coverage).some((coverage) => coverage < 1) ? 'gap' : 'complete')
    };
  }

  function applyMetric(query: MetricQuery, server: PreviewServer): void {
    const latest = [...query.samples].sort((a, b) => Date.parse(b.observed_at) - Date.parse(a.observed_at))[0];
    if (!latest) return;
    server.latestMetricAt = latest.observed_at;
    const read = (names: string[]) => names.map((name) => latest.values[name]).find((value) => typeof value === 'number' && Number.isFinite(value)) ?? null;
    server.metrics = { cpu: read(['cpu', 'cpu.utilization']), memory: read(['memory', 'memory.used_percent', 'memory.utilization']), disk: read(['disk', 'disk.used_percent', 'disk.utilization', 'disk.root.used_percent']) };
  }

  function currentNetworkRate(server: PreviewServer, direction: 'download' | 'upload'): number | null {
    if (server.freshnessState !== 'fresh') return null;
    const history = server.metricHistory?.ranges['15m'];
    if (!PREVIEW_MODE && (!history?.timestamps.length || Date.now() / 1000 - history.timestamps.at(-1)! > 90)) return null;
    const series = direction === 'download' ? history?.networkRx : history?.networkTx;
    return series?.at(-1) ?? null;
  }

  function sampleAge(server: PreviewServer): string {
    if (!server.latestMetricAt) return 'awaiting sample';
    const seconds = Math.max(0, Math.round((Date.now() - Date.parse(server.latestMetricAt)) / 1000));
    return seconds < 5 ? 'live now' : seconds < 60 ? `${seconds}s ago` : `${Math.floor(seconds / 60)}m ago`;
  }

  function displayAddress(server: PreviewServer): string {
    if (server.address) return server.address;
    if (!PREVIEW_MODE && (server.role === 'standalone' || server.role === 'hub') && typeof window !== 'undefined') return window.location.hostname;
    return 'Address unavailable';
  }

  let liveRefreshBusy = false;

  async function refreshMonitoring(): Promise<void> {
    if (liveRefreshBusy || PREVIEW_MODE || activePage !== 'monitoring' || document.hidden) return;
    liveRefreshBusy = true;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 10000);
    try {
      const page = await apiClient.listServers({ signal: controller.signal });
      const refreshed = await mapWithConcurrency(page.items, 4, controller.signal, async (item, signal) => {
        const existing = servers.find((server) => server.id === item.id);
        const server = { ...emptyApiServer(item), traffic: existing?.traffic ?? emptyApiServer(item).traffic, metricHistory: existing?.metricHistory };
        const to = new Date();
        try {
          const query = await apiClient.queryMetrics(item.id, { from: new Date(to.getTime() - 120000).toISOString(), to: to.toISOString(), resolution: 'raw', signal });
          applyMetric(query, server);
          const chart = metricChart(query);
          const empty = { timestamps: [], cpu: [], memory: [], disk: [], coverage: 'unavailable' as const };
          server.metricHistory = { ranges: { '15m': chart, '1h': existing?.metricHistory?.ranges['1h'] ?? empty, '24h': existing?.metricHistory?.ranges['24h'] ?? empty } };
        } catch (error) {
          if (error instanceof ApiError && error.authExpired) authExpired = true;
          server.metricHistory = undefined;
        }
        return server;
      });
      if (activePage === 'monitoring' && !controller.signal.aborted) servers = refreshed;
    } catch (error) { if (error instanceof ApiError && error.authExpired) authExpired = true; }
    finally { window.clearTimeout(timeout); liveRefreshBusy = false; }
  }

  async function refreshSelectedServer(): Promise<void> {
    if (PREVIEW_MODE || activePage !== 'server' || !selectedServerId || document.hidden) return;
    const id = selectedServerId;
    try {
      const result = await enrichApiServer(emptyApiServer(await apiClient.getServer(id)), new AbortController().signal);
      if (activePage === 'server' && selectedServerId === id) servers = servers.map((server) => server.id === id ? result.server : server);
    } catch (error) {
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    }
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
    (['15m', '1h', '24h'] as ChartRange[]).forEach((range, index) => {
      const result = metricResults[index];
      if (result.status === 'fulfilled') {
        ranges[range] = metricChart(result.value);
        if (range === '15m') applyMetric(result.value, enriched);
      } else {
        if (range === '15m') {
          partial = partial || server.capabilities.includes('metrics');
        }
        if (result.reason instanceof ApiError && result.reason.authExpired) authExpired = true;
      }
    });
    if (metricResults.some((result) => result.status === 'fulfilled')) enriched.metricHistory = { ranges };
    if (trafficResult?.periods[0]) {
      const period = trafficResult.periods[0];
      enriched.traffic = { scope: period.scope, from: period.from, to: period.to, timezone: period.timezone, allowanceBytes: period.allowance_bytes, direction: period.direction, countedBytes: period.counted_bytes, continuity: period.continuity };
    } else if (trafficResult === null) {
      partial = partial || server.capabilities.includes('traffic');
    }
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
      modulesState = 'ready';
      modules = [
        { id: 'cpu-controls', name: 'CPU Controls', description: 'Real-time CFS quota throttling and noisy neighbor containment via Linux cgroups v2.', latest_version: '0.1.0', resource_estimate_source: 'static' },
        { id: 'bandwidth-controls', name: 'Bandwidth Controls', description: 'Ingress and egress traffic shaping with tc / fq_codel queueing disciplines.', latest_version: '0.1.0', resource_estimate_source: 'static' },
        { id: 'port-traffic', name: 'Port Traffic Accounting', description: 'Per-port iptables and nftables telemetry collection with byte counters.', latest_version: '0.1.0', resource_estimate_source: 'static' }
      ];
      moduleInstallations = [
        { server_id: selectedServerId, module_id: 'cpu-controls', version: '0.1.0', state: 'enabled', revision: '1', updated_at: '2026-09-09T14:40:00Z' },
        { server_id: selectedServerId, module_id: 'bandwidth-controls', state: 'available', revision: '0', updated_at: '2026-09-09T14:40:00Z' }
      ];
      alertsState = 'ready';
      alerts = [
        { id: 'alert-1', rule_id: 'high-cpu-utilization', server_id: 'server-us-00000002', state: 'pending', last_value: 74.0, last_observation: '2026-09-09T14:34:18Z' },
        { id: 'alert-2', rule_id: 'host-heartbeat-stale', server_id: 'server-us-00000002', state: 'firing', last_observation: '2026-09-09T14:34:18Z' }
      ];
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
      void loadAccounts();
      selectedServerId = servers[0]?.id ?? '';
      restoreState(savedUiState);
      previewState = servers.length ? 'ready' : 'empty';
      if (activePage === 'packages') void loadModules();
      if (activePage === 'alerts') void loadAlerts();
      const enriched = await mapWithConcurrency(servers, 4, controller.signal, (server, signal) => enrichApiServer(server, signal));
      if (!controller.signal.aborted) {
        servers = enriched.map((result) => result.server);
        if (enriched.some((result) => result.partial)) partialWarning = 'Some server data could not be loaded; unavailable values are shown explicitly.';
      }
    } catch (error) {
      if (controller.signal.aborted) return;
      apiError = error instanceof Error ? error.message : 'Unable to connect to Payesh API.';
      previewState = 'error';
      if (error instanceof ApiError && error.authExpired) {
        authExpired = true;
        sessionState = 'signed-out';
      }
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
    if (state.activePage && ['overview', 'monitoring', 'servers', 'server', 'alerts', 'packages', 'settings', 'add-server', 'onboarding'].includes(state.activePage)) activePage = state.activePage;
    if (state.activePage === 'server') activePage = servers.some((server) => server.id === state.selectedServerId) ? 'server' : 'overview';
    if (state.selectedServerId && servers.some((server) => server.id === state.selectedServerId)) selectedServerId = state.selectedServerId;
    if (state.detailTab === 'metrics' || state.detailTab === 'traffic' || state.detailTab === 'logs' || state.detailTab === 'processes') detailTab = state.detailTab;
  }

  onMount(() => {
    let completedInstallServerId = '';
    try {
      savedUiState = JSON.parse(window.localStorage.getItem('payesh-ui-state') ?? 'null');
    } catch {
      // Corrupt local UI state is non-critical
    }
    try {
      const savedInstall = window.localStorage.getItem(ACTIVE_INSTALL_STORAGE_KEY);
      if (savedInstall) {
        const parsed = JSON.parse(savedInstall) as InstallProgressData;
        if (parsed?.currentStage === 'succeeded') {
          completedInstallServerId = parsed.serverId;
          saveActiveInstall(null);
        }
        if (parsed && parsed.jobId && parsed.currentStage !== 'succeeded') {
          activeInstall = parsed;
          if (['connecting', 'connected', 'preflight', 'installing', 'enrolling', 'verifying'].includes(parsed.currentStage)) {
            void pollJob(parsed.jobId);
          }
        }
      }
    } catch {
      // Corrupt install cache is non-critical
    }
    applyTheme(false);
    savedUiState = { ...(savedUiState ?? {}), ...stateFromLocation() };
    if (savedUiState.activePage) activePage = savedUiState.activePage;
    if (savedUiState.selectedServerId) selectedServerId = savedUiState.selectedServerId;
    if (completedInstallServerId && activePage === 'install-progress') {
      activePage = 'server'; selectedServerId = completedInstallServerId; detailTab = 'metrics'; saveUiState();
    }
    if (PREVIEW_MODE) void loadPreviewData(); else void loadApiData();
    window.history.replaceState({ activePage, selectedServerId, detailTab }, '', pageLocation(activePage, selectedServerId));
    const onPopState = (event: PopStateEvent) => {
      restoreState({ ...(event.state ?? {}), ...stateFromLocation() });
      if (activePage === 'packages' && !PREVIEW_MODE) void loadModules();
      if (activePage === 'alerts' && !PREVIEW_MODE) void loadAlerts();
      saveUiState();
    };
    window.addEventListener('popstate', onPopState);
    const liveRefresh = window.setInterval(() => { void refreshSelectedServer(); void refreshMonitoring(); }, 5000);
    return () => {
      window.clearInterval(liveRefresh);
      window.removeEventListener('popstate', onPopState);
      apiAbortController?.abort();
      logAbortController?.abort();
      jobPollController?.abort();
    };
  });
</script>

<svelte:head>
  <title>Payesh — Cloud Fleet Operations</title>
  <meta name="description" content="Payesh lightweight local-first Linux fleet monitoring and management" />
</svelte:head>

<div class="app-shell" data-theme={theme}>
  <aside class="sidebar" aria-label="Primary navigation">
    <div class="brand-lockup">
      <div class="brand-mark" aria-hidden="true">
        <span>P</span>
      </div>
      <div class="brand-text">
        <strong>Payesh</strong>
        <small>Fleet Monitoring</small>
      </div>
    </div>

    <nav class="nav-list">
      <button class:active={activePage === 'overview'} class="nav-item" type="button" on:click={() => navigate('overview')} aria-current={activePage === 'overview' ? 'page' : undefined}>
        <Icon name="overview" size={17} />
        <span>Overview</span>
      </button>
      <button class:active={activePage === 'monitoring'} class="nav-item" type="button" on:click={() => navigate('monitoring')} aria-current={activePage === 'monitoring' ? 'page' : undefined}>
        <Icon name="activity" size={17} />
        <span>Server monitoring</span>
      </button>
      <button class:active={activePage === 'servers' || activePage === 'server' || activePage === 'add-server' || activePage === 'install-progress'} class="nav-item" type="button" on:click={() => navigate('servers')}>
        <Icon name="servers" size={17} />
        <span>Servers</span>
        <span class="nav-count">{servers.length}</span>
      </button>
      <button class:active={activePage === 'alerts'} class="nav-item" type="button" on:click={() => navigate('alerts')}>
        <Icon name="alerts" size={17} />
        <span>Alerts</span>
        {#if firingAlertCount > 0}
          <span class="nav-count firing">{firingAlertCount}</span>
        {:else}
          <span class="nav-count neutral">0</span>
        {/if}
      </button>
      <button class:active={activePage === 'packages'} class="nav-item" type="button" on:click={() => navigate('packages')}>
        <Icon name="packages" size={17} />
        <span>Packages</span>
      </button>
      <button class:active={activePage === 'settings'} class="nav-item" type="button" on:click={() => navigate('settings')} aria-current={activePage === 'settings' ? 'page' : undefined}>
        <Icon name="settings" size={17} />
        <span>Settings</span>
      </button>
    </nav>

    <div class="sidebar-footer">
      <div class="connection-status">
        <span class="connection-dot"></span>
        <span class="connection-label">Local hub</span>
      </div>
      <span class="version-tag">{PREVIEW_MODE ? 'v0.1 preview' : 'API connected'}</span>
    </div>
  </aside>

  <main class="main-content">
    <header class="topbar">
      <div class="breadcrumbs">
        <span class="crumb-root">Workspace</span>
        <span class="crumb-separator" aria-hidden="true">/</span>
        <strong class="crumb-current">
          {activePage === 'server' ? (selectedServer?.name || 'Server Details') : activePage === 'add-server' ? 'Add Server' : activePage === 'install-progress' ? 'Installation Progress' : activePage.charAt(0).toUpperCase() + activePage.slice(1)}
        </strong>
      </div>

      <div class="topbar-actions">
        {#if myAccount?.permission === 'read'}<span class="status-pill pending">Read only</span>{/if}
        {#if PREVIEW_MODE}
          <label class="preview-control">
            <span>Data</span>
            <select bind:value={previewState} on:change={() => { if (previewLoadFailed) previewState = 'error'; }} aria-label="Preview data state">
              <option value="ready">Ready</option>
              <option value="loading">Loading</option>
              <option value="empty">Empty</option>
              <option value="error">API error</option>
            </select>
          </label>
        {/if}
        <button class="icon-button" type="button" on:click={toggleTheme} aria-label={`Switch to ${theme === 'light' ? 'dark' : 'light'} theme`}>
          <Icon name={theme === 'light' ? 'moon' : 'sun'} size={16} />
        </button>
        {#if !PREVIEW_MODE && sessionState === 'authenticated'}
          <button class="button ghost small" type="button" on:click={() => promptSignOut()}>Sign out</button>
        {/if}
        {#if myAccount?.permission !== 'read'}<button class="button primary small" type="button" on:click={() => openAddServer()}>
          <Icon name="plus" size={14} />
          <span>Add server</span>
        </button>{/if}
      </div>
    </header>

    {#if notice}<div class="notice" role="status"><Icon name="check" size={14} /><span>{notice}</span></div>{/if}
    {#if partialWarning}<div class="partial-warning" role="status"><Icon name="alert-triangle" size={15} /><span>{partialWarning}</span></div>{/if}
    {#if insecureConnection && !PREVIEW_MODE && sessionState === 'authenticated'}
      <div class="partial-warning" role="status"><Icon name="alert-triangle" size={15} /><span>This connection is not encrypted (no SSL). <button class="link-button" type="button" on:click={() => navigate('settings')}>Add a domain</button> to turn on HTTPS automatically.</span></div>
    {/if}

    {#if activePage === 'monitoring'}
      <section class="page" aria-labelledby="monitoring-title">
        <div class="page-heading">
          <div><h1 id="monitoring-title">Server monitoring</h1></div>
          <span class="heading-status-badge"><span class="live-ping"></span>{displayServers.length} servers</span>
        </div>
        <div class="summary-grid">
          <article class="summary-card"><div class="card-header"><span class="stat-label">Healthy</span><span class="stat-icon-wrap emerald"><Icon name="check" size={15} /></span></div><strong class="stat-value tabular">{healthyCount}<small class="stat-total"> / {displayServers.length}</small></strong></article>
          <article class="summary-card"><div class="card-header"><span class="stat-label">Needs attention</span><span class="stat-icon-wrap amber"><Icon name="alert-triangle" size={15} /></span></div><strong class="stat-value tabular">{attentionCount}</strong></article>
          <article class="summary-card"><div class="card-header"><span class="stat-label">Fleet traffic</span><span class="stat-icon-wrap cyan"><Icon name="activity" size={15} /></span></div><strong class="stat-value tabular">{formatBytes(overviewTrafficBytes)}</strong></article>
        </div>
        {#if displayServers.length === 0}<div class="state-panel"><h2>No servers to monitor</h2><p>Add a server to see its health here.</p></div>{/if}
        <div class="monitoring-grid">
          {#each displayServers as server}
            <article class="panel monitoring-card">
              <div class="monitoring-top"><div><h2>{server.name}</h2><small class="mono faint">{displayAddress(server)}</small></div><span class={`status-pill ${server.displayState}`}><i class="status-dot"></i>{stateLabel(server.displayState)}</span></div>
              <div class="monitoring-metrics">
                <div><span>CPU</span><strong>{metricValue(server.metrics.cpu)}</strong></div>
                <div><span>Memory</span><strong>{metricValue(server.metrics.memory)}</strong></div>
                <div><span>Disk</span><strong>{metricValue(server.metrics.disk)}</strong></div>
                <div><span>Download</span><strong>{formatNetworkRate(currentNetworkRate(server, 'download'))}</strong></div>
                <div><span>Upload</span><strong>{formatNetworkRate(currentNetworkRate(server, 'upload'))}</strong></div>
              </div>
              <div class="monitoring-bottom"><span>Traffic: {formatBytes(server.traffic.countedBytes)}</span><span>{server.lastHeartbeat ? `Heartbeat ${server.lastHeartbeat.slice(11, 16)} UTC` : server.freshnessReason || server.connectionState}</span></div>
              <button class="button ghost small" type="button" on:click={() => selectServer(server)}>View server details <Icon name="chevron-right" size={14} /></button>
            </article>
          {/each}
        </div>
      </section>
    {:else if activePage === 'servers'}
      <section class="page" aria-labelledby="servers-title">
        <div class="page-heading">
          <div>
            <h1 id="servers-title">Servers</h1>

          </div>
          {#if myAccount?.permission !== 'read'}<button class="button primary" type="button" on:click={() => openAddServer()}>
            <Icon name="plus" size={15} />
            <span>Add server</span>
          </button>{/if}
        </div>

        <div class="table-container">
          <div class="table-header">
            <span class="col-status">Status</span>
            <span class="col-name">Server & Address</span>
            <span class="col-meta">Platform</span>
            <span class="col-metric">CPU</span>
            <span class="col-action"></span>
          </div>
          <div class="server-list">
            {#each displayServers as server}
              <button class="server-row" type="button" on:click={() => selectServer(server)}>
                <div class="col-status">
                  <span class={`status-pill ${server.displayState}`}>
                    <i class="status-dot"></i>
                    <span>{stateLabel(server.displayState)}</span>
                  </span>
                </div>
                <div class="col-name server-identity">
                  <strong>{server.name}</strong>
                  <small>{displayAddress(server)}</small>
                </div>
                <div class="col-meta">
                  <span>{server.role} · {server.platform} ({server.architecture})</span>
                  <small class="faint">{server.freshnessReason || (server.lastHeartbeat ? `Heartbeat ${server.lastHeartbeat.slice(11, 16)} UTC` : server.connectionState)}</small>
                </div>
                <div class="col-metric server-metric">
                  <strong>{metricValue(server.metrics.cpu)}</strong>
                  <div class="metric-microbar">
                    <span style={`width: ${Math.min(100, Math.max(0, server.metrics.cpu ?? 0))}%`}></span>
                  </div>
                </div>
                <div class="col-action">
                  <span class="server-arrow"><Icon name="chevron-right" size={16} /></span>
                </div>
              </button>
            {/each}
          </div>
        </div>
      </section>

    {:else if activePage === 'add-server'}
      <section class="page" aria-labelledby="add-server-title">
        <button class="back-link" type="button" on:click={() => navigate('servers')}>
          <Icon name="arrow-left" size={15} />
          <span>Back to servers</span>
        </button>
        <div class="page-heading">
          <div>
            <h1 id="add-server-title">Add a server</h1>
            <p class="lede">Connect over SSH. Payesh detects the operating system and architecture automatically.</p>
          </div>
        </div>

        {#if activeInstall && ['connecting', 'connected', 'preflight', 'installing', 'enrolling', 'verifying'].includes(activeInstall.currentStage)}
          <div class="active-install-banner" role="status">
            <div class="banner-left">
              <span class="pulse-dot"></span>
              <span>Deployment in progress for <strong>{activeInstall.serverName}</strong> ({activeInstall.simulatedProgress}%)</span>
            </div>
            <button class="button small ghost" type="button" on:click={() => navigate('install-progress', activeInstall?.serverId)}>
              <span>View live log</span>
              <Icon name="arrow-right" size={13} />
            </button>
          </div>
        {/if}

        <article class="panel">
          <form class="form-grid" on:submit|preventDefault={() => void createPendingServer()}>
            <label>Server name<input bind:value={newServerName} maxlength="128" placeholder="e.g. EU-Node-01" required /></label>
            <label>IP address or hostname<input bind:value={installHost} maxlength="255" placeholder="e.g. 192.0.2.1" required /></label>
            <label>SSH port<input type="number" min="1" max="65535" bind:value={installPort} required /></label>
            <label>SSH user<input bind:value={installUser} placeholder="root" required /></label>
            <label>Password<input type="password" bind:value={installPassword} autocomplete="off" placeholder="SSH user password" /></label>
            <label>Private key<textarea bind:value={installKey} rows="3" autocomplete="off" placeholder="Paste OpenSSH private key"></textarea></label>
            <label>Expected host-key fingerprint <small>(recommended)</small><input bind:value={installFingerprint} placeholder="SHA256:…" /></label>
            <div class="setup-actions">
              <button class="button ghost" type="button" on:click={() => navigate('servers')}>Cancel</button>
              <button class="button primary" type="submit" disabled={createServerBusy || (!installPassword && !installKey)}>
                {createServerBusy ? 'Connecting…' : 'Add and install server'}
              </button>
            </div>
          </form>
          {#if jobError}<p class="form-error">{jobError}</p>{/if}
        </article>
      </section>

    {:else if activePage === 'install-progress'}
      <section class="page" aria-labelledby="install-progress-title">
        <button class="back-link" type="button" on:click={() => navigate('servers')}>
          <Icon name="arrow-left" size={15} />
          <span>Back to servers</span>
        </button>
        <div class="page-heading">
          <div>
            <h1 id="install-progress-title">{activeInstall ? `Installing ${activeInstall.serverName}` : 'Server Installation'}</h1>
            <p class="lede">
              {activeInstall
                ? `Connecting over SSH to ${activeInstall.host}:${activeInstall.port} and monitoring deployment progress.`
                : 'No active deployment found.'}
            </p>
          </div>
        </div>

        {#if activeInstall}
          <InstallProgress
            install={activeInstall}
            onCancel={() => promptCancelInstall()}
            onViewServer={(id) => { saveActiveInstall(null); detailTab = 'metrics'; navigate('server', id); }}
            onRetry={() => {
              if (activeInstall) {
                newServerName = activeInstall.serverName;
                installHost = activeInstall.host;
                installPort = String(activeInstall.port);
                installUser = activeInstall.user;
              }
              navigate('add-server');
            }}
            onAddAnother={() => openAddServer()}
          />
        {:else}
          <div class="state-panel">
            <div class="state-icon"><Icon name="servers" size={28} /></div>
            <h2>No active deployment</h2>
            <p>Select a server from the fleet or add a new server to start an installation.</p>
            <button class="button primary" type="button" on:click={() => openAddServer()}>
              <Icon name="plus" size={15} />
              <span>Add a server</span>
            </button>
          </div>
        {/if}
      </section>

    {:else if activePage === 'alerts'}
      <section class="page" aria-labelledby="alerts-title">
        <div class="page-heading">
          <div>
            <h1 id="alerts-title">Alerts</h1>

          </div>
          <button class="button ghost" type="button" on:click={() => void loadAlerts()}>
            <Icon name="refresh" size={14} />
            <span>Reload</span>
          </button>
        </div>

        {#if alertsState === 'loading'}
          <div class="state-panel"><div class="loading-spinner"></div><h2>Loading alert states…</h2></div>
        {:else if alertsState === 'error'}
          <div class="state-panel error-state">
            <h2>Could not load alerts</h2>
            <p>{alertsError}</p>
            <button class="button primary" on:click={() => void loadAlerts()}>Retry</button>
          </div>
        {:else if alertsState === 'empty'}
          <div class="state-panel">
            <div class="state-icon positive"><Icon name="check" size={24} /></div>
            <h2>All systems operating normally</h2>
            <p>No active incidents or firing alert rules across the fleet.</p>
          </div>
        {:else}
          <div class="server-list">
            {#each alerts as alert}
              <article class="server-row alert-row">
                <span class={`status-pill ${alert.state === 'firing' ? 'failed' : alert.state === 'pending' ? 'stale' : 'healthy'}`}>
                  <i class="status-dot"></i>
                  <span>{alert.state}</span>
                </span>
                <span class="server-identity">
                  <strong>{alert.rule_id}</strong>
                  <small>{servers.find((server) => server.id === alert.server_id)?.name || alert.server_id || 'Fleet-wide'}</small>
                </span>
                <span class="server-status">
                  <small class="faint">{alert.last_observation ? new Date(alert.last_observation).toLocaleString() : 'Awaiting observation'}</small>
                </span>
                {#if alert.last_value !== undefined}
                  <span class="server-metric">
                    <strong>{alert.last_value.toFixed(2)}</strong>
                    <small>Value</small>
                  </span>
                {/if}
              </article>
            {/each}
          </div>
        {/if}
      </section>

    {:else if activePage === 'packages'}
      <section class="page" aria-labelledby="packages-title">
        <div class="page-heading">
          <div>
            <h1 id="packages-title">Packages</h1>

          </div>
          <button class="button ghost" type="button" on:click={() => void loadModules(true)}>
            <Icon name="refresh" size={14} />
            <span>Refresh catalog</span>
          </button>
        </div>

        <article class="panel package-target">
          <label>
            <span>Target server</span>
            <select bind:value={packageServerId} on:change={() => void loadServerModules()}>
              {#each servers as server}
                <option value={server.id}>{server.name} ({server.architecture})</option>
              {/each}
            </select>
          </label>
          <div class="package-source-fields">
            <label>Source<select bind:value={packageSource} on:change={() => { packageLocation = ''; packageManifest = ''; packageSignature = ''; packageError = ''; }}><option value="github">GitHub</option><option value="local">Local path</option><option value="url">URL</option></select></label>
            <label>{packageSource === 'github' ? 'Repository' : packageSource === 'local' ? 'Archive path on this server' : 'Archive URL'}<input bind:value={packageLocation} placeholder={packageSource === 'github' ? 'Real-kia/payesh' : packageSource === 'local' ? '/opt/packages/cpu-controls-linux-amd64.tar.gz' : 'https://packages.example.com/cpu-controls-linux-amd64.tar.gz'} /></label>
            {#if packageSource === 'github'}
              <label>Release<input bind:value={packageVersion} placeholder="latest" /></label>
            {:else}
              <details><summary>Package metadata</summary><label>Manifest {packageSource === 'local' ? 'path' : 'URL'}<input bind:value={packageManifest} placeholder="Automatic (.manifest.json)" /></label><label>Signature {packageSource === 'local' ? 'path' : 'URL'}<input bind:value={packageSignature} placeholder="Automatic (.manifest.sig)" /></label></details>
            {/if}
          </div>
        </article>

        {#if modulesState === 'loading'}
          <div class="state-panel"><div class="loading-spinner"></div><h2>Loading modules…</h2></div>
        {:else if modulesState === 'error'}
          <div class="state-panel error-state">
            <h2>Could not load packages</h2>
            <p>{modulesError}</p>
            <button class="button primary" on:click={() => void loadModules(true)}>Retry</button>
          </div>
        {:else if modulesState === 'empty'}
          <div class="state-panel">
            <h2>No packages available</h2>
            <p>The verified package catalog is currently empty.</p>
          </div>
        {:else}
          <div class="module-grid">
            {#each modules as module}
              <article class="panel module-card">
                <div class="module-top">
                  <div class="module-badge">{module.id}</div>
                  <span class="release-tag">v{module.latest_version}</span>
                </div>
                <h2>{module.name}</h2>
                <p class="muted module-desc">{module.description || 'Optional Payesh extension capability.'}</p>
                <div class="module-meta">
                  <span>Status: <strong class="capitalize">{moduleState(module.id)?.state || 'not installed'}</strong></span>
                </div>
                {#if module.id === 'process-monitoring' && moduleState(module.id)?.state === 'enabled'}
                  <button class="button primary small full-width" type="button" on:click={() => { detailTab = 'processes'; navigate('server', packageServerId); }}>View processes</button>
                {/if}
                {#if packageSource === 'github'}<a class="button ghost small full-width" href={packageGitHubURL(module)} target="_blank" rel="noopener noreferrer">{['amd64', 'arm64'].includes(servers.find((server) => server.id === packageServerId)?.architecture || '') ? 'Download archive' : 'View release'}</a>{/if}
                {#if !moduleState(module.id) || ['unavailable', 'available', 'failed'].includes(moduleState(module.id)?.state || '')}
                  {#if myAccount?.permission !== 'read'}<button class="button primary small full-width" disabled={!!packageBusy || !packageServerId || (packageSource !== 'github' && !packageLocation.trim())} on:click={() => void installPackage(module)}>{packageBusy === `${module.id}:install` ? 'Installing…' : 'Install'}</button>{/if}
                {:else if moduleState(module.id)?.state === 'installed-disabled' && myAccount?.permission !== 'read'}
                  <div class="job-actions">
                    <button class="button primary small" disabled={!!packageBusy} on:click={() => void packageAction(module, 'enable')}>Enable</button>
                    <button class="button ghost small" disabled={!!packageBusy} on:click={() => void packageAction(module, 'remove')}>Remove</button>
                  </div>
                {:else if moduleState(module.id)?.state === 'enabled' && myAccount?.permission !== 'read'}
                  <button class="button ghost small full-width" disabled={!!packageBusy} on:click={() => void packageAction(module, 'disable')}>Disable module</button>
                {/if}
              </article>
            {/each}
          </div>
        {/if}
        {#if packageError}<p class="form-error" role="alert">{packageError}</p>{/if}
      </section>

    {:else if activePage === 'settings'}
      <section class="page" aria-labelledby="settings-title">
        <div class="page-heading">
          <div>
            <h1 id="settings-title">Settings</h1>
          </div>
        </div>

        <div class="settings-layout">
          <nav class="settings-nav" aria-label="Settings sections">
            <button type="button" aria-current={settingsSection === 'tls' ? 'page' : undefined} class:chosen={settingsSection === 'tls'} on:click={() => settingsSection = 'tls'}>SSL / TLS</button>
            <button type="button" aria-current={settingsSection === 'updates' ? 'page' : undefined} class:chosen={settingsSection === 'updates'} on:click={() => settingsSection = 'updates'}>Versions & updates</button>
            {#if myAccount?.role === 'owner'}<button type="button" aria-current={settingsSection === 'users' ? 'page' : undefined} class:chosen={settingsSection === 'users'} on:click={() => settingsSection = 'users'}>User management</button>{/if}
          </nav>
          <div class="settings-grid">
          {#if settingsSection === 'users' && myAccount?.role === 'owner'}
          <article class="panel account-management">
            <div class="panel-heading"><div><h2>User management</h2><span class="muted">{accounts.length} users</span></div><button class="button primary small" type="button" on:click={() => addingUser = !addingUser}>{addingUser ? 'Cancel' : 'Add user'}</button></div>
            <label class="user-search">Search users<input bind:value={userSearch} placeholder="Search by username" /></label>
            <div class="account-list">
              <div class="user-table-head"><span>User</span><span>Role</span><span>Access</span><span>Actions</span></div>
              {#each accounts.filter(account => account.username.toLowerCase().includes(userSearch.toLowerCase())) as account}
                <div class="account-row">
                  <strong>{account.username}</strong><span class="user-role">{account.role}</span><span>{account.permission === 'read' ? 'Read only' : 'Can edit'}</span>
                  <div class="settings-actions"><button class="button ghost small" type="button" on:click={() => startEditingAccount(account)}>Edit</button>{#if account.role !== 'owner'}<button class="button ghost small" type="button" on:click={() => void removeAccount(account)}>Remove</button>{/if}</div>
                </div>
                {#if editingAccount === account.username}
                  <form class="form-grid account-edit-form" on:submit|preventDefault={() => void editAccount(account)}>
                    <label>Username<input bind:value={editUsername} minlength="3" maxlength="128" required autocomplete="off" /></label>
                    <label>New password<input type="password" bind:value={editPassword} minlength="12" placeholder="Leave blank to keep current" autocomplete="new-password" /></label>
                    {#if account.role !== 'owner'}
                      <label>Role<select bind:value={editRole}><option value="member">Member</option><option value="admin">Admin</option></select></label>
                      <label>Permission<select bind:value={editPermission}><option value="read">Read only</option><option value="edit">Can edit</option></select></label>
                    {/if}
                    <div class="settings-actions"><button class="button primary small" type="submit" disabled={accountBusy}>Save changes</button><button class="button ghost small" type="button" on:click={() => editingAccount = ''}>Cancel</button></div>
                  </form>
                {/if}
              {/each}
            </div>
            {#if addingUser}<form class="form-grid account-edit-form" on:submit|preventDefault={() => void saveAccount()}>
              <h3 class="form-wide">New user</h3>
              <label>Username<input bind:value={accountUsername} minlength="3" maxlength="128" required autocomplete="off" /></label>
              <label>Password<input type="password" bind:value={accountPassword} minlength="12" required autocomplete="new-password" /></label>
              <label>Role<select bind:value={accountRole}><option value="member">Member</option><option value="admin">Admin</option></select></label>
              <label>Permission<select bind:value={accountPermission}><option value="read">Read only</option><option value="edit">Can edit</option></select></label>
              <button class="button primary" type="submit" disabled={accountBusy}>Create user</button>
            </form>{/if}
            {#if accountError}<p class="form-error" role="alert">{accountError}</p>{/if}
          </article>
          {/if}
          {#if settingsSection === 'updates'}<article class="panel account-management">
            <div class="panel-heading"><div><h2>Versions & updates</h2></div><button class="button ghost small" type="button" disabled={updateCheckBusy || PREVIEW_MODE} on:click={() => void checkLatestUpdate()}>{updateCheckBusy ? 'Checking…' : 'Check GitHub'}</button></div>
            <div class="settings-meta-box">
              <div class="meta-row"><span>Installed version</span><strong>{updateStatus?.current ? `v${updateStatus.current}` : servers.find(server => server.role !== 'node')?.version ?? 'Unavailable'}</strong></div>
            </div>
            {#if updateStatus}
              <div class="settings-meta-box">
                <div class="meta-row"><span>Latest release</span><a href={updateStatus.url} target="_blank" rel="noopener noreferrer">v{updateStatus.latest}</a></div>
                <div class="meta-row"><span>Status</span><span class={`status-pill ${updateStatus.update_available ? 'pending' : 'healthy'}`}>{updateStatus.update_available ? 'Update available' : 'Up to date'}</span></div>
              </div>
            {/if}
            {#if updateCheckError}<p class="form-error" role="alert">{updateCheckError}</p>{/if}
            <h3>Release history</h3>
            {#if updateStatus?.releases?.length}
              <div class="release-list">{#each updateStatus.releases as release}<a class="release-row" href={release.url} target="_blank" rel="noopener noreferrer"><strong>v{release.version}</strong><span>{release.version === updateStatus.current ? 'Installed' : new Date(release.published_at).toLocaleDateString()}</span><span>Release notes ↗</span></a>{/each}</div>
            {:else}<p class="muted">{updateCheckBusy ? 'Loading releases…' : 'Release history unavailable.'}</p>{/if}
            <h3>Update command</h3>
            <code class="update-command">sudo payesh update</code>
            <small class="muted">Private repository: pass a read-only GitHub token with <code>sudo --preserve-env=GITHUB_TOKEN payesh update</code>.</small>
          </article>{/if}
          {#if settingsSection === 'tls'}<article class="panel">
            <div class="panel-heading">
              <div>
                <h2>SSL / TLS</h2>
              </div>
            </div>
            <div class="settings-meta-box">
              <div class="meta-row">
                <span>Status</span>
                {#if httpsStatus?.state === 'active'}
                  <span class="status-pill healthy"><i class="status-dot"></i> HTTPS active</span>
                {:else if httpsStatus?.state === 'pending'}
                  <span class="status-pill pending"><i class="status-dot"></i> Requesting certificate…</span>
                {:else if httpsStatus?.state === 'failed'}
                  <span class="status-pill failed"><i class="status-dot"></i> Certificate request failed</span>
                {:else}
                  <span class="status-pill pending"><i class="status-dot"></i> Not encrypted (HTTP)</span>
                {/if}
              </div>
              {#if httpsStatus?.domain}
                <div class="meta-row"><span>Address</span><span class="mono">{#if httpsStatus.state === 'active'}<a href={httpsURL(httpsStatus)}>{httpsURL(httpsStatus)}</a>{:else}{httpsStatus.domain}{/if}</span></div>
              {/if}
              {#if httpsStatus?.expires_at}
                <div class="meta-row"><span>Renews before</span><span>{new Date(httpsStatus.expires_at).toLocaleDateString()}</span></div>
              {/if}
            </div>
            {#if httpsStatus?.state === 'failed' && httpsStatus.error}<p class="form-error" role="alert">{httpsStatus.error}</p>{/if}
            <form class="form-grid" on:submit|preventDefault={() => void saveHTTPS()}>
              <label>Domain<input bind:value={httpsDomain} placeholder="panel.example.com" autocomplete="off" required disabled={httpsBusy || myAccount?.permission === 'read'} /></label>
              <label>Email (optional)<input type="email" bind:value={httpsEmail} placeholder="you@example.com" autocomplete="email" disabled={httpsBusy || myAccount?.permission === 'read'} /></label>
              <label>Cloudflare API token (optional)<input type="password" bind:value={httpsToken} placeholder="Only needed if port 80 is in use" autocomplete="off" disabled={httpsBusy || myAccount?.permission === 'read'} /></label>
            </form>
            {#if httpsError}<p class="form-error" role="alert">{httpsError}</p>{/if}
            <div class="settings-actions">
              <button class="button primary" type="button" disabled={httpsBusy || myAccount?.permission === 'read' || !httpsDomain.trim()} on:click={() => void saveHTTPS()}>{httpsBusy ? 'Requesting certificate…' : httpsStatus?.domain ? 'Update certificate' : 'Enable HTTPS'}</button>
              {#if httpsStatus?.domain && !httpsBusy && myAccount?.permission !== 'read'}<button class="button ghost" type="button" on:click={() => promptRemoveHTTPS()}>Remove domain</button>{/if}
            </div>
          </article>
          {/if}
          {#if settingsSection === 'tls'}<article class="panel">
            <div class="panel-heading"><h2>Dashboard port</h2></div>
            <form class="port-form" on:submit|preventDefault={() => void saveDashboardPort()}>
              <label>Port<input type="number" min="1" max="65535" required bind:value={dashboardPort} disabled={portBusy || myAccount?.permission === 'read'} /></label>
              <button class="button primary" type="submit" disabled={portBusy || PREVIEW_MODE || myAccount?.permission === 'read'}>{portBusy ? 'Checking port…' : 'Change port'}</button>
            </form>
            {#if portError}<p class="form-error" role="alert">{portError}</p>{/if}
            <p class="muted">Existing nodes keep their current connection address.</p>
          </article>{/if}
          </div>
        </div>
      </section>

    {:else if activePage === 'onboarding' && (PREVIEW_MODE || sessionState !== 'authenticated')}
      <section class="page onboarding-page" aria-labelledby="setup-title">
        <div class="page-heading">
          <div>
            <h1 id="setup-title">Setup your local hub</h1>
            <p class="lede">Set your secure credentials to initialize this Payesh node.</p>
          </div>
        </div>
        <div class="stepper" aria-label="Setup progress">
          {#each ['Access', 'Defaults', 'Enroll'] as label, index}
            <div class:current={setupStep === index + 1} class:done={setupStep > index + 1} class="step">
              <span>{setupStep > index + 1 ? '✓' : index + 1}</span>
              {label}
            </div>
          {/each}
        </div>
        <div class="onboarding-card">
          {#if setupStep === 1}
            <h2>Create owner account</h2>
            <p class="muted">{PREVIEW_MODE ? 'These values stay in this preview and are never transmitted.' : 'The one-time setup secret is verified once over the authenticated API. Passwords are not persisted by this browser.'}</p>
            <div class="form-grid">
              <label>Workspace name<input bind:value={workspaceName} autocomplete="organization" placeholder="e.g. Infrastructure Team" /></label>
              <label>Bootstrap secret<input type="password" bind:value={setupSecret} minlength="16" autocomplete="new-password" placeholder="At least 16 characters" aria-invalid={setupError ? 'true' : undefined} /><small>At least 16 characters.</small></label>
              <label>Owner username<input bind:value={ownerUsername} minlength="3" autocomplete="username" required placeholder="admin" aria-invalid={setupError ? 'true' : undefined} /><small>Use the generated username from installation.</small></label>
              <label>Owner password<input type="password" bind:value={ownerPassword} minlength="12" autocomplete="new-password" placeholder="At least 12 characters" aria-invalid={setupError ? 'true' : undefined} /><small>At least 12 characters.</small></label>
            </div>
          {:else if setupStep === 2}
            <h2>Retention & notifications</h2>
            <p class="muted">Configurable metric sampling and alert routing options.</p>
            <div class="form-grid">
              <label>Metric retention<select bind:value={retention}><option value="7">7 days (low storage)</option><option value="30">30 days (recommended)</option><option value="90">90 days (extended)</option></select></label>
              <label>Notifications<select bind:value={notifications}><option value="none">Disabled</option><option value="email">Email summary</option><option value="webhook">Webhook endpoint</option></select></label>
            </div>
          {:else}
            <h2>Enroll first server</h2>
            <p class="muted">You can pair an existing server immediately or configure one later.</p>
            <div class="choice-row">
              <label class:chosen={enrollmentMode === 'skip'}><input type="radio" bind:group={enrollmentMode} value="skip" /> Skip for now</label>
              <label class:chosen={enrollmentMode === 'connect'}><input type="radio" bind:group={enrollmentMode} value="connect" /> Connect with a pairing token</label>
            </div>
            {#if enrollmentMode === 'connect'}
              {#if PREVIEW_MODE}
                <label class="pairing-field">Pairing token<input type="password" bind:value={pairingToken} minlength="16" autocomplete="off" aria-invalid={setupError ? 'true' : undefined} /><small>At least 16 characters. Preview only; not persisted or transmitted.</small></label>
              {:else if servers.length > 0}
                <label class="pairing-field">Pending server<select bind:value={selectedServerId}>{#each servers as server}<option value={server.id}>{server.name} · {server.id}</option>{/each}</select></label>
                <label class="pairing-field">Pairing token<input type="password" bind:value={pairingToken} minlength="16" autocomplete="off" aria-invalid={setupError ? 'true' : undefined} /><small>Single-use token sent to enrollment endpoint.</small></label>
              {:else}
                <div class="unavailable-panel"><strong>No pending server identity</strong><span>The server must appear in the authenticated fleet before this pairing route can target it.</span></div>
              {/if}
            {/if}
            <div class="review-box">
              <span>Workspace: <strong>{workspaceName || 'Default'}</strong></span>
              <span>Retention: <strong>{retention} days ({notifications})</strong></span>
              <span>Enrollment: <strong>{enrollmentMode === 'skip' ? 'Skipped' : 'Pairing token ready'}</strong></span>
            </div>
          {/if}
          {#if setupError}<p class="form-error" role="alert">{setupError}</p>{/if}
          <div class="setup-actions">
            <button class="button ghost" type="button" on:click={() => setupStep > 1 ? setupStep -= 1 : navigate('overview')}>{setupStep > 1 ? 'Back' : 'Cancel'}</button>
            <button class="button primary" type="button" on:click={() => void completeOnboarding()}>{setupStep === 3 ? (enrollmentMode === 'connect' ? 'Queue enrollment' : 'Save setup') : 'Continue'}</button>
          </div>
        </div>
      </section>

    {:else if activePage === 'server' && selectedServer}
      <section class="page server-page" aria-labelledby="server-title">
        <button class="back-link" type="button" on:click={() => navigate('overview')}>
          <Icon name="arrow-left" size={15} />
          <span>Back to overview</span>
        </button>

        <div class="page-heading server-heading">
          <div>
            <div class="heading-row">
              <h1 id="server-title">{selectedServer.name}</h1>
              <span class={`status-pill ${selectedServer.displayState}`}>
                <i class="status-dot"></i>
                <span>{stateLabel(selectedServer.displayState)}</span>
              </span>
            </div>
            <p class="lede mono">{selectedServer.platform} · {selectedServer.architecture} · v{selectedServer.version}</p>
          </div>
          <div class="server-heading-actions">
            {#if !PREVIEW_MODE && selectedServer.role === 'node'}
              <button class="button danger small" type="button" disabled={deleteServerBusy} on:click={() => promptDeleteServer()}>
                <Icon name="trash" size={14} />
                <span>{deleteServerBusy ? 'Deleting…' : 'Delete server'}</span>
              </button>
            {/if}
          </div>
        </div>

        <div class="server-meta-bar">
          <div class="meta-item">
            <span class="meta-label">Address</span>
            <span class="meta-value mono">{displayAddress(selectedServer)}</span>
          </div>
          <div class="meta-item">
            <span class="meta-label">Connection</span>
            <span class="meta-value capitalize">{selectedServer.connectionState}</span>
          </div>
          <div class="meta-item">
            <span class="meta-label">Freshness</span>
            <span class="meta-value capitalize">{selectedServer.freshnessState}</span>
          </div>
          <div class="meta-item">
            <span class="meta-label">Revision</span>
            <span class="meta-value mono">rev {selectedServer.configurationRevision}</span>
          </div>
          {#if selectedServer.freshnessReason}
            <div class="meta-item alert-hint">
              <span class="meta-label">Note</span>
              <span class="meta-value">{selectedServer.freshnessReason}</span>
            </div>
          {/if}
        </div>

        {#if activeInstall && activeInstall.serverId === selectedServer.id}
          <InstallProgress
            install={activeInstall}
            compact={true}
            onCancel={() => promptCancelInstall()}
            onViewServer={(id) => { saveActiveInstall(null); detailTab = 'metrics'; navigate('server', id); }}
            onRetry={() => { saveActiveInstall(null); }}
            onAddAnother={() => { saveActiveInstall(null); navigate('add-server'); }}
          />
        {:else if !PREVIEW_MODE && selectedServer.role === 'node' && selectedServer.connectionState !== 'connected'}
          <article class="panel server-actions">
            <div class="panel-heading">
              <div>
                <h2>Install over SSH</h2>
              </div>
            </div>
            <form class="form-grid" on:submit|preventDefault={() => void submitInstall()}>
              <label>SSH host<input bind:value={installHost} placeholder="hostname or IP" required /></label>
              <label>Port<input type="number" min="1" max="65535" bind:value={installPort} required /></label>
              <label>SSH user<input bind:value={installUser} required /></label>
              <label>Password (or private key)<input type="password" bind:value={installPassword} autocomplete="off" /></label>
              <label>Private key<textarea bind:value={installKey} rows="2" autocomplete="off"></textarea></label>
              <label>Expected host-key fingerprint<input bind:value={installFingerprint} placeholder="SHA256:…" /></label>
              <button class="button primary small" type="submit" disabled={installBusy || (!installPassword && !installKey)}>
                {installBusy ? 'Connecting…' : 'Install Payesh'}
              </button>
            </form>
            {#if jobError}<p class="form-error" role="alert">{jobError}</p>{/if}
          </article>
        {/if}

        {#if availableTabs.length > 0}
          <div class="tabs" role="tablist" aria-label="Server detail sections">
            {#each availableTabs as tab}
              <button class:active={detailTab === tab} type="button" role="tab" aria-selected={detailTab === tab} on:click={() => { detailTab = tab; saveUiState(); if (tab === 'logs') void loadLogs(selectedServer.id); }}>
                {tabLabel(tab)}
              </button>
            {/each}
          </div>
        {:else if !activeInstall || activeInstall.serverId !== selectedServer.id}
          <div class="panel pending-telemetry-notice">
            <span class="muted tab-empty">Telemetry views will activate once the server completes installation and streams its first heartbeat.</span>
          </div>
        {/if}

        {#if detailTab === 'processes'}
          {#key selectedServer.id}<ProcessTable serverId={selectedServer.id} onPackages={() => { packageServerId = selectedServer.id; navigate('packages'); }} />{/key}
        {:else if detailTab === 'metrics' && hasCapability(selectedServer, 'metrics')}
          <div class="metric-grid">
            {#each [['CPU', 'cpu', selectedServer.metrics.cpu, 'teal'], ['Memory', 'memory', selectedServer.metrics.memory, 'purple'], ['Disk', 'disk', selectedServer.metrics.disk, 'blue']] as metric}
              <article class="metric-card">
                <div class="card-top">
                  <span class="metric-title">{metric[0]}</span>
                  <strong class="metric-big tabular">{metricValue(metric[2] as number | null)}</strong>
                </div>
                {#if metricHistoryValues(selectedServer, metric[1] as 'cpu' | 'memory' | 'disk').length > 1}
                  <div class="sparkline">
                    <Sparkline values={metricHistoryValues(selectedServer, metric[1] as 'cpu' | 'memory' | 'disk')} tone={metric[3] as 'teal' | 'purple' | 'blue'} />
                  </div>
                {:else}
                  <div class="sparkline-unavailable">{metricHistoryValues(selectedServer, metric[1] as 'cpu' | 'memory' | 'disk').some((value) => value !== null) ? 'Collecting history' : 'Awaiting samples'}</div>
                {/if}
                <div class="metric-footer">
                  <span>{sampleAge(selectedServer)}</span>
                  <span class="faint">Window: {chartRange}</span>
                </div>
              </article>
            {/each}
          </div>

          <article class="panel chart-panel">
            <div class="panel-heading">
              <div>
                <h2>CPU and Memory Utilization</h2>
              </div>
              <div class="range-selector">
                <select bind:value={chartRange} aria-label="Chart time range">
                  <option value="15m">Last 15 minutes</option>
                  <option value="1h">Last hour</option>
                  <option value="24h">Last 24 hours</option>
                </select>
              </div>
            </div>

            {#if chartData && chartData.coverage !== 'unavailable'}
              <div class="legend">
                <span><i class="legend-dot teal"></i> CPU Utilization</span>
                <span><i class="legend-dot purple"></i> Memory</span>
                <span class="faint">Scale: 0–100%</span>
              </div>
              {#key `${chartRange}-${theme}-${selectedServer.id}`}
                <ChartPreview data={chartData} range={chartRange} label="CPU and memory history over the selected time range" />
              {/key}
              <div class="resource-highlights">
                <span>Highest CPU: <strong class="tabular">{seriesRange(chartData.cpu)}</strong></span>
                <span>Highest Memory: <strong class="tabular">{seriesRange(chartData.memory)}</strong></span>
              </div>
            {:else}
              <div class="unavailable-panel">
                <strong>Resource history unavailable</strong>
                <span>No telemetry samples have been recorded yet for this range.</span>
              </div>
            {/if}
          </article>

          <article class="panel chart-panel">
            <div class="panel-heading"><h2>Network</h2></div>
            <div class="network-rates">
              <div><span class="muted">Download</span><strong class="tabular">{formatNetworkRate(currentNetworkRate(selectedServer, 'download'))}</strong></div>
              <div><span class="muted">Upload</span><strong class="tabular">{formatNetworkRate(currentNetworkRate(selectedServer, 'upload'))}</strong></div>
            </div>
            {#if chartData && (chartData.networkRx?.some((value) => value !== null) || chartData.networkTx?.some((value) => value !== null))}
              <div class="legend"><span><i class="legend-dot teal"></i> Download</span><span><i class="legend-dot blue"></i> Upload</span><span class="faint">Mbit/s</span></div>
              {#key `${chartRange}-${theme}-${selectedServer.id}-network`}
                <TrafficChart data={chartData} />
              {/key}
            {:else}
              <div class="unavailable-panel"><strong>Waiting for network samples</strong></div>
            {/if}
          </article>

        {:else if detailTab === 'traffic' && hasCapability(selectedServer, 'traffic')}
          <article class="panel chart-panel">
            <div class="panel-heading">
              <div>
                <h2>Download & Upload Bandwidth</h2>
              </div>
              <div class="range-selector">
                <select bind:value={chartRange} aria-label="Traffic chart time range">
                  <option value="15m">Last 15 minutes</option>
                  <option value="1h">Last hour</option>
                  <option value="24h">Last 24 hours</option>
                </select>
              </div>
            </div>

            {#if chartData?.networkRx?.some((v) => v !== null) || chartData?.networkTx?.some((v) => v !== null)}
              <div class="legend">
                <span><i class="legend-dot teal"></i> Download</span>
                <span><i class="legend-dot blue"></i> Upload</span>
                <span class="faint">Unit: Mbit/s</span>
              </div>
              {#key `${chartRange}-${theme}-${selectedServer.id}-traffic`}
                <TrafficChart data={chartData} />
              {/key}
            {:else}
              <div class="unavailable-panel">
                <strong>Waiting for traffic samples</strong>
                <span>At least two consecutive network counter samples are needed to calculate bandwidth rate.</span>
              </div>
            {/if}
          </article>

          {#if selectedServer.traffic.from}
            <article class="panel traffic-panel">
              <div class="panel-heading">
                <div>
                  <h2>{selectedServer.traffic.scope} billing window</h2>
                </div>
                <span class="status-pill {selectedServer.traffic.continuity === 'complete' ? 'healthy' : 'stale'}">
                  <i class="status-dot"></i>
                  <span>{selectedServer.traffic.continuity}</span>
                </span>
              </div>
              <div class="traffic-number">
                <strong class="tabular">{formatBytes(selectedServer.traffic.countedBytes)}</strong>
                <span class="faint">of {formatBytes(selectedServer.traffic.allowanceBytes)} allowance</span>
              </div>
              <div class="progress">
                <span style={`width:${percentage(selectedServer.traffic.countedBytes, selectedServer.traffic.allowanceBytes)}%`}></span>
              </div>
              <div class="traffic-details">
                <div class="detail-cell"><span>Direction</span><strong>{selectedServer.traffic.direction}</strong></div>
                <div class="detail-cell"><span>Timezone</span><strong>{selectedServer.traffic.timezone}</strong></div>
                <div class="detail-cell"><span>Period</span><strong class="mono">{selectedServer.traffic.from.slice(0, 10)} → {selectedServer.traffic.to.slice(0, 10)}</strong></div>
                <div class="detail-cell"><span>Continuity</span><strong>{selectedServer.traffic.continuity}</strong></div>
              </div>
            </article>
          {:else}
            <div class="unavailable-panel">
              <strong>Traffic allowance not configured</strong>
              <span>Bandwidth monitoring is active. Configure an allowance to enable quota tracking.</span>
            </div>
          {/if}

        {:else if detailTab === 'logs' && hasCapability(selectedServer, 'logs')}
          <article class="panel logs-panel">
            <div class="panel-heading">
              <div>
                <h2>System Event Stream</h2>
              </div>
              <button class="button ghost small" type="button" on:click={() => PREVIEW_MODE ? showNotice('Live tail is connected in package 03.') : void loadLogs(selectedServer.id)}>
                <Icon name="refresh" size={13} />
                <span>Reload</span>
              </button>
            </div>
            {#if !PREVIEW_MODE && logState === 'loading'}
              <div class="state-panel"><div class="loading-spinner" aria-hidden="true"></div><h2>Streaming logs…</h2></div>
            {:else if !PREVIEW_MODE && logState === 'error'}
              <div class="unavailable-panel">
                <strong>Could not load logs</strong>
                <span>{logError}</span>
                <button class="button ghost small" type="button" on:click={() => void loadLogs(selectedServer.id)}>Retry</button>
              </div>
            {:else if !PREVIEW_MODE && logState === 'empty'}
              <div class="unavailable-panel"><strong>No log entries recorded</strong><span>No entries returned for this server.</span></div>
            {:else}
              <div class="terminal-log-viewer">
                <div class="log-list">
                  {#each PREVIEW_MODE ? previewLogEntries : logEntries as entry}
                    <div class="log-entry">
                      <time class="mono tabular">{entry.time}</time>
                      <span class={`log-level-badge ${entry.level.toLowerCase()}`}>{entry.level}</span>
                      <span class="log-text mono">
                        <span>{entry.text}</span>
                        <small class="faint">{entry.source} #{entry.cursor}</small>
                      </span>
                    </div>
                  {/each}
                </div>
              </div>
            {/if}
          </article>

        {:else if selectedServer && detailTab !== 'metrics' && !hasCapability(selectedServer, detailTab)}
          <div class="unavailable-panel large"><strong>{tabLabel(detailTab)} unavailable</strong><span>{capabilityMessage(selectedServer, detailTab)}</span></div>
        {:else if selectedServer && !hasCapability(selectedServer, 'metrics')}
          <div class="unavailable-panel large"><strong>Monitoring data unavailable</strong><span>{capabilityMessage(selectedServer, 'metrics')}</span></div>
        {/if}
      </section>

    {:else}
      <!-- FLEET OVERVIEW PAGE -->
      <section class="page overview-page" aria-labelledby="overview-title">
        <div class="page-heading">
          <div>
            <h1 id="overview-title">{PREVIEW_MODE ? "Kia's Workspace" : 'Fleet Overview'}</h1>

          </div>
          <div class="heading-status-badge">
            <span class="live-ping"></span>
            <span class="date-stamp tabular">{PREVIEW_MODE ? 'Preview adapter' : 'Live telemetry stream'}</span>
          </div>
        </div>

        {#if authExpired}
          <div class="login-screen">
            <div class="login-card">
              <div class="brand-mark large"><span>P</span></div>
              <h2>Sign in to hub</h2>
              <p class="muted center">Authenticate to access fleet operations.</p>
              <form class="auth-form" on:submit|preventDefault={() => void submitLogin()}>
                <label>
                  <span>Username</span>
                  <input bind:value={authUsername} autocomplete="username" placeholder="owner" required />
                </label>
                <label>
                  <span>Password</span>
                  <input type="password" bind:value={authPassword} autocomplete="current-password" placeholder="••••••••••••" required />
                </label>
                {#if authError}<p class="form-error" role="alert">{authError}</p>{/if}
                {#if insecureConnection}<p class="insecure-login" role="note"><Icon name="alert-triangle" size={14} /> Not encrypted: your password is sent without SSL. Add a domain in Settings to enable HTTPS.</p>{/if}
                <button class="button primary" type="submit" disabled={authBusy}>
                  {authBusy ? 'Authenticating…' : 'Sign in to Console'}
                </button>
              </form>
            </div>
          </div>
        {:else if previewState === 'loading'}
          <div class="state-panel">
            <div class="loading-spinner" aria-hidden="true"></div>
            <h2>Connecting to fleet…</h2>
            <p>Gathering health telemetry and server states.</p>
          </div>
        {:else if previewState === 'empty'}
          <div class="state-panel">
            <div class="state-icon"><Icon name="servers" size={28} /></div>
            <h2>No servers enrolled</h2>
            <p>Connect your first Linux VPS via SSH to begin monitoring.</p>
            <button class="button primary" type="button" on:click={() => openAddServer()}>
              <Icon name="plus" size={15} />
              <span>Add first server</span>
            </button>
          </div>
        {:else if previewState === 'error'}
          <div class="state-panel error-state">
            <div class="state-icon"><Icon name="alert-triangle" size={28} /></div>
            <h2>Could not connect to fleet</h2>
            <p>{apiError || 'The API endpoint returned an error.'}</p>
            <button class="button primary" type="button" on:click={() => PREVIEW_MODE ? void loadPreviewData() : void loadApiData()}>Retry connection</button>
          </div>
        {:else}
          <!-- STATS CARDS -->
          <div class="summary-grid">
            <article class="summary-card">
              <div class="card-header">
                <span class="stat-label">Healthy Servers</span>
                <span class="stat-icon-wrap emerald"><Icon name="check" size={15} /></span>
              </div>
              <strong class="stat-value tabular">{healthyCount}<small class="stat-total"> / {displayServers.length}</small></strong>
              <div class="stat-badge positive">
                <span>Healthy and responding</span>
              </div>
            </article>

            <article class="summary-card">
              <div class="card-header">
                <span class="stat-label">Needs Attention</span>
                <span class="stat-icon-wrap amber"><Icon name="alert-triangle" size={15} /></span>
              </div>
              <strong class="stat-value tabular">{attentionCount}</strong>
              <div class="stat-badge {attentionCount > 0 ? 'warning' : 'neutral'}">
                <span>{attentionCount > 0 ? 'Stale or pending nodes' : 'Zero failing nodes'}</span>
              </div>
            </article>

            <article class="summary-card">
              <div class="card-header">
                <span class="stat-label">Bandwidth Usage</span>
                <span class="stat-icon-wrap cyan"><Icon name="activity" size={15} /></span>
              </div>
              <strong class="stat-value tabular">{overviewAllowanceBytes === '0' ? 'Unmetered' : formatBytes(overviewTrafficBytes)}</strong>
              <div class="stat-badge neutral">
                <span>{overviewAllowanceBytes === '0' ? 'Live rates available per server' : `of ${formatBytes(overviewAllowanceBytes)} quota`}</span>
              </div>
            </article>
          </div>

          <!-- SERVER LIST SECTION -->
          <div class="section-heading">
            <div>
              <h2>Servers ({displayServers.length})</h2>
            </div>
            <button class="button ghost small" type="button" on:click={() => navigate('monitoring')}>Go to server monitoring <Icon name="chevron-right" size={14} /></button>
          </div>

          <div class="table-container">
            <div class="table-header">
              <span class="col-status">Status</span>
              <span class="col-name">Hostname / Address</span>
              <span class="col-meta">Platform</span>
              <span class="col-metric">CPU %</span>
              <span class="col-action"></span>
            </div>
            <div class="server-list">
              {#each displayServers as server}
                <button class="server-row" type="button" on:click={() => selectServer(server)}>
                  <div class="col-status">
                    <span class={`status-pill ${server.displayState}`} aria-label={stateLabel(server.displayState)}>
                      <i class="status-dot"></i>
                      <span>{stateLabel(server.displayState)}</span>
                    </span>
                  </div>
                  <div class="col-name server-identity">
                    <strong>{server.name}</strong>
                    <small class="mono faint">{displayAddress(server)}</small>
                  </div>
                  <div class="col-meta">
                    <span>{server.role} · {server.platform} ({server.architecture})</span>
                    <small class="faint">{server.freshnessState === 'unknown' ? server.freshnessReason : `Heartbeat ${server.lastHeartbeat?.slice(11, 16)} UTC`}</small>
                  </div>
                  <div class="col-metric server-metric">
                    <strong class="tabular">{metricValue(server.metrics.cpu)}</strong>
                    <div class="metric-microbar">
                      <span style={`width: ${Math.min(100, Math.max(0, server.metrics.cpu ?? 0))}%`}></span>
                    </div>
                  </div>
                  <div class="col-action">
                    <span class="server-arrow" aria-hidden="true"><Icon name="chevron-right" size={16} /></span>
                  </div>
                </button>
              {/each}
            </div>
          </div>

          <!-- LOWER PANELS -->
          <div class="lower-grid">
            <article class="panel">
              <div class="panel-heading">
                <div>
                  <h2>Allowance Overview</h2>
                </div>
                <button class="text-button" type="button" on:click={() => selectedServer && selectServer(selectedServer)}>
                  <span>View metrics</span>
                  <Icon name="chevron-right" size={14} />
                </button>
              </div>
              {#if overviewAllowanceBytes === '0'}
                <div class="unavailable-panel">
                  <strong>No allowance quota configured</strong>
                  <span>Open a server's Traffic tab to review live interface telemetry.</span>
                </div>
              {:else}
                <div class="traffic-number">
                  <strong class="tabular">{formatBytes(overviewTrafficBytes)}</strong>
                  <span class="faint">consumed this billing cycle</span>
                </div>
                <div class="progress">
                  <span style={`width:${percentage(overviewTrafficBytes, overviewAllowanceBytes)}%`}></span>
                </div>
                <p class="muted info-hint tabular">{formatBytes(overviewAllowanceBytes)} fleet quota · UTC timezone</p>
              {/if}
            </article>

            <article class="panel">
              <div class="panel-heading">
                <div>
                  <h2>Telemetry Activity</h2>
                </div>
                <span class="status-pill healthy"><i class="status-dot"></i> Live</span>
              </div>
              <div class="activity-feed">
                <div class="activity-item">
                  <span class="activity-icon-pill emerald"><Icon name="check" size={13} /></span>
                  <div>
                    <strong>Telemetry heartbeats active</strong>
                    <small class="faint">Periodic agent polling connected</small>
                  </div>
                </div>
                <div class="activity-item">
                  <span class="activity-icon-pill amber"><Icon name="activity" size={13} /></span>
                  <div>
                    <strong>Anomaly surveillance armed</strong>
                    <small class="faint">Fleet-wide threshold observers enabled</small>
                  </div>
                </div>
              </div>
            </article>
          </div>

          {#if latestJob}
            <article class="panel job-panel" aria-live="polite">
              <div class="panel-heading">
                <div>
                  <h2>{latestJob.kind}</h2>
                </div>
                <span class={`status-pill ${latestJob.state}`}>
                  <i class="status-dot"></i>
                  <span>{latestJob.state}</span>
                </span>
              </div>
              <div class="job-progress">
                <span style={`width:${Math.max(0, Math.min(100, latestJob.progress))}%`}></span>
              </div>
              <p class="muted info-hint tabular">{latestJob.progress}% · rev {latestJob.revision} · {latestJob.id}</p>
              {#if latestJob.error}<p class="form-error">{latestJob.error.message || 'The job failed.'}</p>{/if}
              {#if jobError}<p class="form-error" role="alert">{jobError}</p>{/if}
              <div class="job-actions">
                {#if ['queued', 'running'].includes(latestJob.state)}
                  <button class="button ghost small" type="button" on:click={() => promptCancelInstall()}>Cancel job</button>
                {/if}
                <button class="button ghost small" type="button" on:click={() => void pollJob(latestJob?.id ?? '')} disabled={jobBusy}>Reload status</button>
              </div>
            </article>
          {/if}
        {/if}
      </section>
    {/if}

    <footer>
      <span>{PREVIEW_MODE ? 'Preview adapter · Fixture data' : 'Authenticated API client · Production'}</span>
      <span>Payesh · Low-overhead Linux Fleet Monitor</span>
    </footer>
  </main>
</div>

<Modal
  open={modalDialog.open}
  title={modalDialog.title}
  description={modalDialog.description}
  tone={modalDialog.tone}
  icon={modalDialog.icon}
  confirmText={modalDialog.confirmText}
  cancelText={modalDialog.cancelText}
  hideCancel={modalDialog.hideCancel}
  busy={modalDialog.busy}
  onConfirm={handleModalConfirm}
  onCancel={closeConfirmModal}
/>

<style>
  :global(*) { box-sizing: border-box; }

  /* --------------------------------------------------------------------------
     DESIGN TOKENS: MODERN SLATE / ZINC AESTHETIC
     -------------------------------------------------------------------------- */
  :global(:root) {
    --canvas: #f4f5f7;
    --surface: #ffffff;
    --surface-muted: #eef1f4;
    --surface-elevated: #ffffff;
    --ink: #252e3a;
    --ink-secondary: #465365;
    --muted: #647183;
    --line: #dce1e7;
    --line-light: #e8ebef;
    --teal: #245caa;
    --teal-bg: #eaf0f8;
    --teal-glow: rgba(36, 92, 170, 0.12);
    --purple: #8c7394;
    --purple-bg: #f0edf2;
    --blue: #657f99;
    --blue-bg: #eef1f5;
    --success: #27734f;
    --success-bg: #edf5f0;
    --warning: #98651f;
    --warning-bg: #faf3e8;
    --danger: #b43e43;
    --danger-bg: #fbefef;
    --primary-contrast: #ffffff;
    --shadow-sm: none;
    --shadow: none;
    --shadow-lg: 0 8px 24px rgba(22, 31, 44, 0.12);
    --radius-sm: 3px;
    --radius-md: 5px;
    --radius-lg: 6px;
    --radius-xl: 8px;
  }

  :global(:root[data-theme='dark']) {
    --canvas: #171e27;
    --surface: #1e2732;
    --surface-muted: #252f3c;
    --surface-elevated: #2b3745;
    --ink: #e5ebf2;
    --ink-secondary: #b6c1cf;
    --muted: #98a6b8;
    --line: #364252;
    --line-light: #2c3846;
    --teal: #80aaf0;
    --teal-bg: #293950;
    --teal-glow: rgba(128, 170, 240, 0.12);
    --purple: #b4a0be;
    --purple-bg: #352e3e;
    --blue: #91a8bf;
    --blue-bg: #2b3745;
    --success: #90c2a1;
    --success-bg: #25382f;
    --warning: #dec18c;
    --warning-bg: #3b3428;
    --danger: #e4a0a2;
    --danger-bg: #3e2c33;
    --primary-contrast: #172438;
  }

  :global(html) { color-scheme: light; }
  :global(html[data-theme='dark']) { color-scheme: dark; }
  :global(body) {
    margin: 0;
    min-width: 320px;
    background: var(--canvas);
    color: var(--ink);
    font-family: "Segoe UI", "Helvetica Neue", Arial, sans-serif;
    -webkit-font-smoothing: antialiased;
    -moz-osx-font-smoothing: grayscale;
  }

  .tabular { font-variant-numeric: tabular-nums; }
  .mono { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; font-size: 13px; }
  .capitalize { text-transform: capitalize; }
  .faint { color: var(--muted); opacity: 0.8; }

  /* --------------------------------------------------------------------------
     APP SHELL & LAYOUT
     -------------------------------------------------------------------------- */
  .app-shell {
    display: grid;
    grid-template-columns: 216px minmax(0, 1fr);
    min-height: 100vh;
  }

  .sidebar {
    display: flex;
    flex-direction: column;
    padding: 20px 14px;
    border-right: 1px solid var(--line);
    background: var(--surface);
    gap: 24px;
  }

  .brand-lockup {
    display: flex;
    align-items: center;
    gap: 12px;
    padding: 6px 10px;
  }

  .brand-mark {
    width: 34px;
    height: 34px;
    display: grid;
    place-items: center;
    border-radius: 9px;
    background: var(--teal);
    color: #ffffff;
    font-weight: 800;
    font-size: 17px;
    box-shadow: none;
  }

  .brand-text strong {
    display: block;
    font-size: 15px;
    font-weight: 700;
    letter-spacing: -0.02em;
    color: var(--ink);
  }
  .brand-text small {
    display: block;
    font-size: 11px;
    color: var(--muted);
    letter-spacing: 0.02em;
  }

  .nav-list {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }

  .nav-item {
    display: flex;
    align-items: center;
    gap: 12px;
    width: 100%;
    padding: 9px 12px;
    border: 1px solid transparent;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--muted);
    font-size: 13px;
    font-weight: 500;
    text-align: left;
    transition: all 0.15s ease;
    cursor: pointer;
  }
  .nav-item:hover {
    background: var(--surface-muted);
    color: var(--ink);
  }
  .nav-item.active {
    background: var(--surface-muted);
    color: var(--ink);
    font-weight: 600;
    border-color: var(--line);
    box-shadow: var(--shadow-sm);
  }

  .monitoring-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: 16px; }
  .monitoring-card { display: flex; flex-direction: column; gap: 18px; }
  .monitoring-top, .monitoring-bottom { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; }
  .monitoring-top h2 { margin: 0 0 4px; font-size: 17px; }
  .monitoring-metrics { display: grid; grid-template-columns: repeat(auto-fit, minmax(110px, 1fr)); gap: 8px; }
  .monitoring-metrics div { padding: 12px; border-radius: var(--radius-md); background: var(--surface-muted); }
  .monitoring-metrics span, .monitoring-metrics strong { display: block; }
  .monitoring-metrics span, .monitoring-bottom { color: var(--muted); font-size: 12px; }
  .monitoring-metrics strong { margin-top: 5px; font-size: 18px; color: var(--ink); }
  .monitoring-bottom { flex-wrap: wrap; }
  .monitoring-card > button { align-self: flex-start; margin-top: auto; }
  .account-management { grid-column: 1 / -1; }
  .account-list { margin-bottom: 20px; }
  .account-row, .user-table-head { display: grid; grid-template-columns: minmax(120px, 1fr) 80px 100px 150px; align-items: center; gap: 12px; padding: 16px 0; border-bottom: 1px solid var(--line); font-size: 13px; }
  .user-table-head { color: var(--muted); font-size: 12px; }
  .account-row .settings-actions { margin: 0; justify-content: flex-end; gap: 8px; }
  .user-role { text-transform: capitalize; }
  @media (max-width: 650px) { .user-table-head { display: none; } .account-row { grid-template-columns: 1fr 1fr; } .account-row .settings-actions { justify-content: flex-start; } }
  .update-command { display: block; width: fit-content; max-width: 100%; padding: 10px 14px; margin: 12px 0; border-radius: var(--radius-md); background: var(--surface-muted); overflow-wrap: anywhere; }

  .nav-count {
    margin-left: auto;
    padding: 2px 7px;
    border-radius: 4px;
    background: var(--surface);
    color: var(--muted);
    font-size: 11px;
    font-weight: 600;
  }
  .nav-count.firing {
    background: var(--danger-bg);
    color: var(--danger);
  }

  .sidebar-footer {
    margin-top: auto;
    padding: 12px 10px;
    border-top: 1px solid var(--line);
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 12px;
    color: var(--muted);
  }

  .connection-status {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .connection-dot {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--teal);
    box-shadow: 0 0 0 3px var(--teal-bg);
  }
  .version-tag {
    font-size: 11px;
    opacity: 0.7;
  }

  /* --------------------------------------------------------------------------
     TOPBAR & BREADCRUMBS
     -------------------------------------------------------------------------- */
  .main-content { min-width: 0; }
  .topbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    height: 58px;
    padding: 0 32px;
    border-bottom: 1px solid var(--line);
    background: var(--surface);
  }

  .breadcrumbs {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
  }
  .crumb-root { color: var(--muted); }
  .crumb-separator { color: var(--muted); opacity: 0.4; }
  .crumb-current { color: var(--ink); font-weight: 600; }

  .topbar-actions {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .preview-control {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--muted);
  }
  .preview-control select {
    padding: 5px 8px;
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    background: var(--surface-muted);
    color: var(--ink);
  }

  /* --------------------------------------------------------------------------
     BUTTONS & CONTROLS
     -------------------------------------------------------------------------- */
  :global(button), :global(input), :global(select), :global(textarea) { font: inherit; }
  :global(button) { cursor: pointer; }
  :global(:focus-visible) {
    outline: 2px solid var(--teal);
    outline-offset: 2px;
  }
  :global(input:not([type])),
  :global(input[type="text"]),
  :global(input[type="password"]),
  :global(input[type="email"]),
  :global(input[type="number"]),
  :global(select),
  :global(textarea) {
    background: var(--surface-muted);
    color: var(--ink);
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    padding: 7px 12px;
    font-size: 13px;
    font-family: inherit;
    transition: border-color var(--transition-fast), box-shadow var(--transition-fast);
  }
  :global(select) {
    cursor: pointer;
    appearance: none;
    -webkit-appearance: none;
    background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='%2394a3b8' stroke-width='2' stroke-linecap='round' stroke-linejoin='round'%3E%3Cpolyline points='6 9 12 15 18 9'%3E%3C/polyline%3E%3C/svg%3E");
    background-repeat: no-repeat;
    background-position: right 10px center;
    padding-right: 28px;
  }
  :global(input:focus),
  :global(select:focus),
  :global(textarea:focus) {
    outline: none;
    border-color: var(--teal);
    box-shadow: 0 0 0 2px var(--teal-glow);
  }
  :global(input::placeholder),
  :global(textarea::placeholder) {
    color: var(--muted);
    opacity: 0.7;
  }

  .button {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 8px;
    padding: 9px 16px;
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
    background: var(--surface);
    color: var(--ink);
    font-size: 13px;
    font-weight: 600;
    transition: all 0.15s ease;
  }
  .button:hover {
    background: var(--surface-muted);
    border-color: var(--muted);
  }
  .button.small {
    padding: 6px 12px;
    font-size: 12px;
  }
  .button.primary {
    background: var(--teal);
    border-color: var(--teal);
    color: var(--primary-contrast);
    box-shadow: none;
  }
  .button.primary:hover {
    filter: brightness(1.1);
  }
  .button.ghost {
    background: transparent;
    border-color: var(--line);
    color: var(--muted);
  }
  .button.ghost:hover {
    background: var(--surface-muted);
    border-color: var(--muted);
    color: var(--ink);
  }
  .button.danger {
    background: var(--danger-bg);
    border-color: transparent;
    color: var(--danger);
  }
  .button.danger:hover {
    filter: brightness(0.95);
  }
  .button.full-width { width: 100%; }

  .icon-button {
    display: grid;
    place-items: center;
    width: 34px;
    height: 34px;
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
    background: var(--surface);
    color: var(--muted);
    transition: all 0.15s ease;
  }
  .icon-button:hover {
    color: var(--ink);
    background: var(--surface-muted);
  }

  .text-button, .back-link {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    border: 0;
    background: transparent;
    color: var(--teal);
    font-size: 13px;
    font-weight: 600;
    cursor: pointer;
    padding: 0;
  }
  .back-link { margin-bottom: 24px; }
  .text-button:hover, .back-link:hover { text-decoration: underline; }

  /* --------------------------------------------------------------------------
     STATUS PILLS & INDICATORS
     -------------------------------------------------------------------------- */
  .status-pill {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 3px 9px;
    border-radius: 4px;
    font-size: 12px;
    font-weight: 600;
    text-transform: capitalize;
  }
  .status-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: currentColor;
  }

  .status-pill.healthy { background: var(--success-bg); color: var(--success); }
  .status-pill.stale, .status-pill.pending { background: var(--warning-bg); color: var(--warning); }
  .status-pill.installing {
    background: rgba(6, 182, 212, 0.12);
    color: #06b6d4;
    border: 1px solid rgba(6, 182, 212, 0.3);
  }
  .status-pill.installing .status-dot {
    background: #06b6d4;
    box-shadow: none;
    animation: none;
  }
  @keyframes pulse-dot {
    0%, 100% { opacity: 1; transform: scale(1); }
    50% { opacity: 0.4; transform: scale(0.85); }
  }
  .status-pill.unreachable, .status-pill.failed { background: var(--danger-bg); color: var(--danger); }
  .status-pill.disabled, .status-pill.unsupported { background: var(--surface-muted); color: var(--muted); }

  /* --------------------------------------------------------------------------
     NOTICES & ALERTS
     -------------------------------------------------------------------------- */
  .notice {
    position: fixed;
    z-index: 100;
    bottom: 24px;
    right: 24px;
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 12px 18px;
    border: 1px solid var(--teal);
    border-radius: var(--radius-md);
    background: var(--surface-elevated);
    box-shadow: var(--shadow-lg);
    color: var(--ink);
    font-size: 13px;
  }
  .partial-warning {
    display: flex;
    align-items: center;
    gap: 10px;
    margin: 16px 32px 0;
    padding: 10px 16px;
    border: 1px solid var(--warning);
    border-radius: var(--radius-md);
    background: var(--warning-bg);
    color: var(--warning);
    font-size: 13px;
  }

  /* --------------------------------------------------------------------------
     PAGE LAYOUT & HEADINGS
     -------------------------------------------------------------------------- */
  .page {
    max-width: 1280px;
    margin: 0 auto;
    padding: 32px 32px 48px;
  }
  .page-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 20px;
    margin-bottom: 28px;
  }
  h1 {
    margin: 0 0 6px;
    font-size: 26px;
    font-weight: 600;
    letter-spacing: -0.03em;
    color: var(--ink);
  }
  h2 {
    margin: 0 0 4px;
    font-size: 18px;
    font-weight: 600;
    letter-spacing: -0.02em;
    color: var(--ink);
  }
  .lede {
    margin: 0;
    font-size: 14px;
    color: var(--muted);
  }
  .heading-status-badge {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 5px 12px;
    border-radius: 4px;
    background: var(--surface);
    border: 1px solid var(--line);
    font-size: 12px;
    color: var(--muted);
  }
  .live-ping {
    width: 7px;
    height: 7px;
    border-radius: 50%;
    background: var(--teal);
    box-shadow: 0 0 0 3px var(--teal-bg);
  }

  /* --------------------------------------------------------------------------
     SUMMARY CARDS (METRIC OVERVIEW)
     -------------------------------------------------------------------------- */
  .summary-grid {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 16px;
    margin-bottom: 32px;
  }
  .summary-card {
    padding: 20px;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
    transition: transform 0.15s ease, box-shadow 0.15s ease;
  }
  .summary-card:hover {
    border-color: var(--line);
  }
  .card-header {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 12px;
  }
  .stat-label {
    font-size: 13px;
    font-weight: 500;
    color: var(--muted);
  }
  .stat-icon-wrap {
    width: 28px;
    height: 28px;
    display: grid;
    place-items: center;
    border-radius: var(--radius-sm);
  }
  .stat-icon-wrap.emerald { background: transparent; color: var(--muted); }
  .stat-icon-wrap.amber { background: transparent; color: var(--muted); }
  .stat-icon-wrap.cyan { background: transparent; color: var(--muted); }

  .stat-value {
    display: block;
    font-size: 30px;
    font-weight: 700;
    letter-spacing: -0.03em;
    color: var(--ink);
    margin-bottom: 10px;
  }
  .stat-total {
    font-size: 15px;
    font-weight: 500;
    color: var(--muted);
  }
  .stat-badge {
    font-size: 12px;
    color: var(--muted);
  }
  .stat-badge.positive { color: var(--teal); }
  .stat-badge.warning { color: var(--warning); }

  /* --------------------------------------------------------------------------
     TABLE CONTAINER & SERVER LIST
     -------------------------------------------------------------------------- */
  .section-heading {
    display: flex;
    align-items: flex-end;
    justify-content: space-between;
    margin-bottom: 14px;
  }
  .table-container {
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
    overflow: hidden;
    margin-bottom: 32px;
  }
  .table-header {
    display: grid;
    grid-template-columns: 140px minmax(200px, 2fr) minmax(180px, 1.5fr) 110px 40px;
    gap: 16px;
    align-items: center;
    padding: 12px 20px;
    border-bottom: 1px solid var(--line);
    background: var(--surface-muted);
    font-size: 12px;
    font-weight: 600;
    color: var(--muted);
    text-transform: none;
    letter-spacing: 0.04em;
  }
  .server-list { display: flex; flex-direction: column; }
  .server-row {
    display: grid;
    grid-template-columns: 140px minmax(200px, 2fr) minmax(180px, 1.5fr) 110px 40px;
    gap: 16px;
    align-items: center;
    width: 100%;
    padding: 16px 20px;
    border: 0;
    border-bottom: 1px solid var(--line);
    background: transparent;
    color: var(--ink);
    text-align: left;
    transition: background 0.15s ease;
    cursor: pointer;
  }
  .server-row:last-child { border-bottom: 0; }
  .server-row:hover { background: var(--surface-muted); }

  .server-identity strong {
    display: block;
    font-size: 14px;
    font-weight: 600;
    color: var(--ink);
  }
  .server-identity small {
    display: block;
    margin-top: 2px;
    font-size: 12px;
  }
  .col-meta {
    font-size: 13px;
    color: var(--ink-secondary);
  }
  .col-meta span { display: block; }
  .col-meta small { display: block; font-size: 11px; margin-top: 2px; }

  .server-metric {
    display: flex;
    flex-direction: column;
    gap: 4px;
  }
  .server-metric strong {
    font-size: 14px;
    font-weight: 600;
  }
  .metric-microbar {
    width: 100%;
    height: 4px;
    border-radius: 4px;
    background: var(--surface-muted);
    overflow: hidden;
  }
  .metric-microbar span {
    display: block;
    height: 100%;
    background: var(--teal);
    border-radius: 4px;
  }

  .col-action {
    display: flex;
    justify-content: flex-end;
    color: var(--muted);
  }

  /* --------------------------------------------------------------------------
     LOWER PANELS
     -------------------------------------------------------------------------- */
  .lower-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 16px;
  }
  .panel {
    padding: 24px;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
  }
  .panel-heading {
    display: flex;
    align-items: flex-start;
    justify-content: space-between;
    gap: 16px;
    margin-bottom: 20px;
  }
  .traffic-number {
    display: flex;
    align-items: baseline;
    gap: 10px;
    margin: 12px 0 16px;
  }
  .traffic-number strong {
    font-size: 32px;
    font-weight: 700;
    letter-spacing: -0.03em;
  }
  .progress {
    height: 8px;
    overflow: hidden;
    border-radius: 4px;
    background: var(--surface-muted);
  }
  .progress span {
    display: block;
    height: 100%;
    border-radius: inherit;
    background: var(--teal);
  }
  .info-hint {
    margin: 12px 0 0;
    font-size: 12px;
  }

  .activity-feed {
    display: flex;
    flex-direction: column;
    gap: 14px;
  }
  .activity-item {
    display: flex;
    align-items: center;
    gap: 12px;
    padding-bottom: 12px;
    border-bottom: 1px solid var(--line-light);
  }
  .activity-item:last-child { border-bottom: 0; padding-bottom: 0; }
  .activity-icon-pill {
    width: 26px;
    height: 26px;
    display: grid;
    place-items: center;
    border-radius: var(--radius-sm);
  }
  .activity-icon-pill.emerald { background: var(--teal-bg); color: var(--teal); }
  .activity-icon-pill.amber { background: var(--warning-bg); color: var(--warning); }
  .activity-item strong { display: block; font-size: 13px; color: var(--ink); }
  .activity-item small { display: block; font-size: 12px; }

  /* --------------------------------------------------------------------------
     SERVER DETAIL VIEW
     -------------------------------------------------------------------------- */
  .heading-row {
    display: flex;
    align-items: center;
    gap: 12px;
  }
  .server-heading-actions { display: flex; align-items: center; gap: 10px; }
  .server-meta-bar {
    display: flex;
    flex-wrap: wrap;
    gap: 18px 28px;
    padding: 16px 20px;
    margin-bottom: 24px;
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
    background: var(--surface);
    box-shadow: var(--shadow-sm);
  }
  .meta-item { display: flex; flex-direction: column; gap: 3px; }
  .meta-label { font-size: 11px; font-weight: 600; text-transform: none; letter-spacing: 0.04em; color: var(--muted); }
  .meta-value { font-size: 13px; font-weight: 500; color: var(--ink); }

  .tabs {
    display: flex;
    gap: 6px;
    padding-bottom: 1px;
    margin-bottom: 24px;
    border-bottom: 1px solid var(--line);
  }
  .tabs button {
    padding: 9px 16px;
    border: 0;
    border-bottom: 2px solid transparent;
    background: transparent;
    color: var(--muted);
    font-size: 14px;
    font-weight: 500;
    transition: all 0.15s ease;
  }
  .tabs button:hover { color: var(--ink); }
  .tabs button.active {
    border-bottom-color: var(--teal);
    color: var(--ink);
    font-weight: 600;
  }

  .network-rates { display: flex; flex-wrap: wrap; gap: 32px; margin-bottom: 20px; }
  .network-rates > div { display: grid; gap: 6px; }
  .network-rates strong { font-size: 24px; }

  .metric-grid {
    display: grid;
    grid-template-columns: repeat(3, 1fr);
    gap: 16px;
    margin-bottom: 20px;
  }
  .metric-card {
    padding: 18px;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
  }
  .card-top {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
  }
  .metric-title { font-size: 13px; font-weight: 600; color: var(--muted); }
  .metric-big { font-size: 24px; font-weight: 700; letter-spacing: -0.02em; }
  .sparkline { height: 48px; margin: 12px 0 8px; }
  .sparkline-unavailable {
    display: grid;
    place-items: center;
    height: 48px;
    margin: 12px 0 8px;
    border-radius: var(--radius-sm);
    background: var(--surface-muted);
    color: var(--muted);
    font-size: 12px;
  }
  .metric-footer {
    display: flex;
    justify-content: space-between;
    font-size: 12px;
    color: var(--muted);
  }

  .legend {
    display: flex;
    align-items: center;
    gap: 16px;
    margin-bottom: 14px;
    font-size: 12px;
    color: var(--muted);
  }
  .legend span { display: inline-flex; align-items: center; gap: 6px; }
  .legend-dot { width: 8px; height: 8px; border-radius: 50%; }
  .legend-dot.teal { background: var(--teal); }
  .legend-dot.purple { background: var(--purple); }
  .legend-dot.blue { background: var(--blue); }

  .resource-highlights {
    display: flex;
    gap: 20px;
    margin-top: 16px;
    padding-top: 14px;
    border-top: 1px solid var(--line);
    font-size: 13px;
    color: var(--muted);
  }
  .resource-highlights strong { color: var(--ink); }

  .traffic-details {
    display: grid;
    grid-template-columns: repeat(4, 1fr);
    gap: 16px;
    margin-top: 24px;
  }
  .detail-cell span { display: block; font-size: 12px; color: var(--muted); }
  .detail-cell strong { display: block; margin-top: 4px; font-size: 13px; color: var(--ink); }

  /* --------------------------------------------------------------------------
     LOG VIEWER (TERMINAL AESTHETIC)
     -------------------------------------------------------------------------- */
  .terminal-log-viewer {
    border-radius: var(--radius-md);
    background: #090a0f;
    border: 1px solid rgba(255, 255, 255, 0.08);
    overflow: hidden;
    padding: 12px 16px;
  }
  .log-list { display: flex; flex-direction: column; gap: 4px; }
  .log-entry {
    display: grid;
    grid-template-columns: 80px 60px 1fr;
    gap: 12px;
    padding: 6px 0;
    border-bottom: 1px solid rgba(255, 255, 255, 0.04);
    font-size: 12px;
    line-height: 1.4;
  }
  .log-entry:last-child { border-bottom: 0; }
  .log-entry time { color: #64748b; }
  .log-level-badge {
    font-weight: 700;
    font-size: 11px;
    border-radius: 4px;
    padding: 1px 4px;
    width: fit-content;
  }
  .log-level-badge.info { color: #10b981; }
  .log-level-badge.warn { color: #f59e0b; }
  .log-level-badge.error { color: #ef4444; }
  .log-text { color: #e2e8f0; word-break: break-all; }
  .log-text small { display: block; margin-top: 2px; font-size: 11px; }

  /* --------------------------------------------------------------------------
     PACKAGE CARDS & MODULES
     -------------------------------------------------------------------------- */
  .module-grid {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(300px, 1fr));
    gap: 18px;
  }
  .module-card { display: flex; flex-direction: column; }
  .module-top {
    display: flex;
    align-items: center;
    justify-content: space-between;
    margin-bottom: 10px;
  }
  .module-badge {
    padding: 2px 8px;
    border-radius: var(--radius-sm);
    background: var(--surface-muted);
    font-size: 11px;
    font-weight: 600;
    text-transform: none;
    color: var(--teal);
  }
  .release-tag { font-size: 12px; color: var(--muted); }
  .module-desc { font-size: 13px; line-height: 1.5; margin: 4px 0 16px; flex: 1; }
  .module-meta { font-size: 12px; margin-bottom: 16px; color: var(--muted); }
  .radio-group { display: flex; gap: 10px; margin-bottom: 10px; }
  .radio-pill { font-size: 12px; color: var(--muted); cursor: pointer; }
  .source-input {
    width: 100%;
    padding: 8px 10px;
    border: 1px solid var(--line);
    border-radius: var(--radius-sm);
    background: var(--surface);
    color: var(--ink);
    font-size: 12px;
    margin-bottom: 12px;
  }

  /* --------------------------------------------------------------------------
     FORMS & INPUTS
     -------------------------------------------------------------------------- */
  .active-install-banner {
    display: flex;
    justify-content: space-between;
    align-items: center;
    background: rgba(6, 182, 212, 0.08);
    border: 1px solid rgba(6, 182, 212, 0.25);
    border-radius: var(--radius-md);
    padding: 10px 16px;
    margin-bottom: 16px;
    font-size: 13px;
    color: var(--ink);
    gap: 12px;
  }
  .active-install-banner .banner-left {
    display: flex;
    align-items: center;
    gap: 10px;
  }
  .pulse-dot {
    width: 9px;
    height: 9px;
    border-radius: 50%;
    background: #06b6d4;
    box-shadow: 0 0 0 0 rgba(6, 182, 212, 0.7);
    animation: none;
  }
  @keyframes banner-pulse {
    0% { transform: scale(0.95); box-shadow: 0 0 0 0 rgba(6, 182, 212, 0.7); }
    70% { transform: scale(1); box-shadow: 0 0 0 6px rgba(6, 182, 212, 0); }
    100% { transform: scale(0.95); box-shadow: 0 0 0 0 rgba(6, 182, 212, 0); }
  }

  .form-grid {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 18px;
    margin: 20px 0;
  }
  .form-grid label {
    display: flex;
    flex-direction: column;
    gap: 7px;
    font-size: 13px;
    font-weight: 500;
    color: var(--ink);
  }
  .form-grid label:first-child { grid-column: 1 / -1; }
  .form-grid input, .form-grid textarea, .form-grid select {
    width: 100%;
    padding: 10px 12px;
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
    background: var(--surface);
    color: var(--ink);
    font-size: 13px;
    transition: border 0.15s ease;
  }
  .form-grid textarea { resize: vertical; }
  .form-grid input:focus, .form-grid textarea:focus, .form-grid select:focus {
    border-color: var(--teal);
  }
  .form-grid small { color: var(--muted); font-size: 11px; }
  .setup-actions {
    grid-column: 1 / -1;
    display: flex;
    align-items: center;
    justify-content: flex-end;
    gap: 12px;
    padding-top: 20px;
    border-top: 1px solid var(--line);
  }
  .form-error {
    margin: 8px 0 0;
    color: var(--danger);
    font-size: 12px;
  }

  /* --------------------------------------------------------------------------
     ONBOARDING & LOGIN SCREEN
     -------------------------------------------------------------------------- */
  .login-screen {
    position: fixed;
    inset: 0;
    z-index: 1000;
    display: grid;
    place-items: center;
    padding: 24px;
    background: rgba(9, 10, 15, 0.8);
    backdrop-filter: blur(12px);
  }
  .login-card {
    width: min(420px, 100%);
    padding: 36px 32px;
    border: 1px solid var(--line);
    border-radius: var(--radius-xl);
    background: var(--surface);
    box-shadow: var(--shadow-lg);
  }
  .login-card .brand-mark.large {
    width: 48px;
    height: 48px;
    margin: 0 auto 16px;
    font-size: 22px;
  }
  .login-card h2 { text-align: center; margin-bottom: 6px; }
  .login-card .center { text-align: center; }
  .auth-form {
    display: flex;
    flex-direction: column;
    gap: 16px;
    margin-top: 24px;
  }
  .auth-form label { display: flex; flex-direction: column; gap: 6px; font-size: 13px; color: var(--ink); }
  .auth-form input {
    padding: 10px 12px;
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
    background: var(--surface-muted);
    color: var(--ink);
  }

  .onboarding-card {
    min-width: 0;
    padding: 32px;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
  }
  .stepper {
    display: flex;
    gap: 24px;
    margin-bottom: 24px;
  }
  .step {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--muted);
    font-size: 13px;
    font-weight: 500;
  }
  .step span {
    width: 24px;
    height: 24px;
    display: grid;
    place-items: center;
    border: 1px solid var(--line);
    border-radius: 50%;
    font-size: 11px;
  }
  .step.current { color: var(--ink); font-weight: 700; }
  .step.current span, .step.done span {
    background: var(--teal);
    border-color: var(--teal);
    color: #fff;
  }
  .choice-row { display: flex; gap: 12px; margin: 20px 0; }
  .choice-row label {
    padding: 10px 14px;
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
    font-size: 13px;
    color: var(--muted);
    cursor: pointer;
  }
  .choice-row label.chosen {
    border-color: var(--teal);
    background: var(--teal-bg);
    color: var(--ink);
  }
  .review-box {
    display: grid;
    grid-template-columns: 1fr;
    gap: 8px;
    padding: 16px;
    border-radius: var(--radius-md);
    background: var(--surface-muted);
    font-size: 13px;
    margin: 20px 0;
  }

  /* --------------------------------------------------------------------------
     STATE PANELS & EMPTY/ERROR STATES
     -------------------------------------------------------------------------- */
  .state-panel {
    display: grid;
    justify-items: center;
    gap: 12px;
    padding: 80px 24px;
    border: 1px dashed var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    text-align: center;
  }
  .state-icon {
    width: 52px;
    height: 52px;
    display: grid;
    place-items: center;
    border-radius: 50%;
    background: var(--surface-muted);
    color: var(--muted);
  }
  .state-icon.positive { color: var(--teal); background: var(--teal-bg); }
  .error-state .state-icon { color: var(--danger); background: var(--danger-bg); }
  .loading-spinner {
    width: 32px;
    height: 32px;
    border: 2px solid var(--line);
    border-top-color: var(--teal);
    border-radius: 50%;
    animation: spin 700ms linear infinite;
  }
  @keyframes spin { to { transform: rotate(360deg); } }

  .unavailable-panel {
    display: grid;
    gap: 6px;
    padding: 24px 18px;
    border: 1px dashed var(--line);
    border-radius: var(--radius-md);
    background: var(--surface-muted);
    color: var(--muted);
    font-size: 13px;
  }
  .unavailable-panel strong { color: var(--ink); }
  .unavailable-panel.large { padding: 48px 24px; text-align: center; }

  /* --------------------------------------------------------------------------
     SETTINGS & HUB ADMINISTRATION
     -------------------------------------------------------------------------- */
  .settings-grid {
    display: flex;
    flex-direction: column;
    gap: 24px;
    max-width: 720px;
  }
  .settings-layout { display: grid; grid-template-columns: 200px minmax(0, 850px); gap: 32px; align-items: start; }
  .settings-nav { display: grid; gap: 4px; border-right: 1px solid var(--line); padding-right: 16px; }
  .settings-nav button { text-align: left; padding: 12px; border: 0; background: transparent; color: var(--muted); cursor: pointer; font: inherit; border-radius: 4px; }
  .settings-nav button.chosen { background: var(--surface-muted); color: var(--ink); font-weight: 600; box-shadow: inset 3px 0 var(--accent); }
  .user-search { display: grid; gap: 8px; margin-bottom: 20px; font-size: 13px; }
  .release-list { border-top: 1px solid var(--line); margin: 16px 0 24px; }
  .release-row { display: grid; grid-template-columns: 100px 1fr auto; gap: 16px; padding: 16px 0; border-bottom: 1px solid var(--line); color: var(--ink); text-decoration: none; font-size: 13px; }
  .port-form { display: flex; gap: 16px; align-items: end; }
  .port-form label { display: grid; gap: 8px; }
  .form-wide { grid-column: 1 / -1; }
  @media (max-width: 760px) { .settings-layout { grid-template-columns: 1fr; gap: 20px; } .settings-nav { display: flex; flex-wrap: wrap; border-right: 0; border-bottom: 1px solid var(--line); padding: 0 0 12px; } .release-row { grid-template-columns: 70px 1fr; } .release-row span:last-child { grid-column: 2; } }
  .settings-meta-box {
    display: flex;
    flex-direction: column;
    gap: 12px;
    padding: 16px;
    background: var(--surface-muted);
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
    margin: 16px 0;
  }
  .meta-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    font-size: 13px;
    color: var(--muted);
  }
  .meta-row span:last-child {
    color: var(--ink);
  }
  .settings-actions {
    display: flex;
    justify-content: flex-start;
    gap: 12px;
    margin-top: 16px;
  }
  .link-button {
    padding: 0;
    border: 0;
    background: none;
    color: inherit;
    font: inherit;
    text-decoration: underline;
    cursor: pointer;
  }
  .insecure-login {
    display: flex;
    align-items: center;
    gap: 8px;
    margin: 0;
    color: var(--warning);
    font-size: 12px;
  }

  /* --------------------------------------------------------------------------
     PACKAGES & EXTENSIONS
     -------------------------------------------------------------------------- */
  .package-target {
    margin-bottom: 24px;
    display: flex;
    flex-direction: column;
    gap: 12px;
  }
  .package-target label {
    display: flex;
    align-items: center;
    gap: 12px;
    font-size: 13px;
    font-weight: 500;
  }
  .package-target label span {
    color: var(--muted);
  }
  .package-target select {
    min-width: 260px;
  }
  .info-hint {
    font-size: 12px;
    margin: 0;
  }
  .module-grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 300px), 1fr));
    gap: 20px;
  }
  .module-card {
    display: flex;
    flex-direction: column;
    gap: 12px;
  }
  .module-top {
    display: flex;
    align-items: center;
    justify-content: space-between;
  }
  .module-badge {
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--teal);
    background: var(--teal-bg);
    padding: 2px 8px;
    border-radius: var(--radius-pill);
    border: 1px solid var(--teal-glow);
  }
  .release-tag {
    font-family: var(--font-mono);
    font-size: 11px;
    color: var(--muted);
  }
  .module-card h2 {
    font-size: 16px;
    font-weight: 600;
    margin: 0;
  }
  .module-desc {
    font-size: 13px;
    line-height: 1.5;
    flex: 1;
    margin: 0;
  }
  .module-meta {
    font-size: 12px;
    color: var(--muted);
    padding-top: 8px;
    border-top: 1px solid var(--line);
  }
  .package-source-fields { display: grid; grid-template-columns: 150px minmax(200px, 1fr) 160px; align-items: start; gap: 16px; width: 100%; }
  .package-source-fields label { display: grid; gap: 8px; min-width: 0; }
  .package-source-fields select, .package-source-fields input { min-width: 0; width: 100%; box-sizing: border-box; }
  .package-source-fields details { grid-column: 2 / -1; }
  .package-source-fields summary { cursor: pointer; color: var(--muted); margin-bottom: 12px; }
  .package-source-fields details label { margin-bottom: 12px; }

  .package-source {
    display: flex;
    flex-direction: column;
    gap: 8px;
    margin-top: 4px;
  }
  .radio-group {
    display: flex;
    gap: 12px;
  }
  .radio-pill {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--muted);
    cursor: pointer;
  }
  .source-input {
    width: 100%;
    font-size: 12px;
    padding: 6px 10px;
  }
  .full-width {
    width: 100%;
  }

  /* --------------------------------------------------------------------------
     ALERTS & INCIDENT LIST
     -------------------------------------------------------------------------- */
  .alert-row {
    display: grid;
    grid-template-columns: 100px 1.5fr 1fr auto;
    align-items: center;
    padding: 14px 20px;
    background: var(--surface);
    border-bottom: 1px solid var(--line);
    transition: background var(--transition-fast);
  }
  .alert-row:hover {
    background: var(--surface-hover);
  }
  .alert-row:last-child {
    border-bottom: 0;
  }

  /* --------------------------------------------------------------------------
     FOOTER
     -------------------------------------------------------------------------- */
  footer {
    display: flex;
    justify-content: space-between;
    max-width: 1280px;
    margin: 0 auto;
    padding: 0 32px 32px;
    color: var(--muted);
    font-size: 12px;
  }

  /* --------------------------------------------------------------------------
     RESPONSIVE BREAKPOINTS
     -------------------------------------------------------------------------- */
  @media (max-width: 960px) {
    .app-shell { grid-template-columns: 1fr; }
    .sidebar {
      position: sticky;
      top: 0;
      z-index: 50;
      flex-direction: row;
      align-items: center;
      padding: 10px 20px;
      border-right: 0;
      border-bottom: 1px solid var(--line);
      gap: 16px;
    }
    .brand-lockup { padding: 0; }
    .nav-list { flex-direction: row; overflow-x: auto; flex: 1; }
    .nav-item { padding: 8px 12px; white-space: nowrap; }
    .sidebar-footer { display: none; }
    .summary-grid { grid-template-columns: 1fr 1fr; }
    .table-header, .server-row {
      grid-template-columns: 120px 1.5fr 1fr 40px;
    }
    .col-metric { display: none; }
  }

  @media (max-width: 640px) {
    .page { padding: 20px 16px 32px; }
    .topbar { padding: 0 16px; }
    .summary-grid, .lower-grid, .metric-grid, .form-grid, .package-source-fields { grid-template-columns: 1fr; }
    .package-source-fields details { grid-column: auto; }
    .table-header { display: none; }
    .server-row {
      grid-template-columns: 1fr auto;
      gap: 8px;
    }
    .col-meta { display: none; }
    footer { flex-direction: column; gap: 8px; padding: 0 16px 24px; }
  }
  @media (prefers-reduced-motion: reduce) { :global(*), :global(*::before), :global(*::after) { animation-duration: 0.01ms !important; transition-duration: 0.01ms !important; } }
</style>
