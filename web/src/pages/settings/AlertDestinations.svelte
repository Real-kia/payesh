<script lang="ts">
  import { onMount } from 'svelte';
  import Icon from '../../Icon.svelte';
  import { ApiError, apiClient, type DestinationInput, type DestinationTestResult, type NotificationDestination } from '../../api';
  import { PREVIEW_MODE } from '../../lib/env';
  import type { ConfirmOptions } from '../../lib/dialogs';
  import { formatDateTimeInTz } from '../../timezone';

  let { isOwner, timezone, onConfirm, onAuthExpired }: {
    isOwner: boolean;
    timezone: string;
    onConfirm: (options: ConfirmOptions) => void;
    onAuthExpired: () => void;
  } = $props();

  type HeaderRow = { name: string; value: string; keep: boolean };
  const FORMATS: { id: string; label: string; hint: string }[] = [
    { id: 'payesh', label: 'Payesh JSON (v1)', hint: 'The native, versioned JSON payload with rule, server, value and incident details.' },
    { id: 'slack', label: 'Slack', hint: 'Slack incoming-webhook message ({"text": …}).' },
    { id: 'discord', label: 'Discord', hint: 'Discord webhook message ({"content": …}).' },
    { id: 'ntfy', label: 'ntfy', hint: 'Plain text with Title, Priority and Tags headers. Use your ntfy topic URL.' },
    { id: 'text', label: 'Text only (JSON)', hint: 'A minimal {"text": …} body for generic receivers.' },
    { id: 'template', label: 'Custom template', hint: 'Your own body, written as a Go text/template.' }
  ];
  const KINDS = [{ id: 'firing', label: 'Firing' }, { id: 'recovered', label: 'Recovered' }, { id: 'reminder', label: 'Reminders' }];

  let items = $state<NotificationDestination[]>([]);
  let limit = $state(20);
  let loading = $state(false);
  let error = $state('');
  let notice = $state('');
  let tests = $state<Record<string, DestinationTestResult | 'running'>>({});

  // Form state. editingId === null means "creating".
  let formOpen = $state(false);
  let editingId = $state<string | null>(null);
  let editing = $state<NotificationDestination | null>(null);
  let busy = $state(false);
  let formError = $state('');
  let name = $state('');
  let kind = $state<'webhook' | 'telegram'>('webhook');
  let url = $state('');
  let secret = $state('');
  let clearSecret = $state(false);
  let chatId = $state('');
  let format = $state('payesh');
  let template = $state('');
  let headerRows = $state<HeaderRow[]>([]);
  let allowPrivate = $state(false);
  let allowHttp = $state(false);
  let timeoutSeconds = $state(10);
  let maxAttempts = $state(3);
  let backoffSeconds = $state(1);
  let maxPerMinute = $state(30);
  let kinds = $state<string[]>([]);
  let tagsText = $state('');
  let rulesText = $state('');
  let serversText = $state('');
  let enabled = $state(true);
  let preview = $state('');
  let previewError = $state('');
  let previewBusy = $state(false);

  const formatHint = $derived(FORMATS.find((f) => f.id === format)?.hint ?? '');
  const atLimit = $derived(items.filter((item) => !item.managed).length >= limit);

  function fail(err: unknown, fallback: string): string {
    if (err instanceof ApiError) {
      if (err.authExpired) onAuthExpired();
      return err.message;
    }
    return fallback;
  }

  async function load(): Promise<void> {
    if (PREVIEW_MODE) return;
    loading = true; error = '';
    try {
      const page = await apiClient.listDestinations();
      items = page.items; limit = page.limit;
    } catch (err) {
      error = fail(err, 'Notification destinations could not be loaded.');
    } finally { loading = false; }
  }

  function splitList(text: string): string[] {
    return text.split(',').map((part) => part.trim()).filter(Boolean);
  }

  function openForm(destination: NotificationDestination | null): void {
    formError = ''; preview = ''; previewError = ''; notice = '';
    editing = destination; editingId = destination?.id ?? null;
    name = destination?.name ?? '';
    kind = destination?.kind ?? 'webhook';
    url = ''; secret = ''; clearSecret = false;
    chatId = destination?.chat_id ?? '';
    format = destination?.format ?? 'payesh';
    template = destination?.template ?? '';
    headerRows = (destination?.header_names ?? []).map((headerName) => ({ name: headerName, value: '', keep: true }));
    allowPrivate = destination?.allow_private_network ?? false;
    allowHttp = destination?.allow_insecure_http ?? false;
    timeoutSeconds = destination?.timeout_seconds ?? 10;
    maxAttempts = destination?.max_attempts ?? 3;
    backoffSeconds = destination?.retry_backoff_seconds ?? 1;
    maxPerMinute = destination?.max_per_minute ?? 30;
    kinds = [...(destination?.filter.kinds ?? [])];
    tagsText = (destination?.filter.tags ?? []).join(', ');
    rulesText = (destination?.filter.rules ?? []).join(', ');
    serversText = (destination?.filter.servers ?? []).join(', ');
    enabled = destination?.enabled ?? true;
    formOpen = true;
  }

  function closeForm(): void { formOpen = false; editing = null; editingId = null; secret = ''; url = ''; headerRows = []; }

  function toggleKind(id: string): void {
    kinds = kinds.includes(id) ? kinds.filter((k) => k !== id) : [...kinds, id];
  }

  function buildInput(): DestinationInput {
    const input: DestinationInput = {
      name: name.trim(), format, allow_private_network: allowPrivate, allow_insecure_http: allowPrivate && allowHttp,
      timeout_seconds: timeoutSeconds, max_attempts: maxAttempts, retry_backoff_seconds: backoffSeconds, max_per_minute: maxPerMinute,
      filter: { kinds, tags: splitList(tagsText), rules: splitList(rulesText), servers: splitList(serversText) }, enabled
    };
    if (!editingId) input.kind = kind;
    if (url.trim()) input.url = url.trim();
    if (kind === 'telegram') input.chat_id = chatId.trim();
    // An untouched secret is left out, so the stored one is kept.
    if (secret) input.secret = secret;
    else if (clearSecret) input.secret = '';
    input.template = format === 'template' ? template : '';
    if (kind === 'webhook') {
      const headers: Record<string, string | null> = {};
      for (const row of headerRows) {
        const headerName = row.name.trim();
        if (!headerName) continue;
        headers[headerName] = row.value ? row.value : row.keep ? null : '';
      }
      // On create, rows without a value are meaningless; the server rejects them.
      input.headers = headers;
    }
    return input;
  }

  async function save(): Promise<void> {
    busy = true; formError = '';
    try {
      const input = buildInput();
      if (editingId) await apiClient.updateDestination(editingId, input);
      else await apiClient.createDestination(input);
      notice = editingId ? 'Destination updated.' : 'Destination added. Use “Send test” to check it.';
      closeForm();
      await load();
    } catch (err) {
      formError = fail(err, 'The destination could not be saved.');
    } finally { busy = false; }
  }

  async function toggleEnabled(destination: NotificationDestination): Promise<void> {
    error = '';
    try {
      await apiClient.updateDestination(destination.id, { enabled: !destination.enabled });
      await load();
    } catch (err) { error = fail(err, 'The destination could not be changed.'); }
  }

  async function sendTest(destination: NotificationDestination): Promise<void> {
    tests = { ...tests, [destination.id]: 'running' };
    error = '';
    try {
      const result = await apiClient.testDestination(destination.id);
      tests = { ...tests, [destination.id]: result };
      await load();
    } catch (err) {
      const { [destination.id]: _removed, ...rest } = tests;
      tests = rest;
      error = fail(err, 'The test notification could not be sent.');
    }
  }

  function remove(destination: NotificationDestination): void {
    onConfirm({
      title: `Delete “${destination.name}”?`, description: 'Alerts will no longer be sent to this destination. This cannot be undone.', tone: 'warning', icon: 'alert-triangle',
      confirmText: 'Delete destination', cancelText: 'Cancel',
      action: async () => {
        try {
          await apiClient.deleteDestination(destination.id);
          notice = 'Destination deleted.';
          await load();
        } catch (err) { error = fail(err, 'The destination could not be deleted.'); }
      }
    });
  }

  async function runPreview(): Promise<void> {
    previewBusy = true; preview = ''; previewError = '';
    try {
      const result = await apiClient.previewDestination({ kind, format, template: format === 'template' ? template : undefined });
      preview = result.body;
    } catch (err) { previewError = fail(err, 'The preview could not be rendered.'); }
    finally { previewBusy = false; }
  }

  function routing(filter: NotificationDestination['filter']): string {
    const parts: string[] = [];
    if (filter.kinds?.length) parts.push(filter.kinds.join(', '));
    if (filter.tags?.length) parts.push(filter.tags.map((tag) => `#${tag}`).join(' '));
    if (filter.rules?.length) parts.push(`rules: ${filter.rules.join(', ')}`);
    if (filter.servers?.length) parts.push(`${filter.servers.length} server${filter.servers.length === 1 ? '' : 's'}`);
    return parts.length ? parts.join(' · ') : 'All alerts';
  }

  function formatLabel(destination: NotificationDestination): string {
    if (destination.kind === 'telegram') return 'Telegram';
    return FORMATS.find((f) => f.id === destination.format)?.label ?? destination.format;
  }

  onMount(() => { void load(); });
</script>

<article class="panel destinations">
  <div class="panel-heading">
    <div class="title"><div><h2>Alert destinations</h2><span class="muted">Where firing, recovered and reminder alerts are delivered.</span></div></div>
    <div class="actions">
      <button class="button ghost small" type="button" disabled={loading || PREVIEW_MODE} onclick={() => void load()}><Icon name="refresh" size={13} />Refresh</button>
      {#if isOwner}<button class="button primary small" type="button" disabled={atLimit || PREVIEW_MODE} onclick={() => openForm(null)}><Icon name="plus" size={13} />Add destination</button>{/if}
    </div>
  </div>
  {#if !isOwner}<p class="muted note">Only the owner can add, change, test or delete destinations.</p>{/if}
  {#if error}<p class="form-error" role="alert">{error}</p>{/if}
  {#if notice}<p class="success-text" role="status">{notice}</p>{/if}

  <div class="list">
    {#each items as destination (destination.id)}
      <div class="dest" class:off={!destination.enabled}>
        <div class="dest-main">
          <div class="dest-head">
            <strong>{destination.name}</strong>
            <span class="chip">{formatLabel(destination)}</span>
            {#if destination.managed}<span class="chip managed" title="Configured through PAYESH_ALERT_* environment variables">Managed by environment</span>{/if}
            {#if !destination.enabled}<span class="chip muted-chip">Disabled</span>{/if}
          </div>
          <div class="dest-meta">
            <span class="mono">{destination.url_preview || '—'}</span>
            <span>{routing(destination.filter)}</span>
            {#if destination.has_secret}<span>Signed / authenticated</span>{/if}
            {#if destination.header_names.length}<span>{destination.header_names.length} custom header{destination.header_names.length === 1 ? '' : 's'}</span>{/if}
          </div>
          {#if destination.secrets_unreadable}
            <p class="form-error">Stored credentials cannot be read (the key changed). Edit the destination and enter the URL and secrets again.</p>
          {/if}
          <div class="status">
            {#if destination.last_delivery}
              <span class="dot" class:ok={destination.last_delivery.ok} class:bad={!destination.last_delivery.ok}></span>
              <span>Last delivery {destination.last_delivery.ok ? 'succeeded' : 'failed'} · {destination.last_delivery.status}{destination.last_delivery.error ? ` · ${destination.last_delivery.error}` : ''} · {formatDateTimeInTz(destination.last_delivery.at, timezone)}</span>
            {:else}
              <span class="dot"></span><span class="muted">No delivery yet</span>
            {/if}
          </div>
          {#if tests[destination.id] === 'running'}
            <p class="muted" role="status">Sending test…</p>
          {:else if tests[destination.id]}
            {@const result = tests[destination.id] as DestinationTestResult}
            <p class={result.ok ? 'success-text' : 'form-error'} role="status">Test {result.ok ? 'delivered' : 'failed'}: {result.status}{result.error ? ` — ${result.error}` : ''} ({result.duration_ms} ms)</p>
          {/if}
        </div>
        {#if isOwner}
          <div class="dest-actions">
            <button class="button ghost small" type="button" disabled={tests[destination.id] === 'running' || PREVIEW_MODE} onclick={() => void sendTest(destination)}><Icon name="bell" size={13} />Send test</button>
            {#if !destination.managed}
              <button class="button ghost small" type="button" onclick={() => void toggleEnabled(destination)}>{destination.enabled ? 'Disable' : 'Enable'}</button>
              <button class="button ghost small" type="button" onclick={() => openForm(destination)}><Icon name="pencil" size={13} />Edit</button>
              <button class="button danger small" type="button" onclick={() => remove(destination)}><Icon name="trash" size={13} />Delete</button>
            {/if}
          </div>
        {/if}
      </div>
    {:else}
      <p class="muted empty">{loading ? 'Loading destinations…' : 'No destinations yet. Alerts still appear on the Alerts page; add a destination to be notified elsewhere.'}</p>
    {/each}
  </div>

  {#if formOpen}
    <form class="dest-form" onsubmit={(event) => { event.preventDefault(); void save(); }}>
      <h3>{editingId ? `Edit “${editing?.name}”` : 'Add destination'}</h3>
      <div class="form-grid">
        {#if !editingId}
          <label>Type<select bind:value={kind} disabled={busy}><option value="webhook">Webhook (Slack, Discord, ntfy, custom)</option><option value="telegram">Telegram bot</option></select></label>
        {/if}
        <label>Name<input type="text" maxlength="80" placeholder="Ops channel" bind:value={name} disabled={busy} /></label>
        <label class="wide">{kind === 'telegram' ? 'Telegram API URL' : 'Webhook URL'}
          <input type="url" autocomplete="off" spellcheck="false" placeholder={editingId ? `Leave blank to keep ${editing?.url_preview ?? 'the current URL'}` : kind === 'telegram' ? 'https://api.telegram.org' : 'https://hooks.example.com/…'} bind:value={url} required={!editingId} disabled={busy} />
          <small>HTTPS is required. The full URL is stored encrypted and is never shown again.</small>
        </label>
        {#if kind === 'telegram'}
          <label>Bot token<input type="password" autocomplete="new-password" placeholder={editing?.has_secret ? 'Leave blank to keep the stored token' : '123456:ABC…'} bind:value={secret} required={!editingId} disabled={busy} /></label>
          <label>Chat ID<input type="text" bind:value={chatId} required disabled={busy} /></label>
        {:else}
          <label>Signing secret (optional)
            <input type="password" autocomplete="new-password" placeholder={editing?.has_secret ? 'Leave blank to keep the stored secret' : 'Used for the X-Payesh-Signature HMAC'} bind:value={secret} disabled={busy} />
            {#if editing?.has_secret}<span class="inline-check"><input type="checkbox" bind:checked={clearSecret} disabled={busy || !!secret} /> Remove the stored secret</span>{/if}
          </label>
        {/if}
      </div>

      {#if kind === 'webhook'}
        <fieldset>
          <legend>Custom headers</legend>
          <small>For example <code>Authorization: Bearer …</code>. Values are stored encrypted and are write-only.</small>
          {#each headerRows as row, index (index)}
            <div class="header-row">
              <input type="text" placeholder="Header name" bind:value={row.name} disabled={busy || row.keep} aria-label="Header name" />
              <input type="password" autocomplete="new-password" placeholder={row.keep ? 'Stored value kept — type to replace' : 'Value'} bind:value={row.value} disabled={busy} aria-label="Header value" />
              <button class="button ghost small" type="button" disabled={busy} onclick={() => (headerRows = headerRows.filter((_, i) => i !== index))} aria-label="Remove header"><Icon name="x" size={13} /></button>
            </div>
          {/each}
          <button class="button ghost small" type="button" disabled={busy || headerRows.length >= 10} onclick={() => (headerRows = [...headerRows, { name: '', value: '', keep: false }])}><Icon name="plus" size={13} />Add header</button>
        </fieldset>

        <fieldset>
          <legend>Payload</legend>
          <label>Format<select bind:value={format} disabled={busy}>{#each FORMATS as option}<option value={option.id}>{option.label}</option>{/each}</select><small>{formatHint}</small></label>
          {#if format === 'template'}
            <label>Template
              <textarea rows="6" spellcheck="false" maxlength="4096" placeholder={'{"text": {{json .Summary}}}'} bind:value={template} disabled={busy}></textarea>
              <small>Fields: <code>.Title .Summary .Kind .State .Rule .RuleID .Expression .Metric .Threshold .Value .Server .ServerID .Tags .Since .OccurredAt .IncidentID .IncidentURL .Reason</code>. Functions: <code>json lower upper join truncate</code>. Digest messages set <code>.Digest</code> and list <code>.Items</code>.</small>
            </label>
          {/if}
          <div class="preview-row">
            <button class="button ghost small" type="button" disabled={busy || previewBusy || (format === 'template' && !template)} onclick={() => void runPreview()}>{previewBusy ? 'Rendering…' : 'Preview with a sample alert'}</button>
          </div>
          {#if previewError}<p class="form-error" role="alert">{previewError}</p>{/if}
          {#if preview}<pre class="preview" aria-label="Sample payload">{preview}</pre>{/if}
        </fieldset>
      {/if}

      <fieldset>
        <legend>Routing</legend>
        <small>Leave everything empty to receive all alerts.</small>
        <div class="kind-row">
          {#each KINDS as option}
            <label class="inline-check"><input type="checkbox" checked={kinds.includes(option.id)} onchange={() => toggleKind(option.id)} disabled={busy} /> {option.label}</label>
          {/each}
        </div>
        <div class="form-grid">
          <label>Only servers tagged<input type="text" placeholder="flow, prod" bind:value={tagsText} disabled={busy} /></label>
          <label>Only these rules (name or ID)<input type="text" placeholder="High CPU, Disk full" bind:value={rulesText} disabled={busy} /></label>
          <label class="wide">Only these server IDs<input type="text" placeholder="srv-…, srv-…" bind:value={serversText} disabled={busy} /></label>
        </div>
      </fieldset>

      <fieldset>
        <legend>Delivery and safety</legend>
        <div class="form-grid">
          <label>Timeout (seconds)<input type="number" min="1" max="30" required bind:value={timeoutSeconds} disabled={busy} /></label>
          <label>Attempts<input type="number" min="1" max="5" required bind:value={maxAttempts} disabled={busy} /></label>
          <label>First retry delay (seconds)<input type="number" min="1" max="30" required bind:value={backoffSeconds} disabled={busy} /></label>
          <label>Max per minute<input type="number" min="1" max="120" required bind:value={maxPerMinute} disabled={busy} /><small>Beyond this, alerts are grouped into one summary message.</small></label>
        </div>
        <label class="inline-check"><input type="checkbox" bind:checked={allowPrivate} disabled={busy} /> Allow private-network addresses (loopback, LAN). Blocked by default to prevent server-side request forgery; cloud metadata addresses stay blocked.</label>
        <label class="inline-check"><input type="checkbox" bind:checked={allowHttp} disabled={busy || !allowPrivate} /> Allow plain <code>http://</code> (only for private-network receivers, e.g. a self-hosted ntfy).</label>
        <label class="inline-check"><input type="checkbox" bind:checked={enabled} disabled={busy} /> Enabled</label>
      </fieldset>

      {#if formError}<p class="form-error" role="alert">{formError}</p>{/if}
      <div class="setup-actions">
        <button class="button primary" type="submit" disabled={busy}>{busy ? 'Saving…' : editingId ? 'Save changes' : 'Add destination'}</button>
        <button class="button ghost" type="button" disabled={busy} onclick={closeForm}>Cancel</button>
      </div>
    </form>
  {/if}
</article>

<style>
  .destinations { margin-top: var(--gap); }
  .actions { display: flex; flex-wrap: wrap; gap: 8px; }
  .note { margin: 0 0 10px; }
  .list { display: grid; gap: 10px; }
  .dest { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 12px; padding: 14px 16px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); }
  .dest.off { opacity: 0.7; }
  .dest-main { flex: 1 1 320px; min-width: 0; display: grid; gap: 6px; }
  .dest-head { display: flex; flex-wrap: wrap; align-items: center; gap: 8px; }
  .chip { padding: 1px 8px; border: 1px solid var(--line); border-radius: 999px; font-size: 11.5px; color: var(--ink-secondary); background: var(--surface-muted); }
  .chip.managed { color: var(--info); border-color: color-mix(in srgb, var(--info) 35%, var(--line)); }
  .muted-chip { color: var(--muted); }
  .dest-meta { display: flex; flex-wrap: wrap; gap: 4px 14px; font-size: 12.5px; color: var(--muted); }
  .mono { font-family: var(--mono, ui-monospace, monospace); overflow-wrap: anywhere; }
  .status { display: flex; align-items: center; gap: 8px; font-size: 12.5px; color: var(--ink-secondary); }
  .dot { flex: none; width: 8px; height: 8px; border-radius: 50%; background: var(--muted); opacity: 0.5; }
  .dot.ok { background: var(--success); opacity: 1; }
  .dot.bad { background: var(--danger); opacity: 1; }
  .dest-actions { display: flex; flex-wrap: wrap; align-items: flex-start; gap: 4px; }
  .empty { padding: 20px 0; text-align: center; }
  .dest-form { margin-top: 18px; padding-top: 16px; border-top: 1px solid var(--line-light); display: grid; gap: 14px; }
  .dest-form h3 { margin: 0; font-size: 15px; }
  fieldset { display: grid; gap: 10px; margin: 0; padding: 12px 14px; border: 1px solid var(--line-light); border-radius: var(--radius-md); min-width: 0; }
  legend { padding: 0 6px; font-size: 12.5px; font-weight: 650; color: var(--ink-secondary); }
  small { color: var(--muted); font-size: 12px; line-height: 1.45; }
  .wide { grid-column: 1 / -1; }
  .header-row { display: grid; grid-template-columns: minmax(120px, 1fr) minmax(160px, 2fr) auto; gap: 8px; align-items: center; }
  .kind-row { display: flex; flex-wrap: wrap; gap: 14px; }
  .inline-check { display: flex; align-items: flex-start; gap: 8px; font-size: 13px; font-weight: 500; }
  .inline-check input { width: auto; margin-top: 3px; }
  textarea { width: 100%; font-family: var(--mono, ui-monospace, monospace); font-size: 12.5px; }
  .preview { margin: 0; padding: 10px 12px; max-height: 260px; overflow: auto; border-radius: var(--radius-md); background: var(--surface-muted); font-size: 12px; white-space: pre-wrap; overflow-wrap: anywhere; }
  .preview-row { display: flex; }
  @media (max-width: 640px) { .header-row { grid-template-columns: 1fr; } }
</style>
