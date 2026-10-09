<script lang="ts">
  import { onMount, tick } from 'svelte';
  import ProcessTable from './ProcessTable.svelte';
  import { networkRates, networkRollupRate } from './network';
  import Icon from './Icon.svelte';
  import Modal from './Modal.svelte';
  import type { InstallProgressData, InstallStage } from './InstallProgress.svelte';
  import Sidebar from './components/Sidebar.svelte';
  import StatusPill from './components/StatusPill.svelte';
  import ServerTable from './components/ServerTable.svelte';
  import OverviewPage from './pages/OverviewPage.svelte';
  import MonitoringPage from './pages/MonitoringPage.svelte';
  import AlertsPage from './pages/AlertsPage.svelte';
  import PackagesPage from './pages/PackagesPage.svelte';
  import JoinCommandPanel from './components/JoinCommandPanel.svelte';
  import SshCredentialFields from './components/SshCredentialFields.svelte';
  import CommandPalette, { type Command } from './components/CommandPalette.svelte';
  import { ApiError, apiClient, mapWithConcurrency, type Account, type UpdateStatus, type UpdateProgress, type AlertState, type Job, type MetricQuery, type StorageNotification, type Module, type Server as ApiServer } from './api';
  import type { DisplayState, PreviewChartData, PreviewServer } from './preview/fixtures';
  import { getSavedTimezone, saveTimezone, formatTimeInTz, formatDateTimeInTz } from './timezone';
  import { PREVIEW_MODE } from './lib/env';
  import { operationKey } from './lib/ops';
  import { displayAddress, stateLabel, sumNetworkRate } from './lib/format';
  import { applyAppearance, loadAppearance, resolveTheme, saveAppearance, type Appearance } from './lib/appearance';
  import type { ConfirmOptions, DialogTone } from './lib/dialogs';

  type Page = 'overview' | 'monitoring' | 'servers' | 'server' | 'alerts' | 'logs' | 'packages' | 'settings' | 'add-server' | 'install-progress' | 'onboarding';
  type DetailTab = 'resources' | 'network' | 'packages' | 'processes' | 'logs' | 'metrics' | 'traffic';
  type ChartRange = '15m' | '1h' | '24h';
  type PreviewState = 'ready' | 'loading' | 'empty' | 'error';
  type SettingsSection = 'general' | 'users' | 'updates' | 'tls' | 'storage' | 'notifications';

  const isMac = typeof navigator !== 'undefined' && /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
  // Plain HTTP to anything but this machine sends the password unencrypted.
  const insecureConnection = typeof window !== 'undefined' && window.location.protocol === 'http:' && !['localhost', '127.0.0.1', '[::1]'].includes(window.location.hostname);
  const totalTrafficBytes = '1526000000000';
  const totalAllowanceBytes = '2500000000000';

  let appearance: Appearance = loadAppearance();
  // The resolved scheme; charts are re-created when it changes so they pick
  // up the new colours.
  let theme: 'light' | 'dark' = resolveTheme(appearance.mode);

  function setAppearance(next: Appearance) {
    appearance = next;
    theme = resolveTheme(next.mode);
    saveAppearance(next);
    applyAppearance(next, true);
  }

  function toggleTheme() {
    setAppearance({ ...appearance, mode: theme === 'light' ? 'dark' : 'light' });
  }

  let currentTimezone = getSavedTimezone();
  function changeTimezone(tz: string) {
    if (tz === currentTimezone) return;
    const oldTz = currentTimezone;
    currentTimezone = tz;
    saveTimezone(tz);
    showSettingsSavedDialog(
      'Timezone updated',
      `Active timezone changed from "${oldTz}" to "${tz}".\n\nAll dashboard charts, event logs, alerts, and time displays now reflect your local time (${formatTimeInTz(new Date(), tz, true)} · ${formatDateTimeInTz(new Date(), tz)}).`
    );
  }

  let servers: PreviewServer[] = [];
  let displayServers: PreviewServer[] = [];
  let activePage: Page = 'overview';
  let selectedServerId = '';
  let detailTab: DetailTab = 'resources';
  let chartRange: ChartRange = '15m';

  let previewState: PreviewState = 'loading';
  let previewLoadFailed = false;
  let apiError = '';
  let partialWarning = '';
  let authExpired = false;
  function focusInput(input: HTMLInputElement) {
    void tick().then(() => { input.focus(); input.select(); });
  }
  function focusLogin(input: HTMLInputElement) {
    const focus = () => {
      if (input.isConnected && !document.querySelector('dialog:modal')) input.focus();
    };
    void tick().then(focus);
    // Signing out can mount this form before its confirmation closes.
    document.addEventListener('close', focus, true);
    return { destroy: () => document.removeEventListener('close', focus, true) };
  }
  let apiAbortController: AbortController | null = null;
  let notificationItems: StorageNotification[] = [];
  let notificationError = '';
  let notificationBusy = false;
  $: unreadNotifications = notificationItems.filter(item => !item.read).length;

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
  async function markNotificationsRead(): Promise<void> {
    try {
      await apiClient.markNotificationsRead();
      await loadNotifications();
      showSettingsSavedDialog('Notifications updated', 'All fleet storage notifications have been marked as read.');
    }
    catch (error) {
      if (error instanceof ApiError && error.authExpired) authExpired = true;
      notificationError = error instanceof ApiError ? error.message : 'Notifications could not be marked read.';
    }
  }
  let settingsSection: SettingsSection = 'general';
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
  let accountError = '';
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
  let updateApplyBusy = false;
  let updateApplyError = '';
  let updateProgress: UpdateProgress | null = null;
  let updatePollTimer: number | undefined;
  let dismissedUpdate = '';
  try { dismissedUpdate = window.localStorage.getItem('payesh-dismissed-update') ?? ''; } catch { /* non-critical */ }
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
  let deleteServerBusy = false;
  let modules: Module[] = [];
  let modulesState: PreviewState = 'loading';
  let modulesError = '';
  // Server the Packages page should target (set when coming from a server).
  let packageServerId = '';
  let packageDialog: Module | null = null;
  let packageDialogServerId = '';
  // Bumped after a package changes so open package lists reload.
  let packagesRefreshKey = 0;
  // The server Packages tab shows catalog names and versions; fetch the
  // (locally cached) catalog the first time it is needed.
  let catalogRequested = false;
  $: if (!PREVIEW_MODE && activePage === 'server' && detailTab === 'packages' && !catalogRequested) { catalogRequested = true; void loadModules(); }
  let alerts: AlertState[] = [];
  let alertsState: PreviewState = 'loading';
  let alertsError = '';

  interface ModalDialogState {
    open: boolean;
    title: string;
    description: string;
    tone: DialogTone;
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


  // Views that are not needed for the first screen load on demand, which keeps
  // the initial bundle small (charts pull in uPlot, for example).
  const lazy = {
    ResourcesPanel: () => import('./pages/server/ResourcesPanel.svelte'),
    NetworkPanel: () => import('./pages/server/NetworkPanel.svelte'),
    PackagesPanel: () => import('./pages/server/PackagesPanel.svelte'),
    LogsPage: () => import('./pages/LogsPage.svelte'),
    GeneralSettings: () => import('./pages/settings/GeneralSettings.svelte'),
    StorageSettings: () => import('./pages/settings/StorageSettings.svelte'),
    NotificationsSettings: () => import('./pages/settings/NotificationsSettings.svelte'),
    UsersSettings: () => import('./pages/settings/UsersSettings.svelte'),
    UpdatesSettings: () => import('./pages/settings/UpdatesSettings.svelte'),
    TlsSettings: () => import('./pages/settings/TlsSettings.svelte'),
    PackageDialog: () => import('./components/PackageDialog.svelte'),
    InstallProgress: () => import('./InstallProgress.svelte'),
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

  function openConfirmModal(opts: ConfirmOptions) {
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

  function showSettingsSavedDialog(title: string, description: string) {
    openConfirmModal({
      title,
      description,
      tone: 'info',
      icon: 'check',
      confirmText: 'Done',
      hideCancel: true,
      action: () => {}
    });
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
  $: hubVersion = servers.find((server) => server.role !== 'node')?.version ?? '';
  $: attentionCount = displayServers.filter((server) => server.displayState !== 'healthy').length;
  const pageTitles: Record<Page, string> = { overview: 'Overview', monitoring: 'Server monitoring', servers: 'Servers', server: 'Server', alerts: 'Alerts', logs: 'Logs', packages: 'Packages', settings: 'Settings', 'add-server': 'Add server', 'install-progress': 'Installation', onboarding: 'Setup' };
  $: pageTitle = activePage === 'server' ? (selectedServer?.name || 'Server details') : activePage === 'install-progress' && activeInstall ? activeInstall.serverName : pageTitles[activePage];
  const settingsSections: { id: SettingsSection; label: string; icon: string }[] = [
    { id: 'general', label: 'General', icon: 'settings' },
    { id: 'tls', label: 'SSL / TLS', icon: 'lock' },
    { id: 'updates', label: 'Versions & updates', icon: 'download' },
    { id: 'storage', label: 'Storage & sampling', icon: 'database' },
    { id: 'notifications', label: 'Notifications', icon: 'bell' },
    { id: 'users', label: 'User management', icon: 'users' }
  ];

  let paletteOpen = false;
  $: paletteCommands = [
    ...(['overview', 'monitoring', 'servers', 'alerts', 'logs', 'packages', 'settings'] as Page[]).map((page): Command => ({ id: `page:${page}`, label: pageTitles[page], icon: page === 'monitoring' ? 'activity' : page === 'logs' ? 'terminal' : page, group: 'Pages', run: () => navigate(page) })),
    ...settingsSections.filter((section) => section.id !== 'users' || myAccount?.role === 'owner').map((section): Command => ({ id: `settings:${section.id}`, label: section.label, icon: section.icon, group: 'Settings', keywords: section.id === 'general' ? 'appearance theme accent timezone density animations' : section.id, run: () => openSettings(section.id) })),
    ...(myAccount?.permission !== 'read' ? [{ id: 'action:add-server', label: 'Add a server', icon: 'plus', group: 'Actions', keywords: 'install ssh join new node', run: () => openAddServer() } satisfies Command] : []),
    { id: 'action:theme', label: `Switch to ${theme === 'light' ? 'dark' : 'light'} theme`, icon: theme === 'light' ? 'moon' : 'sun', group: 'Actions', keywords: 'theme dark light mode appearance', run: toggleTheme } satisfies Command,
    ...(!PREVIEW_MODE && sessionState === 'authenticated' ? [{ id: 'action:sign-out', label: 'Sign out', icon: 'log-out', group: 'Actions', keywords: 'logout exit', run: () => promptSignOut() } satisfies Command] : [])
  ];

  function onGlobalKey(event: KeyboardEvent) {
    if ((event.metaKey || event.ctrlKey) && !event.altKey && event.key.toLowerCase() === 'k') {
      if (!PREVIEW_MODE && authExpired) return;
      event.preventDefault();
      paletteOpen = !paletteOpen;
    }
  }
  $: fleetDownload = sumNetworkRate(displayServers, 'download');
  $: fleetUpload = sumNetworkRate(displayServers, 'upload');
  $: firingAlertCount = alerts.filter((alert) => alert.state === 'firing').length;
  $: overviewTrafficBytes = PREVIEW_MODE ? totalTrafficBytes : displayServers.reduce((total, server) => { try { return (BigInt(total) + BigInt(server.traffic.countedBytes)).toString(); } catch { return total; } }, '0');
  $: overviewAllowanceBytes = PREVIEW_MODE ? totalAllowanceBytes : displayServers.reduce((total, server) => { try { return (BigInt(total) + BigInt(server.traffic.allowanceBytes)).toString(); } catch { return total; } }, '0');
  $: chartData = selectedServer?.metricHistory?.ranges[chartRange] ?? null;
  $: availableTabs = selectedServer ? (['resources', 'network', 'packages', 'processes'] as DetailTab[]).filter((tab) => hasCapability(selectedServer, tab)) : [];
  $: if (selectedServer && availableTabs.length > 0 && !availableTabs.includes(detailTab)) detailTab = availableTabs[0];
  $: if (selectedServer && !labelDraft) labelDraft = selectedServer.name;

  function saveUiState() {
    if (typeof window === 'undefined') return;
    try { window.localStorage.setItem('payesh-ui-state', JSON.stringify({ activePage, selectedServerId, detailTab })); } catch { /* non-critical */ }
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
    if (parts.length === 1 && ['monitoring', 'alerts', 'logs', 'packages', 'settings', 'onboarding'].includes(parts[0])) return { activePage: parts[0] as Page };
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
    if (!activeInstall || !['connecting', 'connected', 'preflight', 'installing', 'enrolling', 'verifying'].includes(activeInstall.currentStage)) {
      saveActiveInstall(null);
    }
    navigate('add-server');
  }

  function navigate(page: Page, serverId = selectedServerId, fromMobileMenu = false) {
    const changed = page !== activePage || serverId !== selectedServerId;
    activePage = page;
    if (fromMobileMenu) void tick().then(() => document.querySelector<HTMLElement>('main h1')?.focus());
    selectedServerId = serverId;
    saveUiState();
    if (typeof window !== 'undefined') {
      window.history.pushState({ activePage, selectedServerId, detailTab }, '', pageLocation(activePage, selectedServerId));
      if (changed) window.scrollTo({ top: 0 });
    }
    if (page === 'server' && !PREVIEW_MODE) void refreshSelectedServer();
    if (page === 'packages' && !PREVIEW_MODE) void loadModules();
    if (page === 'alerts' && !PREVIEW_MODE) void loadAlerts();
    if (page === 'settings' && !PREVIEW_MODE) { void loadAccounts(); void checkLatestUpdate(); void loadNotifications(); }
  }

  function openSettings(section: SettingsSection) {
    settingsSection = section;
    if (section === 'updates' && !PREVIEW_MODE) void checkLatestUpdate();
    if (section === 'notifications') void loadNotifications();
    if (activePage !== 'settings') navigate('settings');
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
    openSettings('updates');
  }

  function startWebUpdate(target: string): void {
    if (updateApplyBusy || webUpdateActive || !target) return;
    openConfirmModal({
      title: `Update Payesh to v${target}?`,
      description: 'The dashboard will restart and be unavailable for a short time. It reloads automatically when the update finishes.',
      tone: 'primary',
      icon: 'refresh',
      confirmText: 'Start update',
      cancelText: 'Not now',
      action: async () => {
        updateApplyBusy = true; updateApplyError = '';
        try {
          const state = await apiClient.applyWebUpdate(target);
          updateProgress = { current: updateStatus?.current ?? '', web_update_supported: true, web_update: state };
          pollUpdateProgress(target);
        } catch (error) { updateApplyError = error instanceof Error ? error.message : 'Could not start the update.'; }
        finally { updateApplyBusy = false; }
      }
    });
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

  function handleOwnPasswordChanged(): void {
    sessionState = 'signed-out'; authExpired = true; navigate('overview');
    showSettingsSavedDialog('Password changed', 'Your account password has been changed. Please sign in again.');
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

  function openPackageDialog(module: Module, serverId = packageServerId) {
    packageDialog = module;
    packageDialogServerId = serverId;
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

  let noticeTimer: number | undefined;
  function showNotice(message: string) {
    notice = message;
    // A newer notice restarts the timer so it is not cleared early.
    window.clearTimeout(noticeTimer);
    noticeTimer = window.setTimeout(() => { notice = ''; }, 4200);
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

  let renaming = false;
  let renameError = '';
  function startRename(): void {
    if (!selectedServer) return;
    labelDraft = selectedServer.name; renameError = ''; renaming = true;
  }

  async function renameSelectedServer(): Promise<void> {
    if (!selectedServer || labelBusy) return;
    const name = labelDraft.trim();
    if (!name || name === selectedServer.name) { renaming = false; return; }
    const id = selectedServer.id;
    labelBusy = true; renameError = '';
    try {
      // Only the name and revision change; keep the loaded metrics and history.
      const updated = PREVIEW_MODE ? { name, configuration_revision: selectedServer.configurationRevision } : await apiClient.updateServerLabel(id, name, selectedServer.configurationRevision, operationKey('label'));
      servers = servers.map((server) => server.id === id ? { ...server, name: updated.name, configurationRevision: updated.configuration_revision } : server);
      labelDraft = updated.name;
      renaming = false;
      showNotice('Server renamed.');
    } catch (error) {
      renameError = error instanceof Error ? error.message : 'Unable to rename this server.';
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

  function hasCapability(server: PreviewServer, capability: DetailTab): boolean {
    if (capability === 'processes') return server.role === 'standalone' || server.role === 'hub';
    if (capability === 'resources' || capability === 'network' || capability === 'packages' || capability === 'metrics' || capability === 'traffic') return true;
    return server.capabilities?.includes(capability) ?? false;
  }

  function tabLabel(tab: DetailTab): string {
    if (tab === 'resources' || tab === 'metrics') return 'Resources';
    if (tab === 'network' || tab === 'traffic') return 'Network';
    if (tab === 'packages') return 'Packages';
    if (tab === 'logs') return 'Logs';
    if (tab === 'processes') return 'Processes';
    return String(tab);
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
    selectedServerId = '';
    try {
      const preview = await import('./preview/fixtures');
      servers = preview.getPreviewServers();
      selectedServerId = servers[0]?.id ?? '';
      modulesState = 'ready';
      modules = [
        { id: 'cpu-controls', name: 'CPU Controls', description: 'Real-time CFS quota throttling and noisy neighbor containment via Linux cgroups v2.', latest_version: '0.1.0', resource_estimate_source: 'static' },
        { id: 'bandwidth-controls', name: 'Bandwidth Controls', description: 'Ingress and egress traffic shaping with tc / fq_codel queueing disciplines.', latest_version: '0.1.0', resource_estimate_source: 'static' },
        { id: 'port-traffic', name: 'Port Traffic Accounting', description: 'Per-port iptables and nftables telemetry collection with byte counters.', latest_version: '0.1.0', resource_estimate_source: 'static' }
      ];
      alertsState = 'ready';
      alerts = [
        { id: 'alert-1', rule_id: 'high-cpu-utilization', server_id: 'server-us-00000002', state: 'pending', last_value: 74.0, last_observation: '2026-09-09T14:34:18Z' },
        { id: 'alert-2', rule_id: 'host-heartbeat-stale', server_id: 'server-us-00000002', state: 'firing', last_observation: '2026-09-09T14:34:18Z' }
      ];
      updateStatus = preview.previewUpdateStatus;
      notificationItems = preview.previewNotifications;
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

  function restoreState(state: { activePage?: Page; selectedServerId?: string; detailTab?: DetailTab } | null) {
    if (!state) return;
    if (state.activePage && ['overview', 'monitoring', 'servers', 'server', 'alerts', 'logs', 'packages', 'settings', 'add-server', 'onboarding'].includes(state.activePage)) activePage = state.activePage;
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
    applyAppearance(appearance);
    // Follow the operating system while the theme is set to System.
    const colorScheme = window.matchMedia?.('(prefers-color-scheme: dark)');
    const onSchemeChange = () => { if (appearance.mode === 'system') { theme = resolveTheme('system'); applyAppearance(appearance, true); } };
    colorScheme?.addEventListener?.('change', onSchemeChange);
    savedUiState = { ...(savedUiState ?? {}), ...stateFromLocation() };
    if (savedUiState.activePage) activePage = savedUiState.activePage;
    if (savedUiState.selectedServerId) selectedServerId = savedUiState.selectedServerId;
    if (completedInstallServerId && activePage === 'install-progress') {
      activePage = 'server'; selectedServerId = completedInstallServerId; detailTab = 'metrics'; saveUiState();
    }
    // Record the canonical URL before loading: the loaders reset the selection
    // until data arrives.
    window.history.replaceState({ activePage, selectedServerId, detailTab }, '', pageLocation(activePage, selectedServerId));
    if (PREVIEW_MODE) void loadPreviewData(); else void loadApiData();
    const onPopState = (event: PopStateEvent) => {
      restoreState({ ...(event.state ?? {}), ...stateFromLocation() });
      if (activePage === 'packages' && !PREVIEW_MODE) void loadModules();
      if (activePage === 'alerts' && !PREVIEW_MODE) void loadAlerts();
      saveUiState();
    };
    window.addEventListener('popstate', onPopState);
    const notificationRefresh = window.setInterval(() => void loadNotifications(), 60000);
    const liveRefresh = window.setInterval(() => { void refreshSelectedServer(); void refreshMonitoring(); }, 15000);
    // Refreshes pause while the tab is hidden; catch up as soon as it is shown.
    const onVisibility = () => { if (!document.hidden) { void refreshSelectedServer(); void refreshMonitoring(); } };
    document.addEventListener('visibilitychange', onVisibility);
    return () => {
      window.clearInterval(liveRefresh);
      window.clearInterval(notificationRefresh);
      window.clearTimeout(updatePollTimer);
      window.removeEventListener('popstate', onPopState);
      colorScheme?.removeEventListener?.('change', onSchemeChange);
      document.removeEventListener('visibilitychange', onVisibility);
      apiAbortController?.abort();
      jobPollController?.abort();
    };
  });
</script>

<svelte:window on:keydown={onGlobalKey} />

<svelte:head>
  <title>Payesh — Cloud Fleet Operations</title>
  <meta name="description" content="Payesh lightweight local-first Linux fleet monitoring and management" />
</svelte:head>

{#snippet chunkFailed()}
  <div class="unavailable-panel">
    <strong>This view could not be loaded</strong>
    <span>The dashboard may have been updated in the meantime. Reload the page to continue.</span>
    <button class="button ghost small" type="button" on:click={() => window.location.reload()}><Icon name="refresh" size={13} />Reload</button>
  </div>
{/snippet}

<a class="skip-link" href="#main-content">Skip to content</a>
<div class="app-shell">
  <Sidebar
    {activePage}
    serverCount={servers.length}
    firingAlerts={firingAlertCount}
    statusLabel={PREVIEW_MODE ? 'Fixture preview' : sessionState === 'authenticated' && !authExpired ? 'Connected to hub' : 'Sign in required'}
    statusTone={PREVIEW_MODE ? 'warn' : sessionState === 'authenticated' && !authExpired ? 'ok' : 'off'}
    user={!authExpired ? myAccount?.username ?? '' : ''}
    inert={!PREVIEW_MODE && authExpired}
    onNavigate={(page, fromMenu) => navigate(page, selectedServerId, fromMenu)}
  />

  <main class="main-content" id="main-content" tabindex="-1">
    <header class="topbar" inert={!PREVIEW_MODE && authExpired}>
      <nav class="breadcrumbs" aria-label="Breadcrumb">
        <span class="crumb-root">Workspace</span>
        <Icon name="chevron-right" size={13} />
        {#if activePage === 'server' || activePage === 'install-progress'}
          <button class="crumb-link" type="button" on:click={() => navigate('servers')}>Servers</button>
          <Icon name="chevron-right" size={13} />
        {/if}
        <strong class="crumb-current" aria-current="page">{pageTitle}</strong>
      </nav>

      <div class="topbar-actions">
        <button class="search-trigger" type="button" on:click={() => (paletteOpen = true)} aria-label="Go to page, server or setting (Ctrl+K)">
          <Icon name="search" size={14} /><span>Go to…</span><kbd>{isMac ? '⌘' : 'Ctrl'} K</kbd>
        </button>
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
        <button class="icon-button theme-toggle" type="button" on:click={toggleTheme} aria-label={`Switch to ${theme === 'light' ? 'dark' : 'light'} theme`} title={`Switch to ${theme === 'light' ? 'dark' : 'light'} theme`}>
          {#key theme}<span class="theme-icon"><Icon name={theme === 'light' ? 'moon' : 'sun'} size={16} /></span>{/key}
        </button>
        {#if !PREVIEW_MODE && sessionState === 'authenticated'}
          <button class="icon-button" type="button" on:click={() => openSettings('notifications')} aria-label={`Notifications${unreadNotifications ? ` (${unreadNotifications} unread)` : ''}`} title="Notifications">
            <Icon name="bell" size={16} />
            {#if unreadNotifications > 0}
              <span class="notification-badge" aria-hidden="true">{unreadNotifications > 99 ? '99+' : unreadNotifications}</span>
            {/if}
          </button>
          <button class="icon-button" type="button" on:click={() => promptSignOut()} aria-label="Sign out" title="Sign out"><Icon name="log-out" size={16} /></button>
        {/if}
        {#if myAccount?.permission !== 'read'}
          <button class="button primary small add-server" type="button" on:click={() => openAddServer()}>
            <Icon name="plus" size={14} />
            <span>Add server</span>
          </button>
        {/if}
      </div>
    </header>

    <div class="banners">
      {#if signOutError}<div class="partial-warning" role="alert"><Icon name="alert-triangle" size={15} /><span>{signOutError}</span></div>{/if}
      {#if showUpdateBanner && updateStatus}
        <div class="partial-warning update-banner" role="status"><Icon name="download" size={15} /><span>Payesh v{updateStatus.latest} is available{updateStatus.current && updateStatus.current !== 'dev' ? ` (installed: v${updateStatus.current})` : ''}. <button class="link-button" type="button" on:click={openUpdateSettings}>View update</button></span><button class="link-button" type="button" aria-label="Dismiss update notice" on:click={dismissUpdateBanner}>Dismiss</button></div>
      {/if}
      {#if partialWarning}<div class="partial-warning" role="status"><Icon name="alert-triangle" size={15} /><span>{partialWarning}</span></div>{/if}
      {#if insecureConnection && !PREVIEW_MODE && sessionState === 'authenticated'}
        <div class="partial-warning" role="status"><Icon name="lock" size={15} /><span>This connection is not encrypted (no SSL). <button class="link-button" type="button" on:click={() => openSettings('tls')}>Add a domain</button> to turn on HTTPS automatically.</span></div>
      {/if}
    </div>

    <!-- A rendering error inside one page must not blank the whole dashboard. -->
    <svelte:boundary onerror={(error) => console.error('payesh: page failed to render', error)}>
    {#if !PREVIEW_MODE && authExpired}
      <section class="login-screen" aria-label="Sign in">
        <div class="login-card">
          <svg class="login-mark" viewBox="0 0 32 32" aria-hidden="true"><rect width="32" height="32" rx="9" /><path d="M6 18h4.2l2.6-6.5 4.4 11 3-7.5H26" /></svg>
          <h1>Sign in to Payesh</h1>
          <p class="muted center">Authenticate to access fleet operations.</p>
          <form class="auth-form" on:submit|preventDefault={() => void submitLogin()}>
            <label>
              <span>Username</span>
              <input use:focusLogin bind:value={authUsername} autocomplete="username" placeholder="admin" required />
            </label>
            <label>
              <span>Password</span>
              <input type="password" bind:value={authPassword} autocomplete="current-password" placeholder="••••••••••••" required />
            </label>
            {#if authError}<p class="form-error" role="alert">{authError}</p>{/if}
            {#if insecureConnection}<p class="insecure-login" role="note"><Icon name="alert-triangle" size={14} /> Not encrypted: your password is sent without SSL. Add a domain in Settings to enable HTTPS.</p>{/if}
            <button class="button primary login-submit" type="submit" disabled={authBusy}>
              {#if authBusy}<span class="version-spinner" aria-hidden="true"></span>Signing in…{:else}Sign in{/if}
            </button>
          </form>
        </div>
      </section>
    {:else if activePage === 'monitoring'}
      <MonitoringPage servers={displayServers} timezone={currentTimezone} download={fleetDownload} upload={fleetUpload} onSelect={selectServer} />
    {:else if activePage === 'servers'}
      <section class="page" aria-labelledby="servers-title">
        <div class="page-heading">
          <div>
            <h1 id="servers-title" tabindex="-1">Servers</h1>
            <p class="lede">{servers.length} {servers.length === 1 ? 'server' : 'servers'} · {servers.length - attentionCount} healthy</p>
          </div>
          {#if myAccount?.permission !== 'read'}<button class="button primary" type="button" on:click={() => openAddServer()}><Icon name="plus" size={15} /><span>Add server</span></button>{/if}
        </div>
        {#if servers.length}
          <ServerTable servers={displayServers} timezone={currentTimezone} onSelect={selectServer} />
        {:else}
          <div class="state-panel"><div class="state-icon"><Icon name="servers" size={24} /></div><h2>No servers yet</h2><p>Add a Linux server over SSH or with a one-line join command.</p></div>
        {/if}
      </section>

    {:else if activePage === 'add-server'}
      <section class="page narrow" aria-labelledby="add-server-title">
        <button class="back-link" type="button" on:click={() => navigate('servers')}><Icon name="arrow-left" size={15} /><span>Back to servers</span></button>
        <div class="page-heading">
          <div>
            <h1 id="add-server-title" tabindex="-1">Add a server</h1>
            <p class="lede">Connect over SSH. Payesh detects the operating system and architecture automatically.</p>
          </div>
        </div>

        {#if activeInstall && ['connecting', 'connected', 'preflight', 'installing', 'enrolling', 'verifying'].includes(activeInstall.currentStage)}
          <div class="active-install-banner" role="status">
            <div class="banner-left"><span class="pulse-dot"></span><span>Deployment in progress for <strong>{activeInstall.serverName}</strong> ({activeInstall.simulatedProgress}%)</span></div>
            <button class="button small ghost" type="button" on:click={() => navigate('install-progress', activeInstall?.serverId)}><span>View live log</span><Icon name="arrow-right" size={13} /></button>
          </div>
        {/if}

        <article class="panel method">
          <div class="panel-heading"><div class="title"><span class="section-icon"><Icon name="terminal" size={16} /></span><div><h2>Install over SSH</h2><span class="muted">Payesh connects, verifies the host key, and installs the agent for you.</span></div></div></div>
          <form class="form-grid" on:submit|preventDefault={() => void createPendingServer()}>
            <label>Server name<input bind:value={newServerName} maxlength="128" placeholder="e.g. EU-Node-01" required /></label>
            <SshCredentialFields bind:host={installHost} bind:port={installPort} bind:user={installUser} bind:method={installAuthMethod} bind:password={installPassword} bind:key={installKey} bind:passphrase={installKeyPassphrase} bind:fingerprint={installFingerprint} />
            <div class="setup-actions">
              <button class="button ghost" type="button" on:click={() => navigate('servers')}>Cancel</button>
              <button class="button primary" type="submit" disabled={createServerBusy || (installAuthMethod === 'password' ? !installPassword : !installKey)}>
                {#if createServerBusy}<span class="version-spinner" aria-hidden="true"></span>Connecting…{:else}Add and install server{/if}
              </button>
            </div>
          </form>
          {#if jobError}<p class="form-error" role="alert">{jobError}</p>{/if}
        </article>

        <div class="or-divider" aria-hidden="true"><span>or</span></div>

        <JoinCommandPanel onCreated={(created) => (servers = [...servers, emptyApiServer(created)])} />
      </section>

    {:else if activePage === 'install-progress'}
      <section class="page" aria-labelledby="install-progress-title">
        <button class="back-link" type="button" on:click={() => navigate('servers')}><Icon name="arrow-left" size={15} /><span>Back to servers</span></button>
        <div class="page-heading">
          <div>
            <h1 id="install-progress-title" tabindex="-1">{!activeInstall ? 'Server installation' : activeInstall.currentStage === 'failed' ? `Could not install ${activeInstall.serverName}` : activeInstall.currentStage === 'cancelled' ? `Installation of ${activeInstall.serverName} cancelled` : `Installing ${activeInstall.serverName}`}</h1>
            <p class="lede">{!activeInstall ? 'No active deployment found.' : activeInstall.currentStage === 'failed' || activeInstall.currentStage === 'cancelled' ? `SSH target ${activeInstall.user}@${activeInstall.host}:${activeInstall.port}.` : `Connecting over SSH to ${activeInstall.host}:${activeInstall.port} and monitoring deployment progress.`}</p>
          </div>
        </div>

        {#if activeInstall}
          {#await lazy.InstallProgress() then { default: InstallProgress }}
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
          {:catch}{@render chunkFailed()}{/await}
        {:else}
          <div class="state-panel">
            <div class="state-icon"><Icon name="servers" size={28} /></div>
            <h2>No active deployment</h2>
            <p>Select a server from the fleet or add a new server to start an installation.</p>
            <button class="button primary" type="button" on:click={() => openAddServer()}><Icon name="plus" size={15} /><span>Add a server</span></button>
          </div>
        {/if}
      </section>

    {:else if activePage === 'alerts'}
      <AlertsPage {alerts} loadState={alertsState} error={alertsError} {servers} timezone={currentTimezone} onReload={() => void loadAlerts()} />
    {:else if activePage === 'logs'}
      {#await lazy.LogsPage() then { default: LogsPage }}<LogsPage {servers} timezone={currentTimezone} onAuthExpired={() => (authExpired = true)} />{:catch}{@render chunkFailed()}{/await}
    {:else if activePage === 'packages'}
      <PackagesPage {modules} loadState={modulesState} error={modulesError} packageError="" readOnly={myAccount?.permission === 'read'} busy={!!packageDialog} hasServers={servers.length > 0} onRefresh={() => void loadModules(true)} onInstall={(module) => openPackageDialog(module)} />

    {:else if activePage === 'settings'}
      <section class="page" aria-labelledby="settings-title">
        <div class="page-heading">
          <div>
            <h1 id="settings-title" tabindex="-1">Settings</h1>
            <p class="lede">Hub configuration and your personal preferences.</p>
          </div>
        </div>

        <div class="settings-layout">
          <nav class="settings-nav" aria-label="Settings sections" on:focusin={(event) => {
            // Keyboard focus only: scrolling on mouse down moves the button away from the pointer and swallows the click.
            if (event.target instanceof HTMLButtonElement && event.target.matches(':focus-visible')) event.target.scrollIntoView({ block: 'nearest', inline: 'nearest' });
          }}>
            {#each settingsSections as section (section.id)}
              {#if section.id !== 'users' || myAccount?.role === 'owner'}
                <button type="button" aria-current={settingsSection === section.id ? 'page' : undefined} class:chosen={settingsSection === section.id} on:click={() => openSettings(section.id)}>
                  <Icon name={section.icon} size={15} />
                  <span>{section.label}</span>
                  {#if section.id === 'notifications' && unreadNotifications}<span class="nav-badge">{unreadNotifications}</span>{/if}
                </button>
              {/if}
            {/each}
          </nav>
          <div class="settings-content">
            {#key settingsSection}
              <div class="settings-section">
                {#if settingsSection === 'general'}
                  {#await lazy.GeneralSettings() then { default: GeneralSettings }}<GeneralSettings {appearance} timezone={currentTimezone} onAppearance={setAppearance} onTimezone={changeTimezone} />{:catch}{@render chunkFailed()}{/await}
                {:else if settingsSection === 'storage'}
                  {#await lazy.StorageSettings() then { default: StorageSettings }}<StorageSettings isOwner={myAccount?.role === 'owner'} onSaved={showSettingsSavedDialog} onAuthExpired={() => (authExpired = true)} onNotificationsChanged={() => void loadNotifications()} />{:catch}{@render chunkFailed()}{/await}
                {:else if settingsSection === 'notifications'}
                  {#await lazy.NotificationsSettings() then { default: NotificationsSettings }}<NotificationsSettings items={notificationItems} busy={notificationBusy} error={notificationError} canEdit={myAccount?.permission === 'edit'} timezone={currentTimezone} onRefresh={() => void loadNotifications()} onMarkRead={() => void markNotificationsRead()} />{:catch}{@render chunkFailed()}{/await}
                {:else if settingsSection === 'users' && myAccount?.role === 'owner'}
                  {#await lazy.UsersSettings() then { default: UsersSettings }}<UsersSettings {accounts} currentUser={myAccount.username} onChanged={loadAccounts} onSaved={showSettingsSavedDialog} onConfirm={openConfirmModal} onNotice={showNotice} onOwnPasswordChanged={handleOwnPasswordChanged} />{:catch}{@render chunkFailed()}{/await}
                  {#if accountError}<p class="form-error" role="alert">{accountError}</p>{/if}
                {:else if settingsSection === 'updates'}
                  {#await lazy.UpdatesSettings() then { default: UpdatesSettings }}<UpdatesSettings status={updateStatus} installedVersion={updateStatus?.current ? `v${updateStatus.current}` : hubVersion ? `v${hubVersion}` : 'Unavailable'} checkBusy={updateCheckBusy} checkError={updateCheckError} applyBusy={updateApplyBusy} applyError={updateApplyError} {webUpdate} isOwner={myAccount?.role === 'owner'} {isNewerVersion} onCheck={() => void checkLatestUpdate()} onUpdate={startWebUpdate} />{:catch}{@render chunkFailed()}{/await}
                {:else if settingsSection === 'tls'}
                  {#await lazy.TlsSettings() then { default: TlsSettings }}<TlsSettings readOnly={myAccount?.permission === 'read'} {insecureConnection} onSaved={showSettingsSavedDialog} onConfirm={openConfirmModal} onNotice={showNotice} onAuthExpired={() => (authExpired = true)} />{:catch}{@render chunkFailed()}{/await}
                {/if}
              </div>
            {/key}
          </div>
        </div>
      </section>

    {:else if activePage === 'onboarding' && (PREVIEW_MODE || sessionState !== 'authenticated')}
      <section class="page narrow onboarding-page" aria-labelledby="setup-title">
        <div class="page-heading">
          <div>
            <h1 id="setup-title" tabindex="-1">Set up your hub</h1>
            <p class="lede">Create the owner account to initialize this Payesh hub.</p>
          </div>
        </div>
        <ol class="stepper" aria-label="Setup progress">
          {#each ['Access', 'Defaults', 'Enroll'] as label, index}
            <li class:current={setupStep === index + 1} class:done={setupStep > index + 1} class="step" aria-current={setupStep === index + 1 ? 'step' : undefined}>
              <span>{#if setupStep > index + 1}<Icon name="check" size={12} />{:else}{index + 1}{/if}</span>
              {label}
            </li>
          {/each}
        </ol>
        <div class="panel onboarding-card">
          {#key setupStep}
            <div class="step-body">
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
            </div>
          {/key}
          {#if setupError}<p class="form-error" role="alert">{setupError}</p>{/if}
          <div class="setup-actions">
            <button class="button ghost" type="button" on:click={() => setupStep > 1 ? setupStep -= 1 : navigate('overview')}>{setupStep > 1 ? 'Back' : 'Cancel'}</button>
            <button class="button primary" type="button" on:click={() => void completeOnboarding()}>{setupStep === 3 ? (enrollmentMode === 'connect' ? 'Queue enrollment' : 'Save setup') : 'Continue'}</button>
          </div>
        </div>
      </section>

    {:else if activePage === 'server' && selectedServer}
      <section class="page server-page" aria-labelledby="server-title">
        <button class="back-link" type="button" on:click={() => navigate('servers')}><Icon name="arrow-left" size={15} /><span>All servers</span></button>

        <div class="page-heading server-heading">
          <div class="server-title">
            <span class="server-avatar" aria-hidden="true"><Icon name="server" size={20} /></span>
            <div>
              <div class="heading-row">
                {#if renaming}
                  <form class="rename" on:submit|preventDefault={() => void renameSelectedServer()}>
                    <input bind:value={labelDraft} maxlength="128" aria-label="Server name" disabled={labelBusy} use:focusInput on:keydown={(event) => { if (event.key === 'Escape') renaming = false; }} />
                    <button class="button primary small" type="submit" disabled={labelBusy || !labelDraft.trim()}>{labelBusy ? 'Saving…' : 'Save'}</button>
                    <button class="button ghost small" type="button" disabled={labelBusy} on:click={() => (renaming = false)}>Cancel</button>
                  </form>
                {:else}
                  <h1 id="server-title" tabindex="-1">{selectedServer.name}</h1>
                  {#if myAccount?.permission !== 'read'}<button class="icon-button rename-button" type="button" aria-label="Rename server" title="Rename" on:click={startRename}><Icon name="pencil" size={14} /></button>{/if}
                {/if}
                <StatusPill state={selectedServer.displayState} label={stateLabel(selectedServer.displayState)} />
              </div>
              <p class="lede mono">{selectedServer.platform} · {selectedServer.architecture} · v{selectedServer.version}</p>
              {#if renameError}<p class="form-error" role="alert">{renameError}</p>{/if}
            </div>
          </div>
          <div class="server-heading-actions">
            {#if !PREVIEW_MODE && selectedServer.role === 'node' && myAccount?.permission !== 'read'}
              <button class="button danger small" type="button" disabled={deleteServerBusy} on:click={() => promptDeleteServer()}>
                <Icon name="trash" size={14} />
                <span>{deleteServerBusy ? 'Deleting…' : 'Delete server'}</span>
              </button>
            {/if}
          </div>
        </div>

        <dl class="server-meta-bar">
          <div class="meta-item"><dt>Address</dt><dd class="mono">{displayAddress(selectedServer)}</dd></div>
          <div class="meta-item"><dt>Role</dt><dd class="capitalize">{selectedServer.role === 'node' ? 'Node' : 'Master'}</dd></div>
          <div class="meta-item"><dt>Connection</dt><dd class="capitalize"><span class={`dot ${selectedServer.connectionState}`}></span>{selectedServer.connectionState.replace('-', ' ')}</dd></div>
          <div class="meta-item"><dt>Freshness</dt><dd class="capitalize">{selectedServer.freshnessState}</dd></div>
          <div class="meta-item"><dt>Revision</dt><dd class="mono">rev {selectedServer.configurationRevision}</dd></div>
          {#if selectedServer.freshnessReason}
            <div class="meta-item note"><dt>Note</dt><dd>{selectedServer.freshnessReason}</dd></div>
          {/if}
        </dl>

        {#if activeInstall && activeInstall.serverId === selectedServer.id}
          {#await lazy.InstallProgress() then { default: InstallProgress }}
          <InstallProgress
            install={activeInstall}
            compact={true}
            onCancel={() => promptCancelInstall()}
            onViewServer={(id) => { saveActiveInstall(null); detailTab = 'metrics'; navigate('server', id); }}
            onRetry={() => { saveActiveInstall(null); }}
            onAddAnother={() => { saveActiveInstall(null); navigate('add-server'); }}
          />
          {:catch}{@render chunkFailed()}{/await}
        {:else if !PREVIEW_MODE && selectedServer.role === 'node' && selectedServer.connectionState !== 'connected'}
          <article class="panel server-actions">
            <div class="panel-heading"><div class="title"><span class="section-icon"><Icon name="terminal" size={16} /></span><div><h2>Install over SSH</h2><span class="muted">This node has not connected yet. Install or reinstall the agent.</span></div></div></div>
            <form class="form-grid" on:submit|preventDefault={() => void submitInstall()}>
              <SshCredentialFields bind:host={installHost} bind:port={installPort} bind:user={installUser} bind:method={installAuthMethod} bind:password={installPassword} bind:key={installKey} bind:passphrase={installKeyPassphrase} bind:fingerprint={installFingerprint} hostLabel="SSH host" showHint={false} />
              <div class="setup-actions">
                <button class="button primary" type="submit" disabled={installBusy || (installAuthMethod === 'password' ? !installPassword : !installKey)}>
                  {#if installBusy}<span class="version-spinner" aria-hidden="true"></span>Connecting…{:else}Install Payesh{/if}
                </button>
              </div>
            </form>
            {#if jobError}<p class="form-error" role="alert">{jobError}</p>{/if}
          </article>
        {/if}

        {#if selectedServer.connectionState === 'connected' && availableTabs.length > 0}
          <div class="tabs" role="tablist" aria-label="Server detail sections">
            {#each availableTabs as tab}
              <button class:active={detailTab === tab} type="button" role="tab" aria-selected={detailTab === tab} on:click={() => { detailTab = tab; saveUiState(); }}>
                <Icon name={tab === 'processes' ? 'cpu' : tab === 'network' ? 'activity' : tab === 'packages' ? 'packages' : 'overview'} size={14} />
                {tabLabel(tab)}
              </button>
            {/each}
          </div>
        {/if}

        {#if selectedServer.connectionState !== 'connected'}
          <div class="unavailable-panel large"><Icon name="radio" size={22} /><strong>Node is not connected</strong><span>Metrics and telemetry will appear after the node reconnects.</span></div>
        {:else if detailTab === 'packages'}
          <div class="tab-body">
            {#key selectedServer.id}
              {#await lazy.PackagesPanel() then { default: PackagesPanel }}<PackagesPanel server={selectedServer} catalog={modules} timezone={currentTimezone} readOnly={myAccount?.permission === 'read'} refreshKey={packagesRefreshKey} onManage={(module) => openPackageDialog(module, selectedServer.id)} onBrowse={() => { packageServerId = selectedServer.id; navigate('packages'); }} onConfirm={openConfirmModal} onNotice={showNotice} onAuthExpired={() => (authExpired = true)} />{:catch}{@render chunkFailed()}{/await}
            {/key}
          </div>
        {:else if detailTab === 'processes'}
          {#key selectedServer.id}<ProcessTable serverId={selectedServer.id} onPackages={() => { packageServerId = selectedServer.id; navigate('packages'); }} />{/key}
        {:else if (detailTab === 'resources' || detailTab === 'metrics') && (hasCapability(selectedServer, 'resources') || hasCapability(selectedServer, 'metrics'))}
          <div class="tab-body">
            {#await lazy.ResourcesPanel() then { default: ResourcesPanel }}<ResourcesPanel server={selectedServer} {chartData} bind:range={chartRange} timezone={currentTimezone} {theme} />{:catch}{@render chunkFailed()}{/await}
          </div>
        {:else if (detailTab === 'network' || detailTab === 'traffic') && (hasCapability(selectedServer, 'network') || hasCapability(selectedServer, 'traffic'))}
          <div class="tab-body">
            {#key selectedServer.id}
              {#await lazy.NetworkPanel() then { default: NetworkPanel }}<NetworkPanel server={selectedServer} {chartData} bind:range={chartRange} timezone={currentTimezone} {theme} onAuthExpired={() => (authExpired = true)} />{:catch}{@render chunkFailed()}{/await}
            {/key}
          </div>
        {:else if selectedServer && detailTab !== 'metrics' && !hasCapability(selectedServer, detailTab)}
          <div class="unavailable-panel large"><strong>{tabLabel(detailTab)} unavailable</strong><span>{capabilityMessage(selectedServer, detailTab)}</span></div>
        {:else if selectedServer && !hasCapability(selectedServer, 'metrics')}
          <div class="unavailable-panel large"><strong>Monitoring data unavailable</strong><span>{capabilityMessage(selectedServer, 'metrics')}</span></div>
        {/if}
      </section>

    {:else}
      <OverviewPage
        servers={displayServers}
        loadState={previewState}
        {apiError}
        readOnly={myAccount?.permission === 'read'}
        timezone={currentTimezone}
        download={fleetDownload}
        upload={fleetUpload}
        trafficBytes={overviewTrafficBytes}
        allowanceBytes={overviewAllowanceBytes}
        {latestJob}
        {jobError}
        {jobBusy}
        onSelect={selectServer}
        onAddServer={() => openAddServer()}
        onNavigate={(page) => navigate(page)}
        onRetry={() => PREVIEW_MODE ? void loadPreviewData() : void loadApiData()}
        onCancelJob={() => promptCancelInstall()}
        onReloadJob={() => void pollJob(latestJob?.id ?? '')}
      />
    {/if}
    {#snippet failed(error, reset)}
      <section class="page">
        <div class="state-panel error-state">
          <div class="state-icon"><Icon name="alert-triangle" size={26} /></div>
          <h2>This page could not be shown</h2>
          <p>{error instanceof Error ? error.message : 'An unexpected error occurred.'} Other pages keep working.</p>
          <div class="job-actions"><button class="button primary" type="button" on:click={reset}>Try again</button><button class="button ghost" type="button" on:click={() => { reset(); navigate('overview'); }}>Go to overview</button></div>
        </div>
      </section>
    {/snippet}
    </svelte:boundary>
  </main>
</div>

<CommandPalette bind:open={paletteOpen} commands={paletteCommands} servers={displayServers} onServer={selectServer} />

{#if notice}{#key notice}<div class="notice" role="status"><Icon name="check" size={15} /><span>{notice}</span></div>{/key}{/if}
{#if packageDialog}
  {#await lazy.PackageDialog() then { default: PackageDialog }}<PackageDialog module={packageDialog} {servers} initialServerId={packageDialogServerId} onClose={() => (packageDialog = null)} onChanged={() => (packagesRefreshKey += 1)} onNotice={showNotice} onAuthExpired={() => (authExpired = true)} />{:catch}{@render chunkFailed()}{/await}
{/if}

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
  /* ------------------------------------------------------------------------
     Shell
     ------------------------------------------------------------------------ */
  .app-shell { display: grid; grid-template-columns: 236px minmax(0, 1fr); min-height: 100vh; }
  .main-content { min-width: 0; }
  .main-content:focus { outline: none; }
  .skip-link { position: fixed; z-index: 2000; top: 10px; left: 10px; padding: 8px 14px; border-radius: var(--radius-md); background: var(--accent); color: var(--accent-contrast); font-weight: 600; text-decoration: none; transform: translateY(-160%); transition: transform var(--dur) var(--ease-out); }
  .skip-link:focus { transform: none; }

  .topbar {
    position: sticky;
    top: 0;
    z-index: 40;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 16px;
    height: 58px;
    padding: 0 var(--page-pad-x);
    border-bottom: 1px solid var(--line);
    background: var(--surface);
  }
  .breadcrumbs { display: flex; flex: 1; align-items: center; gap: 8px; min-width: 0; color: var(--muted); font-size: 13px; }
  .breadcrumbs :global(svg) { opacity: 0.5; }
  .crumb-link { padding: 0; border: 0; background: none; color: var(--muted); font-size: 13px; }
  .crumb-link:hover { color: var(--ink); text-decoration: underline; text-underline-offset: 3px; }
  .crumb-current { overflow: hidden; color: var(--ink); font-weight: 620; text-overflow: ellipsis; white-space: nowrap; }
  .topbar-actions { display: flex; align-items: center; gap: 8px; }
  .search-trigger {
    display: inline-flex;
    align-items: center;
    gap: 8px;
    min-width: 200px;
    height: 36px;
    padding: 0 8px 0 12px;
    border: 1px solid var(--line);
    border-radius: var(--radius-md);
    background: var(--surface-muted);
    color: var(--muted);
    font-size: 13px;
    transition: border-color var(--transition-fast), color var(--transition-fast);
  }
  .search-trigger:hover { border-color: var(--line-strong); color: var(--ink-secondary); }
  .search-trigger span { flex: 1; text-align: left; }
  .search-trigger kbd { padding: 1px 6px; border: 1px solid var(--line); border-radius: 4px; background: var(--surface); font-family: var(--font-sans); font-size: 10.5px; }
  .preview-control { display: flex; align-items: center; gap: 6px; color: var(--muted); font-size: 12px; }
  .preview-control select { padding-top: 5px; padding-bottom: 5px; }
  .theme-icon { display: grid; place-items: center; animation: theme-spin var(--dur-slow) var(--ease-spring); }
  @keyframes theme-spin { from { opacity: 0; transform: rotate(-90deg) scale(0.6); } to { opacity: 1; transform: none; } }
  .notification-badge {
    position: absolute;
    top: -5px;
    right: -5px;
    min-width: 17px;
    height: 17px;
    padding: 0 4px;
    border: 2px solid var(--surface);
    border-radius: var(--radius-pill);
    background: var(--danger);
    color: #fff;
    font-size: 9.5px;
    font-weight: 700;
    line-height: 13px;
    text-align: center;
    pointer-events: none;
    animation: pop-in var(--dur) var(--ease-spring);
  }
  .banners:empty { display: none; }
  .update-banner { justify-content: space-between; }

  /* ------------------------------------------------------------------------
     Pages
     ------------------------------------------------------------------------ */
  :global(.page) {
    max-width: 1320px;
    margin: 0 auto;
    padding: var(--page-pad-y) var(--page-pad-x) 56px;
    animation: rise-in var(--dur-slow) var(--ease-out);
  }
  :global(.page.narrow) { max-width: 900px; }
  :global(.page-heading) { display: flex; align-items: flex-start; justify-content: space-between; gap: 20px; margin-bottom: 24px; }
  :global(.page-heading .lede) { margin-top: 2px; }

  /* Sign in ------------------------------------------------------------------ */
  .login-screen {
    position: fixed;
    inset: 0;
    z-index: 1000;
    display: grid;
    place-items: center;
    padding: 24px;
    overflow-y: auto;
    background:
      radial-gradient(60% 50% at 20% 10%, var(--accent-glow), transparent 70%),
      radial-gradient(50% 50% at 90% 90%, color-mix(in srgb, var(--purple) 18%, transparent), transparent 70%),
      var(--canvas);
    animation: fade-in var(--dur-slow) var(--ease-out);
  }
  .login-card {
    width: min(400px, 100%);
    padding: 34px 30px 30px;
    border: 1px solid var(--line);
    border-radius: var(--radius-xl);
    background: var(--surface);
    box-shadow: var(--shadow-lg);
    animation: pop-in var(--dur-slow) var(--ease-spring);
  }
  .login-mark { display: block; width: 46px; height: 46px; margin: 0 auto 16px; filter: drop-shadow(0 8px 18px var(--accent-glow)); }
  .login-mark rect { fill: var(--accent); }
  .login-mark path { fill: none; stroke: var(--accent-contrast); stroke-width: 2.6; stroke-linecap: round; stroke-linejoin: round; }
  .login-card h1 { margin-bottom: 4px; font-size: 20px; text-align: center; }
  .auth-form { display: flex; flex-direction: column; gap: 14px; margin-top: 24px; }
  .auth-form label { display: flex; flex-direction: column; gap: 6px; font-size: 13px; font-weight: 550; }
  .auth-form input { padding: 10px 12px; }
  .login-submit { min-height: 40px; margin-top: 4px; }
  .insecure-login { display: flex; align-items: center; gap: 8px; margin: 0; color: var(--warning); font-size: 12px; }

  /* Add server ------------------------------------------------------------- */
  .active-install-banner {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 12px;
    margin-bottom: 16px;
    padding: 10px 14px;
    border: 1px solid color-mix(in srgb, var(--info) 35%, transparent);
    border-radius: var(--radius-md);
    background: var(--info-bg);
    font-size: 13px;
    animation: rise-in var(--dur) var(--ease-out);
  }
  .banner-left { display: flex; align-items: center; gap: 10px; }
  .pulse-dot { position: relative; width: 9px; height: 9px; border-radius: 50%; background: var(--info); }
  .pulse-dot::after { content: ''; position: absolute; inset: 0; border-radius: inherit; background: inherit; animation: ping 1.8s var(--ease-out) infinite; }
  .method .form-grid { margin-bottom: 0; }
  .or-divider { display: flex; align-items: center; gap: 14px; margin: 18px 0; color: var(--muted); font-size: 11px; font-weight: 700; letter-spacing: 0.08em; text-transform: uppercase; }
  .or-divider::before, .or-divider::after { content: ''; flex: 1; height: 1px; background: var(--line); }

  /* Settings ------------------------------------------------------------------ */
  .settings-layout { display: grid; grid-template-columns: 220px minmax(0, 860px); gap: 28px; align-items: start; }
  .settings-nav { position: sticky; top: 76px; display: grid; gap: 2px; }
  .settings-nav button {
    display: flex;
    align-items: center;
    gap: 10px;
    min-height: 38px;
    padding: 0 12px;
    border: 0;
    border-radius: var(--radius-md);
    background: transparent;
    color: var(--ink-secondary);
    font-size: 13.5px;
    text-align: left;
    transition: background-color var(--transition-fast), color var(--transition-fast);
  }
  .settings-nav button :global(svg) { color: var(--muted); }
  .settings-nav button:hover { background: var(--surface-muted); color: var(--ink); }
  .settings-nav button.chosen { background: var(--surface); color: var(--ink); font-weight: 620; box-shadow: var(--shadow-sm), inset 0 0 0 1px var(--line); }
  .settings-nav button.chosen :global(svg) { color: var(--accent); }
  .nav-badge { margin-left: auto; padding: 0 7px; border-radius: var(--radius-pill); background: var(--danger); color: #fff; font-size: 11px; font-weight: 700; }
  .settings-content { min-width: 0; }
  .settings-section { display: grid; gap: var(--gap); animation: rise-in var(--dur) var(--ease-out); }
  .settings-section :global(.panel + .panel) { margin-top: 0; }

  /* Onboarding ---------------------------------------------------------------- */
  .stepper { display: flex; gap: 8px; margin: 0 0 18px; padding: 0; list-style: none; }
  .step { display: flex; flex: 1; align-items: center; gap: 10px; padding: 10px 12px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); color: var(--muted); font-size: 13px; font-weight: 550; transition: border-color var(--dur), color var(--dur); }
  .step span { display: grid; place-items: center; width: 24px; height: 24px; border: 1px solid var(--line-strong); border-radius: 50%; font-size: 11px; font-weight: 700; transition: background-color var(--dur), border-color var(--dur), color var(--dur); }
  .step.current { border-color: var(--accent); color: var(--ink); }
  .step.current span, .step.done span { border-color: var(--accent); background: var(--accent); color: var(--accent-contrast); }
  .step.done { color: var(--ink-secondary); }
  .step-body { animation: rise-in var(--dur) var(--ease-out); }
  .onboarding-card h2 { margin-bottom: 6px; }
  .choice-row { display: flex; flex-wrap: wrap; gap: 10px; margin: 18px 0; }
  .choice-row label { display: inline-flex; align-items: center; gap: 8px; padding: 10px 14px; border: 1px solid var(--line); border-radius: var(--radius-md); color: var(--ink-secondary); font-size: 13px; cursor: pointer; transition: border-color var(--transition-fast), background-color var(--transition-fast); }
  .choice-row label.chosen { border-color: var(--accent); background: var(--accent-bg); color: var(--ink); }
  .pairing-field { display: grid; gap: 7px; margin-bottom: 14px; font-size: 13px; font-weight: 550; }
  .pairing-field small { color: var(--muted); font-weight: 400; }
  .review-box { display: grid; gap: 8px; margin: 18px 0; padding: 14px 16px; border-radius: var(--radius-md); background: var(--surface-muted); font-size: 13px; }

  /* Server detail ------------------------------------------------------------- */
  .server-heading { align-items: center; }
  .server-title { display: flex; align-items: center; gap: 14px; min-width: 0; }
  .server-avatar { display: grid; flex-shrink: 0; place-items: center; width: 46px; height: 46px; border-radius: var(--radius-lg); background: var(--accent-bg); color: var(--accent); }
  .heading-row { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; }
  .heading-row h1 { margin: 0; }
  .rename-button { width: 30px; height: 30px; border-color: transparent; background: transparent; opacity: 0.6; }
  .heading-row:hover .rename-button, .rename-button:focus-visible { opacity: 1; }
  .rename { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; animation: fade-in var(--dur-fast) var(--ease-out); }
  .rename input { width: min(320px, 70vw); font-size: 18px; font-weight: 600; }

  .server-heading-actions { display: flex; align-items: center; gap: 10px; }
  .server-meta-bar {
    display: flex;
    flex-wrap: wrap;
    gap: 14px 30px;
    margin: 0 0 22px;
    padding: 14px 20px;
    border: 1px solid var(--line);
    border-radius: var(--radius-lg);
    background: var(--surface);
    box-shadow: var(--shadow);
  }
  .meta-item { display: grid; gap: 3px; margin: 0; }
  .meta-item dt { color: var(--muted); font-size: 11px; font-weight: 600; letter-spacing: 0.04em; text-transform: uppercase; }
  .meta-item dd { display: flex; align-items: center; gap: 6px; margin: 0; font-size: 13px; font-weight: 550; }
  .meta-item.note dd { color: var(--warning); }
  .dot { width: 7px; height: 7px; border-radius: 50%; background: var(--muted); }
  .dot.connected { background: var(--success); }
  .dot.disconnected { background: var(--danger); }
  .dot.never-connected { background: var(--warning); }
  .server-actions { margin-bottom: var(--gap); }
  .tabs button { display: inline-flex; align-items: center; gap: 7px; }
  .tab-body { animation: fade-in var(--dur) var(--ease-out); }
  .unavailable-panel.large :global(svg) { color: var(--muted); }

  /* ------------------------------------------------------------------------
     Responsive
     ------------------------------------------------------------------------ */
  @media (max-width: 1200px) {
    .search-trigger { min-width: 0; }
    .search-trigger span { display: none; }
  }
  @media (max-width: 1100px) {
    .settings-layout { grid-template-columns: 1fr; gap: 16px; }
    .settings-nav { position: static; display: flex; gap: 4px; padding: 4px; overflow-x: auto; border: 1px solid var(--line); border-radius: var(--radius-lg); background: var(--surface-muted); }
    .settings-nav button { flex: 0 0 auto; white-space: nowrap; }
  }
  @media (max-width: 960px) {
    .app-shell { grid-template-columns: minmax(0, 1fr); }
    .topbar { position: static; }
  }
  @media (max-width: 640px) {
    .topbar { height: 54px; padding: 0 16px; gap: 10px; }
    .breadcrumbs { flex: 1; }
    .preview-control span { display: none; }
    .topbar-actions { gap: 6px; }
    .breadcrumbs > :global(:not(.crumb-current)) { display: none; }
    .add-server span { display: none; }
    .search-trigger { min-width: 0; width: 36px; padding: 0; justify-content: center; }
    .search-trigger span, .search-trigger kbd { display: none; }
    :global(.page) { padding: 20px 16px 36px; }
    :global(.page-heading) { flex-wrap: wrap; gap: 10px; margin-bottom: 18px; }
    .partial-warning { margin: 12px 16px 0; }
    .server-meta-bar { gap: 12px 20px; padding: 12px 14px; }
    .stepper { flex-direction: column; }
  }
</style>
