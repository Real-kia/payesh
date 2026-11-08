<script lang="ts">
  import Icon from '../Icon.svelte';
  import LoadingModal from './LoadingModal.svelte';
  import { apiClient, type JoinLink, type Server } from '../api';
  import { operationKey } from '../lib/ops';
  import { PREVIEW_MODE } from '../lib/env';
  import { preferredRelay, rememberRelay, type RelayOption } from '../lib/relays';

  // With `existing`, the command joins that already-listed server instead of
  // creating a new one (for example after an SSH installation failed). Without
  // it, the command can be for one new server or reusable for many.
  let { onCreated, relays = [], onRelayUsed, existing }: { onCreated?: (server: Server) => void; relays?: RelayOption[]; onRelayUsed?: () => void; existing?: { id: string; name: string } } = $props();

  // How many servers the command is for.
  type Scope = 'one' | 'many';
  let scope = $state<Scope>('one');
  // Where the server gets Payesh from: GitHub (needs international access),
  // this hub directly, or this hub through a relay node in the server's network.
  type Source = 'github' | 'hub' | 'relay';
  let source = $state<Source>('github');
  let relayId = $state('');
  let nodeRoute = $state<'auto' | 'proxy' | 'direct'>('auto');
  let maxUses = $state(20);

  let readyRelays = $derived(relays.filter((relay) => relay.ready));
  let otherRelays = $derived(relays.filter((relay) => !relay.ready));
  let selectedRelay = $derived(relays.find((relay) => relay.id === relayId));
  const label = (relay: RelayOption) => `${relay.name} · ${relay.address}${relay.countryCode ? ` · ${relay.countryCode}` : ''}`;
  const sourceLabels: Record<Source, string> = { github: 'GitHub', hub: 'This hub', relay: 'A relay node' };

  function reset() { command = ''; expiresAt = ''; error = ''; copied = false; }

  function chooseSource(next: Source) {
    if (source === next) return;
    source = next;
    reset();
    if (next === 'relay' && !relayId) relayId = preferredRelay(relays);
  }

  function chooseScope(next: Scope) {
    if (scope === next) return;
    scope = next;
    reset();
    // GitHub's installer takes one server's token, so a reusable command comes from the hub.
    if (next === 'many' && source === 'github') source = 'hub';
    if (next === 'many') void loadLinks();
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
  let links = $state<JoinLink[]>([]);
  let copiedLink = $state('');

  async function loadLinks(): Promise<void> {
    if (PREVIEW_MODE) return;
    try { links = await apiClient.listJoinLinks(); } catch { links = []; }
  }

  async function stopLink(id: string): Promise<void> {
    try { await apiClient.revokeJoinLink(id); links = links.filter((link) => link.id !== id); if (command && links.every((link) => link.command !== command)) reset(); }
    catch (cause) { error = cause instanceof Error ? cause.message : 'Could not stop that command.'; }
  }

  async function copyText(text: string, done: () => void): Promise<void> {
    try { await navigator.clipboard.writeText(text); done(); }
    catch { error = 'Copy failed. Select the command and copy it manually.'; }
  }

  // Creates a pending server and a single-use enrollment token, and builds the
  // one-line installer command that joins the node to this hub.
  async function create(): Promise<void> {
    const serverName = existing?.name ?? name.trim();
    if ((scope === 'one' && !serverName) || busy) return;
    if (source === 'relay' && !relayId) { error = 'Choose a relay node.'; return; }
    busy = true; reset();
    loadingOpen = true;
    if (source === 'relay') {
      loadingTitle = 'Preparing relay join…';
      loadingMessage = 'Connecting to relay node, verifying network proxy, and generating command. This may take 15–30 seconds…';
    } else {
      loadingTitle = 'Generating join command…';
      loadingMessage = 'Creating the enrollment and join command…';
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
      if (scope === 'many') {
        const link = await apiClient.createJoinLink({ max_uses: maxUses, ...(source === 'relay' ? { relay_server_id: relayId, node_route: nodeRoute } : {}) });
        command = link.command;
        expiresAt = link.expires_at;
        if (source === 'relay') { rememberRelay(relayId); onRelayUsed?.(); }
        await loadLinks();
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
</script>

<article class="panel">
  <div class="panel-heading"><div class="title"><div><h2>Run one command on the server</h2><span class="muted">Log in to the server and paste it. Run it as a user with sudo.</span></div></div></div>
  <form class="join-form" onsubmit={(event) => { event.preventDefault(); void create(); }}>
    {#if !existing}
      <div class="scope" role="radiogroup" aria-label="How many servers">
        <button type="button" role="radio" aria-checked={scope === 'one'} class:active={scope === 'one'} onclick={() => chooseScope('one')}>One server</button>
        <button type="button" role="radio" aria-checked={scope === 'many'} class:active={scope === 'many'} onclick={() => chooseScope('many')}>Many servers</button>
      </div>
      {#if scope === 'one'}
        <label>Server name<input bind:value={name} maxlength="128" placeholder="e.g. EU-Node-02" required /></label>
      {:else}
        <p class="note">One command you can paste on as many servers as you like. Each server adds itself to the list, named after its hostname.</p>
        <label>Servers allowed
          <select bind:value={maxUses}>
            {#each [5, 10, 20, 50, 100, 200] as count}<option value={count}>Up to {count}</option>{/each}
          </select>
        </label>
      {/if}
    {/if}

    <details class="network">
      <summary><span>Download from <strong>{sourceLabels[source]}</strong></span><span class="change">Change</span></summary>
      <div class="network-body">
        <div class="segmented" role="radiogroup" aria-label="Download from">
          {#if scope === 'one'}<button type="button" role="radio" aria-checked={source === 'github'} onclick={() => chooseSource('github')}><Icon name="globe" size={13} />GitHub</button>{/if}
          <button type="button" role="radio" aria-checked={source === 'hub'} onclick={() => chooseSource('hub')}><Icon name="server" size={13} />This hub</button>
          <button type="button" role="radio" aria-checked={source === 'relay'} onclick={() => chooseSource('relay')}><Icon name="shield" size={13} />Relay node</button>
        </div>
        <p class="note">Use <strong>This hub</strong> or a <strong>relay</strong> when the server cannot reach GitHub.</p>
        {#if source === 'relay'}
          {#if selectedRelay?.ready}
            <div class="relay-active-banner" role="status">
              <span class="relay-active-icon"><Icon name="shield" size={15} /></span>
              <div>
                <strong>{selectedRelay.name} is already an active relay</strong>
                <span class="muted">Forwarding for {selectedRelay.nodes} node{selectedRelay.nodes === 1 ? '' : 's'}. No extra setup needed.</span>
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
      </div>
    </details>

    <button class="button primary" type="submit" disabled={busy || (scope === 'one' && !existing && !name.trim())}>{#if busy}<span class="version-spinner" aria-hidden="true"></span>Creating…{:else}Create command{/if}</button>
  </form>
  {#if error}<p class="form-error" role="alert">{error}</p>{/if}
  {#if command}
    <div class="join-command">
      <div class="terminal-block">
        <code>{command}</code>
        <button class="icon-button" type="button" aria-label="Copy join command" title={copied ? 'Copied' : 'Copy command'} onclick={() => void copyText(command, () => { copied = true; setTimeout(() => (copied = false), 2000); })}><Icon name={copied ? 'check' : 'copy'} size={14} /></button>
      </div>
      <p class="muted"><Icon name="clock" size={12} />
        {#if scope === 'many'}Adds up to {maxUses} servers until {new Date(expiresAt).toLocaleString()}. Anyone with it can add a server, so treat it like a password.
        {:else}Works once, until {new Date(expiresAt).toLocaleString()}. Treat it like a password.{/if}
      </p>
    </div>
  {/if}
  {#if scope === 'many' && links.length}
    <div class="links">
      <h3>Active commands</h3>
      <ul>
        {#each links as link (link.id)}
          <li>
            <span><strong>{link.uses} of {link.max_uses}</strong> added{link.relay ? ` · via ${link.relay}` : ''} · until {new Date(link.expires_at).toLocaleString()}</span>
            <span class="row-actions">
              <button class="button ghost small" type="button" onclick={() => void copyText(link.command, () => { copiedLink = link.id; setTimeout(() => (copiedLink = ''), 2000); })}>{copiedLink === link.id ? 'Copied' : 'Copy'}</button>
              <button class="button ghost small" type="button" onclick={() => void stopLink(link.id)}>Stop</button>
            </span>
          </li>
        {/each}
      </ul>
    </div>
  {/if}
  <LoadingModal open={loadingOpen} title={loadingTitle} message={loadingMessage} />
</article>

<style>
  .join-form { display: grid; gap: 16px; }
  .join-form label { display: grid; gap: 8px; font-size: 14px; font-weight: 550; }
  .note { margin: 0; font-size: 13px; line-height: 1.5; color: var(--ink-secondary); }
  .scope { display: inline-flex; width: fit-content; padding: 3px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); }
  .scope button { min-height: 36px; padding: 0 16px; border: 0; border-radius: calc(var(--radius-md) - 2px); background: transparent; color: var(--ink-secondary); font: inherit; font-size: 14px; font-weight: 600; cursor: pointer; }
  .scope button.active { background: var(--surface); color: var(--ink); box-shadow: 0 0 0 1px var(--line); }
  .network { border: 1px solid var(--line); border-radius: var(--radius-md); }
  .network summary { display: flex; align-items: center; justify-content: space-between; gap: 12px; min-height: 44px; padding: 0 14px; font-size: 14px; color: var(--ink-secondary); cursor: pointer; list-style: none; }
  .network summary::-webkit-details-marker { display: none; }
  .network summary strong { color: var(--ink); }
  .network .change { color: var(--accent); font-weight: 600; }
  .network[open] .change { display: none; }
  .network-body { display: grid; gap: 14px; padding: 4px 14px 16px; }
  .relay-active-banner { display: flex; align-items: flex-start; gap: 12px; padding: 12px 14px; border-radius: var(--radius-md); background: var(--surface-muted); border: 1px solid var(--accent); font-size: 13px; }
  .relay-active-banner strong { display: block; font-size: 14px; color: var(--accent); margin-bottom: 2px; }
  .relay-active-banner .muted { font-size: 13px; line-height: 1.4; }
  .relay-active-icon { display: inline-flex; align-items: center; justify-content: center; width: 28px; height: 28px; border-radius: var(--radius-sm, 6px); background: var(--accent-bg); color: var(--accent); flex-shrink: 0; }
  .join-command { display: grid; gap: 10px; margin-top: 16px; }
  .terminal-block { display: flex; align-items: flex-start; gap: 10px; padding: 12px 12px 12px 14px; border-radius: var(--radius-md); background: var(--term-bg); color: var(--term-ink); }
  .terminal-block code { flex: 1; min-width: 0; font-family: var(--font-mono); font-size: 13px; line-height: 1.6; overflow-wrap: anywhere; white-space: pre-wrap; }
  .terminal-block .icon-button { width: 36px; height: 36px; border-color: transparent; background: rgba(255, 255, 255, 0.07); color: var(--term-ink); }
  .join-command p { display: flex; align-items: flex-start; gap: 6px; margin: 0; font-size: 13px; }
  .links { margin-top: 20px; padding-top: 16px; border-top: 1px solid var(--line); }
  .links h3 { margin: 0 0 8px; font-size: 14px; }
  .links ul { display: grid; margin: 0; padding: 0; list-style: none; }
  .links li { display: flex; flex-wrap: wrap; align-items: center; justify-content: space-between; gap: 8px 12px; padding: 10px 0; border-bottom: 1px solid var(--line); font-size: 13px; }
  .links li:last-child { border-bottom: 0; }
  .row-actions { display: inline-flex; gap: 6px; }
</style>
