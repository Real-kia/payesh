<script lang="ts">
  import Icon from '../Icon.svelte';

  let { title, message = '', tone = 'success' }: {
    title: string;
    message?: string;
    tone?: 'success' | 'error';
  } = $props();
</script>

<div class="feedback" class:error={tone === 'error'} role={tone === 'error' ? 'alert' : 'status'} aria-atomic="true">
  <span class="symbol"><Icon name={tone === 'error' ? 'alert-triangle' : 'check'} size={16} /></span>
  <div class="copy">
    <strong>{title}</strong>
    {#if message}<p>{message}</p>{/if}
  </div>
</div>

<style>
  .feedback {
    --tone: var(--success);
    --tint: var(--success-bg);
    display: flex;
    align-items: flex-start;
    gap: 12px;
    padding: 10px 14px;
    border: 1px solid color-mix(in srgb, var(--tone) 26%, var(--line));
    border-radius: var(--radius-md);
    background: color-mix(in srgb, var(--tint) 55%, var(--surface));
  }

  .feedback.error { --tone: var(--danger); --tint: var(--danger-bg); }
  .symbol {
    display: grid;
    place-items: center;
    flex: 0 0 28px;
    height: 28px;
    border-radius: 50%;
    color: var(--tone);
    background: var(--tint);
  }

  .copy { align-self: center; min-width: 0; overflow-wrap: anywhere; }
  strong { display: block; color: var(--ink); font-size: 13px; }
  p { margin: 2px 0 0; color: var(--ink-secondary); font-size: 12.5px; line-height: 1.45; }
</style>
