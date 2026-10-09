<script lang="ts">
  import { onMount, tick } from 'svelte';
  import ProcessTable from './ProcessTable.svelte';
  import ChartPreview from './ChartPreview.svelte';
  import Sparkline from './Sparkline.svelte';
  import TrafficChart from './TrafficChart.svelte';
  import CircleChart from './CircleChart.svelte';
  import { networkRates, networkRollupRate, formatNetworkRate } from './network';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import InstallProgress, { type InstallProgressData, type InstallStage } from './InstallProgress.svelte';
  import { ApiError, apiClient, mapWithConcurrency, type Account, type HTTPSStatus, type NodeTransportStatus, type UpdateStatus, type UpdateProgress, type AlertState, type Job, type MetricQuery, type TrafficUsage, type StorageStatus, type StorageNotification, type Module, type ModuleInstallation, type PackageSource, type Server as ApiServer } from './api';
  import type { DisplayState, PreviewChartData, PreviewLogEntry, PreviewServer } from './preview/fixtures';
  import { getSavedTimezone, saveTimezone, getAvailableTimezones, formatTimeInTz, formatDateTimeInTz, formatHeartbeatInTz, formatLogTimeInTz } from './timezone';

  type Theme = 'light' | 'dark';
  type Page = 'overview' | 'monitoring' | 'servers' | 'server' | 'alerts' | 'packages' | 'settings' | 'add-server' | 'install-progress' | 'onboarding';
  type DetailTab = 'resources' | 'network' | 'logs' | 'processes' | 'metrics' | 'traffic';
  type ChartRange = '15m' | '1h' | '24h';
  type PreviewState = 'ready' | 'loading' | 'empty' | 'error';

  // Development builds use the real API by default. Opt into fixtures
  // explicitly so a local preview can never accidentally mask API failures.
  const PREVIEW_MODE = import.meta.env.VITE_PAYESH_PREVIEW === 'true';
  // Plain HTTP to anything but this machine sends the password unencrypted.
  const insecureConnection = typeof window !== 'undefined' && window.location.protocol === 'http:' && !['localhost', '127.0.0.1', '[::1]'].includes(window.location.hostname);
  const totalTrafficBytes = '1526000000000';
  const totalAllowanceBytes = '2500000000000';

  let currentTimezone = getSavedTimezone();
  function changeTimezone(tz: string) {
    currentTimezone = tz;
    saveTimezone(tz);
  }

  let alertsPage = 0;
  let alertsPageSize = 25;
  $: totalAlertPages = Math.max(1, Math.ceil(alerts.length / alertsPageSize));
  $: pagedAlerts = alerts.slice(alertsPage * alertsPageSize, (alertsPage + 1) * alertsPageSize);

  let servers: PreviewServer[] = [];
  let displayServers: PreviewServer[] = [];
  let previewLogEntries: PreviewLogEntry[] = [];
  let activePage: Page = 'overview';
  let mobileNavOpen = false;
  let selectedServerId = '';
  let detailTab: DetailTab = 'resources';
  let chartRange: ChartRange = '15m';
  const trafficDefaultEnd = new Date(Math.floor(Date.now() / 3600000) * 3600000);
  let trafficFrom = new Date(trafficDefaultEnd.getTime() - 86400000).toISOString().slice(0, 16);
  let trafficTo = trafficDefaultEnd.toISOString().slice(0, 16);
  let trafficUsage: TrafficUsage | null = null;
  let trafficUsageBusy = false;
  let trafficUsageError = '';
  let trafficUsageServerId = '';
  let trafficUsageController: AbortController | null = null;
  $: if (selectedServerId !== trafficUsageServerId) {
    trafficUsageController?.abort();
    trafficUsageServerId = selectedServerId;
    trafficUsage = null;
    trafficUsageError = '';
  }

  async function loadTrafficUsage(): Promise<void> {
    trafficUsage = null;
    trafficUsageError = '';
    const from = new Date(trafficFrom + 'Z');
    const to = new Date(trafficTo + 'Z');
    if (!Number.isFinite(from.getTime()) || !Number.isFinite(to.getTime()) || to <= from || to.getTime() - from.getTime() > 90 * 86400000 || from.getTime() % 3600000 || to.getTime() % 3600000 || to.getTime() > Math.floor(Date.now() / 3600000) * 3600000) {
      trafficUsageError = 'Choose completed UTC hours, from before to, spanning at most 90 days.';
      return;
    }
    const id = selectedServer.id;
    const controller = new AbortController();
    trafficUsageController?.abort();
    trafficUsageController = controller;
    trafficUsageBusy = true;
    const timeout = window.setTimeout(() => controller.abort(), 15000);
    try {
      const result = await apiClient.queryTrafficUsage(id, { from: from.toISOString(), to: to.toISOString(), signal: controller.signal });
      if (trafficUsageController === controller && !controller.signal.aborted && selectedServer.id === id) trafficUsage = result;
    } catch (error) {
      if (trafficUsageController === controller && selectedServer.id === id) trafficUsageError = error instanceof ApiError ? error.message : 'Traffic history could not be loaded. Please retry.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      window.clearTimeout(timeout);
      if (trafficUsageController === controller) trafficUsageBusy = false;
    }
  }

  let theme: Theme = initialTheme();
  let previewState: PreviewState = 'loading';
  let previewLoadFailed = false;
  let apiError = '';
  let partialWarning = '';
  let authExpired = false;
  function focusLogin(input: HTMLInputElement) {
    const focus = () => {
      if (input.isConnected && !document.querySelector('dialog:modal')) input.focus();
    };
    void tick().then(focus);
    // Signing out can mount this form before its confirmation closes.
    document.addEventListener('close', focus, true);
    return { destroy: () => document.removeEventListener('close', focus, true) };
  }
  let logState: PreviewState = 'ready';
  let logError = '';
  let logEntries: PreviewLogEntry[] = [];
  let apiAbortController: AbortController | null = null;
  let nodeTransportStatus: NodeTransportStatus | null = null;
  let nodePort = '';
  let nodePortBusy = false;
  let nodePortError = '';
  let nodeTransportLoadError = '';
  let nodeTransportLoading = false;
  let nodePortPoll: ReturnType<typeof setTimeout> | null = null;
  let nodePageCursor = '';
  let nodePageHistory: string[] = [];
  let httpsStatus: HTTPSStatus | null = null;
  let httpsStatusLoading = false;
  const browserHTTPS = typeof window !== 'undefined' && window.location.protocol === 'https:';
  let storageStatus: StorageStatus | null = null;
  $: recoverySnapshotBytes = storageStatus?.recovery_snapshot_bytes ?? 0;
  $: storageUsageBytes = (storageStatus?.database_bytes ?? 0) + recoverySnapshotBytes;
  let databaseLimitGB = 1;
  let samplingSeconds = 15;
  let pressureSamplingSeconds = 60;
  let adaptiveSampling = true;
  let storageNotificationsEnabled = true;
  let storageBusy = false;
  let storageError = '';
  let storageSaved = '';
  let notificationItems: StorageNotification[] = [];
  let notificationError = '';
  let notificationBusy = false;
  $: unreadNotifications = notificationItems.filter(item => !item.read).length;

  async function loadStorageSettings(): Promise<void> {
    storageBusy = true; storageError = ''; storageSaved = '';
    try {
      storageStatus = await apiClient.getStorageSettings();
      databaseLimitGB = storageStatus.settings.max_database_bytes / 1000000000;
      samplingSeconds = storageStatus.settings.sample_seconds;
      pressureSamplingSeconds = storageStatus.settings.pressure_sample_seconds;
      adaptiveSampling = storageStatus.settings.adaptive_sampling;
      storageNotificationsEnabled = storageStatus.settings.notifications_enabled;
    } catch (error) {
      if (error instanceof ApiError && error.authExpired) authExpired = true;
      storageError = error instanceof ApiError ? error.message : 'Storage settings could not be loaded.';
    }
    finally { storageBusy = false; }
  }
  async function saveStorageSettings(): Promise<void> {
    if (!storageStatus) return;
    storageBusy = true; storageError = ''; storageSaved = '';
    try {
      storageStatus = await apiClient.saveStorageSettings({ ...storageStatus.settings, max_database_bytes: Math.round(databaseLimitGB * 1000000000), sample_seconds: samplingSeconds, pressure_sample_seconds: pressureSamplingSeconds, adaptive_sampling: adaptiveSampling, notifications_enabled: storageNotificationsEnabled });
      storageSaved = 'Settings saved. Collection changes take effect on the next sampling cycle; cleanup runs automatically.';
      await loadNotifications();
    } catch (error) {
      if (error instanceof ApiError && error.authExpired) authExpired = true;
      storageError = error instanceof ApiError ? error.message : 'Storage settings could not be saved.';
    }
    finally { storageBusy = false; }
  }
  async function loadNotifications(): Promise<void> {
    if (notificationBusy || PREVIEW_MODE || sessionState !== 'authenticated' || authExpired || document.hidden) return;
    notificationBusy = true; notificationError = '';
    try { notificationItems = (await apiClient.getNotifications()).items; }
    catch (error) {
      if (error instanceof ApiError && error.authExpired) authExpired = true;
      notificationError = error instanceof ApiError ? error.message : 'Notifications could not be loaded.';
    }
    finally { notificationBusy = false; }
  }
  let storageRefreshBusy = false;
  async function refreshStorageUsage(): Promise<void> {
    if (PREVIEW_MODE || sessionState !== 'authenticated' || authExpired || activePage !== 'settings' || settingsSection !== 'storage' || storageBusy || storageRefreshBusy || !storageStatus || document.hidden) return;
    storageRefreshBusy = true;
    try {
      const result = await apiClient.getStorageSettings();
      if (storageStatus && result.settings.revision === storageStatus.settings.revision) storageStatus = { ...storageStatus, database_bytes: result.database_bytes, recovery_snapshot_bytes: result.recovery_snapshot_bytes ?? 0, effective_sample_seconds: result.effective_sample_seconds, settings: { ...storageStatus.settings, pressure_state: result.settings.pressure_state } };
    } catch (error) {
      if (error instanceof ApiError && error.authExpired) authExpired = true;
      // Keep the last measurement for other failures; Refresh reports errors.
    }
    finally { storageRefreshBusy = false; }
  }
  async function markNotificationsRead(): Promise<void> {
    try { await apiClient.markNotificationsRead(); await loadNotifications(); }
    catch (error) {
      if (error instanceof ApiError && error.authExpired) authExpired = true;
      notificationError = error instanceof ApiError ? error.message : 'Notifications could not be marked read.';
    }
  }
  let settingsSection: 'general' | 'users' | 'updates' | 'tls' | 'storage' | 'notifications' = 'general';
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
  let signOutError = '';
  let setupStep = 1;
  let workspaceName = PREVIEW_MODE ? "Kia's workspace" : '';
  let setupSecret = '';
  let ownerPassword = '';
  let ownerUsername = '';
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
  const RELEASES_PER_PAGE = 5;
  let releasePage = 0;
  let updateApplyBusy = false;
  let updateApplyError = '';
  let updateProgress: UpdateProgress | null = null;
  let updatePollTimer: number | undefined;
  let dismissedUpdate = '';
  try { dismissedUpdate = window.localStorage.getItem('payesh-dismissed-update') ?? ''; } catch { /* non-critical */ }
  $: installedAhead = !!updateStatus && (updateStatus.installed_ahead ?? isNewerVersion(updateStatus.current, updateStatus.latest));
  $: releaseList = updateStatus?.releases ?? [];
  $: releasePageCount = Math.max(1, Math.ceil(releaseList.length / RELEASES_PER_PAGE));
  $: if (releasePage > releasePageCount - 1) releasePage = releasePageCount - 1;
  $: visibleReleases = releaseList.slice(releasePage * RELEASES_PER_PAGE, (releasePage + 1) * RELEASES_PER_PAGE);
  $: webUpdate = updateProgress?.web_update ?? updateStatus?.web_update ?? null;
  $: webUpdateActive = webUpdate?.state === 'queued' || webUpdate?.state === 'running';
  $: showUpdateBanner = !PREVIEW_MODE && sessionState === 'authenticated' && !!updateStatus?.update_available && dismissedUpdate !== updateStatus.latest && !webUpdateActive;
  let installHost = '';
  let installPort = '22';
  let installUser = 'root';
  let installAuthMethod: 'password' | 'key' = 'password';
  let installPassword = '';
  let installKey = '';
  let installKeyPassphrase = '';
  let installFingerprint = '';
  let installBusy = false;
  function chooseInstallAuth(method: 'password' | 'key'): void {
    if (installAuthMethod === method) return;
    installAuthMethod = method;
    installPassword = '';
    installKey = '';
    installKeyPassphrase = '';
  }
  let nodeControlPort = '22';
  let nodeControlUser = 'root';
  let nodeControlPassword = '';
  let nodeControlKey = '';
  let nodeControlFingerprint = '';
  let nodeControlBusy = false;
  let nodeControlError = '';
  let labelDraft = '';
  let labelBusy = false;
  let newServerName = '';
  let createServerBusy = false;
  let joinName = '';
  let joinBusy = false;
  let joinError = '';
  let joinCommand = '';
  let joinExpiresAt = '';
  let joinCopied = false;
  let deleteServerBusy = false;
  let modules: Module[] = [];
  let modulesState: PreviewState = 'loading';
  let modulesError = '';
  let packageServerId = '';
  let moduleInstallations: ModuleInstallation[] = [];
  let packageBusy = '';
  let packageDialog: Module | null = null;
  let packageStep: 'target' | 'source' = 'target';
  let packageStatusBusy = false;
  let packageStatusFailed = false;
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
      description: 'Are you sure you want to sign out of Payesh? You will need your username and password to log in again.',
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
  $: connectedCount = displayServers.filter((server) => server.connectionState === 'connected').length;
  $: fleetDownload = sumNetworkRate(displayServers, 'download');
  $: fleetUpload = sumNetworkRate(displayServers, 'upload');
  $: firingAlertCount = alerts.filter((alert) => alert.state === 'firing').length;
  $: overviewTrafficBytes = PREVIEW_MODE ? totalTrafficBytes : displayServers.reduce((total, server) => { try { return (BigInt(total) + BigInt(server.traffic.countedBytes)).toString(); } catch { return total; } }, '0');
  $: overviewAllowanceBytes = PREVIEW_MODE ? totalAllowanceBytes : displayServers.reduce((total, server) => { try { return (BigInt(total) + BigInt(server.traffic.allowanceBytes)).toString(); } catch { return total; } }, '0');
  $: chartData = selectedServer?.metricHistory?.ranges[chartRange] ?? null;
  $: availableTabs = selectedServer ? (['resources', 'network', 'logs', 'processes'] as DetailTab[]).filter((tab) => hasCapability(selectedServer, tab)) : [];
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
    if (myAccount?.permission === 'read') return;
    newServerName = '';
    installHost = '';
    installPort = '22';
    installUser = 'root';
    installAuthMethod = 'password';
    installPassword = '';
    installKey = '';
    installKeyPassphrase = '';
    installFingerprint = '';
    jobError = '';
    joinName = ''; joinError = ''; joinCommand = ''; joinExpiresAt = ''; joinCopied = false;
    if (!activeInstall || !['connecting', 'connected', 'preflight', 'installing', 'enrolling', 'verifying'].includes(activeInstall.currentStage)) {
      saveActiveInstall(null);
    }
    navigate('add-server');
  }

  function navigate(page: Page, serverId = selectedServerId) {
    const fromMobileMenu = mobileNavOpen;
    mobileNavOpen = false;
    activePage = page;
    if (fromMobileMenu) void tick().then(() => document.querySelector<HTMLElement>('main h1')?.focus());
    selectedServerId = serverId;
    saveUiState();
    if (typeof window !== 'undefined') {
      window.history.pushState({ activePage, selectedServerId, detailTab }, '', pageLocation(activePage, selectedServerId));
    }
    if (page === 'server' && !PREVIEW_MODE) void refreshSelectedServer();
    if (page === 'packages' && !PREVIEW_MODE) void loadModules();
    if (page === 'alerts' && !PREVIEW_MODE) void loadAlerts();
    if (page === 'settings' && !PREVIEW_MODE) { void loadHTTPS(); void loadAccounts(); void checkLatestUpdate(); if (settingsSection === 'storage') void loadStorageSettings(); void loadNotifications(); }
  }

  async function checkLatestUpdate(): Promise<void> {
    if (updateCheckBusy || PREVIEW_MODE) return;
    updateCheckBusy = true; updateCheckError = '';
    try { updateStatus = await apiClient.checkLatestUpdate(); }
    catch (error) { updateCheckError = error instanceof Error ? error.message : 'Could not check the latest version.'; }
    finally { updateCheckBusy = false; }
  }

  function versionParts(value: string): number[] {
    return value.replace(/^v/, '').split(/[-+]/)[0].split('.').map((part) => Number.parseInt(part, 10) || 0);
  }

  function isNewerVersion(candidate: string, installed: string): boolean {
    if (!/^v?\d+\.\d+\.\d+/.test(installed) || !/^v?\d+\.\d+\.\d+/.test(candidate)) return false;
    const a = versionParts(candidate), b = versionParts(installed);
    for (let i = 0; i < Math.max(a.length, b.length); i++) {
      if ((a[i] ?? 0) !== (b[i] ?? 0)) return (a[i] ?? 0) > (b[i] ?? 0);
    }
    return false;
  }

  function dismissUpdateBanner(): void {
    if (!updateStatus) return;
    dismissedUpdate = updateStatus.latest;
    try { window.localStorage.setItem('payesh-dismissed-update', dismissedUpdate); } catch { /* non-critical */ }
  }

  function openUpdateSettings(): void {
    settingsSection = 'updates';
    navigate('settings');
  }

  async function startWebUpdate(target: string): Promise<void> {
    if (updateApplyBusy || webUpdateActive) return;
    if (!window.confirm(`Update Payesh to v${target}? The dashboard will restart and be unavailable for a short time.`)) return;
    updateApplyBusy = true; updateApplyError = '';
    try {
      const state = await apiClient.applyWebUpdate(target);
      updateProgress = { current: updateStatus?.current ?? '', web_update_supported: true, web_update: state };
      pollUpdateProgress(target);
    } catch (error) { updateApplyError = error instanceof Error ? error.message : 'Could not start the update.'; }
    finally { updateApplyBusy = false; }
  }

  function pollUpdateProgress(target: string): void {
    window.clearTimeout(updatePollTimer);
    const deadline = Date.now() + 30 * 60_000;
    const tick = async () => {
      if (sessionState !== 'authenticated' || authExpired) return;
      if (Date.now() >= deadline) { updateApplyError = 'Update status timed out. Check the server before trying again.'; return; }
      try {
        const progress = await apiClient.getUpdateProgress();
        updateProgress = progress;
        const state = progress.web_update;
        if (progress.current === target) {
          showNotice(`Updated to v${target}. Reloading…`);
          window.setTimeout(() => window.location.reload(), 1500);
          return;
        }
        if (state?.state === 'failed') { updateApplyError = state.message || 'The update failed.'; return; }
      } catch {
        // The dashboard restarts during an update; keep polling until it returns.
      }
      updatePollTimer = window.setTimeout(tick, 3000);
    };
    updatePollTimer = window.setTimeout(tick, 3000);
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

  async function loadNodeTransport(): Promise<void> {
    if (nodeTransportLoading) return;
    if (nodePortPoll) { clearTimeout(nodePortPoll); nodePortPoll = null; }
    nodeTransportLoading = true;
    let keepPolling = true;
    try {
      const previouslyObservedPort = nodeTransportStatus?.port;
      nodeTransportStatus = await apiClient.getNodeTransportSettings({ cursor: nodePageCursor || undefined });
      nodeTransportLoadError = '';
      if (!nodePort || nodePort === previouslyObservedPort) nodePort = nodeTransportStatus.port;
    } catch (error) {
      if (error instanceof ApiError && error.status === 400 && nodePageCursor) {
        nodePageCursor = ''; nodePageHistory = [];
        nodeTransportLoadError = 'The node list changed. Reloading the first page.';
      } else if (error instanceof ApiError && error.status === 404) {
        nodeTransportLoadError = 'This hub version does not support separate node port settings. Update the hub to use this feature.';
        keepPolling = false;
      } else {
        nodeTransportLoadError = error instanceof Error ? error.message : 'Could not read node transport settings.';
        if (error instanceof ApiError && error.authExpired) { authExpired = true; keepPolling = false; }
      }
    } finally {
      nodeTransportLoading = false;
      if (keepPolling && activePage === 'settings' && settingsSection === 'tls') {
        nodePortPoll = setTimeout(() => void loadNodeTransport(), nodeTransportLoadError ? 10000 : 5000);
      }
    }
  }

  async function saveNodePort(): Promise<void> {
    if (nodePortBusy || !nodeTransportStatus || Number(nodePort) === Number(nodeTransportStatus.port)) return;
    nodePortBusy = true; nodePortError = '';
    try {
      nodeTransportStatus = await apiClient.setNodeTransportPort(Number(nodePort));
      nodePort = nodeTransportStatus.port;
      nodePageCursor = ''; nodePageHistory = [];
      showNotice('Node port opened. Nodes will verify the new connection and migrate automatically.');
    } catch (error) { nodePortError = error instanceof Error ? error.message : 'Could not change node port.'; }
    finally { nodePortBusy = false; }
  }

  async function changeNodePage(next: boolean): Promise<void> {
    if (nodeTransportLoading || nodePortBusy) return;
    if (next && nodeTransportStatus?.next_cursor) {
      nodePageHistory = [...nodePageHistory, nodePageCursor];
      nodePageCursor = nodeTransportStatus.next_cursor;
    } else if (!next && nodePageHistory.length) {
      nodePageCursor = nodePageHistory.at(-1)!;
      nodePageHistory = nodePageHistory.slice(0, -1);
    } else return;
    await loadNodeTransport();
  }

  function retireNodePorts(): void {
    openConfirmModal({ title: 'Retire previous node endpoints?', description: 'All enrolled nodes have connected to the current node port. Previous node ports and node access through the dashboard will be disabled.', tone: 'warning', icon: 'alert-triangle', confirmText: 'Retire endpoints', cancelText: 'Cancel', action: async () => {
      nodePortBusy = true; nodePortError = '';
      try { nodeTransportStatus = await apiClient.retireNodeTransportPorts(); nodePageCursor = ''; nodePageHistory = []; showNotice('Previous node endpoints retired.'); }
      catch (error) { nodePortError = error instanceof Error ? error.message : 'Could not retire previous endpoints.'; }
      finally { nodePortBusy = false; }
    } });
  }

  async function loadHTTPS(): Promise<void> {
    void loadNodeTransport();
    if (httpsPoll) { clearTimeout(httpsPoll); httpsPoll = null; }
    httpsStatusLoading = true; httpsError = '';
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
    } finally { httpsStatusLoading = false; }
  }

  async function saveHTTPS(): Promise<void> {
    if (PREVIEW_MODE || httpsBusy) return;
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
      description: 'The certificate is deleted and the dashboard goes back to plain HTTP. Logins will no longer be encrypted, and nodes using this certificate will lose their TLS connection.',
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

    let hasCachedCatalog = false;
    if (!force && typeof window !== 'undefined') {
      try {
        const cached = JSON.parse(window.localStorage.getItem(cacheKey) ?? 'null') as { savedAt?: number; items?: Module[] } | null;
        if (cached?.savedAt && Array.isArray(cached.items)) {
          modules = cached.items; modulesState = modules.length ? 'ready' : 'empty';
          hasCachedCatalog = true;


        }
      } catch { /* corrupt cache is ignored and replaced by a fresh response */ }
    }
    if (!hasCachedCatalog) modulesState = 'loading';
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 10000);
    try {
      const page = await apiClient.listModules({ signal: controller.signal, refresh: force });
      modules = page.items;
      modulesError = page.warning ?? '';
      modulesState = modules.length ? 'ready' : 'empty';
      if (typeof window !== 'undefined') window.localStorage.setItem(cacheKey, JSON.stringify({ savedAt: Date.now(), items: modules }));

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
    packageStatusBusy = true; packageStatusFailed = false;
    moduleInstallations = [];
    const target = packageServerId;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 10000);
    try {
      const result = await apiClient.listServerModules(target, { signal: controller.signal });
      if (target === packageServerId) moduleInstallations = result.items;
    } catch (error) {
      if (target !== packageServerId) return;
      packageStatusFailed = true;
      packageError = error instanceof DOMException && error.name === 'AbortError' ? 'Package status took too long to load. The cached catalog is still available.' : error instanceof Error ? error.message : 'Unable to load package state.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      window.clearTimeout(timeout);
      if (target === packageServerId) packageStatusBusy = false;
    }
  }

  function openPackageDialog(module: Module) {
    packageDialog = module; packageStep = 'target'; packageError = '';
    packageSource = 'github'; packageLocation = ''; packageVersion = ''; packageManifest = ''; packageSignature = '';
    packageServerId = ''; moduleInstallations = [];
  }

  function moduleState(moduleId: string): ModuleInstallation | undefined {
    return moduleInstallations.find((item) => item.module_id === moduleId);
  }

  let packageSource: PackageSource['kind'] = 'github';
  let packageLocation = '';
  let packageVersion = '';
  let packageManifest = '';
  let packageSignature = '';

  async function installPackage(module: Module): Promise<void> {
    if (!packageServerId || packageBusy) return;
    packageBusy = `${module.id}:install`; packageError = '';
    try {
      const source: PackageSource = { kind: packageSource, location: packageLocation.trim() || (packageSource === 'github' ? module.repository || 'Real-kia/payesh' : '') };
      if (packageSource === 'github') source.version = packageVersion.trim() || module.release || 'latest';
      if (packageSource !== 'github' && packageManifest.trim()) source.manifest_location = packageManifest.trim();
      if (packageSource !== 'github' && packageSignature.trim()) source.signature_location = packageSignature.trim();
      const result = await apiClient.installModuleSource(packageServerId, module.id, source, moduleState(module.id)?.revision ?? '0', operationKey('package-install'));
      moduleInstallations = [...moduleInstallations.filter((item) => item.module_id !== module.id), result];
      showNotice(`${module.name} installed.`);
      packageDialog = null;
    } catch (error) {
      const message = error instanceof Error ? error.message : 'Package installation failed.';
      await loadServerModules();
      packageError = message;
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally { packageBusy = ''; }
  }

  function packageGitHubURL(module: Module): string {
    const arch = servers.find((server) => server.id === packageServerId)?.architecture;
    const repo = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(packageLocation.trim()) ? packageLocation.trim() : module.repository || 'Real-kia/payesh';
    const release = packageVersion.trim() || module.release || 'latest';
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
      activeInstall.simulatedProgress = job.progress;
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

    if (job.state === 'running' && p > activeInstall.simulatedProgress) {
      activeInstall.logs = [...activeInstall.logs, { time: nowStr, text: `Hub reported installation progress: ${p}%`, level: 'info' }];
    }
    activeInstall.simulatedProgress = p;
    activeInstall.currentStage = targetStage;
    saveActiveInstall(activeInstall);
  }

  async function createJoinCommand(): Promise<void> {
    const name = joinName.trim();
    if (!name || joinBusy) return;
    joinBusy = true; joinError = ''; joinCommand = ''; joinExpiresAt = ''; joinCopied = false;
    let createdServerId: string | null = null;
    let createdRevision = '0';
    try {
      const transport = await apiClient.getNodeTransportSettings({ limit: 1 });
      if (!transport.url) {
        joinError = 'Enable HTTPS for this hub under Settings → SSL / TLS first. Nodes need its public address to connect.';
        return;
      }
      const created = await apiClient.createServer({ name, address: 'joins by command' });
      createdServerId = created.id;
      createdRevision = created.configuration_revision ?? '0';
      const issued = await apiClient.createEnrollmentToken(created.id, operationKey('join-token'));
      const job = await apiClient.enrollServer(created.id, { token: issued.token, idempotency_key: operationKey('join-enroll') });
      servers = [...servers, emptyApiServer(created)];
      joinExpiresAt = issued.expires_at;
      joinCommand = `curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh -s -- --release-mode preview --role node --join-url ${transport.url} --join-job ${job.id} --join-token ${issued.token}${transport.ca_sha256 ? ` --join-ca-sha256 ${transport.ca_sha256}` : ''}`;
    } catch (error) {
      if (createdServerId) {
        try { await apiClient.deleteServer(createdServerId, createdRevision); } catch { /* the pending server can be removed from the server list */ }
      }
      joinError = error instanceof Error ? error.message : 'Unable to create the join command.';
    } finally {
      joinBusy = false;
    }
  }

  async function copyJoinCommand(): Promise<void> {
    try {
      await navigator.clipboard.writeText(joinCommand);
      joinCopied = true;
    } catch {
      joinCopied = false;
      joinError = 'Copy failed. Select the command and copy it manually.';
    }
  }

  async function createPendingServer(): Promise<void> {
    if (!newServerName.trim() || !installHost.trim() || !installUser.trim() || (installAuthMethod === 'password' ? !installPassword : !installKey) || createServerBusy) return;
    createServerBusy = true; jobError = '';
    const host = installHost.trim();
    const port = Number(installPort);
    const user = installUser.trim();
    const serverName = newServerName.trim();
    let createdServerId: string | null = null;
    try {
      const created = await apiClient.createServer({ name: serverName, address: host });
      createdServerId = created.id;
      const server = emptyApiServer(created);
      servers = [...servers, server];
      selectedServerId = server.id;
      labelDraft = server.name;
      const job = await apiClient.enqueueInstall({
        server_id: server.id,
        host,
        port,
        user,
        ...(installAuthMethod === 'password' ? { password: installPassword } : { private_key: installKey, ...(installKeyPassphrase ? { private_key_passphrase: installKeyPassphrase } : {}) }),
        expected_host_key_fingerprint: installFingerprint.trim() || undefined,
        role: 'node',
        start: true,
        idempotency_key: operationKey('install')
      });
      servers = servers.map((entry) => entry.id === server.id ? { ...entry, displayState: 'installing' as DisplayState } : entry);
      installPassword = ''; installKey = ''; installKeyPassphrase = '';

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
        simulatedProgress: job.progress,
        logs: [
          { time: timeStr, text: `Installation job queued for '${serverName}' · Job ${job.id}`, level: 'info' }
        ]
      });

      recordJob(job);
      showNotice(`Connecting to ${serverName} over SSH to install Payesh...`);
      navigate('install-progress', server.id);
    } catch (error) {
      if (createdServerId) navigate('server', createdServerId);
      jobError = error instanceof ApiError && error.code === 'install_executor_unavailable'
        ? 'Server added, but installation did not start. SSH installation is not configured on this hub. Ask the hub administrator to enable it, then retry using Install Payesh.'
        : error instanceof Error ? error.message : 'Unable to create server.';
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
    signOutError = '';
    try {
      await apiClient.logout();
    } catch (error) {
      if (!(error instanceof ApiError && error.authExpired)) {
        signOutError = 'Unable to sign out. Your session may still be active. Try again.';
        return;
      }
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
    if (!selectedServer || !installHost.trim() || !installUser.trim() || (installAuthMethod === 'password' ? !installPassword : !installKey) || installBusy) return;
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
        ...(installAuthMethod === 'password' ? { password: installPassword } : { private_key: installKey, ...(installKeyPassphrase ? { private_key_passphrase: installKeyPassphrase } : {}) }),
        expected_host_key_fingerprint: installFingerprint.trim() || undefined,
        role: 'node',
        start: true,
        idempotency_key: operationKey('install')
      });
      installPassword = ''; installKey = ''; installKeyPassphrase = '';

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
        simulatedProgress: job.progress,
        logs: [
          { time: timeStr, text: `Installation job queued for '${serverName}' · Job ${job.id}`, level: 'info' }
        ]
      });

      recordJob(job);
      showNotice(`Connecting to ${serverName} over SSH to install Payesh...`);
    } catch (error) {
      jobError = error instanceof ApiError && error.code === 'install_executor_unavailable'
        ? 'Installation did not start. SSH installation is not configured on this hub. Ask the hub administrator to enable it, then retry using Install Payesh.'
        : error instanceof Error ? error.message : 'Unable to queue the installation.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      installBusy = false;
    }
  }

  async function controlSelectedNode(action: 'restart' | 'disable' | 'enable'): Promise<void> {
    if (!selectedServer || selectedServer.role !== 'node' || nodeControlBusy) return;
    nodeControlBusy = true; nodeControlError = '';
    try {
      await apiClient.controlNode(selectedServer.id, {
        action, port: Number(nodeControlPort), user: nodeControlUser.trim(),
        ...(nodeControlPassword ? { password: nodeControlPassword } : {}),
        ...(nodeControlKey ? { private_key: nodeControlKey } : {}),
        expected_host_key_fingerprint: nodeControlFingerprint.trim()
      });
      nodeControlPassword = ''; nodeControlKey = '';
      showNotice(`Node service ${action} command completed on ${selectedServer.name}.`);
      await refreshSelectedServer();
    } catch (error) {
      nodeControlError = error instanceof Error ? error.message : 'Node service action failed.';
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      nodeControlBusy = false;
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
    if (capability === 'resources' || capability === 'network' || capability === 'metrics' || capability === 'traffic') return true;
    return server.capabilities?.includes(capability) ?? false;
  }

  function tabLabel(tab: DetailTab): string {
    if (tab === 'resources' || tab === 'metrics') return 'Resources';
    if (tab === 'network' || tab === 'traffic') return 'Network';
    if (tab === 'logs') return 'Logs';
    if (tab === 'processes') return 'Processes';
    return String(tab);
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
      if (activeInstall.currentStage === 'failed' && server.connection_state !== 'connected') return 'failed';
      if (activeInstall.currentStage !== 'succeeded' && activeInstall.currentStage !== 'failed') return 'installing';
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

  function sumNetworkRate(fleet: PreviewServer[], direction: 'download' | 'upload'): number | null {
    const rates = fleet.filter((server) => server.connectionState === 'connected').map((server) => currentNetworkRate(server, direction));
    return rates.length && rates.every((rate) => rate !== null) ? rates.reduce<number>((sum, rate) => sum + (rate ?? 0), 0) : null;
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
    if (liveRefreshBusy || PREVIEW_MODE || sessionState !== 'authenticated' || authExpired || !['overview', 'monitoring', 'servers'].includes(activePage) || document.hidden) return;
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
      if (['overview', 'monitoring', 'servers'].includes(activePage) && !controller.signal.aborted) servers = refreshed;
    } catch (error) { if (error instanceof ApiError && error.authExpired) authExpired = true; }
    finally { window.clearTimeout(timeout); liveRefreshBusy = false; }
  }

  let selectedRefreshBusy = false;
  async function refreshSelectedServer(): Promise<void> {
    if (selectedRefreshBusy || PREVIEW_MODE || sessionState !== 'authenticated' || authExpired || activePage !== 'server' || !selectedServerId || document.hidden) return;
    const id = selectedServerId;
    const range = chartRange;
    const controller = new AbortController();
    const timeout = window.setTimeout(() => controller.abort(), 10000);
    selectedRefreshBusy = true;
    try {
      const [item, query, latestQuery, trafficQuery] = await Promise.all([
        apiClient.getServer(id, { signal: controller.signal }),
        apiClient.queryMetrics(id, { ...rangeWindow(range), resolution: range === '15m' ? 'raw' : range === '1h' ? 'minute' : 'hour', signal: controller.signal }),
        range === '15m' ? Promise.resolve(null) : apiClient.queryMetrics(id, { from: new Date(Date.now() - 120000).toISOString(), to: new Date().toISOString(), resolution: 'raw', signal: controller.signal }),
        apiClient.queryTraffic(id, { ...rangeWindow('24h'), signal: controller.signal }).catch((error) => { if (error instanceof ApiError && error.authExpired) authExpired = true; return null; })
      ]);
      if (activePage !== 'server' || selectedServerId !== id || controller.signal.aborted) return;
      if (activeInstall?.serverId === id && activeInstall.currentStage === 'failed' && item.connection_state === 'connected') saveActiveInstall(null);
      servers = servers.map((previous) => {
        if (previous.id !== id) return previous;
        const updated = { ...previous, ...emptyApiServer(item), metrics: previous.metrics, traffic: previous.traffic, latestMetricAt: previous.latestMetricAt, metricHistory: previous.metricHistory };
        applyMetric(latestQuery ?? query, updated);
        const empty: PreviewChartData = { timestamps: [], cpu: [], memory: [], disk: [], coverage: 'unavailable' };
        const ranges = updated.metricHistory?.ranges ?? { '15m': empty, '1h': empty, '24h': empty };
        updated.metricHistory = { ranges: { ...ranges, [range]: metricChart(query), ...(latestQuery ? { '15m': metricChart(latestQuery) } : {}) } };
        const period = trafficQuery?.periods[0];
        if (period) updated.traffic = { scope: period.scope, from: period.from, to: period.to, timezone: period.timezone, allowanceBytes: period.allowance_bytes, direction: period.direction, countedBytes: period.counted_bytes, continuity: period.continuity };
        return updated;
      });
    } catch (error) {
      if (error instanceof ApiError && error.authExpired) authExpired = true;
    } finally {
      window.clearTimeout(timeout);
      selectedRefreshBusy = false;
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
      if (activeInstall?.currentStage === 'failed' && page.items.some((server) => server.id === activeInstall?.serverId && server.connection_state === 'connected')) saveActiveInstall(null);
      sessionState = 'authenticated';
      servers = page.items.map(emptyApiServer);
      void loadAccounts();
      void checkLatestUpdate().then(() => { if (updateStatus?.web_update && ['queued', 'running'].includes(updateStatus.web_update.state) && updateStatus.web_update.target) pollUpdateProgress(updateStatus.web_update.target); });
      selectedServerId = servers[0]?.id ?? '';
      restoreState(savedUiState);
      previewState = servers.length ? 'ready' : 'empty';
      if (activePage === 'packages') void loadModules();
      if (activePage === 'alerts') void loadAlerts();
      if (activePage === 'settings') void loadHTTPS();
      void loadNotifications();
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
      logEntries = result.entries.map((entry) => ({ time: formatLogTimeInTz(entry.timestamp, currentTimezone), level: (entry.severity ?? 'INFO').toUpperCase() as PreviewLogEntry['level'], text: entry.text, source: source.label, cursor: entry.cursor }));
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
    if (state.detailTab) detailTab = state.detailTab === 'metrics' ? 'resources' : state.detailTab === 'traffic' ? 'network' : state.detailTab;
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
        if (Array.isArray(parsed?.logs)) {
          parsed.logs = parsed.logs.filter((log) => !/^(Connecting over SSH to |Checking installation files\.|Downloading from hub\.|Connected via SSH\. Host key verified & trusted\.|Running preflight inspection: Linux OS detected\.|Executing payesh-install and configuring systemd service in background\.\.\.|Service started\. Enrolling node TLS certificate with hub\.\.\.|Awaiting initial telemetry heartbeat\.\.\.)/.test(log.text));
        }
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
    const notificationRefresh = window.setInterval(() => void loadNotifications(), 60000);
    const liveRefresh = window.setInterval(() => { void refreshSelectedServer(); void refreshMonitoring(); void refreshStorageUsage(); }, 15000);
    return () => {
      window.clearInterval(liveRefresh);
      window.clearInterval(notificationRefresh);
      window.clearTimeout(updatePollTimer);
      if (httpsPoll) clearTimeout(httpsPoll);
      if (nodePortPoll) clearTimeout(nodePortPoll);
      window.removeEventListener('popstate', onPopState);
      trafficUsageController?.abort();
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
  <aside class="sidebar" aria-label="Primary navigation" inert={!PREVIEW_MODE && authExpired}>
    <div class="brand-lockup">
      <div class="brand-mark" aria-hidden="true">
        <span>P</span>
      </div>
      <div class="brand-text">
        <strong>Payesh</strong>
      </div>
    </div>

    <button class="button ghost mobile-menu-toggle" type="button" aria-expanded={mobileNavOpen} aria-controls="primary-navigation" on:click={() => mobileNavOpen = !mobileNavOpen}>Menu <Icon name="chevron-right" size={14} /></button>
    <nav id="primary-navigation" class="nav-list" class:menu-open={mobileNavOpen}>
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
      <span class="version-tag">{PREVIEW_MODE ? 'Fixture preview' : sessionState === 'authenticated' && !authExpired ? 'Connected' : 'Sign in required'}</span>
    </div>
  </aside>

  <main class="main-content">
    <header class="topbar" inert={!PREVIEW_MODE && authExpired}>
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
          <button class="icon-button notification-bell-button" type="button" on:click={() => { settingsSection = 'notifications'; navigate('settings'); void loadNotifications(); }} aria-label={`Notifications${unreadNotifications ? ` (${unreadNotifications} unread)` : ''}`} title="Notifications">
            <Icon name="bell" size={16} />
            {#if unreadNotifications > 0}
              <span class="notification-badge" aria-hidden="true">{unreadNotifications > 99 ? '99+' : unreadNotifications}</span>
            {/if}
          </button>
          <button class="button ghost small" type="button" on:click={() => promptSignOut()}>Sign out</button>
        {/if}
        {#if myAccount?.permission !== 'read'}<button class="button primary small" type="button" on:click={() => openAddServer()}>
          <Icon name="plus" size={14} />
          <span>Add server</span>
        </button>{/if}
      </div>
    </header>

    {#if notice}<div class="notice" role="status"><Icon name="check" size={14} /><span>{notice}</span></div>{/if}
    {#if signOutError}<div class="partial-warning" role="alert"><Icon name="alert-triangle" size={15} /><span>{signOutError}</span></div>{/if}
    {#if showUpdateBanner && updateStatus}
      <div class="partial-warning update-banner" role="status"><Icon name="alert-triangle" size={15} /><span>Payesh v{updateStatus.latest} is available{updateStatus.current && updateStatus.current !== 'dev' ? ` (installed: v${updateStatus.current})` : ''}. <button class="link-button" type="button" on:click={openUpdateSettings}>View update</button></span><button class="link-button" type="button" aria-label="Dismiss update notice" on:click={dismissUpdateBanner}>Dismiss</button></div>
    {/if}
    {#if partialWarning}<div class="partial-warning" role="status"><Icon name="alert-triangle" size={15} /><span>{partialWarning}</span></div>{/if}
    {#if insecureConnection && !PREVIEW_MODE && sessionState === 'authenticated'}
      <div class="partial-warning" role="status"><Icon name="alert-triangle" size={15} /><span>This connection is not encrypted (no SSL). <button class="link-button" type="button" on:click={() => navigate('settings')}>Add a domain</button> to turn on HTTPS automatically.</span></div>
    {/if}

    {#if !PREVIEW_MODE && authExpired}
      <section class="page" aria-label="Sign in">
          <div class="login-screen">
            <div class="login-card">
              <div class="brand-mark large"><span>P</span></div>
              <h1>Sign in to hub</h1>
              <p class="muted center">Authenticate to access fleet operations.</p>
              <form class="auth-form" on:submit|preventDefault={() => void submitLogin()}>
                <label>
                  <span>Username</span>
                  <input use:focusLogin bind:value={authUsername} autocomplete="username" placeholder="owner" required />
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
      </section>
    {:else if activePage === 'monitoring'}
      <section class="page" aria-labelledby="monitoring-title">
        <div class="page-heading">
          <div><h1 id="monitoring-title" tabindex="-1">Server monitoring</h1></div>
          <span class="heading-status-badge"><span class="live-ping"></span>{displayServers.length} servers</span>
        </div>
        <div class="summary-grid">
          <article class="summary-card"><div class="card-header"><span class="stat-label">Healthy</span><span class="stat-icon-wrap emerald"><Icon name="check" size={15} /></span></div><strong class="stat-value tabular">{healthyCount}<small class="stat-total"> / {displayServers.length}</small></strong></article>
          <article class="summary-card"><div class="card-header"><span class="stat-label">Needs attention</span><span class="stat-icon-wrap amber"><Icon name="alert-triangle" size={15} /></span></div><strong class="stat-value tabular">{attentionCount}</strong></article>
          <article class="summary-card"><div class="card-header"><span class="stat-label">Download & Upload</span><span class="stat-icon-wrap cyan"><Icon name="activity" size={15} /></span></div><strong class="stat-value tabular">↓ {formatNetworkRate(fleetDownload)} · ↑ {formatNetworkRate(fleetUpload)}</strong></article>
        </div>
        {#if displayServers.length === 0}<div class="state-panel"><h2>No servers to monitor</h2><p>Add a server to see its health here.</p></div>{/if}
        <div class="monitoring-grid">
          {#each displayServers as server}
            <div class="panel monitoring-card clickable" role="button" tabindex="0" on:click={() => selectServer(server)} on:keydown={(e) => { if (e.key === 'Enter' || e.key === ' ') selectServer(server); }}>
              <div class="monitoring-top"><div><h2>{server.name}</h2><small class="mono faint">{displayAddress(server)}</small></div><span class={`status-pill ${server.displayState}`}><i class="status-dot"></i>{stateLabel(server.displayState)}</span></div>
              {#if server.connectionState === 'connected'}<div class="monitoring-metrics">
                {#each [['CPU', 'cpu', 'teal'], ['Memory', 'memory', 'purple'], ['Disk', 'disk', 'blue']] as metric}
                  {@const value = server.metrics[metric[1] as 'cpu' | 'memory' | 'disk']}
                  {@const history = server.metricHistory?.ranges['15m']?.[metric[1] as 'cpu' | 'memory' | 'disk'] ?? []}
                  <div class={`monitoring-gauge ${metric[2]}`} class:resource-warning={value !== null && value >= 75} class:resource-critical={value !== null && value >= 90}>
                    <span>{metric[0]}</span><strong>{metricValue(value)}</strong>
                    <div class="monitoring-spark" aria-label={`${metric[0]} recent history`}>
                      {#if history.filter(point => point !== null).length > 1}<Sparkline values={history} tone={metric[2] as 'teal' | 'purple' | 'blue'} />{:else}<small class="faint">Awaiting history</small>{/if}
                    </div>
                    <div class="resource-track" role="meter" aria-label={`${metric[0]} usage`} aria-valuemin="0" aria-valuemax="100" aria-valuenow={value ?? undefined} aria-valuetext={value === null ? 'Unavailable' : metricValue(value)}><i style:width={`${Math.max(0, Math.min(100, value ?? 0))}%`}></i></div>
                  </div>
                {/each}
                <div class="monitoring-network"><span>↓ Download</span><strong>{formatNetworkRate(currentNetworkRate(server, 'download'))}</strong></div>
                <div class="monitoring-network"><span>↑ Upload</span><strong>{formatNetworkRate(currentNetworkRate(server, 'upload'))}</strong></div>
              </div>{/if}
              <div class="monitoring-bottom"><span>Recent activity · {sampleAge(server)}</span>{#if server.connectionState === 'connected' && server.traffic.allowanceBytes !== '0'}<span>Billing period: {formatBytes(server.traffic.countedBytes)}</span>{/if}<span>{server.lastHeartbeat ? formatHeartbeatInTz(server.lastHeartbeat, currentTimezone) : server.freshnessReason || server.connectionState}</span></div>
              <button class="button ghost small" type="button" on:click|stopPropagation={() => selectServer(server)}>View server details <Icon name="chevron-right" size={14} /></button>
            </div>
          {/each}
        </div>
      </section>
    {:else if activePage === 'servers'}
      <section class="page" aria-labelledby="servers-title">
        <div class="page-heading">
          <div>
            <h1 id="servers-title" tabindex="-1">Servers</h1>

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
            <span class="col-metric">CPU</span>
            <span class="col-metric">Memory</span>
            <span class="col-rxtx">Rx / Tx</span>
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
                  <small class="mono faint">{displayAddress(server)} · {server.lastHeartbeat ? formatHeartbeatInTz(server.lastHeartbeat, currentTimezone) : (server.freshnessReason || server.connectionState)}</small>
                </div>
                <div class="col-metric server-metric">
                  {#if server.connectionState === 'connected'}
                    <strong class="tabular">{metricValue(server.metrics.cpu)}</strong>
                    <div class="metric-microbar">
                      <span style={`width: ${Math.min(100, Math.max(0, server.metrics.cpu ?? 0))}%`}></span>
                    </div>
                  {:else}
                    <span class="faint">—</span>
                  {/if}
                </div>
                <div class="col-metric server-metric">
                  {#if server.connectionState === 'connected'}
                    <strong class="tabular">{metricValue(server.metrics.memory)}</strong>
                    <div class="metric-microbar">
                      <span class="bar-memory" style={`width: ${Math.min(100, Math.max(0, server.metrics.memory ?? 0))}%`}></span>
                    </div>
                  {:else}
                    <span class="faint">—</span>
                  {/if}
                </div>
                <div class="col-rxtx">
                  {#if server.connectionState === 'connected'}
                    <span class="rxtx-rates tabular">
                      <span class="rate-down">↓ {formatNetworkRate(currentNetworkRate(server, 'download'))}</span>
                      <span class="rate-sep">·</span>
                      <span class="rate-up">↑ {formatNetworkRate(currentNetworkRate(server, 'upload'))}</span>
                    </span>
                  {:else}
                    <span class="faint">—</span>
                  {/if}
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
            <h1 id="add-server-title" tabindex="-1">Add a server</h1>
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
            <fieldset class="ssh-auth-choice form-wide">
              <legend>SSH authentication</legend>
              <div class="ssh-auth-options">
                <label><input type="radio" name="new-server-auth" checked={installAuthMethod === 'password'} on:change={() => chooseInstallAuth('password')} /> Password</label>
                <label><input type="radio" name="new-server-auth" checked={installAuthMethod === 'key'} on:change={() => chooseInstallAuth('key')} /> Private key</label>
              </div>
              <small>Choose one method. Credentials are used for this installation.</small>
            </fieldset>
            {#if installAuthMethod === 'password'}
              <label class="form-wide">SSH password<input type="password" bind:value={installPassword} autocomplete="off" placeholder="SSH user password" required /></label>
            {:else}
              <label class="form-wide">OpenSSH private key<textarea bind:value={installKey} rows="4" autocomplete="off" placeholder="Paste OpenSSH private key" required></textarea></label>
              <label class="form-wide">Key passphrase <small>(if encrypted)</small><input type="password" bind:value={installKeyPassphrase} autocomplete="off" /></label>
            {/if}
            <label>Host-key fingerprint <small>(required for new hosts)</small><input bind:value={installFingerprint} placeholder="SHA256:…" /></label>
            <small class="form-wide muted">Verify the SHA256 fingerprint through a trusted channel before installing. A matching existing known_hosts entry can be reused.</small>
            <div class="setup-actions">
              <button class="button ghost" type="button" on:click={() => navigate('servers')}>Cancel</button>
              <button class="button primary" type="submit" disabled={createServerBusy || (installAuthMethod === 'password' ? !installPassword : !installKey)}>
                {createServerBusy ? 'Connecting…' : 'Add and install server'}
              </button>
            </div>
          </form>
          {#if jobError}<p class="form-error">{jobError}</p>{/if}
        </article>
        <article class="panel">
          <h2>Or run one command on the server</h2>
          <p class="muted">Use this when you are already logged in to the server. Name it, copy the command, and run it there as a user with sudo. The server installs Payesh and joins this hub.</p>
          <form class="form-grid" on:submit|preventDefault={() => void createJoinCommand()}>
            <label class="form-wide">Server name<input bind:value={joinName} maxlength="128" placeholder="e.g. EU-Node-02" required /></label>
            <div class="setup-actions form-wide">
              <button class="button primary" type="submit" disabled={joinBusy || !joinName.trim()}>{joinBusy ? 'Creating…' : 'Create command'}</button>
            </div>
          </form>
          {#if joinError}<p class="form-error" role="alert">{joinError}</p>{/if}
          {#if joinCommand}
            <div class="join-command">
              <pre tabindex="0" aria-label="Join command"><code>{joinCommand}</code></pre>
              <button class="button small" type="button" on:click={() => void copyJoinCommand()}>{joinCopied ? 'Copied' : 'Copy command'}</button>
              <p class="muted">The command contains a single-use token and stops working at {new Date(joinExpiresAt).toLocaleString()}. Treat it like a password.</p>
            </div>
          {/if}
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
            <h1 id="install-progress-title" tabindex="-1">{activeInstall ? `Installing ${activeInstall.serverName}` : 'Server Installation'}</h1>
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
                const serverId = activeInstall.serverId;
                installHost = activeInstall.host;
                installPort = String(activeInstall.port);
                installUser = activeInstall.user;
                saveActiveInstall(null);
                navigate('server', serverId);
              }
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
            <h1 id="alerts-title" tabindex="-1">Alerts</h1>

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
          <div class="alerts-pagination-bar">
            <div class="alerts-count-info">
              <span>Showing {alerts.length === 0 ? 0 : alertsPage * alertsPageSize + 1}–{Math.min(alerts.length, (alertsPage + 1) * alertsPageSize)} of {alerts.length}</span>
            </div>
            <div class="alerts-pager">
              <label class="page-size-selector">
                <span>Per page:</span>
                <select bind:value={alertsPageSize} on:change={() => alertsPage = 0}>
                  <option value={10}>10</option>
                  <option value={25}>25</option>
                  <option value={50}>50</option>
                  <option value={100}>100</option>
                </select>
              </label>
              <button class="button ghost small" type="button" disabled={alertsPage === 0} on:click={() => alertsPage--}>Previous</button>
              <span class="pager-info">Page {alertsPage + 1} of {totalAlertPages}</span>
              <button class="button ghost small" type="button" disabled={alertsPage >= totalAlertPages - 1} on:click={() => alertsPage++}>Next</button>
            </div>
          </div>

          <div class="server-list">
            {#each pagedAlerts as alert}
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
                  <small class="faint">{alert.last_observation ? formatDateTimeInTz(alert.last_observation, currentTimezone) : 'Awaiting observation'}</small>
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

          {#if totalAlertPages > 1}
            <div class="alerts-pagination-bar bottom">
              <button class="button ghost small" type="button" disabled={alertsPage === 0} on:click={() => alertsPage--}>Previous</button>
              <span class="pager-info">Page {alertsPage + 1} of {totalAlertPages}</span>
              <button class="button ghost small" type="button" disabled={alertsPage >= totalAlertPages - 1} on:click={() => alertsPage++}>Next</button>
            </div>
          {/if}
        {/if}
      </section>

    {:else if activePage === 'packages'}
      <section class="page" aria-labelledby="packages-title">
        <div class="page-heading">
          <div>
            <h1 id="packages-title" tabindex="-1">Packages</h1>

          </div>
          <button class="button ghost" type="button" on:click={() => void loadModules(true)}>
            <Icon name="refresh" size={14} />
            <span>Refresh catalog</span>
          </button>
        </div>


        {#if modulesError && modulesState === 'ready'}<p class="muted" role="status">{modulesError}</p>{/if}
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
                <div class="module-meta"><span>Install on your master or a node</span></div>
                {#if myAccount?.permission !== 'read'}<button class="button primary small full-width" disabled={module.install_supported === false || !!packageBusy || !servers.length} on:click={() => openPackageDialog(module)}>{module.install_supported === false ? 'Requires Payesh update' : 'Install'}</button>{/if}
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
            <h1 id="settings-title" tabindex="-1">Settings</h1>
          </div>
        </div>

        <div class="settings-layout">
          <nav class="settings-nav" aria-label="Settings sections" on:focusin={(event) => {
            if (event.target instanceof HTMLButtonElement) event.target.scrollIntoView({ block: 'nearest', inline: 'nearest' });
          }}>
            <button type="button" aria-current={settingsSection === 'general' ? 'page' : undefined} class:chosen={settingsSection === 'general'} on:click={() => { settingsSection = 'general'; }}>General</button>
            <button type="button" aria-current={settingsSection === 'tls' ? 'page' : undefined} class:chosen={settingsSection === 'tls'} on:click={() => { settingsSection = 'tls'; if (!PREVIEW_MODE) void loadHTTPS(); }}>SSL / TLS</button>
            <button type="button" aria-current={settingsSection === 'updates' ? 'page' : undefined} class:chosen={settingsSection === 'updates'} on:click={() => { settingsSection = 'updates'; if (!PREVIEW_MODE) void checkLatestUpdate(); }}>Versions & updates</button>
            <button type="button" aria-current={settingsSection === 'storage' ? 'page' : undefined} class:chosen={settingsSection === 'storage'} on:click={() => { settingsSection = 'storage'; if (!PREVIEW_MODE) void loadStorageSettings(); }}>Storage & sampling</button>
            <button type="button" aria-current={settingsSection === 'notifications' ? 'page' : undefined} class:chosen={settingsSection === 'notifications'} on:click={() => { settingsSection = 'notifications'; void loadNotifications(); }}>Notifications{unreadNotifications ? ` (${unreadNotifications})` : ''}</button>
            {#if myAccount?.role === 'owner'}<button type="button" aria-current={settingsSection === 'users' ? 'page' : undefined} class:chosen={settingsSection === 'users'} on:click={() => settingsSection = 'users'}>User management</button>{/if}
          </nav>
          <div class="settings-grid">
          {#if settingsSection === 'general'}
          <article class="panel account-management">
            <div class="panel-heading"><div><h2>General</h2></div></div>
            <div class="settings-meta-box">
              <div class="meta-row"><span>Active timezone</span><strong>{currentTimezone}</strong></div>
              <div class="meta-row"><span>Time preview</span><strong class="tabular">{formatTimeInTz(new Date(), currentTimezone, true)} · {formatDateTimeInTz(new Date(), currentTimezone)}</strong></div>
            </div>
            <form class="form-grid" on:submit|preventDefault={() => {}}>
              <label>Timezone
                <select value={currentTimezone} on:change={(e) => changeTimezone(e.currentTarget.value)}>
                  {#each getAvailableTimezones() as tz}
                    <option value={tz}>{tz}</option>
                  {/each}
                </select>
              </label>
            </form>
          </article>
          {:else if settingsSection === 'storage'}
          <article class="panel account-management storage-settings-panel">
            <div class="panel-heading"><div><h2>Storage & sampling</h2></div><button class="button ghost small" disabled={storageBusy || PREVIEW_MODE} on:click={() => void loadStorageSettings()}>Refresh</button></div>
            {#if storageStatus}
              <div class="settings-meta-box">
                <div class="meta-row"><span>Total storage usage</span><strong>{formatBytes(String(storageUsageBytes))} / {formatBytes(String(storageStatus.settings.max_database_bytes))}</strong></div>
                <div class="progress"><span style={`width:${Math.min(100, storageUsageBytes / storageStatus.settings.max_database_bytes * 100)}%`}></span></div>
                {#if recoverySnapshotBytes > 0}
                  <div class="meta-row"><span>Database and journals</span><strong>{formatBytes(String(storageStatus.database_bytes))}</strong></div>
                  <div class="meta-row"><span>Recovery snapshots</span><strong>{formatBytes(String(recoverySnapshotBytes))}</strong></div>
                {/if}
                <div class="meta-row"><span>Current sampling interval</span><strong>{storageStatus.effective_sample_seconds} seconds{storageStatus.settings.pressure_state === 'saving' && storageStatus.settings.adaptive_sampling ? ' · storage-saving mode' : ''}</strong></div>
              </div>
              <form class="form-grid" on:submit|preventDefault={() => void saveStorageSettings()}>
                <label>Maximum storage size (GB)<input type="number" min="0.128" max="64" step="0.001" required bind:value={databaseLimitGB} disabled={storageBusy || myAccount?.role !== 'owner'} /></label>
                <label>Normal sample interval (seconds)<input type="number" min="5" max="3600" step="1" required bind:value={samplingSeconds} disabled={storageBusy || myAccount?.role !== 'owner'} /></label>
                <label>Storage-saving interval (seconds)<input type="number" min={samplingSeconds} max="3600" step="1" required bind:value={pressureSamplingSeconds} disabled={storageBusy || myAccount?.role !== 'owner'} /></label>
                <label>Automatic sampling reduction<select bind:value={adaptiveSampling} disabled={storageBusy || myAccount?.role !== 'owner'}><option value={true}>Enabled</option><option value={false}>Disabled</option></select></label>
                <label>Storage notifications<select bind:value={storageNotificationsEnabled} disabled={storageBusy || myAccount?.role !== 'owner'}><option value={true}>Enabled</option><option value={false}>Disabled</option></select></label>
                {#if myAccount?.role === 'owner'}<button class="button primary" type="submit" disabled={storageBusy || PREVIEW_MODE}>{storageBusy ? 'Saving…' : 'Save settings'}</button>{/if}
              </form>
            {:else if storageBusy}<p role="status">Loading storage settings…</p>{/if}
            {#if storageError}<p class="form-error" role="alert">{storageError}</p>{/if}
            {#if storageSaved}<p class="success-text" role="status">{storageSaved}</p>{/if}
          </article>
          {:else if settingsSection === 'notifications'}
          <article class="panel account-management">
            <div class="panel-heading"><div><h2>Notifications</h2></div><div class="settings-actions"><button class="button ghost small" type="button" disabled={notificationBusy} on:click={() => void loadNotifications()}>Refresh</button>{#if myAccount?.permission === 'edit'}<button class="button ghost small" type="button" disabled={!unreadNotifications || notificationBusy} on:click={() => void markNotificationsRead()}>Mark all read</button>{/if}</div></div>
            {#if notificationError}<p class="form-error" role="alert">{notificationError}</p>{/if}
            <div class="notification-list">{#each notificationItems as item (item.id)}<article class:unread={!item.read}><div><strong>{item.kind === 'storage_full' ? 'Database limit reached' : item.kind === 'storage_cleanup' ? 'Old history removed' : item.kind === 'storage_saving' ? 'Database storage pressure' : item.kind === 'storage_warning' ? 'Database nearly full' : 'Storage recovered'}</strong><time datetime={item.created_at}>{formatDateTimeInTz(item.created_at, currentTimezone)}</time></div><p>{item.message}</p></article>{:else}<p class="muted">{notificationBusy ? 'Loading notifications…' : 'No storage notifications yet.'}</p>{/each}</div>
          </article>
          {:else if settingsSection === 'users' && myAccount?.role === 'owner'}
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
                    <label><span id="edit-password-label">New password</span><input type="password" bind:value={editPassword} minlength="12" placeholder="Leave blank to keep current" autocomplete="new-password" aria-labelledby="edit-password-label" /></label>
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
              <label><span id="new-password-label">Password</span><input type="password" bind:value={accountPassword} minlength="12" required autocomplete="new-password" aria-labelledby="new-password-label" /></label>
              <label>Role<select bind:value={accountRole}><option value="member">Member</option><option value="admin">Admin</option></select></label>
              <label>Permission<select bind:value={accountPermission}><option value="read">Read only</option><option value="edit">Can edit</option></select></label>
              <button class="button primary" type="submit" disabled={accountBusy}>Create user</button>
            </form>{/if}
            {#if accountError}<p class="form-error" role="alert">{accountError}</p>{/if}
          </article>
          {/if}
          {#if settingsSection === 'updates'}<article class="panel account-management">
            <div class="panel-heading"><div><h2>Versions & updates</h2></div><button class="button ghost small" type="button" disabled={updateCheckBusy || PREVIEW_MODE} on:click={() => void checkLatestUpdate()}>{#if updateCheckBusy}<span class="version-spinner" aria-hidden="true"></span>Checking version…{:else}<Icon name="refresh" size={14} />Check version{/if}</button></div>
            <div class="settings-meta-box">
              <div class="meta-row"><span>Installed version</span><strong>{updateStatus?.current ? `v${updateStatus.current}` : servers.find(server => server.role !== 'node')?.version ?? 'Unavailable'}</strong></div>
            </div>
            {#if updateCheckBusy}<div class="version-checking" role="status"><span class="version-spinner" aria-hidden="true"></span><div><strong>Checking for updates</strong></div></div>{/if}
            {#if updateStatus && !updateCheckBusy && !updateCheckError}
              <div class="settings-meta-box">
                <div class="meta-row"><span>Latest release</span><a href={updateStatus.url} target="_blank" rel="noopener noreferrer">v{updateStatus.latest}</a></div>
                <div class="meta-row"><span>Status</span><span class={`status-pill ${installedAhead || updateStatus.update_available ? 'pending' : 'healthy'}`} role="status"><Icon name={updateStatus.update_available ? 'refresh' : 'check'} size={14} />{installedAhead ? 'Beta · ahead of latest release' : updateStatus.update_available ? 'Update available' : 'Up to date'}</span></div>
              </div>
              {#if webUpdateActive}
                <p class="update-progress" role="status">Updating to v{webUpdate?.target}… {webUpdate?.state === 'queued' ? 'Waiting for the update service.' : 'Installing. The dashboard will restart and reload automatically.'}</p>
              {:else if updateStatus.update_available && myAccount?.role === 'owner'}
                {#if updateStatus.web_update_supported}
                  <button class="button primary small" type="button" disabled={updateApplyBusy} on:click={() => void startWebUpdate(updateStatus?.latest ?? '')}>{updateApplyBusy ? 'Starting…' : `Update to v${updateStatus.latest}`}</button>
                {:else}
                  <p class="muted">Updating from the browser is not set up on this server yet. Run the command below once; later updates can be done here.</p>
                {/if}
              {/if}
            {/if}
            {#if updateCheckError}<p class="form-error" role="alert">{updateCheckError}</p>{/if}
            {#if updateApplyError}<p class="form-error" role="alert">{updateApplyError}</p>{/if}
            <h3>Release history</h3>
            {#if releaseList.length}
              <div class="release-list">{#each visibleReleases as release (release.version)}<div class="release-row"><strong>v{release.version}</strong><span>{release.version === updateStatus?.current ? 'Installed' : new Date(release.published_at).toLocaleDateString()}</span><span class="release-actions"><a href={release.url} target="_blank" rel="noopener noreferrer">Release notes ↗</a>{#if myAccount?.role === 'owner' && updateStatus?.web_update_supported && !webUpdateActive && isNewerVersion(release.version, updateStatus.current)}<button class="button ghost small" type="button" disabled={updateApplyBusy} on:click={() => void startWebUpdate(release.version)}>Install</button>{/if}</span></div>{/each}</div>
              {#if releasePageCount > 1}
                <div class="release-pager"><button class="button ghost small" type="button" disabled={releasePage === 0} on:click={() => releasePage -= 1}>Newer</button><span class="muted">Page {releasePage + 1} of {releasePageCount} · {releaseList.length} releases</span><button class="button ghost small" type="button" disabled={releasePage >= releasePageCount - 1} on:click={() => releasePage += 1}>Older</button></div>
              {/if}
            {:else}<p class="muted">{updateCheckBusy ? 'Loading releases…' : 'Release history unavailable.'}</p>{/if}
            <h3>Update command</h3>
            <code class="update-command">sudo payesh update</code>
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
                {#if PREVIEW_MODE}
                  <span class="status-pill">Fixture preview · no hub connection</span>
                {:else if browserHTTPS || httpsStatus?.state === 'active'}
                  <span class="status-pill healthy"><i class="status-dot"></i> HTTPS active</span>
                {:else if httpsStatus?.state === 'pending'}
                  <span class="status-pill pending"><i class="status-dot"></i> Requesting certificate…</span>
                {:else if httpsStatus?.state === 'failed'}
                  <span class="status-pill failed"><i class="status-dot"></i> Certificate request failed</span>
                {:else if httpsStatusLoading || (!httpsStatus && !httpsError)}
                  <span class="status-pill pending">Checking HTTPS status…</span>
                {:else if httpsError}
                  <span class="status-pill pending">HTTPS settings unavailable</span>
                {:else}
                  <span class="status-pill pending"><i class="status-dot"></i> Not encrypted (HTTP)</span>
                {/if}
              </div>
              {#if browserHTTPS && httpsStatus?.state !== 'active'}<div class="meta-row"><span>Connection</span><span>This browser connection uses HTTPS.</span></div>{/if}
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
              <button class="button primary" type="button" disabled={httpsBusy || PREVIEW_MODE || myAccount?.permission === 'read' || !httpsDomain.trim()} on:click={() => void saveHTTPS()}>{httpsBusy ? 'Requesting certificate…' : httpsStatus?.domain ? 'Update certificate' : 'Enable HTTPS'}</button>
              {#if httpsStatus?.domain && !httpsBusy && myAccount?.permission !== 'read'}<button class="button ghost" type="button" on:click={() => promptRemoveHTTPS()}>Remove domain</button>{/if}
            </div>
          </article>
          {/if}
          {#if settingsSection === 'tls'}<article class="panel connection-settings-panel">
            <div class="panel-heading"><h2>Dashboard port</h2></div>
            <form class="port-form" on:submit|preventDefault={() => void saveDashboardPort()}>
              <label>Dashboard port<input type="number" min="1" max="65535" required bind:value={dashboardPort} disabled={portBusy || myAccount?.permission === 'read'} /></label>
              <button class="button primary" type="submit" disabled={portBusy || PREVIEW_MODE || myAccount?.permission === 'read'}>{portBusy ? 'Checking port…' : 'Change port'}</button>
            </form>
            {#if portError}<p class="form-error" role="alert">{portError}</p>{/if}
          </article>{/if}
          {#if settingsSection === 'tls'}<article class="panel connection-settings-panel">
            <div class="panel-heading"><h2>Node transport port</h2></div>
            <form class="port-form" on:submit|preventDefault={() => void saveNodePort()}>
              <label>Node transport port<input type="number" min="1" max="65535" required bind:value={nodePort} disabled={nodePortBusy || PREVIEW_MODE || myAccount?.permission === 'read'} /></label>
              <button class="button primary" type="submit" disabled={nodePortBusy || PREVIEW_MODE || !nodeTransportStatus?.url || Number(nodePort) === Number(nodeTransportStatus.port) || myAccount?.permission === 'read'}>{nodePortBusy ? 'Saving…' : 'Change node port'}</button>
            </form>
            {#if nodeTransportLoading && !nodeTransportStatus}<p class="muted" role="status">Loading node transport settings…</p>{/if}
            {#if nodeTransportLoadError}
              <p class="form-error" role="alert">{nodeTransportLoadError}</p>
              <button class="button ghost" type="button" disabled={nodeTransportLoading} on:click={() => void loadNodeTransport()}>{nodeTransportLoading ? 'Retrying…' : 'Retry node settings'}</button>
            {/if}
            {#if nodePortError}<p class="form-error" role="alert">{nodePortError}</p>{/if}
            {#if nodeTransportStatus}
              {#if nodeTransportStatus.url}<p class="mono">{nodeTransportStatus.url}</p>{:else}<p class="muted">Enable HTTPS to connect nodes.</p>{/if}
              <p class="muted">{nodeTransportStatus.migrated} of {nodeTransportStatus.total} nodes on current port.</p>
              {#each nodeTransportStatus.nodes as node (node.id)}
                <div class="meta-row"><span>{node.name}</span><span>{node.state === 'migrated' ? 'Migrated' : node.state === 'update-required' ? 'Agent update required' : node.state === 'failed' ? 'Migration failed · retrying' : 'Pending connection'}</span></div>
                {#if node.error}<p class="muted">{node.name}: {node.error}</p>{/if}
              {/each}
              {#if nodePageHistory.length || nodeTransportStatus.next_cursor}
                <div class="node-page-actions" aria-label="Node migration pages">
                  <button class="button ghost small" type="button" disabled={nodeTransportLoading || nodePortBusy || !nodePageHistory.length} on:click={() => void changeNodePage(false)}>Previous nodes</button>
                  <span class="muted">Page {nodePageHistory.length + 1}</span>
                  <button class="button ghost small" type="button" disabled={nodeTransportLoading || nodePortBusy || !nodeTransportStatus.next_cursor} on:click={() => void changeNodePage(true)}>Next nodes</button>
                </div>
              {/if}
              {#if nodeTransportStatus.previous_ports.length || nodeTransportStatus.legacy_dashboard}
                <button class="button ghost" type="button" disabled={nodePortBusy || nodeTransportStatus.pending > 0 || myAccount?.permission === 'read'} on:click={retireNodePorts}>Retire previous node endpoints</button>
              {/if}
            {/if}
          </article>{/if}
          </div>
        </div>
      </section>

    {:else if activePage === 'onboarding' && (PREVIEW_MODE || sessionState !== 'authenticated')}
      <section class="page onboarding-page" aria-labelledby="setup-title">
        <div class="page-heading">
          <div>
            <h1 id="setup-title" tabindex="-1">Setup your local hub</h1>
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
            <h2>Storage & notifications</h2>
            <p class="muted">Monitoring starts with a 1 GB database limit, 15-second sampling, and in-app storage notifications enabled. Old history is cleaned up automatically when space runs low.</p>
            <p class="muted">After setup, open Settings → Storage & sampling to change the limit, sampling intervals, or notifications.</p>

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
              <span>Storage defaults: <strong>1 GB · 15-second samples · notifications enabled</strong></span>
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
              <h1 id="server-title" tabindex="-1">{selectedServer.name}</h1>
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
              <fieldset class="ssh-auth-choice form-wide">
                <legend>SSH authentication</legend>
                <div class="ssh-auth-options">
                  <label><input type="radio" name="retry-server-auth" checked={installAuthMethod === 'password'} on:change={() => chooseInstallAuth('password')} /> Password</label>
                  <label><input type="radio" name="retry-server-auth" checked={installAuthMethod === 'key'} on:change={() => chooseInstallAuth('key')} /> Private key</label>
                </div>
              </fieldset>
              {#if installAuthMethod === 'password'}
                <label class="form-wide">SSH password<input type="password" bind:value={installPassword} autocomplete="off" required /></label>
              {:else}
                <label class="form-wide">OpenSSH private key<textarea bind:value={installKey} rows="4" autocomplete="off" required></textarea></label>
                <label class="form-wide">Key passphrase <small>(if encrypted)</small><input type="password" bind:value={installKeyPassphrase} autocomplete="off" /></label>
              {/if}
              <label>Host-key fingerprint <small>(required for new hosts)</small><input bind:value={installFingerprint} placeholder="SHA256:…" /></label>
              <button class="button primary small" type="submit" disabled={installBusy || (installAuthMethod === 'password' ? !installPassword : !installKey)}>
                {installBusy ? 'Connecting…' : 'Install Payesh'}
              </button>
            </form>
            {#if jobError}<p class="form-error" role="alert">{jobError}</p>{/if}
          </article>
        {/if}

        {#if selectedServer.connectionState === 'connected' && availableTabs.length > 0}
          <div class="tabs" role="tablist" aria-label="Server detail sections">
            {#each availableTabs as tab}
              <button class:active={detailTab === tab} type="button" role="tab" aria-selected={detailTab === tab} on:click={() => { detailTab = tab; saveUiState(); if (tab === 'logs') void loadLogs(selectedServer.id); }}>
                {tabLabel(tab)}
              </button>
            {/each}
          </div>
        {/if}

        {#if selectedServer.connectionState !== 'connected'}
          <div class="unavailable-panel large"><strong>Node is not connected</strong><span>Metrics and telemetry will appear after the node reconnects.</span></div>
        {:else if detailTab === 'processes'}
          {#key selectedServer.id}<ProcessTable serverId={selectedServer.id} onPackages={() => { packageServerId = selectedServer.id; navigate('packages'); }} />{/key}
        {:else if (detailTab === 'resources' || detailTab === 'metrics') && (hasCapability(selectedServer, 'resources') || hasCapability(selectedServer, 'metrics'))}
          <div class="metric-grid">
            <article class="metric-card">
              <div class="card-top">
                <span class="metric-title">CPU</span>
                <strong class="metric-big tabular">{metricValue(selectedServer.metrics.cpu)}</strong>
              </div>
              {#if metricHistoryValues(selectedServer, 'cpu').length > 1}
                <div class="sparkline">
                  <Sparkline values={metricHistoryValues(selectedServer, 'cpu')} tone="teal" />
                </div>
              {:else}
                <div class="sparkline-unavailable">{metricHistoryValues(selectedServer, 'cpu').some((value) => value !== null) ? 'Collecting history' : 'Awaiting samples'}</div>
              {/if}
              <div class="metric-footer">
                <span>{sampleAge(selectedServer)}</span>
                <span class="faint">Window: {chartRange}</span>
              </div>
            </article>

            <article class="metric-card">
              <div class="card-top">
                <span class="metric-title">Memory</span>
                <strong class="metric-big tabular">{metricValue(selectedServer.metrics.memory)}</strong>
              </div>
              {#if metricHistoryValues(selectedServer, 'memory').length > 1}
                <div class="sparkline">
                  <Sparkline values={metricHistoryValues(selectedServer, 'memory')} tone="purple" />
                </div>
              {:else}
                <div class="sparkline-unavailable">{metricHistoryValues(selectedServer, 'memory').some((value) => value !== null) ? 'Collecting history' : 'Awaiting samples'}</div>
              {/if}
              <div class="metric-footer">
                <span>{sampleAge(selectedServer)}</span>
                <span class="faint">Window: {chartRange}</span>
              </div>
            </article>

            <article class="metric-card disk-metric-card">
              <div class="card-top">
                <span class="metric-title">Disk</span>
                <strong class="metric-big tabular">{metricValue(selectedServer.metrics.disk)}</strong>
              </div>
              <div class="disk-card-circle">
                <CircleChart value={selectedServer.metrics.disk} size={64} strokeWidth={8} label="Used" />
              </div>
              <div class="metric-footer">
                <span>{sampleAge(selectedServer)}</span>
                <span class="faint">{selectedServer.metrics.disk !== null ? `${(100 - selectedServer.metrics.disk).toFixed(0)}% free` : 'Window: ' + chartRange}</span>
              </div>
            </article>
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
                <span class="faint">Scale: 0–100% · Time: {currentTimezone}</span>
              </div>
              {#key `${chartRange}-${theme}-${selectedServer.id}-${currentTimezone}`}
                <ChartPreview data={chartData} range={chartRange} timezone={currentTimezone} label="CPU and memory history over the selected time range" />
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

          <article class="panel chart-panel disk-detail-panel">
            <div class="panel-heading">
              <div><h2>Disk Storage</h2></div>
            </div>
            <div class="disk-detail-layout">
              <CircleChart value={selectedServer.metrics.disk} size={130} strokeWidth={10} label="Used" sublabel={selectedServer.metrics.disk !== null ? `${(100 - selectedServer.metrics.disk).toFixed(1)}% free` : ''} />
              <div class="disk-detail-stats">
                <div class="disk-stat-box">
                  <span class="stat-lbl">Used Space</span>
                  <strong class="tabular">{selectedServer.metrics.disk !== null ? `${selectedServer.metrics.disk.toFixed(1)}%` : '—'}</strong>
                </div>
                <div class="disk-stat-box">
                  <span class="stat-lbl">Available Space</span>
                  <strong class="tabular">{selectedServer.metrics.disk !== null ? `${(100 - selectedServer.metrics.disk).toFixed(1)}%` : '—'}</strong>
                </div>
                <div class="disk-stat-box">
                  <span class="stat-lbl">Health</span>
                  <strong class="tabular">{selectedServer.metrics.disk === null ? '—' : selectedServer.metrics.disk >= 90 ? 'Critical' : selectedServer.metrics.disk >= 75 ? 'Warning' : 'Healthy'}</strong>
                </div>
              </div>
            </div>
          </article>

        {:else if (detailTab === 'network' || detailTab === 'traffic') && (hasCapability(selectedServer, 'network') || hasCapability(selectedServer, 'traffic'))}
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
            <div class="network-rates">
              <div><span class="muted">Download</span><strong class="tabular">{formatNetworkRate(currentNetworkRate(selectedServer, 'download'))}</strong></div>
              <div><span class="muted">Upload</span><strong class="tabular">{formatNetworkRate(currentNetworkRate(selectedServer, 'upload'))}</strong></div>
            </div>

            {#if chartData && (chartData.networkRx?.some((v) => v !== null) || chartData.networkTx?.some((v) => v !== null))}
              <div class="legend">
                <span><i class="legend-dot teal"></i> Download</span>
                <span><i class="legend-dot blue"></i> Upload</span>
                <span class="faint">Unit: Mbit/s · Time: {currentTimezone}</span>
              </div>
              {#key `${chartRange}-${theme}-${selectedServer.id}-${currentTimezone}-traffic`}
                <TrafficChart data={chartData} timezone={currentTimezone} />
              {/key}
            {:else}
              <div class="unavailable-panel">
                <strong>Waiting for network samples</strong>
                <span>At least two consecutive network counter samples are needed to calculate bandwidth rate.</span>
              </div>
            {/if}
          </article>

          <article class="panel traffic-panel">
            <div class="panel-heading"><div><h2>Traffic used in a date range</h2></div></div>
            <form class="traffic-range-form" on:submit|preventDefault={() => void loadTrafficUsage()}>
              <label>From<input type="datetime-local" step="3600" required bind:value={trafficFrom} disabled={trafficUsageBusy} /></label>
              <label>To<input type="datetime-local" step="3600" required bind:value={trafficTo} disabled={trafficUsageBusy} /></label>
              <button class="button primary" type="submit" disabled={trafficUsageBusy || PREVIEW_MODE}>{#if trafficUsageBusy}<span class="version-spinner" aria-hidden="true"></span>Calculating…{:else}Calculate usage{/if}</button>
            </form>
            {#if trafficUsageError}<p class="error-text" role="alert">{trafficUsageError}</p>{/if}
            {#if trafficUsage}
              <p class="muted">{formatDateTimeInTz(trafficUsage.from, currentTimezone)} → {formatDateTimeInTz(trafficUsage.to, currentTimezone)}</p>
              <div class="network-rates traffic-usage-results" aria-live="polite">
                <div><span class="muted">Download</span><strong class="tabular usage-download">{trafficUsage.download_bytes === null ? 'Unavailable' : formatBytes(trafficUsage.download_bytes)}</strong><small class="muted">{trafficUsage.download_hours}/{trafficUsage.requested_hours} hours with recorded totals</small></div>
                <div><span class="muted">Upload</span><strong class="tabular usage-upload">{trafficUsage.upload_bytes === null ? 'Unavailable' : formatBytes(trafficUsage.upload_bytes)}</strong><small class="muted">{trafficUsage.upload_hours}/{trafficUsage.requested_hours} hours with recorded totals</small></div>
                <div><span class="muted">Total recorded</span><strong class="tabular">{trafficUsage.total_bytes === null ? 'Unavailable' : formatBytes(trafficUsage.total_bytes)}</strong></div>
              </div>
              {#if trafficUsage.download_hours < trafficUsage.requested_hours || trafficUsage.upload_hours < trafficUsage.requested_hours}
                <p class="warning-text" role="status">History is incomplete. These are available totals; missing hours and uncertain counters are excluded.</p>
              {/if}
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
            <h1 id="overview-title" tabindex="-1">Fleet overview</h1>
          </div>
          <div class="heading-status-badge">
            <span class="live-ping"></span>
            <span class="date-stamp tabular">{PREVIEW_MODE ? 'Fixture preview' : 'Updates every 15s'}</span>
          </div>
        </div>

        {#if previewState === 'loading'}
          <div class="state-panel">
            <div class="loading-spinner" aria-hidden="true"></div>
            <h2>Connecting to fleet…</h2>
            <p>Gathering health telemetry and server states.</p>
          </div>
        {:else if previewState === 'empty'}
          <div class="empty-onboarding">
            <div class="empty-onboarding-main">
              <span class="eyebrow">Get started · 1 of 3</span>
              <div class="state-icon"><Icon name="servers" size={26} /></div>
              <h2>Bring your first server online</h2>
              <p>Connect a Linux server over SSH. Payesh checks node transport reachability before installing the agent, then shows its live metrics here.</p>
              {#if myAccount?.permission === 'read'}
                <p class="muted">Ask a user with edit access to add the first server.</p>
              {:else}
                <button class="button primary" type="button" on:click={() => openAddServer()}>
                  <Icon name="plus" size={15} />
                  <span>Add first server</span>
                </button>
              {/if}
            </div>
            <ol class="empty-onboarding-steps" aria-label="Server setup steps">
              <li><span>01</span><div><strong>Enter server details</strong><small>Hostname, SSH user, and one sign-in method.</small></div></li>
              <li><span>02</span><div><strong>Verify the connection</strong><small>Payesh checks SSH and the TLS node endpoint.</small></div></li>
              <li><span>03</span><div><strong>Monitor live metrics</strong><small>See health and resource data after enrollment.</small></div></li>
            </ol>
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
              {#if attentionCount > 0}
                <div class="stat-badge warning">
                  <span>Stale or pending nodes</span>
                </div>
              {/if}
            </article>

            <article class="summary-card">
              <div class="card-header">
                <span class="stat-label">Download & Upload</span>
                <span class="stat-icon-wrap cyan"><Icon name="activity" size={15} /></span>
              </div>
              <strong class="stat-value tabular">↓ {formatNetworkRate(fleetDownload)} · ↑ {formatNetworkRate(fleetUpload)}</strong>
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
              <span class="col-metric">CPU</span>
              <span class="col-metric">Memory</span>
              <span class="col-rxtx">Rx / Tx</span>
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
                    <small class="mono faint">{displayAddress(server)} · {server.lastHeartbeat ? formatHeartbeatInTz(server.lastHeartbeat, currentTimezone) : (server.freshnessReason || server.connectionState)}</small>
                  </div>
                  <div class="col-metric server-metric">
                    {#if server.connectionState === 'connected'}
                      <strong class="tabular">{metricValue(server.metrics.cpu)}</strong>
                      <div class="metric-microbar">
                        <span style={`width: ${Math.min(100, Math.max(0, server.metrics.cpu ?? 0))}%`}></span>
                      </div>
                    {:else}
                      <span class="faint">—</span>
                    {/if}
                  </div>
                  <div class="col-metric server-metric">
                    {#if server.connectionState === 'connected'}
                      <strong class="tabular">{metricValue(server.metrics.memory)}</strong>
                      <div class="metric-microbar">
                        <span class="bar-memory" style={`width: ${Math.min(100, Math.max(0, server.metrics.memory ?? 0))}%`}></span>
                      </div>
                    {:else}
                      <span class="faint">—</span>
                    {/if}
                  </div>
                  <div class="col-rxtx">
                    {#if server.connectionState === 'connected'}
                      <span class="rxtx-rates tabular">
                        <span class="rate-down">↓ {formatNetworkRate(currentNetworkRate(server, 'download'))}</span>
                        <span class="rate-sep">·</span>
                        <span class="rate-up">↑ {formatNetworkRate(currentNetworkRate(server, 'upload'))}</span>
                      </span>
                    {:else}
                      <span class="faint">—</span>
                    {/if}
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
                <p class="muted info-hint tabular">{formatBytes(overviewAllowanceBytes)} fleet quota · {currentTimezone} timezone</p>
              {/if}
            </article>

            <article class="panel">
              <div class="panel-heading">
                <div>
                  <h2>Collection status</h2>
                </div>
                <span class={`status-pill ${attentionCount ? 'stale' : 'healthy'}`}><i class="status-dot"></i>{attentionCount ? 'Review needed' : 'Receiving data'}</span>
              </div>
              <div class="activity-feed">
                <div class="activity-item">
                  <span class="activity-icon-pill emerald"><Icon name="check" size={13} /></span>
                  <div>
                    <strong>{connectedCount} of {displayServers.length} servers connected</strong>
                    <small class="faint">{healthyCount} healthy · {attentionCount} need attention</small>
                  </div>
                </div>
                <div class="activity-item">
                  <span class="activity-icon-pill amber"><Icon name="activity" size={13} /></span>
                  <div>
                    <strong>Alerts and incidents</strong>
                    <button class="text-button" type="button" on:click={() => navigate('alerts')}>Review alert history <Icon name="chevron-right" size={14} /></button>
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
  </main>
</div>

<Modal open={!!packageDialog} title={packageDialog ? `${packageDialog.name}` : 'Install package'} description={packageStep === 'target' ? 'Choose where to install or manage this package.' : `Install on ${servers.find(server => server.id === packageServerId)?.name ?? 'selected server'}.`} icon="packages" confirmText={packageStep === 'target' ? 'Continue' : packageBusy ? 'Installing…' : 'Install package'} busy={!!packageBusy} confirmDisabled={!packageServerId || packageStatusBusy || packageStatusFailed || (packageStep === 'source' && (['enabled', 'installed-disabled', 'downloading', 'verifying', 'installing', 'updating', 'removing'].includes(moduleState(packageDialog?.id ?? '')?.state ?? '') || (packageSource !== 'github' && !packageLocation.trim())))} onCancel={() => { packageDialog = null; }} onConfirm={() => { if (packageStep === 'target') packageStep = 'source'; else if (packageDialog) void installPackage(packageDialog); }}>
  {#if packageStep === 'target'}
    <label class="package-dialog-label">Install on<select bind:value={packageServerId} disabled={!!packageBusy} on:change={() => void loadServerModules()}><option value="" disabled>Choose master or node</option>{#each servers as server}<option value={server.id}>{server.name} · {server.role === 'node' ? 'Node' : 'Master'} ({server.architecture})</option>{/each}</select></label>
    {#if packageStatusBusy}<p class="muted" role="status">Loading package status…</p>{/if}
  {:else}
    <button class="button ghost small" disabled={!!packageBusy} on:click={() => packageStep = 'target'}>Change server</button>
          <div class="package-source-fields">
            <label>Source<select bind:value={packageSource} on:change={() => { packageLocation = ''; packageManifest = ''; packageSignature = ''; packageError = ''; }}><option value="github">GitHub</option><option value="local">Local path</option><option value="url">URL</option></select></label>
            <label>{packageSource === 'github' ? 'Repository' : packageSource === 'local' ? 'Archive path on this server' : 'Archive URL'}<input bind:value={packageLocation} placeholder={packageSource === 'github' ? 'Real-kia/payesh' : packageSource === 'local' ? '/opt/packages/cpu-controls-linux-amd64.tar.gz' : 'https://packages.example.com/cpu-controls-linux-amd64.tar.gz'} /></label>
            {#if packageSource === 'github'}
              <label>Release<input bind:value={packageVersion} placeholder="Catalog default" /></label>
            {:else}
              <details><summary>Package metadata</summary><label>Manifest {packageSource === 'local' ? 'path' : 'URL'}<input bind:value={packageManifest} placeholder="Automatic (.manifest.json)" /></label><label>Signature {packageSource === 'local' ? 'path' : 'URL'}<input bind:value={packageSignature} placeholder="Automatic (.manifest.sig)" /></label></details>
            {/if}
          </div>
    {#if packageDialog}
      {@const state = moduleState(packageDialog.id)?.state}
      {#if state === 'enabled' || state === 'installed-disabled'}
        <p class="muted">This package is {state === 'enabled' ? 'enabled' : 'installed and disabled'} on this server.</p>
        <div class="job-actions"><button class="button ghost small" disabled={!!packageBusy} on:click={() => { if (packageDialog) void packageAction(packageDialog, state === 'enabled' ? 'disable' : 'enable'); }}>{state === 'enabled' ? 'Disable' : 'Enable'}</button>{#if state === 'installed-disabled'}<button class="button ghost small" disabled={!!packageBusy} on:click={() => { if (packageDialog) void packageAction(packageDialog, 'remove'); }}>Remove</button>{/if}</div>
      {/if}
      {#if packageSource === 'github'}<a href={packageGitHubURL(packageDialog)} target="_blank" rel="noopener noreferrer">View package archive ↗</a>{/if}
    {/if}
  {/if}
  {#if packageError}<p class="form-error" role="alert">{packageError}</p>{#if packageStatusFailed}<button class="button ghost small" on:click={() => void loadServerModules()}>Retry status</button>{/if}{/if}
</Modal>

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

  .button.mobile-menu-toggle { display: none; }
  .node-page-actions { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; margin: 16px 0; }
  h1:focus { outline: none; }

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

  .monitoring-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(min(100%, 420px), 1fr)); gap: 16px; }
  .monitoring-card { display: flex; flex-direction: column; gap: 18px; }
  .monitoring-top, .monitoring-bottom { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; }
  .monitoring-top h2 { margin: 0 0 4px; font-size: 17px; }
  .monitoring-metrics { display: grid; grid-template-columns: repeat(6, minmax(0, 1fr)); gap: 10px; }
  .monitoring-metrics > div { padding: 12px; border-radius: var(--radius-md); background: var(--surface-muted); }
  .monitoring-metrics span, .monitoring-metrics strong { display: block; }
  .monitoring-metrics span, .monitoring-bottom { color: var(--muted); font-size: 12px; }
  .monitoring-metrics strong { margin-top: 5px; font-size: 18px; color: var(--ink); }
  .monitoring-gauge { grid-column: span 2; border: 1px solid var(--line); --metric-color: var(--teal); }
  .monitoring-gauge.purple { --metric-color: var(--purple); }
  .monitoring-gauge.blue { --metric-color: var(--blue); }
  .monitoring-gauge.resource-warning { --metric-color: var(--warning, #d97706); }
  .monitoring-gauge.resource-critical { --metric-color: var(--danger); }
  .monitoring-gauge strong { color: var(--metric-color); }
  .monitoring-spark { height: 40px; margin: 12px 0 8px; }
  .resource-track { height: 4px; background: var(--line); border-radius: 9px; overflow: hidden; }
  .resource-track i { display: block; height: 100%; background: var(--metric-color); transition: width .4s ease; }
  .monitoring-network { grid-column: span 3; border: 1px solid var(--line); }
  .monitoring-network strong { font-size: 15px; }
  .package-dialog-label { display: grid; gap: 8px; }
  .package-dialog-label select { width: 100%; }
  .version-spinner { display: inline-block; width: 15px; height: 15px; border: 2px solid var(--line); border-top-color: var(--teal); border-radius: 50%; animation: version-spin .8s linear infinite; flex-shrink: 0; }
  .version-checking { display: flex; align-items: center; gap: 14px; background: var(--surface-muted); border: 1px solid var(--line); border-radius: var(--radius-md); padding: 18px; margin: 12px 0; }
  .version-checking .version-spinner { width: 24px; height: 24px; }
  .version-checking small { display: block; margin-top: 5px; color: var(--muted); }
  @keyframes version-spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) { .version-spinner { animation: none; } .resource-track i { transition: none; } }
  .monitoring-bottom { flex-wrap: wrap; }
  .monitoring-card > button { align-self: flex-start; margin-top: auto; }
  .account-management { grid-column: 1 / -1; }
  .account-list { margin-bottom: 20px; }
  .account-row, .user-table-head { display: grid; grid-template-columns: minmax(120px, 1fr) 80px 100px 150px; align-items: center; gap: 12px; padding: 16px 0; border-bottom: 1px solid var(--line); font-size: 13px; }
  .user-table-head { color: var(--muted); font-size: 12px; }
  .account-row .settings-actions { margin: 0; justify-content: flex-end; gap: 8px; }
  .user-role { text-transform: capitalize; }
  @media (max-width: 1100px) { .user-table-head { display: none; } .account-row { grid-template-columns: 1fr 1fr; } .account-row .settings-actions { justify-content: flex-start; } }
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
  .button:hover:not(:disabled) {
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
  .button.primary:hover:not(:disabled) {
    background: var(--teal);
    border-color: var(--teal);
    filter: brightness(1.1);
  }
  .button.ghost {
    background: transparent;
    border-color: var(--line);
    color: var(--muted);
  }
  .button.ghost:hover:not(:disabled) {
    background: var(--surface-muted);
    border-color: var(--muted);
    color: var(--ink);
  }
  .button.danger {
    background: var(--danger-bg);
    border-color: transparent;
    color: var(--danger);
  }
  .button.danger:hover:not(:disabled) {
    filter: brightness(0.95);
  }
  .button.full-width { width: 100%; }
  .button:disabled { opacity: 0.55; cursor: not-allowed; }

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
    margin-bottom: 24px;
  }
  .summary-card {
    padding: 18px 20px;
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
    grid-template-columns: 130px minmax(180px, 2fr) 95px 95px minmax(140px, 1.2fr) 36px;
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
    grid-template-columns: 130px minmax(180px, 2fr) 95px 95px minmax(140px, 1.2fr) 36px;
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
  .metric-microbar .bar-memory {
    background: var(--purple, #a855f7);
  }

  .col-rxtx {
    display: flex;
    align-items: center;
    font-size: 13px;
  }
  .rxtx-rates {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    font-weight: 500;
  }
  .rate-down {
    color: var(--teal);
  }
  .rate-up {
    color: var(--blue, #3b82f6);
  }
  .rate-sep {
    color: var(--muted);
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

  .success-text { color: var(--success); }
  .notification-list { display: grid; gap: 12px; }
  .notification-list article { border: 1px solid var(--line); border-radius: 8px; padding: 16px; }
  .notification-list article.unread { border-left: 3px solid var(--warning); }
  .notification-list article > div { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 8px; }
  .notification-list time { color: var(--muted); font-size: 12px; }
  .notification-list p { overflow-wrap: anywhere; }
  .traffic-range-form { display: flex; flex-wrap: wrap; gap: 16px; align-items: end; margin-bottom: 20px; }
  .traffic-range-form label { display: grid; gap: 8px; flex: 1 1 200px; min-width: 0; }
  .traffic-range-form input { min-width: 0; width: 100%; padding: 10px; border: 1px solid var(--line); border-radius: 8px; background: var(--surface); color: var(--ink); }
  .error-text { color: var(--danger); }
  .warning-text { color: var(--warning); }
  .traffic-usage-results { margin-top: 24px; }
  .usage-download { color: var(--teal); }
  .usage-upload { color: var(--blue); }
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
  .join-command { display: grid; gap: 10px; margin-top: 16px; }
  .join-command pre {
    margin: 0;
    padding: 12px;
    overflow-x: auto;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
    background: var(--surface-muted);
    font-size: 12px;
  }
  .join-command .button { justify-self: start; }
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
  .login-card h1 { text-align: center; margin-bottom: 6px; font-size: 18px; }
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
  .empty-onboarding {
    display: grid;
    grid-template-columns: minmax(0, 1.15fr) minmax(260px, .85fr);
    overflow: hidden;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
  }
  .empty-onboarding-main { padding: 40px; background: linear-gradient(135deg, var(--surface), var(--teal-bg)); }
  .empty-onboarding-main .eyebrow { color: var(--teal); font-size: 12px; font-weight: 700; letter-spacing: .04em; text-transform: uppercase; }
  .empty-onboarding-main .state-icon { margin: 28px 0 18px; color: var(--teal); background: var(--surface); }
  .empty-onboarding-main h2 { margin: 0 0 12px; font-size: clamp(24px, 3vw, 32px); letter-spacing: -.03em; line-height: 1.15; }
  .empty-onboarding-main p { max-width: 52ch; margin: 0 0 22px; color: var(--ink-secondary); line-height: 1.6; }
  .empty-onboarding-main .button { min-height: 42px; }
  .empty-onboarding-steps { display: grid; align-content: center; gap: 0; margin: 0; padding: 20px 30px; list-style: none; }
  .empty-onboarding-steps li { display: flex; gap: 16px; padding: 22px 0; border-bottom: 1px solid var(--line-light); }
  .empty-onboarding-steps li:last-child { border-bottom: 0; }
  .empty-onboarding-steps li > span { flex: 0 0 30px; color: var(--teal); font-size: 12px; font-weight: 700; font-variant-numeric: tabular-nums; }
  .empty-onboarding-steps li div { display: grid; gap: 6px; }
  .empty-onboarding-steps strong { color: var(--ink); font-size: 14px; }
  .empty-onboarding-steps small { color: var(--muted); line-height: 1.5; }
  @media (max-width: 800px) { .empty-onboarding { grid-template-columns: 1fr; } .empty-onboarding-main { padding: 28px; } .empty-onboarding-steps { padding: 0 28px 8px; } .empty-onboarding-steps li { padding: 16px 0; } }
  @media (max-width: 480px) { .empty-onboarding-main { padding: 24px; } .empty-onboarding-steps { padding: 0 24px 8px; } }
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
    min-width: 0;
  }
  .settings-intro { margin-top: 8px; font-size: 14px; line-height: 1.6; color: var(--muted); }
  .settings-layout { display: grid; grid-template-columns: 200px minmax(0, 850px); gap: 32px; align-items: start; }
  .settings-nav { display: grid; gap: 4px; border-right: 1px solid var(--line); padding-right: 16px; }
  .settings-nav button { text-align: left; padding: 12px; min-height: 44px; font-size: 14px; line-height: 1.4; border: 0; background: transparent; color: var(--muted); cursor: pointer; font-family: inherit; border-radius: var(--radius-md); }
  .settings-nav button.chosen { background: var(--teal-bg); color: var(--ink); font-weight: 600; box-shadow: inset 3px 0 var(--teal); }
  .ssh-auth-choice { margin: 0; padding: 0; border: 0; min-width: 0; }
  .ssh-auth-choice legend { margin-bottom: 8px; font-size: 13px; font-weight: 600; color: var(--ink); }
  .ssh-auth-choice > small { display: block; margin-top: 8px; color: var(--muted); font-size: 12px; }
  .ssh-auth-options { display: flex; flex-wrap: wrap; gap: 8px; }
  .ssh-auth-options label { display: inline-flex; align-items: center; gap: 8px; min-height: 42px; padding: 0 14px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); color: var(--ink); cursor: pointer; }
  .ssh-auth-options label:has(input:checked) { border-color: var(--teal); background: var(--teal-bg); }
  .ssh-auth-options input { width: auto; margin: 0; accent-color: var(--teal); }
  .user-search { display: grid; gap: 8px; margin-bottom: 20px; font-size: 13px; }
  .release-list { border-top: 1px solid var(--line); margin: 16px 0 12px; }
  .release-row { align-items: center; }
  .release-row { padding: 10px 0; }
  .release-actions { display: flex; align-items: center; gap: 12px; justify-content: flex-end; }
  .release-pager { display: flex; align-items: center; justify-content: space-between; gap: 12px; margin-bottom: 24px; font-size: 13px; }
  .update-progress { margin: 12px 0; padding: 10px 14px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); font-size: 13px; }
  .update-banner { justify-content: space-between; }
  .update-banner span { flex: 1; }
  .release-row { display: grid; grid-template-columns: 100px 1fr auto; gap: 16px; padding: 16px 0; border-bottom: 1px solid var(--line); color: var(--ink); text-decoration: none; font-size: 13px; }
  .connection-settings-panel > p { font-size: 14px; line-height: 1.6; color: var(--muted); }
  .connection-settings-panel .panel-heading { margin-bottom: 16px; }
  .port-form { display: flex; flex-wrap: wrap; gap: 12px; align-items: end; }
  .port-form label { display: grid; gap: 8px; font-size: 13px; color: var(--ink-secondary); }
  .port-form input { width: 120px; max-width: 100%; box-sizing: border-box; }
  .form-wide { grid-column: 1 / -1; }
  @media (max-width: 1100px) { .settings-layout { grid-template-columns: 1fr; gap: 20px; } .settings-nav { display: flex; gap: 6px; overflow-x: auto; max-width: 100%; border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface); padding: 6px; scrollbar-width: thin; } .settings-nav button { flex: 0 0 auto; padding: 10px 14px; white-space: nowrap; } .settings-nav button.chosen { box-shadow: inset 0 -3px var(--teal); } .release-row { grid-template-columns: 70px 1fr; } .release-row .release-actions { grid-column: 1 / -1; justify-content: space-between; } }
  @media (max-width: 640px) {
    .settings-nav { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); overflow: visible; }
    .settings-nav button { min-width: 0; padding: 10px; white-space: normal; }
  }
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
  .package-source-fields { display: grid; grid-template-columns: 1fr; align-items: start; gap: 16px; width: 100%; }
  .package-source-fields label { display: grid; gap: 8px; min-width: 0; }
  .package-source-fields select, .package-source-fields input { min-width: 0; width: 100%; box-sizing: border-box; }
  .package-source-fields details { grid-column: auto; }
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
     NOTIFICATION BELL & BADGE
     -------------------------------------------------------------------------- */
  .notification-bell-button {
    position: relative;
  }
  .notification-badge {
    position: absolute;
    top: -2px;
    right: -2px;
    min-width: 16px;
    height: 16px;
    padding: 0 4px;
    border-radius: 8px;
    background: var(--danger, #ef4444);
    color: #fff;
    font-size: 10px;
    font-weight: 700;
    line-height: 16px;
    text-align: center;
    pointer-events: none;
  }

  /* --------------------------------------------------------------------------
     ALERTS PAGINATION
     -------------------------------------------------------------------------- */
  .alerts-pagination-bar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 16px;
    padding: 12px 16px;
    margin-bottom: 16px;
    background: var(--surface);
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
    font-size: 13px;
  }
  .alerts-pagination-bar.bottom {
    margin-top: 16px;
    margin-bottom: 0;
    justify-content: flex-end;
  }
  .alerts-pager {
    display: flex;
    align-items: center;
    gap: 12px;
  }
  .page-size-selector {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--muted);
  }
  .page-size-selector select {
    padding: 4px 8px;
    font-size: 12px;
    border-radius: var(--radius-sm);
  }
  .pager-info {
    font-size: 12px;
    color: var(--muted);
  }

  /* --------------------------------------------------------------------------
     DISK CARD & CIRCLE LAYOUT
     -------------------------------------------------------------------------- */
  .disk-metric-card .disk-card-circle {
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 8px 0;
  }
  .disk-detail-panel .disk-detail-layout {
    display: flex;
    align-items: center;
    gap: 32px;
    padding: 16px 8px;
    flex-wrap: wrap;
  }
  .disk-detail-stats {
    display: grid;
    grid-template-columns: repeat(3, minmax(110px, 1fr));
    gap: 16px;
    flex: 1;
  }
  .disk-stat-box {
    display: flex;
    flex-direction: column;
    gap: 4px;
    padding: 12px 16px;
    background: var(--surface-muted);
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
  }
  .disk-stat-box .stat-lbl {
    font-size: 12px;
    color: var(--muted);
  }
  .disk-stat-box strong {
    font-size: 16px;
    font-weight: 600;
    color: var(--ink);
  }

  /* --------------------------------------------------------------------------
     MONITORING CARD CLICKABLE
     -------------------------------------------------------------------------- */
  .monitoring-card.clickable {
    cursor: pointer;
    transition: transform 0.15s ease, box-shadow 0.15s ease, border-color 0.15s ease;
  }
  .monitoring-card.clickable:hover {
    border-color: var(--teal);
    box-shadow: var(--shadow-md, 0 4px 12px rgba(0, 0, 0, 0.08));
    transform: translateY(-1px);
  }

  /* --------------------------------------------------------------------------
     RESPONSIVE BREAKPOINTS
     -------------------------------------------------------------------------- */
  @media (max-width: 960px) {
    .app-shell { grid-template-columns: minmax(0, 1fr); }
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
    .sidebar { flex-wrap: wrap; }
    .brand-lockup { padding: 0; flex: 1; }
    .button.mobile-menu-toggle { display: inline-flex; min-height: 40px; }
    .nav-list { display: none; flex: 0 0 100%; width: 100%; min-width: 0; }
    .nav-list.menu-open { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 6px; }
    .nav-item { padding: 10px 12px; white-space: normal; min-height: 44px; }
    .sidebar-footer { display: none; }
    .summary-grid { grid-template-columns: 1fr 1fr; }
    .table-header, .server-row {
      grid-template-columns: 120px 1.5fr 80px 80px 36px;
    }
    .col-rxtx { display: none; }
  }

  @media (max-width: 640px) {
    .nav-list.menu-open { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    .sidebar { padding: 10px 16px; }
    .page { padding: 20px 16px 32px; }
    .page-heading { flex-wrap: wrap; gap: 10px; margin-bottom: 20px; }
    .summary-card { padding: 16px; }
    .summary-grid { gap: 12px; }
    .button, .icon-button { min-height: 40px; }
    .topbar-actions { flex-wrap: wrap; gap: 8px; }
    :global(input), :global(select), :global(textarea) { max-width: 100%; }
    .topbar { padding: 12px 16px; height: auto; min-height: 58px; flex-wrap: wrap; gap: 10px; }
    .settings-grid .panel-heading, .settings-actions { flex-wrap: wrap; }
    .meta-row { flex-wrap: wrap; gap: 8px; overflow-wrap: anywhere; }
    .summary-grid, .lower-grid, .metric-grid, .form-grid, .package-source-fields { grid-template-columns: 1fr; }
    .summary-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
    .summary-card:last-child { grid-column: 1 / -1; }
    .summary-card .card-header { margin-bottom: 6px; }
    .summary-card .stat-value { font-size: 25px; margin-bottom: 6px; }
    .summary-card .stat-label { font-size: 12px; }
    .summary-card .stat-icon-wrap { width: 20px; height: 20px; }
    .summary-card .stat-badge { font-size: 11px; }

    .package-source-fields details { grid-column: auto; }
    .table-header { display: none; }
    .server-row:not(.alert-row) {
      grid-template-columns: minmax(0, 1fr) auto;
      grid-template-areas: 'name action' 'status action';
      gap: 8px 12px;
      padding: 14px 16px;
    }
    .server-row:not(.alert-row) .col-name { grid-area: name; min-width: 0; }
    .server-row:not(.alert-row) .col-status { grid-area: status; }
    .server-row:not(.alert-row) .col-action { grid-area: action; }
    .col-meta { display: none; }
  }
  @media (prefers-reduced-motion: reduce) { :global(*), :global(*::before), :global(*::after) { animation-duration: 0.01ms !important; transition-duration: 0.01ms !important; } }
</style>
