<script lang="ts">
  import Icon from '../Icon.svelte';
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
  let relayName = $state('');

  function chooseSource(next: Source) {
    source = next;
    if (next === 'relay' && !relayId) relayId = preferredRelay(relays);
  }

  let name = $state('');
  let busy = $state(false);
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
    busy = true; error = ''; command = ''; expiresAt = ''; copied = false; relayName = '';
    if (PREVIEW_MODE) { error = 'Join commands are issued by the hub and are unavailable in the fixture preview.'; busy = false; return; }
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
        relayName = script.relay ?? '';
        if (source === 'relay') { rememberRelay(relayId); onRelayUsed?.(); }
      }
    } catch (cause) {
      if (createdId) {
        try { await apiClient.deleteServer(createdId, createdRevision); } catch { /* the pending server can be removed from the server list */ }
      }
      error = cause instanceof Error ? cause.message : 'Unable to create the join command.';
    } finally {
      busy = false;
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
      <small>{source === 'github' ? 'The server downloads the latest release from GitHub.' : source === 'hub' ? 'The server downloads this hub\'s build from the hub. Use it when GitHub is blocked.' : 'For servers the hub cannot reach directly: everything goes through a node in the server\'s network, set up automatically.'}</small>
    </fieldset>
    {#if source === 'relay'}
      <label>Relay node
        <select bind:value={relayId} required>
          <option value="" disabled>Choose a connected node</option>
          {#each relays as relay (relay.id)}<option value={relay.id}>{relay.name} · {relay.address}{relay.ready ? ' · ready' : ''}</option>{/each}
        </select>
      </label>
      <label>The node then reaches the hub
        <select bind:value={nodeRoute}>
          <option value="auto">Directly when possible, otherwise through the relay</option>
          <option value="proxy">Always through the relay</option>
          <option value="direct">Always directly</option>
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
      {#if relayName}<p class="muted"><Icon name="shield" size={12} /> Runs through relay {relayName}; the relay is ready.</p>{/if}
    </div>
  {/if}
</article>

<style>
  .join-form { display: flex; flex-wrap: wrap; align-items: flex-end; gap: 12px; }
  .join-form label { display: grid; flex: 1 1 260px; gap: 7px; font-size: 13px; font-weight: 550; }
  .source { flex: 1 1 100%; min-width: 0; margin: 0; padding: 0; border: 0; }
  .source legend { margin-bottom: 8px; font-size: 13px; font-weight: 550; }
  .source small { display: block; margin-top: 8px; color: var(--muted); font-size: 11.5px; }
  .join-command { display: grid; gap: 10px; margin-top: 16px; animation: rise-in var(--dur) var(--ease-out); }
  .terminal-block { display: flex; align-items: flex-start; gap: 10px; padding: 12px 12px 12px 14px; border-radius: var(--radius-md); background: var(--term-bg); color: var(--term-ink); }
  .terminal-block code { flex: 1; font-family: var(--font-mono); font-size: 12px; line-height: 1.6; overflow-wrap: anywhere; white-space: pre-wrap; }
  .terminal-block .icon-button { width: 30px; height: 30px; border-color: transparent; background: rgba(255, 255, 255, 0.07); color: var(--term-ink); }
  .join-command p { display: flex; align-items: center; gap: 6px; margin: 0; font-size: 12px; }
</style>
