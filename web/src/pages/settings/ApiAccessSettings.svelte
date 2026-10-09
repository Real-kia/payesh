<script lang="ts">
  import { onMount } from 'svelte';
  import Icon from '../../Icon.svelte';
  import { ApiError, apiClient, type ApiToken, type CreatedApiToken, type ApiTokenActivity, type Server } from '../../api';
  import { tokenStatus, filterTokens } from '../../lib/tokenManagement';
  import type { ConfirmOptions } from '../../lib/dialogs';
  import { PREVIEW_MODE } from '../../lib/env';
  import { previewApiTokens, getPreviewServers } from '../../preview/fixtures';

  let { isOwner, currentUser, canEdit, onConfirm, onNotice, onAuthExpired }: {
    isOwner: boolean;
    // Only a token's own account can edit or rotate it; the owner can still
    // inspect and revoke everyone's tokens.
    currentUser: string;
    canEdit: boolean;
    onConfirm: (options: ConfirmOptions) => void;
    onNotice: (message: string) => void;
    onAuthExpired: () => void;
  } = $props();

  // The server only accepts, issues, and lists API tokens over HTTPS.
  const httpsAvailable = PREVIEW_MODE || (typeof window !== 'undefined' && window.location.protocol === 'https:');
  let tokens = $state<ApiToken[]>([]);
  let loading = $state(true);
  let busy = $state(false);
  let error = $state('');
  let name = $state('');
  let permission = $state<'read' | 'edit'>('read');
  let lifetime = $state(90);
  let created = $state<CreatedApiToken | null>(null);
  let copied = $state('');
  let servers = $state<Pick<Server, 'id' | 'name'>[]>([]);
  let now = $state(Date.now());
  let serverIds = $state<string[]>([]);
  let actions = $state<string[]>([]);
  let search = $state('');
  let ownerFilter = $state('');
  let editing = $state('');
  let editName = $state('');
  let editPermission = $state<'read' | 'edit'>('read');
  let editLifetime = $state(0);
  let editServerIds = $state<string[]>([]);
  let editActions = $state<string[]>([]);
  let activityToken = $state('');
  let activity = $state<ApiTokenActivity[]>([]);
  let activityLoading = $state(false);
  const owners = $derived([...new Set(tokens.map((token) => token.username))].sort());
  const visible = $derived(filterTokens(tokens, ownerFilter, search));
  const expiring = $derived(tokens.filter((token) => tokenStatus(token, now) === 'expiring'));
  const actionOptions = [{ value: 'monitoring', label: 'Monitoring (server data only)' }, { value: 'read', label: 'Read resources' }, { value: 'write', label: 'Change resources' }];

  function startEdit(token: ApiToken): void {
    editing = token.id; editName = token.name; editPermission = token.permission; editLifetime = 0;
    editServerIds = [...(token.server_ids ?? [])]; editActions = [...(token.actions ?? [])];
  }

  async function saveEdit(token: ApiToken): Promise<void> {
    if (busy) return;
    busy = true; error = '';
    try {
      const update = { name: editName.trim(), permission: editPermission, server_ids: editServerIds, actions: editActions, ...(editLifetime ? { expires_in_days: editLifetime } : {}) };
      const updated = PREVIEW_MODE ? { ...token, ...update, ...(editLifetime ? { expires_at: new Date(Date.now() + editLifetime * 86400000).toISOString() } : {}) } : await apiClient.updateApiToken(token.id, update);
      tokens = tokens.map((item) => item.id === token.id ? updated : item);
      editing = ''; onNotice('Token updated.');
    } catch (err) { reportError(err, 'Could not update the token.'); }
    finally { busy = false; }
  }

  function reportError(err: unknown, fallback: string): void {
    if (err instanceof ApiError && err.authExpired) onAuthExpired();
    error = err instanceof Error ? err.message : fallback;
  }

  function rotate(token: ApiToken): void {
    onConfirm({ title: `Rotate “${token.name}”?`, description: 'The old secret stops working immediately. Copy the replacement and update your clients. Access and expiry are preserved.', tone: 'warning', icon: 'alert-triangle', confirmText: 'Rotate token', cancelText: 'Cancel', action: async () => {
      busy = true; error = ''; copied = '';
      try {
        created = PREVIEW_MODE ? { ...token, token: 'pyt_preview-rotated-token-not-real' } : await apiClient.rotateApiToken(token.id);
        if (!PREVIEW_MODE) await load();
        onNotice('Token rotated. Copy the new secret.');
      } catch (err) { reportError(err, 'Could not rotate the token.'); }
      finally { busy = false; }
    } });
  }

  function revokeAll(): void {
    if (isOwner && !ownerFilter) return;
    onConfirm({ title: `Revoke all tokens${ownerFilter ? ` for ${ownerFilter}` : ' for your account'}?`, description: 'Every token for this user, including expired tokens, will be revoked. Their agents and scripts lose access immediately.', tone: 'warning', icon: 'alert-triangle', confirmText: 'Revoke all tokens', cancelText: 'Cancel', action: async () => {
      try {
        if (!PREVIEW_MODE) await apiClient.revokeAllApiTokens(ownerFilter || undefined);
        tokens = tokens.filter((token) => isOwner && token.username !== ownerFilter); created = null; editing = ''; activityToken = '';
        onNotice('All tokens for this user revoked.');
      } catch (err) { reportError(err, 'Could not revoke tokens.'); }
    } });
  }

  async function showActivity(token: ApiToken): Promise<void> {
    if (activityToken === token.id) { activityToken = ''; return; }
    activityToken = token.id; activity = []; activityLoading = true; error = '';
    try {
      const result = PREVIEW_MODE ? [] : (await apiClient.getApiTokenActivity(token.id)).items;
      if (activityToken === token.id) activity = result;
    } catch (err) { reportError(err, 'Could not load token activity.'); }
    finally { if (activityToken === token.id) activityLoading = false; }
  }

  const endpoint = typeof window === 'undefined' ? '/api/v1/mcp' : `${window.location.origin}/api/v1/mcp`;
  const secret = $derived(created?.token ?? '');
  const snippets = $derived([
    { id: 'token', label: 'Token', text: secret },
    { id: 'claude', label: 'Claude Code', text: `claude mcp add --transport http payesh ${endpoint} --header "Authorization: Bearer ${secret}"` },
    { id: 'json', label: 'MCP client config (JSON)', text: JSON.stringify({ mcpServers: { payesh: { type: 'http', url: endpoint, headers: { Authorization: `Bearer ${secret}` } } } }, null, 2) },
    { id: 'curl', label: 'REST API', text: `curl -H "Authorization: Bearer ${secret}" ${endpoint.replace(/\/mcp$/, '/servers')}` }
  ]);

  function formatDay(value: string | undefined): string {
    return value ? new Date(value).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' }) : 'Never';
  }

  async function load(): Promise<void> {
    if (PREVIEW_MODE) { tokens = structuredClone(previewApiTokens); loading = false; return; }
    try {
      tokens = (await apiClient.listApiTokens()).items;
    } catch (err) {
      if (err instanceof ApiError && err.authExpired) onAuthExpired();
      error = err instanceof Error ? err.message : 'API tokens could not be loaded.';
    } finally { loading = false; }
  }

  async function create(): Promise<void> {
    if (!name.trim() || busy) return;
    busy = true; error = ''; copied = '';
    try {
      created = PREVIEW_MODE
        ? { ...previewApiTokens[0], id: `tok-${Date.now()}`, name: name.trim(), permission, server_ids: serverIds, actions, created_at: new Date().toISOString(), expires_at: new Date(Date.now() + lifetime * 86400000).toISOString(), last_used_at: undefined, token: 'pyt_preview-token-not-real' }
        : await apiClient.createApiToken({ name: name.trim(), permission, expires_in_days: lifetime, server_ids: serverIds, actions });
      name = '';
      if (PREVIEW_MODE && created) tokens = [created, ...tokens]; else await load();
    } catch (err) {
      if (err instanceof ApiError && err.authExpired) onAuthExpired();
      error = err instanceof Error ? err.message : 'Could not create the token.';
    } finally { busy = false; }
  }

  async function copy(id: string, text: string): Promise<void> {
    try { await navigator.clipboard.writeText(text); copied = id; setTimeout(() => { if (copied === id) copied = ''; }, 1600); }
    catch { error = 'Copy failed. Select the text and copy it manually.'; }
  }

  function revoke(token: ApiToken): void {
    onConfirm({ title: `Revoke “${token.name}”?`, description: 'Anything using this token, such as an AI agent or script, loses access immediately.', tone: 'warning', icon: 'alert-triangle', confirmText: 'Revoke token', cancelText: 'Cancel', action: async () => {
      error = '';
      try {
        if (!PREVIEW_MODE) await apiClient.revokeApiToken(token.id);
        tokens = tokens.filter((item) => item.id !== token.id);
        if (created?.id === token.id) created = null;
        if (editing === token.id) editing = '';
        if (activityToken === token.id) activityToken = '';
        onNotice('Token revoked.');
      } catch (err) { reportError(err, 'Could not revoke the token.'); }
    } });
  }

  onMount(() => {
    if (!httpsAvailable) return;
    void load();
    const timer = setInterval(() => { now = Date.now(); }, 30000);
    const controller = new AbortController();
    if (PREVIEW_MODE) servers = getPreviewServers();
    else void (async () => {
      const all: Pick<Server, 'id' | 'name'>[] = [];
      let cursor: string | undefined;
      do {
        const result = await apiClient.listServers({ cursor, signal: controller.signal });
        all.push(...result.items); cursor = result.next_cursor;
      } while (cursor);
      servers = all;
    })().catch((err) => { if (!controller.signal.aborted) reportError(err, 'Could not load servers for access limits.'); });
    return () => { clearInterval(timer); controller.abort(); };
  });
</script>

<article class="panel">
  <div class="panel-heading">
    <div class="title"><span class="section-icon"><Icon name="terminal" size={16} /></span><div><h2>API &amp; AI access</h2><span class="muted">Tokens let scripts and AI agents (MCP clients such as Claude) use this hub as your account.</span></div></div>
  </div>

  {#if !httpsAvailable}
    <div class="warning" role="note">
      <Icon name="lock" size={16} />
      <div><strong>Not supported over HTTP</strong><p>API tokens and AI agent (MCP) access only work over HTTPS, because tokens sent over HTTP can be read on the network. Set up a domain under SSL / TLS, then open the dashboard with https:// to create tokens.</p></div>
    </div>
  {:else}
  <form class="form-grid create" onsubmit={(event) => { event.preventDefault(); void create(); }}>
    <label>Token name<input bind:value={name} maxlength="64" required placeholder="For example: Claude Code on my laptop" autocomplete="off" /></label>
    <label>Access
      <select bind:value={permission} onchange={(event) => { if (event.currentTarget.value === 'read') actions = actions.filter((action) => action !== 'write'); }}>
        <option value="read">Read only</option>
        <option value="edit" disabled={!canEdit}>Can edit{canEdit ? '' : ' (your account is read only)'}</option>
      </select>
      <small>Read only is enough for monitoring questions. Edit lets the agent change settings and servers.</small>
    </label>
    <label>Expires after
      <select bind:value={lifetime}>
        <option value={7}>7 days</option><option value={30}>30 days</option><option value={90}>90 days</option><option value={365}>1 year</option>
      </select>
    </label>
    <label>Allowed servers<select multiple bind:value={serverIds} aria-label="Allowed servers">{#each servers as server}<option value={server.id}>{server.name}</option>{/each}</select><small>No selection allows all servers. Select one or more to restrict access.</small></label>
    <label>Allowed actions<select multiple bind:value={actions} aria-label="Allowed actions">{#each actionOptions as option}<option value={option.value} disabled={option.value === 'write' && permission === 'read'}>{option.label}</option>{/each}</select><small>No selection allows every action covered by the access level. Monitoring limits access to server data.</small></label>
    <div class="setup-actions"><button class="button primary" type="submit" disabled={busy || !name.trim()}>{busy ? 'Creating…' : 'Create token'}</button></div>
  </form>

  {#if created}
    <div class="created" role="status">
      <p class="once"><Icon name="lock" size={13} /> Copy the token now. It is not shown again; treat it like a password.</p>
      {#each snippets as snippet (snippet.id)}
        <div class="snippet">
          <span class="snippet-label">{snippet.label}</span>
          <div class="terminal-block">
            <code>{snippet.text}</code>
            <button class="icon-button" type="button" aria-label={`Copy ${snippet.label}`} title={copied === snippet.id ? 'Copied' : 'Copy'} onclick={() => void copy(snippet.id, snippet.text)}><Icon name={copied === snippet.id ? 'check' : 'copy'} size={14} /></button>
          </div>
        </div>
      {/each}
      <div class="setup-actions"><button class="button ghost small" type="button" onclick={() => (created = null)}>Done</button></div>
    </div>
  {/if}

  {#if expiring.length}<p class="warning" role="status">{expiring.length} {expiring.length === 1 ? 'token expires' : 'tokens expire'} within 7 days. Extend or replace them before clients lose access.</p>{/if}
  <div class="filters">
    <label>Search tokens<input type="search" bind:value={search} placeholder="Name, user or token hint" /></label>
    {#if isOwner}<label>User<select bind:value={ownerFilter}><option value="">All users</option>{#each owners as owner}<option value={owner}>{owner}</option>{/each}</select></label>{/if}
    <button class="button ghost small" type="button" disabled={busy || !tokens.length || (isOwner && !ownerFilter)} onclick={revokeAll}>{isOwner && !ownerFilter ? 'Select a user to revoke all' : `Revoke all${ownerFilter ? ` for ${ownerFilter}` : ' my tokens'}`}</button>
  </div>
  <div class="table">
    <div class="head"><span>Token / created</span><span>Access</span><span>Last used</span><span>Expires</span><span>Manage</span></div>
    {#each visible as token (token.id)}
      <div class="row">
        <span class="who"><strong>{token.name}</strong><span class="muted mono">{token.hint}…{#if isOwner} · {token.username}{/if}</span><span class="muted">Created {formatDay(token.created_at)}</span></span>
        <span class="who muted">{token.permission === 'read' ? 'Read only' : 'Can edit'}<small>{token.server_ids?.length ? `${token.server_ids.length} selected servers` : 'All servers'}</small><small>{token.actions?.length ? token.actions.join(', ') : 'All permitted actions'}</small></span>
        <span class="who muted"><span><span class="cell-label">Last used </span>{token.last_used_at ? new Date(token.last_used_at).toLocaleString() : 'Never'}</span>{#if token.last_used_ip}<small class="mono">{token.last_used_ip}</small>{/if}</span>
        <span class="who muted"><span><span class="cell-label">Expires </span>{formatDay(token.expires_at)}</span><strong class:expiry-warning={tokenStatus(token, now) !== 'active'}>{tokenStatus(token, now) === 'expired' ? 'Expired' : tokenStatus(token, now) === 'expiring' ? 'Expires soon' : 'Active'}</strong></span>
        <span class="actions">{#if token.username === currentUser}<button class="button ghost small" type="button" disabled={busy} onclick={() => editing === token.id ? editing = '' : startEdit(token)}>Edit</button><button class="button ghost small" type="button" disabled={busy || tokenStatus(token, now) === 'expired'} onclick={() => rotate(token)}>Rotate</button>{/if}<button class="button ghost small" type="button" onclick={() => void showActivity(token)}>Activity</button><button class="button ghost small" type="button" disabled={busy} onclick={() => revoke(token)}>Revoke</button></span>
      </div>
      {#if editing === token.id}
        <form class="form-grid editor" onsubmit={(event) => { event.preventDefault(); void saveEdit(token); }}>
          <label>Token name<input bind:value={editName} required maxlength="64" /></label>
          <label>Access<select bind:value={editPermission} onchange={(event) => { if (event.currentTarget.value === 'read') editActions = editActions.filter((action) => action !== 'write'); }}><option value="read">Read only</option><option value="edit" disabled={!canEdit}>Can edit</option></select></label>
          <label>Expiry<select bind:value={editLifetime}><option value={0}>Keep current expiry</option><option value={7}>7 days from now</option><option value={30}>30 days from now</option><option value={90}>90 days from now</option><option value={365}>1 year from now</option></select><small>Extending an expired token restores access.</small></label>
          <label>Allowed servers<select multiple bind:value={editServerIds}>{#each servers as server}<option value={server.id}>{server.name}</option>{/each}{#each editServerIds.filter((id) => !servers.some((server) => server.id === id)) as id}<option value={id}>{id} (unavailable)</option>{/each}</select><small>No selection allows all servers.</small></label>
          <label>Allowed actions<select multiple bind:value={editActions}>{#each actionOptions as option}<option value={option.value} disabled={option.value === 'write' && editPermission === 'read'}>{option.label}</option>{/each}</select><small>No selection allows every action covered by the access level.</small></label>
          <div class="setup-actions"><button class="button ghost small" type="button" onclick={() => editing = ''}>Cancel</button><button class="button primary small" type="submit" disabled={busy || !editName.trim()}>Save changes</button></div>
        </form>
      {/if}
      {#if activityToken === token.id}
        <div class="activity"><h3>Activity for {token.name}</h3><p class="muted">Latest 100 requests, including failed requests.</p>{#each activity as item}<div class="activity-row"><time datetime={item.at}>{new Date(item.at).toLocaleString()}</time><code>{item.method} {item.path}</code><span>{item.status === 0 ? 'Started / outcome unavailable' : `Status ${item.status}`}</span><span class="mono">{item.ip}</span></div>{:else}<p class="muted">{activityLoading ? 'Loading activity…' : 'No recorded activity.'}</p>{/each}</div>
      {/if}
    {:else}
      <p class="muted none">{loading ? 'Loading tokens…' : tokens.length ? 'No tokens match these filters.' : 'No tokens yet.'}</p>
    {/each}
  </div>
  {#if error}<p class="form-error" role="alert">{error}</p>{/if}
  {/if}
</article>

<style>
  .warning { display: flex; align-items: flex-start; gap: 10px; padding: 14px; border-radius: var(--radius-md); background: var(--warning-bg); color: var(--warning); font-size: 13px; }
  .warning p { margin: 4px 0 0; color: var(--ink-secondary); line-height: 1.5; }
  .create { margin-bottom: 16px; }
  .created { display: grid; gap: 12px; margin-bottom: 18px; padding: 16px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); animation: rise-in var(--dur) var(--ease-out); }
  .once { display: flex; align-items: center; gap: 6px; margin: 0; font-size: 12.5px; font-weight: 600; }
  .snippet { display: grid; gap: 6px; }
  .snippet-label { color: var(--muted); font-size: 11.5px; font-weight: 600; letter-spacing: 0.03em; text-transform: uppercase; }
  .terminal-block { display: flex; align-items: flex-start; gap: 10px; padding: 12px 12px 12px 14px; border-radius: var(--radius-md); background: var(--term-bg); color: var(--term-ink); }
  .terminal-block code { flex: 1; min-width: 0; font-family: var(--font-mono); font-size: 12px; line-height: 1.6; overflow-wrap: anywhere; white-space: pre-wrap; }
  .terminal-block .icon-button { width: 30px; height: 30px; flex-shrink: 0; border-color: transparent; background: rgba(255, 255, 255, 0.07); color: var(--term-ink); }
  .head, .row { display: grid; grid-template-columns: minmax(140px, 1fr) 115px 145px 110px 150px; gap: 12px; align-items: center; }
  .head { padding: 0 0 8px; border-bottom: 1px solid var(--line); color: var(--muted); font-size: 11.5px; font-weight: 600; letter-spacing: 0.03em; text-transform: uppercase; }
  .row { padding: 12px 0; border-bottom: 1px solid var(--line-light); font-size: 13px; }
  .who { display: grid; gap: 2px; min-width: 0; }
  .who strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .mono { font-family: var(--font-mono); font-size: 11.5px; }
  .actions { display: flex; flex-wrap: wrap; gap: 4px; justify-content: flex-end; }
  .filters { display: flex; flex-wrap: wrap; align-items: end; gap: 12px; margin-bottom: 16px; }
  .filters label { display: grid; gap: 6px; flex: 1; min-width: 150px; }
  .expiry-warning { color: var(--warning); }
  .editor, .activity { padding: 16px; margin: 8px 0; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); }
  .activity-row { display: flex; flex-wrap: wrap; gap: 12px; padding: 8px 0; border-bottom: 1px solid var(--line); font-size: 12px; overflow-wrap: anywhere; }
  .activity h3 { margin: 0; }
  .none { padding: 16px 0; }
  .cell-label { display: none; }
  @media (max-width: 1000px) {
    .head { display: none; }
    .cell-label { display: inline; margin-right: 4px; }
    .row { grid-template-columns: 1fr auto; }
    .row > span:nth-child(2), .row > span:nth-child(3), .row > span:nth-child(4) { grid-column: 1; }
    .actions { grid-column: 1 / -1; justify-content: flex-start; }
  }
</style>
