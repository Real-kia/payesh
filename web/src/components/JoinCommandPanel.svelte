<script lang="ts">
  import Icon from '../Icon.svelte';
  import LoadingModal from './LoadingModal.svelte';
  import { apiClient, type Server } from '../api';
  import { operationKey } from '../lib/ops';
  import { PREVIEW_MODE } from '../lib/env';
  import { preferredRelay, rememberRelay, type RelayOption } from '../lib/relays';

  // With `existing`, the command joins that already-listed server instead of
  // creating a new one (for example after an SSH installation failed).
  let { onCreated, relays = [], onRelayUsed, existing }: { onCreated?: (server: Server) => void; relays?: RelayOption[]; onRelayUsed?: () => void; existing?: { id: string; name: string } } = $props();

  // Where the server gets Payesh from: GitHub (needs international access),
  // this hub directly, or this hub through a relay node in the server's network.
  type Source = 'github' | 'hub' | 'relay';
  let source = $state<Source>('github');
  let relayId = $state('');
  let nodeRoute = $state<'auto' | 'proxy' | 'direct'>('auto');

  let readyRelays = $derived(relays.filter((relay) => relay.ready));
  let otherRelays = $derived(relays.filter((relay) => !relay.ready));
  let selectedRelay = $derived(relays.find((relay) => relay.id === relayId));
  const label = (relay: RelayOption) => `${relay.name} · ${relay.address}${relay.countryCode ? ` · ${relay.countryCode}` : ''}`;

  function chooseSource(next: Source) {
    if (source === next) return;
    source = next;
    command = '';
    expiresAt = '';
    error = '';
    if (next === 'relay' && !relayId) relayId = preferredRelay(relays);
  }

  let name = $state('');
  let busy = $state(false);
  let loadingOpen = $state(false);
  let loadingTitle = $state('');
  let loadingMessage = $state('');
  let error = $state('');
  let command = $state('');
  let expiresAt = $state('');
  let copied = $state(false);

  // Creates a pending server and a single-use enrollment token, and builds the
  // one-line installer command that joins the node to this hub.
  async function create(): Promise<void> {
    const serverName = existing?.name ?? name.trim();
    if (!serverName || busy) return;
    if (source === 'relay' && !relayId) { error = 'Choose a relay node.'; return; }
    busy = true; error = ''; command = ''; expiresAt = ''; copied = false;
    loadingOpen = true;
    if (source === 'relay') {
      loadingTitle = 'Preparing relay join…';
      loadingMessage = 'Connecting to relay node, verifying network proxy, and generating command. This may take 15–30 seconds…';
    } else if (source === 'hub') {
      loadingTitle = 'Generating join command…';
      loadingMessage = 'Preparing download bundles and generating one-time command…';
    } else {
      loadingTitle = 'Generating join command…';
      loadingMessage = 'Creating single-use enrollment token and join command…';
    }
    if (PREVIEW_MODE) { error = 'Join commands are issued by the hub and are unavailable in the fixture preview.'; busy = false; loadingOpen = false; return; }
    let createdId: string | null = null;
    let createdRevision = '0';
    try {
      const transport = await apiClient.getNodeTransportSettings({ limit: 1 });
      if (!transport.url) {
        error = 'Enable HTTPS for this hub under Settings → SSL / TLS first. Nodes need its public address to connect.';
        return;
      }
      const created = existing ? { id: existing.id } as Server : await apiClient.createServer({ name: serverName, address: 'joins by command' });
      if (!existing) {
        createdId = created.id;
        createdRevision = created.configuration_revision ?? '0';
      }
      const issued = await apiClient.createEnrollmentToken(created.id, operationKey('join-token'));
      const job = await apiClient.enrollServer(created.id, { token: issued.token, idempotency_key: operationKey('join-enroll') });
      if (!existing) onCreated?.(created);
      expiresAt = issued.expires_at;
      if (source === 'github') {
        command = `curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh -s -- --release-mode preview --role node --join-url ${transport.url} --join-job ${job.id} --join-token ${issued.token}${transport.ca_sha256 ? ` --join-ca-sha256 ${transport.ca_sha256}` : ''}`;
      } else {
        const script = await apiClient.createJoinScript(created.id, { job_id: job.id, token: issued.token, ...(source === 'relay' ? { relay_server_id: relayId, node_route: nodeRoute } : {}) });
        command = script.command;
        expiresAt = script.expires_at;
        if (source === 'relay') { rememberRelay(relayId); onRelayUsed?.(); }
      }
    } catch (cause) {
      if (createdId) {
        try { await apiClient.deleteServer(createdId, createdRevision); } catch { /* the pending server can be removed from the server list */ }
      }
      error = cause instanceof Error ? cause.message : 'Unable to create the join command.';
    } finally {
      busy = false;
      loadingOpen = false;
    }
  }

  async function copy(): Promise<void> {
    try {
      await navigator.clipboard.writeText(command);
      copied = true;
      setTimeout(() => (copied = false), 2000);
    } catch {
      copied = false;
      error = 'Copy failed. Select the command and copy it manually.';
    }
  }
</script>

<article class="panel">
  <div class="panel-heading"><div class="title"><span class="section-icon"><Icon name="copy" size={16} /></span><div><h2>Run one command on the server</h2><span class="muted">For a server you are already logged in to. Run it as a user with sudo.</span></div></div></div>
  <form class="join-form" onsubmit={(event) => { event.preventDefault(); void create(); }}>
    {#if !existing}<label>Server name<input bind:value={name} maxlength="128" placeholder="e.g. EU-Node-02" required /></label>{/if}
    <fieldset class="source">
      <legend>Download from</legend>
      <div class="segmented">
        <button type="button" aria-pressed={source === 'github'} onclick={() => chooseSource('github')}><Icon name="globe" size={13} />GitHub</button>
        <button type="button" aria-pressed={source === 'hub'} onclick={() => chooseSource('hub')}><Icon name="server" size={13} />This hub</button>
        <button type="button" aria-pressed={source === 'relay'} onclick={() => chooseSource('relay')}><Icon name="shield" size={13} />Relay node</button>
      </div>
    </fieldset>
    {#if source === 'relay'}
      {#if selectedRelay?.ready}
        <div class="relay-active-banner" role="status">
          <span class="relay-active-icon"><Icon name="shield" size={15} /></span>
          <div>
            <strong>{selectedRelay.name} is already operating as an active relay</strong>
            <span class="muted">Currently forwarding for {selectedRelay.nodes} node{selectedRelay.nodes === 1 ? '' : 's'}. It will be used immediately without additional relay configuration.</span>
          </div>
        </div>
      {/if}
      <label>Relay node
        <select bind:value={relayId} required>
          <option value="" disabled>Choose a connected node</option>
          {#if readyRelays.length}
            <optgroup label="Active relay nodes (ready)">
              {#each readyRelays as relay (relay.id)}
                <option value={relay.id}>{label(relay)} · [Active relay{relay.nodes ? ` · ${relay.nodes} node${relay.nodes === 1 ? '' : 's'}` : ''}]</option>
              {/each}
            </optgroup>
          {/if}
          {#if otherRelays.length}
            <optgroup label={readyRelays.length ? 'Other connected nodes' : 'Connected nodes'}>
              {#each otherRelays as relay (relay.id)}
                <option value={relay.id}>{label(relay)}</option>
              {/each}
            </optgroup>
          {/if}
        </select>
      </label>
      <label>Node connection
        <select bind:value={nodeRoute}>
          <option value="auto">Auto</option>
          <option value="proxy">Always via relay</option>
          <option value="direct">Always direct</option>
        </select>
      </label>
    {/if}
    <button class="button primary" type="submit" disabled={busy || (!existing && !name.trim())}>{#if busy}<span class="version-spinner" aria-hidden="true"></span>Creating…{:else}Create command{/if}</button>
  </form>
  {#if error}<p class="form-error" role="alert">{error}</p>{/if}
  {#if command}
    <div class="join-command">
      <div class="terminal-block">
        <code>{command}</code>
        <button class="icon-button" type="button" aria-label="Copy join command" title={copied ? 'Copied' : 'Copy command'} onclick={() => void copy()}><Icon name={copied ? 'check' : 'copy'} size={14} /></button>
      </div>
      <p class="muted"><Icon name="clock" size={12} /> Single-use token, valid until {new Date(expiresAt).toLocaleString()}. Treat it like a password.</p>
    </div>
  {/if}
  <LoadingModal open={loadingOpen} title={loadingTitle} message={loadingMessage} />
</article>

<style>
  .join-form { display: flex; flex-wrap: wrap; align-items: flex-end; gap: 12px; }
  .join-form label { display: grid; flex: 1 1 260px; gap: 7px; font-size: 13px; font-weight: 550; }
  .source { flex: 1 1 100%; min-width: 0; margin: 0; padding: 0; border: 0; }
  .source legend { margin-bottom: 8px; font-size: 13px; font-weight: 550; }
  .relay-active-banner {
    display: flex;
    align-items: flex-start;
    gap: 12px;
    width: 100%;
    padding: 12px 14px;
    border-radius: var(--radius-md);
    background: var(--surface-muted);
    border: 1px solid var(--accent);
    color: var(--ink);
    font-size: 13px;
    animation: rise-in var(--dur, 0.2s) var(--ease-out, ease-out);
  }
  .relay-active-banner strong { display: block; font-size: 13.5px; color: var(--accent); margin-bottom: 2px; }
  .relay-active-banner .muted { font-size: 12px; line-height: 1.4; color: var(--muted); }
  .relay-active-icon {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: 28px;
    height: 28px;
    border-radius: var(--radius-sm, 6px);
    background: var(--accent-bg);
    color: var(--accent);
    flex-shrink: 0;
    margin-top: 1px;
  }
  .join-command { display: grid; gap: 10px; margin-top: 16px; animation: rise-in var(--dur) var(--ease-out); }
  .terminal-block { display: flex; align-items: flex-start; gap: 10px; padding: 12px 12px 12px 14px; border-radius: var(--radius-md); background: var(--term-bg); color: var(--term-ink); }
  .terminal-block code { flex: 1; font-family: var(--font-mono); font-size: 12px; line-height: 1.6; overflow-wrap: anywhere; white-space: pre-wrap; }
  .terminal-block .icon-button { width: 30px; height: 30px; border-color: transparent; background: rgba(255, 255, 255, 0.07); color: var(--term-ink); }
  .join-command p { display: flex; align-items: center; gap: 6px; margin: 0; font-size: 12px; }
</style>
