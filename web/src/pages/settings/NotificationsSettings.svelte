<script lang="ts">
  import ActionFeedback from '../../components/ActionFeedback.svelte';
  import Icon from '../../Icon.svelte';
  import { formatDateTimeInTz } from '../../timezone';
  import type { StorageNotification } from '../../api';
  import type { ConfirmOptions } from '../../lib/dialogs';
  import AlertDestinations from './AlertDestinations.svelte';

  let { items, busy, error, canEdit, isOwner, timezone, onRefresh, onMarkRead, onConfirm, onAuthExpired }: {
    items: StorageNotification[];
    busy: boolean;
    error: string;
    canEdit: boolean;
    isOwner: boolean;
    timezone: string;
    onRefresh: () => void;
    onMarkRead: () => void;
    onConfirm: (options: ConfirmOptions) => void;
    onAuthExpired: () => void;
  } = $props();

  const unread = $derived(items.filter((item) => !item.read).length);

  function title(kind: string): string {
    return kind === 'storage_full' ? 'Database limit reached' : kind === 'storage_cleanup' ? 'Old history removed' : kind === 'storage_saving' ? 'Database storage pressure' : kind === 'storage_warning' ? 'Database nearly full' : 'Storage recovered';
  }
  function tone(kind: string): string {
    return kind === 'storage_full' ? 'danger' : kind === 'storage_warning' || kind === 'storage_saving' ? 'warning' : kind === 'storage_cleanup' ? 'info' : 'success';
  }
</script>

<article class="panel">
  <div class="panel-heading">
    <div class="title"><div><h2>Notifications</h2><span class="muted">{unread ? `${unread} unread` : 'You are all caught up'}</span></div></div>
    <div class="actions">
      <button class="button ghost small" type="button" disabled={busy} onclick={onRefresh}><Icon name="refresh" size={13} />Refresh</button>
      {#if canEdit}<button class="button ghost small" type="button" disabled={!unread || busy} onclick={onMarkRead}><Icon name="check" size={13} />Mark all read</button>{/if}
    </div>
  </div>
  <p class="intro">Storage warnings, automatic cleanup, and recovery updates appear here.</p>
  {#if error}
    <div class="inbox-error"><ActionFeedback tone="error" title="Notification request failed" message={error} /></div>
  {/if}
  <div class="list">
    {#each items as item (item.id)}
      <article class={`item ${tone(item.kind)}`} class:unread={!item.read}>
        <span class="marker">
          <Icon name={item.kind === 'storage_cleanup' ? 'trash' : tone(item.kind) === 'success' ? 'check' : 'alert-triangle'} size={16} />
        </span>
        <div class="body">
          <div class="head"><div class="item-title"><strong>{title(item.kind)}</strong>{#if !item.read}<span class="unread-label">Unread</span>{/if}</div><time datetime={item.created_at}>{formatDateTimeInTz(item.created_at, timezone)}</time></div>
          <p>{item.message}</p>
        </div>
      </article>
    {:else}
      {#if busy}
        <p class="empty" role="status">Loading notifications…</p>
      {:else if !error}
        <div class="empty">
          <span class="state-icon"><Icon name="bell" size={20} /></span>
          <strong>No storage notifications yet</strong>
        </div>
      {/if}
    {/each}
  </div>
</article>

<AlertDestinations {isOwner} {timezone} {onConfirm} {onAuthExpired} />

<style>
  .actions { display: flex; flex-wrap: wrap; gap: 8px; }
  .list { display: grid; gap: 10px; }
  .item { --tone: var(--muted); display: flex; gap: 12px; padding: 14px 16px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); }
  .item.danger { --tone: var(--danger); }
  .item.warning { --tone: var(--warning); }
  .item.info { --tone: var(--info); }
  .item.success { --tone: var(--success); }
  .item.unread { border-color: color-mix(in srgb, var(--tone) 40%, var(--line)); background: color-mix(in srgb, var(--tone) 6%, var(--surface)); }
  .marker { display: grid; place-items: center; flex: 0 0 32px; height: 32px; border-radius: var(--radius-md); color: var(--tone); background: color-mix(in srgb, var(--tone) 12%, var(--surface)); }
  .item-title { display: flex; flex-wrap: wrap; align-items: center; gap: 4px 8px; }
  .unread-label { color: var(--tone); font-size: 11px; font-weight: 650; }
  .body { flex: 1; min-width: 0; }
  .head { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 6px 12px; }
  .head strong { font-size: 13.5px; }
  time { color: var(--muted); font-size: 12px; }
  p { margin: 6px 0 0; color: var(--ink-secondary); font-size: 13px; overflow-wrap: anywhere; }
  .intro { margin: 0 0 14px; color: var(--muted); }
  .inbox-error { margin-bottom: 12px; }
  .empty { display: grid; justify-items: center; gap: 8px; margin: 0; padding: 28px 12px; border: 1px dashed var(--line); border-radius: var(--radius-md); color: var(--muted); }
</style>
