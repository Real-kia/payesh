<script lang="ts">
  import Icon from '../Icon.svelte';
  import { apiClient, type Server } from '../api';
  import { operationKey } from '../lib/ops';
  import { PREVIEW_MODE } from '../lib/env';

  let { onCreated }: { onCreated: (server: Server) => void } = $props();

  let name = $state('');
  let busy = $state(false);
  let error = $state('');
  let command = $state('');
  let expiresAt = $state('');
  let copied = $state(false);

  // Creates a pending server and a single-use enrollment token, and builds the
  // one-line installer command that joins the node to this hub.
  async function create(): Promise<void> {
    const serverName = name.trim();
    if (!serverName || busy) return;
    busy = true; error = ''; command = ''; expiresAt = ''; copied = false;
    if (PREVIEW_MODE) { error = 'Join commands are issued by the hub and are unavailable in the fixture preview.'; busy = false; return; }
    let createdId: string | null = null;
    let createdRevision = '0';
    try {
      const transport = await apiClient.getNodeTransportSettings({ limit: 1 });
      if (!transport.url) {
        error = 'Enable HTTPS for this hub under Settings → SSL / TLS first. Nodes need its public address to connect.';
        return;
      }
      const created = await apiClient.createServer({ name: serverName, address: 'joins by command' });
      createdId = created.id;
      createdRevision = created.configuration_revision ?? '0';
      const issued = await apiClient.createEnrollmentToken(created.id, operationKey('join-token'));
      const job = await apiClient.enrollServer(created.id, { token: issued.token, idempotency_key: operationKey('join-enroll') });
      onCreated(created);
      expiresAt = issued.expires_at;
      command = `curl -fsSL https://raw.githubusercontent.com/Real-kia/payesh/master/install.sh | sudo sh -s -- --release-mode preview --role node --join-url ${transport.url} --join-job ${job.id} --join-token ${issued.token}${transport.ca_sha256 ? ` --join-ca-sha256 ${transport.ca_sha256}` : ''}`;
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
    <label>Server name<input bind:value={name} maxlength="128" placeholder="e.g. EU-Node-02" required /></label>
    <button class="button primary" type="submit" disabled={busy || !name.trim()}>{#if busy}<span class="version-spinner" aria-hidden="true"></span>Creating…{:else}Create command{/if}</button>
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
</article>

<style>
  .join-form { display: flex; flex-wrap: wrap; align-items: flex-end; gap: 12px; }
  .join-form label { display: grid; flex: 1 1 260px; gap: 7px; font-size: 13px; font-weight: 550; }
  .join-command { display: grid; gap: 10px; margin-top: 16px; animation: rise-in var(--dur) var(--ease-out); }
  .terminal-block { display: flex; align-items: flex-start; gap: 10px; padding: 12px 12px 12px 14px; border-radius: var(--radius-md); background: var(--term-bg); color: var(--term-ink); }
  .terminal-block code { flex: 1; font-family: var(--font-mono); font-size: 12px; line-height: 1.6; overflow-wrap: anywhere; white-space: pre-wrap; }
  .terminal-block .icon-button { width: 30px; height: 30px; border-color: transparent; background: rgba(255, 255, 255, 0.07); color: var(--term-ink); }
  .join-command p { display: flex; align-items: center; gap: 6px; margin: 0; font-size: 12px; }
</style>
