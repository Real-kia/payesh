<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import Icon from '../../Icon.svelte';
  import StatusPill from '../../components/StatusPill.svelte';
  import { ApiError, apiClient, type HTTPSStatus, type NodeTransportStatus } from '../../api';
  import { PREVIEW_MODE } from '../../lib/env';
  import type { ConfirmOptions } from '../../lib/dialogs';

  let { readOnly, insecureConnection, onSaved, onConfirm, onNotice, onAuthExpired }: {
    readOnly: boolean;
    insecureConnection: boolean;
    onSaved: (title: string, description: string) => void;
    onConfirm: (options: ConfirmOptions) => void;
    onNotice: (message: string) => void;
    onAuthExpired: () => void;
  } = $props();

  const browserHTTPS = typeof window !== 'undefined' && window.location.protocol === 'https:';

  let httpsStatus = $state<HTTPSStatus | null>(null);
  let httpsStatusLoading = $state(false);
  let httpsDomain = $state('');
  let httpsEmail = $state('');
  let httpsToken = $state('');
  let httpsBusy = $state(false);
  let httpsError = $state('');
  let httpsPoll: ReturnType<typeof setTimeout> | null = null;

  let dashboardPort = $state('');
  let portBusy = $state(false);
  let portError = $state('');

  let nodeTransportStatus = $state<NodeTransportStatus | null>(null);
  let nodePort = $state('');
  let nodePortBusy = $state(false);
  let nodePortError = $state('');
  let nodeTransportLoadError = $state('');
  let nodeTransportLoading = $state(false);
  let nodePortPoll: ReturnType<typeof setTimeout> | null = null;
  let nodePageCursor = '';
  let nodePageHistory = $state<string[]>([]);
  let destroyed = false;

  function httpsURL(status: HTTPSStatus): string {
    const port = status.https_port && status.https_port !== '443' ? `:${status.https_port}` : '';
    return `https://${status.domain}${port}`;
  }

  async function loadHTTPS(): Promise<void> {
    if (PREVIEW_MODE || destroyed) return;
    if (httpsPoll) { clearTimeout(httpsPoll); httpsPoll = null; }
    httpsStatusLoading = true; httpsError = '';
    try {
      httpsStatus = await apiClient.getHTTPSSettings();
      if (!dashboardPort) dashboardPort = httpsStatus.https_port ?? '8787';
      if (!httpsDomain && httpsStatus.domain) httpsDomain = httpsStatus.domain;
      if (httpsStatus.state === 'pending' && !destroyed) httpsPoll = setTimeout(() => void loadHTTPS(), 3000);
      else httpsBusy = false;
      // HTTPS just came up while this page is on plain HTTP: move to it. The
      // session cookie belongs to this address, so the user signs in again.
      if (httpsStatus.state === 'active' && insecureConnection && httpsStatus.domain) {
        const target = httpsURL(httpsStatus);
        onNotice(`HTTPS is ready. Opening ${target} …`);
        setTimeout(() => { window.location.href = target + window.location.pathname; }, 1500);
      }
    } catch (error) {
      httpsBusy = false;
      if (error instanceof ApiError && error.authExpired) onAuthExpired();
      httpsError = error instanceof Error ? error.message : 'Could not read HTTPS settings.';
    } finally { httpsStatusLoading = false; }
  }

  async function saveHTTPS(): Promise<void> {
    if (PREVIEW_MODE || httpsBusy) return;
    httpsError = '';
    httpsBusy = true;
    try {
      httpsStatus = await apiClient.setHTTPSDomain({ domain: httpsDomain.trim(), email: httpsEmail.trim() || undefined, cloudflare_api_token: httpsToken.trim() || undefined });
      onSaved('SSL / TLS updated', `Domain "${httpsDomain.trim()}" has been configured for HTTPS.\n\nAutomatic certificate request and validation have started.`);
      httpsToken = '';
      httpsPoll = setTimeout(() => void loadHTTPS(), 3000);
    } catch (error) {
      httpsBusy = false;
      httpsError = error instanceof Error ? error.message : 'Could not start the certificate request.';
    }
  }

  function promptRemoveHTTPS() {
    onConfirm({
      title: 'Remove domain?',
      description: 'The certificate is deleted and the dashboard goes back to plain HTTP. Logins will no longer be encrypted, and nodes using this certificate will lose their TLS connection.',
      tone: 'warning',
      icon: 'alert-triangle',
      confirmText: 'Remove domain',
      cancelText: 'Keep HTTPS',
      action: async () => {
        await apiClient.removeHTTPSDomain();
        httpsDomain = ''; httpsEmail = ''; httpsToken = '';
        await loadHTTPS();
        onSaved('Domain removed', 'Custom domain and SSL certificate have been removed. The dashboard returned to default HTTP.');
      }
    });
  }

  async function saveDashboardPort(): Promise<void> {
    portBusy = true; portError = '';
    try {
      const status = await apiClient.setDashboardPort(Number(dashboardPort));
      httpsStatus = status;
      const target = new URL(window.location.href);
      target.port = dashboardPort;
      if (status.state === 'active' && status.domain) { target.hostname = status.domain; target.protocol = 'https:'; }
      window.location.assign(target.toString());
    } catch (error) { portError = error instanceof Error ? error.message : 'Could not change port.'; }
    finally { portBusy = false; }
  }

  async function loadNodeTransport(): Promise<void> {
    if (PREVIEW_MODE || nodeTransportLoading || destroyed) return;
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
        if (error instanceof ApiError && error.authExpired) { onAuthExpired(); keepPolling = false; }
      }
    } finally {
      nodeTransportLoading = false;
      if (keepPolling && !destroyed) nodePortPoll = setTimeout(() => void loadNodeTransport(), nodeTransportLoadError ? 10000 : 5000);
    }
  }

  async function saveNodePort(): Promise<void> {
    if (nodePortBusy || !nodeTransportStatus || Number(nodePort) === Number(nodeTransportStatus.port)) return;
    nodePortBusy = true; nodePortError = '';
    try {
      nodeTransportStatus = await apiClient.setNodeTransportPort(Number(nodePort));
      nodePort = nodeTransportStatus.port;
      nodePageCursor = ''; nodePageHistory = [];
      onNotice('Node port opened. Nodes will verify the new connection and migrate automatically.');
      onSaved('Node port updated', `Node transport port updated to ${nodePort}. Connected nodes will migrate to this port automatically.`);
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
    onConfirm({ title: 'Retire previous node endpoints?', description: 'All enrolled nodes have connected to the current node port. Previous node ports and node access through the dashboard will be disabled.', tone: 'warning', icon: 'alert-triangle', confirmText: 'Retire endpoints', cancelText: 'Cancel', action: async () => {
      nodePortBusy = true; nodePortError = '';
      try {
        nodeTransportStatus = await apiClient.retireNodeTransportPorts();
        nodePageCursor = ''; nodePageHistory = [];
        onNotice('Previous node endpoints retired.');
        onSaved('Endpoints retired', 'Previous node transport endpoints have been retired.');
      }
      catch (error) { nodePortError = error instanceof Error ? error.message : 'Could not retire previous endpoints.'; }
      finally { nodePortBusy = false; }
    } });
  }

  function nodeStateLabel(state: string): string {
    return state === 'migrated' ? 'Migrated' : state === 'update-required' ? 'Agent update required' : state === 'failed' ? 'Migration failed · retrying' : 'Pending connection';
  }

  onMount(() => { void loadHTTPS(); void loadNodeTransport(); });
  onDestroy(() => {
    destroyed = true;
    if (httpsPoll) clearTimeout(httpsPoll);
    if (nodePortPoll) clearTimeout(nodePortPoll);
  });
</script>

<article class="panel">
  <div class="panel-heading">
    <div class="title"><span class="section-icon"><Icon name="lock" size={16} /></span><div><h2>SSL / TLS</h2><span class="muted">Serve the dashboard over HTTPS with a free Let's Encrypt certificate.</span></div></div>
  </div>
  <div class="settings-meta-box">
    <div class="meta-row">
      <span>Status</span>
      {#if PREVIEW_MODE}
        <StatusPill state="disabled" label="Fixture preview · no hub connection" />
      {:else if browserHTTPS || httpsStatus?.state === 'active'}
        <StatusPill state="healthy" label="HTTPS active" />
      {:else if httpsStatus?.state === 'pending'}
        <StatusPill state="installing" label="Requesting certificate…" />
      {:else if httpsStatus?.state === 'failed'}
        <StatusPill state="failed" label="Certificate request failed" />
      {:else if httpsStatusLoading || (!httpsStatus && !httpsError)}
        <StatusPill state="pending" label="Checking HTTPS status…" />
      {:else if httpsError}
        <StatusPill state="pending" label="HTTPS settings unavailable" />
      {:else}
        <StatusPill state="pending" label="Not encrypted (HTTP)" />
      {/if}
    </div>
    {#if browserHTTPS && httpsStatus?.state !== 'active'}<div class="meta-row"><span>Connection</span><span>This browser connection uses HTTPS.</span></div>{/if}
    {#if httpsStatus?.domain}
      <div class="meta-row"><span>Address</span><span class="mono">{#if httpsStatus.state === 'active'}<a href={httpsURL(httpsStatus)}>{httpsURL(httpsStatus)}</a>{:else}{httpsStatus.domain}{/if}</span></div>
    {/if}
    {#if httpsStatus?.expires_at}<div class="meta-row"><span>Renews before</span><span>{new Date(httpsStatus.expires_at).toLocaleDateString()}</span></div>{/if}
  </div>
  {#if httpsStatus?.state === 'failed' && httpsStatus.error}<p class="form-error" role="alert">{httpsStatus.error}</p>{/if}
  <form class="form-grid" onsubmit={(event) => { event.preventDefault(); void saveHTTPS(); }}>
    <label>Domain<input bind:value={httpsDomain} placeholder="panel.example.com" autocomplete="off" required disabled={httpsBusy || readOnly} /></label>
    <label><span>Email <small>(optional)</small></span><input type="email" bind:value={httpsEmail} placeholder="you@example.com" autocomplete="email" disabled={httpsBusy || readOnly} /></label>
    <label><span>Cloudflare API token <small>(optional)</small></span><input type="password" bind:value={httpsToken} placeholder="Only needed if port 80 is in use" autocomplete="off" disabled={httpsBusy || readOnly} /></label>
  </form>
  {#if httpsError}<p class="form-error" role="alert">{httpsError}</p>{/if}
  <div class="settings-actions">
    <button class="button primary" type="button" disabled={httpsBusy || PREVIEW_MODE || readOnly || !httpsDomain.trim()} onclick={() => void saveHTTPS()}>{#if httpsBusy}<span class="version-spinner" aria-hidden="true"></span>Requesting certificate…{:else}{httpsStatus?.domain ? 'Update certificate' : 'Enable HTTPS'}{/if}</button>
    {#if httpsStatus?.domain && !httpsBusy && !readOnly}<button class="button ghost" type="button" onclick={() => promptRemoveHTTPS()}>Remove domain</button>{/if}
  </div>
</article>

<article class="panel">
  <div class="panel-heading"><div class="title"><span class="section-icon"><Icon name="globe" size={16} /></span><div><h2>Dashboard port</h2><span class="muted">The browser is moved to the new port after it is verified.</span></div></div></div>
  <form class="port-form" onsubmit={(event) => { event.preventDefault(); void saveDashboardPort(); }}>
    <label>Dashboard port<input type="number" min="1" max="65535" required bind:value={dashboardPort} disabled={portBusy || readOnly} /></label>
    <button class="button primary" type="submit" disabled={portBusy || PREVIEW_MODE || readOnly}>{portBusy ? 'Checking port…' : 'Change port'}</button>
  </form>
  {#if portError}<p class="form-error" role="alert">{portError}</p>{/if}
</article>

<article class="panel">
  <div class="panel-heading"><div class="title"><span class="section-icon"><Icon name="radio" size={16} /></span><div><h2>Node transport port</h2><span class="muted">Nodes connect to the hub here over mutual TLS.</span></div></div></div>
  <form class="port-form" onsubmit={(event) => { event.preventDefault(); void saveNodePort(); }}>
    <label>Node transport port<input type="number" min="1" max="65535" required bind:value={nodePort} disabled={nodePortBusy || PREVIEW_MODE || readOnly} /></label>
    <button class="button primary" type="submit" disabled={nodePortBusy || PREVIEW_MODE || !nodeTransportStatus?.url || Number(nodePort) === Number(nodeTransportStatus.port) || readOnly}>{nodePortBusy ? 'Saving…' : 'Change node port'}</button>
  </form>
  {#if nodeTransportLoading && !nodeTransportStatus}<p class="muted" role="status">Loading node transport settings…</p>{/if}
  {#if nodeTransportLoadError}
    <p class="form-error" role="alert">{nodeTransportLoadError}</p>
    <button class="button ghost small retry" type="button" disabled={nodeTransportLoading} onclick={() => void loadNodeTransport()}>{nodeTransportLoading ? 'Retrying…' : 'Retry node settings'}</button>
  {/if}
  {#if nodePortError}<p class="form-error" role="alert">{nodePortError}</p>{/if}
  {#if nodeTransportStatus}
    <div class="settings-meta-box">
      <div class="meta-row"><span>Endpoint</span>{#if nodeTransportStatus.url}<span class="mono">{nodeTransportStatus.url}</span>{:else}<span class="muted">Enable HTTPS to connect nodes.</span>{/if}</div>
      <div class="meta-row"><span>Migration</span><span>{nodeTransportStatus.migrated} of {nodeTransportStatus.total} nodes on current port</span></div>
      {#if nodeTransportStatus.total > 0}<div class="progress"><span style:width={`${Math.round((nodeTransportStatus.migrated / nodeTransportStatus.total) * 100)}%`}></span></div>{/if}
    </div>
    {#each nodeTransportStatus.nodes as node (node.id)}
      <div class="node-row">
        <span>{node.name}</span>
        <StatusPill state={node.state === 'migrated' ? 'healthy' : node.state === 'failed' ? 'failed' : node.state === 'update-required' ? 'stale' : 'pending'} label={nodeStateLabel(node.state)} />
      </div>
      {#if node.error}<p class="muted small">{node.name}: {node.error}</p>{/if}
    {/each}
    {#if nodePageHistory.length || nodeTransportStatus.next_cursor}
      <div class="node-page-actions" aria-label="Node migration pages">
        <button class="button ghost small" type="button" disabled={nodeTransportLoading || nodePortBusy || !nodePageHistory.length} onclick={() => void changeNodePage(false)}>Previous nodes</button>
        <span class="muted">Page {nodePageHistory.length + 1}</span>
        <button class="button ghost small" type="button" disabled={nodeTransportLoading || nodePortBusy || !nodeTransportStatus.next_cursor} onclick={() => void changeNodePage(true)}>Next nodes</button>
      </div>
    {/if}
    {#if nodeTransportStatus.previous_ports.length || nodeTransportStatus.legacy_dashboard}
      <button class="button ghost retire" type="button" disabled={nodePortBusy || nodeTransportStatus.pending > 0 || readOnly} onclick={retireNodePorts}>Retire previous node endpoints</button>
    {/if}
  {/if}
</article>

<style>
  .port-form { display: flex; flex-wrap: wrap; gap: 12px; align-items: flex-end; }
  .port-form label { display: grid; gap: 7px; color: var(--ink); font-size: 13px; font-weight: 550; }
  .port-form input { width: 140px; }
  .node-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 10px 0; border-bottom: 1px solid var(--line-light); font-size: 13px; }
  .node-row:last-of-type { border-bottom: 0; }
  .node-page-actions { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 12px; margin: 14px 0; }
  .small { margin: 4px 0 8px; font-size: 12px; }
  .retry, .retire { margin-top: 10px; }
</style>
