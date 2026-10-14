<script lang="ts">
  import { onMount } from 'svelte';
  import Icon from '../../Icon.svelte';
  import {
    ApiError,
    apiClient,
    type ApiToken,
    type ApiTokenActivity,
    type CreatedApiToken,
    type TokenPermissionMode,
    type TokenPermissions
  } from '../../api';
  import { tokenStatus, filterTokens } from '../../lib/tokenManagement';
  import type { ConfirmOptions } from '../../lib/dialogs';
  import { PREVIEW_MODE } from '../../lib/env';
  import { previewApiTokens } from '../../preview/fixtures';

  let { isOwner, currentUser, canEdit, onConfirm, onNotice, onAuthExpired }: {
    isOwner: boolean;
    currentUser: string;
    canEdit: boolean;
    onConfirm: (options: ConfirmOptions) => void;
    onNotice: (message: string) => void;
    onAuthExpired: () => void;
  } = $props();

  const httpsAvailable = PREVIEW_MODE || (typeof window !== 'undefined' && window.location.protocol === 'https:');
  const permissionOptions: { value: keyof TokenPermissions; label: string; description: string }[] = [
    { value: 'monitoring', label: 'Monitoring and metrics', description: 'Server status, CPU, memory, and processes' },
    { value: 'logs', label: 'Logs', description: 'Read server log sources and entries' },
    { value: 'traffic', label: 'Traffic', description: 'Traffic history, forecasts, and allowances' },
    { value: 'alerts', label: 'Alerts', description: 'Alert rules, incidents, and maintenance windows' },
    { value: 'packages', label: 'Packages and modules', description: 'Module catalog and lifecycle actions' },
    { value: 'operations', label: 'Server operations', description: 'Enrollment, installs, control, and jobs' },
    { value: 'settings', label: 'Settings', description: 'Hub and notification settings' },
    { value: 'updates', label: 'Updates', description: 'Release and update operations' }
  ];

  const emptyPermissions = (): TokenPermissions =>
    Object.fromEntries(permissionOptions.map(({ value }) => [value, 'none'])) as TokenPermissions;

  let tokens = $state<ApiToken[]>([]);
  let loading = $state(true);
  let busy = $state(false);
  let error = $state('');
  let now = $state(Date.now());

  let createOpen = $state(false);
  let name = $state('');
  let lifetime = $state(90);
  let permissions = $state<TokenPermissions>(emptyPermissions());

  let editTarget = $state<ApiToken | null>(null);
  let editName = $state('');
  let editLifetime = $state(0);
  let editPermissions = $state<TokenPermissions>(emptyPermissions());

  let created = $state<CreatedApiToken | null>(null);
  let copied = $state('');
  let search = $state('');
  let ownerFilter = $state('');
  let activityToken = $state('');
  let activity = $state<ApiTokenActivity[]>([]);
  let activityLoading = $state(false);

  const owners = $derived([...new Set(tokens.map((token) => token.username))].sort());
  const visible = $derived(filterTokens(tokens, ownerFilter, search));
  const expiring = $derived(tokens.filter((token) => tokenStatus(token, now) === 'expiring'));

  function setPermission(target: 'create' | 'edit', category: keyof TokenPermissions, value: TokenPermissionMode): void {
    const next = target === 'create' ? { ...permissions } : { ...editPermissions };
    next[category] = value;
    if (target === 'create') permissions = next;
    else editPermissions = next;
  }

  function setAllPermissions(target: 'create' | 'edit', value: TokenPermissionMode): void {
    const next = emptyPermissions();
    permissionOptions.forEach(({ value: category }) => { next[category] = value; });
    if (target === 'create') permissions = next;
    else editPermissions = next;
  }

  function effectivePermission(value: TokenPermissions): 'read' | 'edit' {
    return canEdit && Object.values(value).includes('write') ? 'edit' : 'read';
  }

  function tokenPermissions(token: ApiToken): TokenPermissions {
    if (token.permissions) return { ...emptyPermissions(), ...token.permissions };
    const legacy = emptyPermissions();
    if (!token.actions?.length) {
      permissionOptions.forEach(({ value }) => { legacy[value] = token.permission === 'edit' ? 'write' : 'read'; });
    }
    if (token.actions?.includes('monitoring')) legacy.monitoring = 'read';
    if (token.actions?.includes('read')) permissionOptions.forEach(({ value }) => { legacy[value] = 'read'; });
    if (token.actions?.includes('write')) permissionOptions.forEach(({ value }) => { legacy[value] = 'write'; });
    return legacy;
  }

  function grantedSummary(token: ApiToken): string {
    const grants = tokenPermissions(token);
    return permissionOptions
      .filter(({ value }) => grants[value] !== 'none')
      .map(({ label, value }) => `${label}: ${grants[value] === 'write' ? 'read and write' : 'read only'}`)
      .join(' · ') || 'No API access';
  }

  function openCreate(): void {
    editTarget = null;
    createOpen = true;
    error = '';
  }

  function closeCreate(): void {
    createOpen = false;
    error = '';
  }

  function openEdit(token: ApiToken): void {
    createOpen = false;
    editTarget = token;
    editName = token.name;
    editLifetime = 0;
    editPermissions = tokenPermissions(token);
    error = '';
  }

  function closeEdit(): void {
    editTarget = null;
    error = '';
  }

  function reportError(err: unknown, fallback: string): void {
    if (err instanceof ApiError && err.authExpired) onAuthExpired();
    error = err instanceof Error ? err.message : fallback;
  }

  async function saveEdit(): Promise<void> {
    if (!editTarget || busy) return;
    busy = true;
    error = '';
    try {
      const update = {
        name: editName.trim(),
        permission: effectivePermission(editPermissions),
        permissions: editPermissions,
        ...(editLifetime ? { expires_in_days: editLifetime } : {})
      };
      const updated = PREVIEW_MODE
        ? { ...editTarget, ...update, permission: effectivePermission(editPermissions), ...(editLifetime ? { expires_at: new Date(Date.now() + editLifetime * 86400000).toISOString() } : {}) }
        : await apiClient.updateApiToken(editTarget.id, update);
      tokens = tokens.map((item) => item.id === editTarget?.id ? updated : item);
      editTarget = null;
      onNotice('API key updated.');
    } catch (err) {
      reportError(err, 'Could not update the API key.');
    } finally {
      busy = false;
    }
  }

  async function create(): Promise<void> {
    if (!name.trim() || busy) return;
    busy = true;
    error = '';
    copied = '';
    try {
      created = PREVIEW_MODE
        ? { ...previewApiTokens[0], id: `tok-${Date.now()}`, name: name.trim(), permission: effectivePermission(permissions), permissions, created_at: new Date().toISOString(), expires_at: new Date(Date.now() + lifetime * 86400000).toISOString(), last_used_at: undefined, token: 'pyt_preview-token-not-real' }
        : await apiClient.createApiToken({ name: name.trim(), permission: effectivePermission(permissions), expires_in_days: lifetime, permissions });
      name = '';
      createOpen = false;
      if (PREVIEW_MODE && created) tokens = [created, ...tokens];
      else await load();
      onNotice('API key created. Copy the secret now.');
    } catch (err) {
      reportError(err, 'Could not create the API key.');
    } finally {
      busy = false;
    }
  }

  function rotate(token: ApiToken): void {
    onConfirm({
      title: `Rotate “${token.name}”?`,
      description: 'The old secret stops working immediately. Copy the replacement and update your clients. Access and expiry are preserved.',
      tone: 'warning', icon: 'alert-triangle', confirmText: 'Rotate API key', cancelText: 'Cancel',
      action: async () => {
        busy = true;
        error = '';
        copied = '';
        try {
          created = PREVIEW_MODE ? { ...token, token: 'pyt_preview-rotated-token-not-real' } : await apiClient.rotateApiToken(token.id);
          if (!PREVIEW_MODE) await load();
          onNotice('API key rotated. Copy the new secret.');
        } catch (err) {
          reportError(err, 'Could not rotate the API key.');
        } finally {
          busy = false;
        }
      }
    });
  }

  function revokeAll(): void {
    if (isOwner && !ownerFilter) return;
    onConfirm({
      title: `Revoke all API keys${ownerFilter ? ` for ${ownerFilter}` : ' for your account'}?`,
      description: 'Every key for this user, including expired keys, will be revoked.',
      tone: 'warning', icon: 'alert-triangle', confirmText: 'Revoke all keys', cancelText: 'Cancel',
      action: async () => {
        try {
          if (!PREVIEW_MODE) await apiClient.revokeAllApiTokens(ownerFilter || undefined);
          tokens = tokens.filter((token) => isOwner && token.username !== ownerFilter);
          editTarget = null;
          created = null;
          activityToken = '';
          onNotice('All API keys for this user were revoked.');
        } catch (err) {
          reportError(err, 'Could not revoke API keys.');
        }
      }
    });
  }

  async function showActivity(token: ApiToken): Promise<void> {
    if (activityToken === token.id) { activityToken = ''; return; }
    activityToken = token.id;
    activity = [];
    activityLoading = true;
    error = '';
    try {
      const result = PREVIEW_MODE ? [] : (await apiClient.getApiTokenActivity(token.id)).items;
      if (activityToken === token.id) activity = result;
    } catch (err) {
      reportError(err, 'Could not load API key activity.');
    } finally {
      if (activityToken === token.id) activityLoading = false;
    }
  }

  function revoke(token: ApiToken): void {
    onConfirm({
      title: `Revoke “${token.name}”?`,
      description: 'Anything using this API key loses access immediately.',
      tone: 'warning', icon: 'alert-triangle', confirmText: 'Revoke key', cancelText: 'Cancel',
      action: async () => {
        error = '';
        try {
          if (!PREVIEW_MODE) await apiClient.revokeApiToken(token.id);
          tokens = tokens.filter((item) => item.id !== token.id);
          if (editTarget?.id === token.id) editTarget = null;
          if (created?.id === token.id) created = null;
          if (activityToken === token.id) activityToken = '';
          onNotice('API key revoked.');
        } catch (err) {
          reportError(err, 'Could not revoke the API key.');
        }
      }
    });
  }

  const endpoint = typeof window === 'undefined' ? '/api/v1/mcp' : `${window.location.origin}/api/v1/mcp`;
  const secret = $derived(created?.token ?? '');
  const snippets = $derived([
    { id: 'token', label: 'Secret', text: secret },
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
      error = err instanceof Error ? err.message : 'API keys could not be loaded.';
    } finally {
      loading = false;
    }
  }

  async function copy(id: string, text: string): Promise<void> {
    try {
      await navigator.clipboard.writeText(text);
      copied = id;
      setTimeout(() => { if (copied === id) copied = ''; }, 1600);
    } catch {
      error = 'Copy failed. Select the text and copy it manually.';
    }
  }

  onMount(() => {
    if (!httpsAvailable) return;
    void load();
    const timer = setInterval(() => { now = Date.now(); }, 30000);
    return () => clearInterval(timer);
  });
</script>

<article class="panel">
  <div class="panel-heading">
    <div class="title">
      <span class="section-icon"><Icon name="terminal" size={16} /></span>
      <div><h2>API &amp; AI access</h2><span class="muted">Create keys for scripts and AI agents, then choose exactly what each key can do.</span></div>
    </div>
    {#if httpsAvailable && !createOpen && !editTarget}<button class="button primary" type="button" onclick={openCreate}><Icon name="plus" size={15} /> Create API key</button>{/if}
  </div>

  {#if !httpsAvailable}
    <div class="warning" role="note"><Icon name="lock" size={16} /><div><strong>Not supported over HTTP</strong><p>API keys and AI agent access only work over HTTPS. Set up SSL / TLS, then open the dashboard with https://.</p></div></div>
  {:else}
    {#if createOpen}
      <div class="api-modal-backdrop" role="presentation" onclick={(event) => { if (event.target === event.currentTarget) closeCreate(); }}>
        <dialog open class="api-modal" aria-labelledby="create-key-title">
          <div class="flow-heading"><div><span class="eyebrow">New credential</span><h3 id="create-key-title">Create API key</h3><p class="muted">Give this key a name, expiry, and the smallest set of permissions it needs.</p></div><button class="icon-button modal-close" type="button" aria-label="Close create API key dialog" onclick={closeCreate}><Icon name="x" size={17} /></button></div>
          <form class="form-grid" onsubmit={(event) => { event.preventDefault(); void create(); }}>
            <label>Key name<input bind:value={name} maxlength="64" required placeholder="For example: Claude Code on my laptop" autocomplete="off" /></label>
            <label>Expires after<select bind:value={lifetime}><option value={7}>7 days</option><option value={30}>30 days</option><option value={90}>90 days</option><option value={365}>1 year</option></select></label>
            <fieldset class="permissions"><legend>Category access</legend><div class="bulk-row"><span>Set all categories</span><select aria-label="Set all categories" onchange={(event) => { setAllPermissions('create', event.currentTarget.value as TokenPermissionMode); event.currentTarget.value = ''; }}><option value="">Choose an access level…</option><option value="none">None</option><option value="read">Read only</option><option value="write" disabled={!canEdit}>Read and write</option></select></div>{#each permissionOptions as option}<div class="permission-row"><span><strong>{option.label}</strong><small>{option.description}</small></span><select aria-label={`${option.label} access`} value={permissions[option.value] ?? 'none'} onchange={(event) => setPermission('create', option.value, event.currentTarget.value as TokenPermissionMode)}><option value="none">None</option><option value="read">Read only</option><option value="write" disabled={!canEdit}>Read and write</option></select></div>{/each}</fieldset>
            <div class="setup-actions"><button class="button ghost" type="button" onclick={closeCreate}>Cancel</button><button class="button primary" type="submit" disabled={busy || !name.trim()}>{busy ? 'Creating…' : 'Create API key'}</button></div>
          </form>
        </dialog>
      </div>
    {/if}

    {#if editTarget}
      <section class="flow-panel" aria-labelledby="edit-key-title">
        <div class="flow-heading"><div><span class="eyebrow">Manage credential</span><h3 id="edit-key-title">Edit API key</h3><p class="muted">Update “{editTarget.name}”. Existing clients keep the same secret.</p></div><button class="button ghost small" type="button" onclick={closeEdit}>Cancel</button></div>
        <form class="form-grid" onsubmit={(event) => { event.preventDefault(); void saveEdit(); }}>
          <label>Key name<input bind:value={editName} required maxlength="64" /></label>
          <label>Expiry<select bind:value={editLifetime}><option value={0}>Keep current expiry</option><option value={7}>7 days from now</option><option value={30}>30 days from now</option><option value={90}>90 days from now</option><option value={365}>1 year from now</option></select><small>Extending an expired key restores access.</small></label>
          <fieldset class="permissions"><legend>Category access</legend><div class="bulk-row"><span>Set all categories</span><select aria-label="Set all categories" onchange={(event) => { setAllPermissions('edit', event.currentTarget.value as TokenPermissionMode); event.currentTarget.value = ''; }}><option value="">Choose an access level…</option><option value="none">None</option><option value="read">Read only</option><option value="write" disabled={!canEdit}>Read and write</option></select></div>{#each permissionOptions as option}<div class="permission-row"><span><strong>{option.label}</strong><small>{option.description}</small></span><select aria-label={`${option.label} edit access`} value={editPermissions[option.value] ?? 'none'} onchange={(event) => setPermission('edit', option.value, event.currentTarget.value as TokenPermissionMode)}><option value="none">None</option><option value="read">Read only</option><option value="write" disabled={!canEdit}>Read and write</option></select></div>{/each}</fieldset>
          <div class="setup-actions"><button class="button ghost" type="button" onclick={closeEdit}>Cancel</button><button class="button primary" type="submit" disabled={busy || !editName.trim()}>{busy ? 'Saving…' : 'Save changes'}</button></div>
        </form>
      </section>
    {/if}

    {#if created}
      <section class="created" role="status"><div class="flow-heading"><div><span class="eyebrow">One-time secret</span><h3>API key ready</h3><p class="muted">Copy this secret now. It will not be shown again.</p></div><button class="button ghost small" type="button" onclick={() => (created = null)}>Done</button></div>{#each snippets as snippet (snippet.id)}<div class="snippet"><span class="snippet-label">{snippet.label}</span><div class="terminal-block"><code>{snippet.text}</code><button class="icon-button" type="button" aria-label={`Copy ${snippet.label}`} title={copied === snippet.id ? 'Copied' : 'Copy'} onclick={() => void copy(snippet.id, snippet.text)}><Icon name={copied === snippet.id ? 'check' : 'copy'} size={14} /></button></div></div>{/each}</section>
    {/if}

    {#if !createOpen && !editTarget}
      {#if expiring.length}<p class="warning" role="status">{expiring.length} {expiring.length === 1 ? 'API key expires' : 'API keys expire'} within 7 days.</p>{/if}
      <div class="list-heading"><div><h3>API keys</h3><p class="muted">{tokens.length ? `${tokens.length} key${tokens.length === 1 ? '' : 's'} configured` : 'No keys configured yet.'}</p></div>{#if tokens.length}<button class="button ghost small" type="button" disabled={busy || (isOwner && !ownerFilter)} onclick={revokeAll}>{isOwner && !ownerFilter ? 'Select a user to revoke all' : `Revoke all${ownerFilter ? ` for ${ownerFilter}` : ' my keys'}`}</button>{/if}</div>
      <div class="filters"><label>Search keys<input type="search" bind:value={search} placeholder="Name, user or key hint" /></label>{#if isOwner}<label>User<select bind:value={ownerFilter}><option value="">All users</option>{#each owners as owner}<option value={owner}>{owner}</option>{/each}</select></label>{/if}</div>
      <div class="table"><div class="head"><span>Key / created</span><span>Access</span><span>Last used</span><span>Expires</span><span>Manage</span></div>{#each visible as token (token.id)}<div class="row"><span class="who"><strong>{token.name}</strong><span class="muted mono">{token.hint}…{#if isOwner} · {token.username}{/if}</span><span class="muted">Created {formatDay(token.created_at)}</span></span><span class="who muted">{token.permission === 'read' ? 'Read only account' : 'Read and write account'}<small>{grantedSummary(token)}</small></span><span class="who muted"><span><span class="cell-label">Last used </span>{token.last_used_at ? new Date(token.last_used_at).toLocaleString() : 'Never'}</span>{#if token.last_used_ip}<small class="mono">{token.last_used_ip}</small>{/if}</span><span class="who muted"><span><span class="cell-label">Expires </span>{formatDay(token.expires_at)}</span><strong class:expiry-warning={tokenStatus(token, now) !== 'active'}>{tokenStatus(token, now) === 'expired' ? 'Expired' : tokenStatus(token, now) === 'expiring' ? 'Expires soon' : 'Active'}</strong></span><span class="actions">{#if token.username === currentUser}<button class="button ghost small" type="button" disabled={busy} onclick={() => openEdit(token)}>Edit</button><button class="button ghost small" type="button" disabled={busy || tokenStatus(token, now) === 'expired'} onclick={() => rotate(token)}>Rotate</button>{/if}<button class="button ghost small" type="button" onclick={() => void showActivity(token)}>Activity</button><button class="button ghost small" type="button" disabled={busy} onclick={() => revoke(token)}>Revoke</button></span></div>{#if activityToken === token.id}<div class="activity"><h3>Activity for {token.name}</h3><p class="muted">Latest 100 requests, including failed requests.</p>{#each activity as item}<div class="activity-row"><time datetime={item.at}>{new Date(item.at).toLocaleString()}</time><code>{item.method} {item.path}</code><span>Status {item.status}</span><span class="mono">{item.ip}</span></div>{:else}<p class="muted">{activityLoading ? 'Loading activity…' : 'No recorded activity.'}</p>{/each}</div>{/if}{:else}<div class="empty-state"><p class="muted">{loading ? 'Loading API keys…' : tokens.length ? 'No API keys match these filters.' : 'Create an API key to connect scripts or AI agents.'}</p>{#if !tokens.length}<button class="button primary" type="button" onclick={openCreate}><Icon name="plus" size={15} /> Create API key</button>{/if}</div>{/each}</div>
    {/if}
    {#if error}<p class="form-error" role="alert">{error}</p>{/if}
  {/if}
</article>

<style>
  .panel-heading, .flow-heading, .list-heading { display: flex; align-items: flex-start; justify-content: space-between; gap: 16px; }
  .flow-heading { margin-bottom: 18px; }
  .flow-heading h3, .list-heading h3, .created h3 { margin: 2px 0 4px; }
  .flow-heading p, .list-heading p, .created p { margin: 0; }
  .eyebrow { color: var(--accent); font-size: 11px; font-weight: 700; letter-spacing: .08em; text-transform: uppercase; }
  .flow-panel { margin: 4px 0 22px; padding: 20px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); }
  .api-modal-backdrop { position: fixed; z-index: 50; inset: 0; display: flex; align-items: center; justify-content: center; box-sizing: border-box; width: 100%; height: 100%; padding: clamp(16px, 4vw, 32px); overflow-y: auto; background: rgba(5, 9, 15, .58); backdrop-filter: blur(3px); }
  .api-modal { position: relative; inset: auto; display: flex; flex-direction: column; box-sizing: border-box; flex: 0 1 700px; width: min(700px, 100%); max-width: 700px; max-height: min(860px, calc(100dvh - 48px)); margin: 0; overflow: auto; padding: clamp(20px, 3vw, 28px); border: 1px solid var(--line); border-radius: var(--radius-xl); background: var(--surface-elevated, var(--surface)); color: var(--ink); box-shadow: 0 24px 70px rgba(0, 0, 0, .34); animation: api-modal-in var(--dur-slow) var(--ease-spring); }
  .api-modal .flow-heading { padding-right: 44px; }
  .api-modal .form-grid { margin: 20px 0 0; }
  .api-modal .permissions { max-height: min(440px, 48vh); overflow-y: auto; overscroll-behavior: contain; }
  .api-modal .setup-actions { position: sticky; bottom: -1px; margin: 0 -2px -2px; padding: 18px 2px 2px; background: linear-gradient(var(--surface-elevated, var(--surface)), var(--surface-elevated, var(--surface))); }
  .modal-close { flex: 0 0 auto; }
  @keyframes api-modal-in { from { opacity: 0; transform: translateY(10px) scale(.98); } to { opacity: 1; transform: none; } }
  .created { display: grid; gap: 12px; margin-bottom: 22px; padding: 20px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); }
  .warning { display: flex; align-items: flex-start; gap: 10px; padding: 14px; border-radius: var(--radius-md); background: var(--warning-bg); color: var(--warning); font-size: 13px; }
  .warning p { margin: 4px 0 0; color: var(--ink-secondary); line-height: 1.5; }
  .permissions { display: grid; gap: 0; grid-column: 1 / -1; border: 1px solid var(--line); border-radius: var(--radius-md); padding: 0 14px 8px; }
  .permissions legend { padding: 0 6px; font-weight: 600; }
  .bulk-row { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 10px 0; border-bottom: 1px solid var(--line); font-weight: 600; }
  .bulk-row select { width: 190px; }
  .permission-row { display: grid; grid-template-columns: 1fr 150px; gap: 14px; align-items: center; padding: 9px 0; border-bottom: 1px solid var(--line-light); }
  .permission-row:last-child { border-bottom: 0; }
  .permission-row span, .who { display: grid; gap: 2px; min-width: 0; }
  .permission-row small { color: var(--muted); }
  .snippet { display: grid; gap: 6px; }
  .snippet-label { color: var(--muted); font-size: 11.5px; font-weight: 600; letter-spacing: .03em; text-transform: uppercase; }
  .terminal-block { display: flex; align-items: flex-start; gap: 10px; padding: 12px 12px 12px 14px; border-radius: var(--radius-md); background: var(--term-bg); color: var(--term-ink); }
  .terminal-block code { flex: 1; min-width: 0; font-family: var(--font-mono); font-size: 12px; line-height: 1.6; overflow-wrap: anywhere; white-space: pre-wrap; }
  .terminal-block .icon-button { width: 30px; height: 30px; flex-shrink: 0; border-color: transparent; background: rgba(255, 255, 255, .07); color: var(--term-ink); }
  .list-heading { align-items: center; margin: 8px 0 16px; }
  .filters { display: flex; flex-wrap: wrap; align-items: end; gap: 12px; margin-bottom: 16px; }
  .filters label { display: grid; gap: 6px; flex: 1; min-width: 150px; }
  .head, .row { display: grid; grid-template-columns: minmax(140px, 1fr) 230px 145px 110px 150px; gap: 12px; align-items: center; }
  .head { padding: 0 0 8px; border-bottom: 1px solid var(--line); color: var(--muted); font-size: 11.5px; font-weight: 600; letter-spacing: .03em; text-transform: uppercase; }
  .row { padding: 12px 0; border-bottom: 1px solid var(--line-light); font-size: 13px; }
  .who strong { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .mono { font-family: var(--font-mono); font-size: 11.5px; }
  .actions { display: flex; flex-wrap: wrap; gap: 4px; justify-content: flex-end; }
  .expiry-warning { color: var(--warning); }
  .activity { padding: 16px; margin: 8px 0; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface-muted); }
  .activity h3 { margin: 0; }
  .activity-row { display: flex; flex-wrap: wrap; gap: 12px; padding: 8px 0; border-bottom: 1px solid var(--line); font-size: 12px; overflow-wrap: anywhere; }
  .empty-state { display: grid; justify-items: start; gap: 12px; padding: 28px 0 12px; }
  .empty-state p { margin: 0; }
  .cell-label { display: none; }
  /* The table sits beside the sidebar and settings nav, so it stacks on its own width, not the viewport's. */
  .table { container-type: inline-size; }
  .panel-heading { flex-wrap: wrap; gap: 12px; }
  .panel-heading .title { flex: 1 1 260px; min-width: 0; }
  @container (max-width: 860px) { .head { display: none; } .cell-label { display: inline; margin-right: 4px; } .row { grid-template-columns: minmax(0, 1fr) auto; } .row > span:nth-child(2), .row > span:nth-child(3), .row > span:nth-child(4) { grid-column: 1; } .actions { grid-column: 1 / -1; justify-content: flex-start; } }
  @media (max-width: 640px) { .panel-heading, .flow-heading, .list-heading { flex-direction: column; align-items: stretch; } .panel-heading > button, .flow-heading > button, .list-heading > button { align-self: flex-start; } .permission-row { grid-template-columns: 1fr; gap: 8px; } .bulk-row { align-items: stretch; flex-direction: column; } .bulk-row select { width: 100%; } .api-modal-backdrop { align-items: flex-end; padding: 12px; } .api-modal { flex-basis: auto; width: 100%; max-width: none; max-height: calc(100dvh - 24px); padding: 20px 16px 16px; border-radius: var(--radius-xl) var(--radius-xl) var(--radius-md) var(--radius-md); } .api-modal .permissions { max-height: none; } .api-modal .setup-actions { flex-direction: column-reverse; align-items: stretch; } .api-modal .setup-actions .button { width: 100%; } }
</style>
