<script lang="ts">
  import Icon from '../../Icon.svelte';
  import { formatDateTimeInTz } from '../../timezone';
  import type { StorageNotification } from '../../api';

  let { items, busy, error, canEdit, timezone, onRefresh, onMarkRead }: {
    items: StorageNotification[];
    busy: boolean;
    error: string;
    canEdit: boolean;
    timezone: string;
    onRefresh: () => void;
    onMarkRead: () => void;
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
    <div class="title"><span class="section-icon"><Icon name="bell" size={16} /></span><div><h2>Notifications</h2><span class="muted">{unread ? `${unread} unread` : 'You are all caught up'}</span></div></div>
    <div class="actions">
      <button class="button ghost small" type="button" disabled={busy} onclick={onRefresh}><Icon name="refresh" size={13} />Refresh</button>
      {#if canEdit}<button class="button ghost small" type="button" disabled={!unread || busy} onclick={onMarkRead}><Icon name="check" size={13} />Mark all read</button>{/if}
    </div>
  </div>
  {#if error}<p class="form-error" role="alert">{error}</p>{/if}
  <div class="list stagger">
    {#each items as item, index (item.id)}
      <article class={`item ${tone(item.kind)}`} class:unread={!item.read} style:--i={Math.min(index, 8)}>
        <span class="marker" aria-hidden="true"></span>
        <div class="body">
          <div class="head"><strong>{title(item.kind)}</strong><time datetime={item.created_at}>{formatDateTimeInTz(item.created_at, timezone)}</time></div>
          <p>{item.message}</p>
        </div>
      </article>
    {:else}
      <div class="empty">
        <span class="state-icon positive"><Icon name="check" size={20} /></span>
        <p class="muted">{busy ? 'Loading notifications…' : 'No storage notifications yet.'}</p>
      </div>
    {/each}
  </div>
</article>

<style>
  .actions { display: flex; flex-wrap: wrap; gap: 8px; }
  .list { display: grid; gap: 10px; }
  .item { --tone: var(--muted); display: flex; gap: 12px; padding: 14px 16px; border: 1px solid var(--line); border-radius: var(--radius-md); background: var(--surface); }
  .item.danger { --tone: var(--danger); }
  .item.warning { --tone: var(--warning); }
  .item.info { --tone: var(--info); }
  .item.success { --tone: var(--success); }
  .item.unread { border-color: color-mix(in srgb, var(--tone) 40%, var(--line)); background: color-mix(in srgb, var(--tone) 6%, var(--surface)); }
  .marker { flex-shrink: 0; width: 8px; height: 8px; margin-top: 6px; border-radius: 50%; background: var(--tone); opacity: 0.35; }
  .item.unread .marker { opacity: 1; }
  .body { flex: 1; min-width: 0; }
  .head { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 6px 12px; }
  .head strong { font-size: 13.5px; }
  time { color: var(--muted); font-size: 12px; }
  p { margin: 6px 0 0; color: var(--ink-secondary); font-size: 13px; overflow-wrap: anywhere; }
  .empty { display: grid; justify-items: center; gap: 8px; padding: 28px 0; }
</style>
